package quota

// The fetcher holds the last reading of each provider and the little state needed
// to keep it current.
//
// Reading is deliberately lazy. Nothing is fetched on a timer: a reading is taken
// when the UI asks for one and none is fresh, so a daemon nobody is looking at
// makes no outbound calls at all. That also keeps the whole feature inside the
// existing dashboard poll instead of adding a lifecycle of its own.
//
// Failure handling is the interesting part. A vendor that rate-limits, times out
// or misbehaves must never produce something the UI has to show, so a failed read
// keeps the previous reading and simply does not replace it. Only when the last
// good reading is older than StaleGrace does the provider drop out of the
// response — at which point it is indistinguishable from a provider that declares
// no quota at all.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"tresor/internal/config"
	"tresor/internal/oauth"
	"tresor/internal/proxy"
	"tresor/internal/store"
)

const (
	// Timeout bounds one quota request. It matches the model-discovery timeout:
	// a slow vendor must not hold a page open, and the answer is not urgent.
	Timeout = 15 * time.Second
	// RefreshTTL is how long a reading is served before it is considered stale.
	// A stale reading is still served; a refresh starts in the background.
	RefreshTTL = 5 * time.Minute
	// StaleGrace is how long a reading keeps being served after the last
	// successful one. Past it the provider is left out of the response entirely,
	// because a balance from hours ago is worse than no balance.
	StaleGrace = time.Hour
	// maxBody caps a quota reply. Balance documents are tiny; anything large is
	// not one.
	maxBody = 1 << 20
)

// TokenResolver is the part of the OAuth manager a quota read needs: a valid
// access token plus the identity headers the real client sends. It is kept as an
// interface so this package does not depend on the manager's full surface, and so
// tests can supply one.
type TokenResolver interface {
	ResolveValidToken(downstreamID string) (string, map[string]string, error)
}

// Fetcher reads provider allowances and remembers the last good reading of each.
type Fetcher struct {
	store *store.Store
	// resolver is nil on a daemon without OAuth support. An OAuth provider then
	// simply cannot be read, which shows as no reading — never as an error.
	resolver TokenResolver

	clientMu sync.RWMutex
	client   *http.Client

	mu    sync.Mutex
	cache map[string]*entry
	// tried is when each provider was last asked unsucessfully. It bounds how
	// often a broken provider is retried, so an endpoint that is down does not
	// cost a request on every poll.
	tried map[string]time.Time

	// group collapses concurrent refreshes of one provider, so several open tabs
	// asking at once produce one request rather than one each.
	group singleflight.Group

	// now is time.Now except in tests.
	now func() time.Time
}

// entry is what the fetcher remembers about one provider.
type entry struct {
	reading Reading
	// fetchedAt is when the reading's reply was read.
	fetchedAt time.Time
}

// New returns a fetcher. mode is the daemon's outbound proxy mode.
func New(s *store.Store, resolver TokenResolver, mode proxy.Mode) *Fetcher {
	f := &Fetcher{
		store:    s,
		resolver: resolver,
		cache:    map[string]*entry{},
		tried:    map[string]time.Time{},
		now:      time.Now,
	}
	f.client = buildClient(mode)
	return f
}

// buildClient mirrors the engine's outbound transport: same proxy resolution, so
// a quota read follows the same route a request would.
func buildClient(mode proxy.Mode) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               proxy.ProxyFunc(mode),
			IdleConnTimeout:     30 * time.Second,
			MaxIdleConns:        25,
			MaxIdleConnsPerHost: 5,
			DisableCompression:  true,
		},
		Timeout: Timeout,
	}
}

// SetTokenResolver attaches the OAuth manager once it exists.
func (f *Fetcher) SetTokenResolver(r TokenResolver) {
	f.mu.Lock()
	f.resolver = r
	f.mu.Unlock()
}

// SetProxyMode rebuilds the outbound client when the daemon's proxy mode changes.
func (f *Fetcher) SetProxyMode(mode proxy.Mode) {
	f.clientMu.Lock()
	f.client = buildClient(mode)
	f.clientMu.Unlock()
}

func (f *Fetcher) httpClient() *http.Client {
	f.clientMu.RLock()
	defer f.clientMu.RUnlock()
	return f.client
}

// retryAfter is how long a provider that failed is left alone before it is asked
// again. A declaration that is simply wrong must not cost a request per poll.
const retryAfter = 5 * time.Minute

// Snapshot returns every provider's current reading, and starts a background
// refresh for any that has gone stale.
//
// A provider with no reading — never fetched, or last read longer ago than
// StaleGrace — is simply absent from the map. Callers must treat a missing key as
// "this provider reports nothing", never as an error.
func (f *Fetcher) Snapshot() map[string]Reading {
	now := f.now()
	out := map[string]Reading{}
	var refresh []string

	f.mu.Lock()
	for id, e := range f.cache {
		age := now.Sub(e.fetchedAt)
		if age > StaleGrace {
			// Too old to be honest about. Dropping the entry is what makes a
			// provider that has been unreachable for an hour look unsupported.
			delete(f.cache, id)
			continue
		}
		r := e.reading
		r.Stale = age > RefreshTTL
		out[id] = r
		if r.Stale {
			refresh = append(refresh, id)
		}
	}
	f.mu.Unlock()

	// Providers that declare a quota but hold no reading are asked again, so a
	// dropped entry can come back once the vendor answers. A provider that failed
	// recently is left alone until retryAfter passes.
	for _, id := range f.unsatisfiedLocked(now) {
		refresh = append(refresh, id)
	}
	for _, id := range refresh {
		go f.refreshInBackground(id)
	}
	return out
}

// RefreshAll reads every enabled provider that declares a quota, in parallel, and
// returns the readings that succeeded.
//
// It exists for the moment a person opens the provider list, which is the one time
// a stale figure is not good enough: Snapshot alone serves what is cached and only
// schedules the missing reads, so a tab opened before the first read ever finished
// would sit blank until the next poll. Failures are dropped rather than reported —
// a provider that reports nothing is not an error, and the list says so by simply
// not drawing anything for it.
//
// A provider disabled since the last read is deliberately left alone; the UI hides
// its row, so reading it would be an outbound call nobody asked for.
func (f *Fetcher) RefreshAll(ctx context.Context) map[string]Reading {
	ds, err := f.store.ListDownstreams()
	if err != nil {
		return f.Snapshot()
	}
	var ids []string
	for i := range ds {
		if d := &ds[i]; d.IsEnabled && d.Quota != nil && !d.Quota.IsEmpty() {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return f.Snapshot()
	}

	var wg sync.WaitGroup
	wg.Add(len(ids))
	for _, id := range ids {
		go func(id string) {
			defer wg.Done()
			// Each read gets its own budget: one unreachable provider must not eat
			// the window the others were given.
			rctx, cancel := context.WithTimeout(ctx, Timeout)
			defer cancel()
			_, _ = f.readOnce(rctx, id)
		}(id)
	}
	wg.Wait()
	return f.Snapshot()
}

// unsatisfiedLocked lists providers that declare a quota and have no usable
// reading, skipping the ones asked unsuccessfully within retryAfter. It takes the
// lock itself so callers need not hold it.
func (f *Fetcher) unsatisfiedLocked(now time.Time) []string {
	ds, err := f.store.ListDownstreams()
	if err != nil {
		return nil
	}
	var out []string
	f.mu.Lock()
	for i := range ds {
		d := &ds[i]
		if d.Quota == nil || d.Quota.IsEmpty() || !d.IsEnabled {
			continue
		}
		if _, ok := f.cache[d.ID]; ok {
			continue
		}
		if at, failed := f.tried[d.ID]; failed && now.Sub(at) < retryAfter {
			continue
		}
		out = append(out, d.ID)
	}
	f.mu.Unlock()
	return out
}

// Forget drops a provider's reading, so the next look asks again. It is called
// when a downstream's quota declaration or credentials change: a reading taken
// against the old endpoint says nothing about the new one.
func (f *Fetcher) Forget(id string) {
	f.mu.Lock()
	delete(f.cache, id)
	delete(f.tried, id)
	f.mu.Unlock()
}

// Refresh reads one provider now, bypassing the cache, and records the result. It
// is what the UI's Test and ⟳ buttons call.
func (f *Fetcher) Refresh(ctx context.Context, id string) (Reading, error) {
	return f.readOnce(ctx, id)
}

// refreshInBackground reads one provider without a caller waiting. Refreshes of
// the same provider collapse, so a burst of polls makes one request.
func (f *Fetcher) refreshInBackground(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	_, _ = f.readOnce(ctx, id)
}

// readOnce performs one read, shared with any read of the same provider already in
// flight. It exists so there is exactly one place a read happens, and therefore no
// nested singleflight call.
func (f *Fetcher) readOnce(ctx context.Context, id string) (Reading, error) {
	v, err, _ := f.group.Do(id, func() (any, error) {
		return f.read(ctx, id)
	})
	if err != nil {
		return Reading{}, err
	}
	r, _ := v.(Reading)
	return r, nil
}

// read performs the request and records what came of it.
func (f *Fetcher) read(ctx context.Context, id string) (Reading, error) {
	ds, err := f.store.GetDownstream(id)
	if err != nil {
		f.noteFailure(id)
		return Reading{}, err
	}
	if ds.Quota == nil || ds.Quota.IsEmpty() {
		// Nothing to ask. Not an error: this provider simply reports nothing.
		return Reading{}, errNoDeclaration
	}
	body, err := f.fetch(ctx, ds)
	if err != nil {
		f.noteFailure(id)
		return Reading{}, err
	}
	now := f.now()
	reading, ok := Build(ds.Quota, body, now)
	if !ok {
		// The vendor answered, but with nothing this declaration can read: no plan,
		// a changed shape, an empty object. Kept as a failure so the previous
		// reading stands rather than being replaced by an empty card.
		f.noteFailure(id)
		return Reading{}, errNothingToShow
	}
	f.mu.Lock()
	f.cache[id] = &entry{reading: reading, fetchedAt: now}
	delete(f.tried, id)
	f.mu.Unlock()
	return reading, nil
}

// noteFailure records that a provider could not be read, so it is not retried on
// every poll. The previous reading, if any, is left exactly as it was.
func (f *Fetcher) noteFailure(id string) {
	f.mu.Lock()
	f.tried[id] = f.now()
	f.mu.Unlock()
}

// errNoDeclaration and errNothingToShow are internal: they never reach a browser,
// because a failed read is reported as the previous reading or as nothing at all.
var (
	errNoDeclaration = errors.New("this provider declares no quota")
	errNothingToShow = errors.New("the reply holds nothing this declaration reads")
)

// fetch performs the authenticated GET a quota declaration describes.
//
// Credentials come from the downstream's own auth block, resolved exactly as a
// model-list fetch resolves them: an OAuth token with its identity headers, or
// the API key plus any declared extra headers. That is what lets a relay gated on
// an identity header be described declaratively.
func (f *Fetcher) fetch(ctx context.Context, ds *store.Downstream) ([]byte, error) {
	origin := urlOrigin(effectiveBaseURL(ds))
	target, err := ds.Quota.ResolveURL(origin)
	if err != nil {
		return nil, err
	}

	token, headers, err := f.credentials(ctx, ds)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := f.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("quota endpoint answered %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, errors.New("quota endpoint answered with nothing")
	}
	return body, nil
}

// credentials resolves the downstream's auth into a token and the extra headers
// the real client would send.
func (f *Fetcher) credentials(ctx context.Context, ds *store.Downstream) (string, map[string]string, error) {
	if ds.IsOAuth() {
		f.mu.Lock()
		resolver := f.resolver
		f.mu.Unlock()
		if resolver == nil {
			return "", nil, errors.New("OAuth is not available")
		}
		token, headers, err := resolver.ResolveValidToken(ds.ID)
		if err != nil {
			return "", nil, err
		}
		return token, headers, nil
	}
	token := ds.EffectiveAPIKey()
	if ds.Auth == nil || len(ds.Auth.ExtraHeaders) == 0 {
		return token, nil, nil
	}
	// Declared headers apply to api_key providers too. A claim resolves to nothing
	// without a token to read it from, which is the same rule a model fetch uses.
	resolved, err := oauth.ResolveHeaderValues(ds.Auth.ExtraHeaders, token, nil)
	if err != nil {
		return token, nil, nil
	}
	return token, resolved, nil
}

// effectiveBaseURL is the base URL a downstream's requests go to: the primary
// format's override when it has one, else the plain base. A relative quota URL is
// resolved against this, so a region install asks its own host.
func effectiveBaseURL(ds *store.Downstream) string {
	if len(ds.ApiFormats) > 0 && ds.FormatURLs != nil {
		if u := ds.FormatURLs[ds.ApiFormats[0]]; u != "" {
			return u
		}
	}
	return ds.BaseURL
}

// urlOrigin is scheme plus host of a URL, for resolving a root-relative path. An
// unparsable URL yields "", which makes a relative quota declaration an error
// rather than a request to nowhere.
func urlOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// Discover asks an endpoint what it reports, so a declaration can be filled in
// without the user writing a path.
//
// The endpoint is called with whatever credentials the downstream already has, and
// the reply's numeric fields are ranked. A single unambiguous figure is not
// returned as a choice at all — it is written into the stored declaration, so the
// common case needs no decision and the balance simply appears. Several figures
// come back as a ranked list for the UI to offer, because picking for the user is
// where a guess would become a wrong number.
func (f *Fetcher) Discover(ctx context.Context, id, endpoint string) (DiscoverResult, error) {
	ds, err := f.store.GetDownstream(id)
	if err != nil {
		return DiscoverResult{}, err
	}
	// The endpoint comes from the caller, not from a saved declaration: an
	// endpoint on its own is not a valid declaration (it would read nothing), so
	// requiring it to be stored first would make discovery unreachable from an
	// empty editor. The complete declaration is written in one step below.
	if strings.TrimSpace(endpoint) == "" {
		if ds.Quota == nil || ds.Quota.URL == "" {
			return DiscoverResult{}, errNoDeclaration
		}
		endpoint = ds.Quota.URL
	}
	probe := *ds
	probe.Quota = &config.QuotaCfg{URL: endpoint}
	body, err := f.fetch(ctx, &probe)
	if err != nil {
		return DiscoverResult{}, err
	}
	candidates := withCurrency(body, Discover(body))
	if len(candidates) == 0 {
		return DiscoverResult{}, errNothingToShow
	}

	// One clear winner: adopt it. This is what makes the feature invisible — the
	// user pastes an endpoint, and a balance appears.
	if len(candidates) == 1 || candidates[0].score-candidates[1].score >= 25 {
		next := config.QuotaCfg{URL: endpoint}
		if ds.Quota != nil {
			next = *ds.Quota
			next.URL = endpoint
		}
		next.Balance = config.QuotaPath(candidates[0].Path)
		if next.Currency == "" && candidates[0].Currency != "" {
			next.Currency = candidates[0].Currency
		}
		if err := next.Validate("quota"); err == nil {
			ds.Quota = &next
			if err := f.store.UpdateDownstream(ds); err != nil {
				return DiscoverResult{}, err
			}
			f.Forget(id)
			reading, err := f.readOnce(ctx, id)
			return DiscoverResult{Adopted: true, Reading: &reading}, err
		}
	}
	return DiscoverResult{Candidates: candidates}, nil
}

// DiscoverResult is what a discovery run found.
type DiscoverResult struct {
	// Adopted means the daemon filled the declaration in itself, and Reading is
	// the result: no choice was needed.
	Adopted bool `json:"adopted,omitempty"`
	// Reading is present when a balance was adopted and read.
	Reading *Reading `json:"reading,omitempty"`
	// Candidates is a ranked choice, present when the reply held several plausible
	// figures. A picker offers these; the user never sees a path.
	Candidates []Candidate `json:"candidates,omitempty"`
}
