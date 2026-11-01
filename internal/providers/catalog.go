package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"tresor/internal/proxy"
)

// The provider catalog is an online index of installable provider profiles. It
// has exactly two sources: the remote index, and a local cache of the last
// successful fetch. There is deliberately no compiled-in copy — a seed baked
// into the binary would be a second, silently diverging definition of what is
// installable, and it would keep offering providers whose upstream profile has
// since been withdrawn.
//
// Beyond those sources it has what the icon index (internal/icons/index.go) has:
// an env override, a cache directory, an age limit, a cooldown after a failure,
// singleflight so concurrent callers share one fetch, and a proxy-aware client.
//
// It lives in the daemon rather than in a client so the CLI and the web UI see
// one catalog, and so the maintainer can add or repair a provider without a
// Tresor release.

const (
	// DefaultCatalogURL is where the official index lives.
	DefaultCatalogURL = "https://raw.githubusercontent.com/Ladbaby/Tresor-providers/main/index.json"

	// CatalogEnv overrides DefaultCatalogURL. "off" disables the catalog
	// entirely, which is what an air-gapped or audited deployment wants.
	CatalogEnv = "TRESOR_PROVIDER_CATALOG"

	catalogFileName = "catalog.json"
	catalogMaxAge   = 24 * time.Hour
	catalogTimeout  = 20 * time.Second
	catalogCooldown = time.Hour
	catalogMaxBytes = 2 << 20
	catalogSchema   = "tresor-provider-catalog-v1"
)

// CatalogEntry is one provider the catalog offers. The index stays small by
// carrying a URL to the full profile rather than the profile itself.
type CatalogEntry struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Icon    string            `json:"icon,omitempty"`
	Version int               `json:"version"`
	Summary map[string]string `json:"summary,omitempty"`
	// URL is the HTTPS location of the full profile.
	URL string `json:"url"`
	// Digest is the canonical profile digest, when the index publishes one.
	// Checked after download so a changed profile cannot masquerade as the
	// reviewed one.
	Digest string `json:"digest,omitempty"`
	// Features are the host features the profile needs, mirrored from the
	// profile so the UI can mark an entry unavailable without downloading it.
	Features   []string `json:"features,omitempty"`
	MinVersion string   `json:"min_version,omitempty"`
	// NeedsPlugins mirrors whether the profile binds plugins, so the UI can ask
	// for consent before downloading anything.
	NeedsPlugins bool `json:"needs_plugins,omitempty"`
}

// catalogFile is the on-disk and wire shape of the index.
type catalogFile struct {
	Schema  string         `json:"schema"`
	Updated time.Time      `json:"updated,omitempty"`
	Entries []CatalogEntry `json:"entries"`
}

// Availability explains whether an entry can be installed on this daemon.
type Availability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Catalog caches the provider index and knows how to fetch profiles.
type Catalog struct {
	mu       sync.RWMutex
	data     catalogFile
	ready    bool
	source   string
	lastErr  error
	cooldown time.Time

	cacheDir string
	log      *log.Logger
	client   *http.Client

	// indexURL and features are fields so tests can override them.
	indexURL string
	features []string
	version  string

	sf singleflight.Group
}

// NewCatalog returns a catalog backed by cacheDir. features and version
// describe this daemon so entries that need something it lacks are reported as
// unavailable with a specific reason.
func NewCatalog(cacheDir string, mode proxy.Mode, features []string, version string, logger *log.Logger) *Catalog {
	if logger == nil {
		logger = log.New(os.Stderr, "providers: ", log.LstdFlags)
	}
	c := &Catalog{
		cacheDir: cacheDir,
		log:      logger,
		client:   catalogClient(mode),
		indexURL: catalogURL(),
		features: features,
		version:  version,
	}
	c.data = catalogFile{Entries: []CatalogEntry{}}
	// A cached index makes the catalog usable before any network call, and keeps
	// it usable when the network is not.
	if err := c.loadCache(); err != nil {
		c.log.Printf("warning: provider catalog cache unusable: %v", err)
	}
	// A disabled catalog is empty but answerable: the UI shows "no catalog"
	// rather than waiting forever for entries that will never arrive.
	if c.indexURL == "" {
		c.ready = true
		c.source = "disabled"
	}
	return c
}

// catalogURL resolves the effective index URL, honouring CatalogEnv.
func catalogURL() string {
	v := strings.TrimSpace(os.Getenv(CatalogEnv))
	if v == "" {
		return DefaultCatalogURL
	}
	if strings.EqualFold(v, "off") {
		return ""
	}
	return v
}

func catalogClient(mode proxy.Mode) *http.Client {
	return &http.Client{
		Timeout: catalogTimeout,
		Transport: &http.Transport{
			Proxy:           proxy.ProxyFunc(mode),
			IdleConnTimeout: 30 * time.Second,
		},
	}
}

// SetProxyMode rebuilds the client so a live proxy_mode change takes effect.
func (c *Catalog) SetProxyMode(mode proxy.Mode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = catalogClient(mode)
}

// SetClient overrides the HTTP client. Intended for tests, which use it to
// trust a local TLS server: profile URLs are required to be HTTPS, and that rule
// is deliberately not relaxed for testing.
func (c *Catalog) SetClient(client *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = client
}

// SetIndexURL overrides the index URL. Intended for tests.
func (c *Catalog) SetIndexURL(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.indexURL = u
}

// loadCache reads the cached index when it is still fresh enough.
func (c *Catalog) loadCache() error {
	if c.cacheDir == "" {
		return nil
	}
	path := filepath.Join(c.cacheDir, catalogFileName)
	info, err := os.Stat(path)
	if err != nil {
		return nil // absent is normal
	}
	if time.Since(info.ModTime()) > catalogMaxAge {
		return nil // stale: the caller will refresh
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	if f.Schema != catalogSchema {
		return fmt.Errorf("unsupported catalog schema %q", f.Schema)
	}
	if f.Entries == nil {
		f.Entries = []CatalogEntry{}
	}
	c.mu.Lock()
	c.data = f
	c.ready = true
	c.source = "cache"
	c.mu.Unlock()
	return nil
}

// Ready reports whether the catalog has usable entries.
func (c *Catalog) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// LastError reports why the most recent refresh failed, or nil. The UI needs this:
// an empty catalog with no reason is indistinguishable from a broken one.
func (c *Catalog) LastError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastErr
}

// Source reports where the current entries came from: cache, remote, or disabled
// when the catalog was turned off. Empty means nothing has been fetched yet.
func (c *Catalog) Source() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.source
}

// Entries returns the catalog entries with their availability on this daemon,
// sorted by name for a stable UI.
func (c *Catalog) Entries() []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CatalogEntry, len(c.data.Entries))
	copy(out, c.data.Entries)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Entry returns one entry by id.
func (c *Catalog) Entry(id string) (CatalogEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.data.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

// Availability reports whether this daemon can install the entry, and why not
// when it cannot. This is what turns "your build is too old" into a specific
// message instead of a feature-rejection error at install time.
func (c *Catalog) Availability(e CatalogEntry) Availability {
	var missing []string
	for _, want := range e.Features {
		if !containsFold(c.features, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return Availability{
			Reason: fmt.Sprintf("requires %s, which this Tresor build does not provide; update Tresor to install it",
				strings.Join(missing, ", ")),
		}
	}
	// A development build reports a git revision rather than a release version,
	// so the minimum-version check cannot be answered. The feature check above
	// stays authoritative; this one is skipped rather than silently satisfied.
	if e.MinVersion != "" {
		if _, ok := parseVersion(c.version); ok && !versionAtLeast(c.version, e.MinVersion) {
			return Availability{
				Reason: fmt.Sprintf("requires Tresor %s or newer (this build is %s)", e.MinVersion, c.version),
			}
		}
	}
	return Availability{Available: true}
}

// Refresh fetches the remote index. A failure is reported but never discards
// entries already known, so a transient network problem does not empty the
// catalog.
//
// The cooldown is a guard against callers that ask repeatedly, so it applies here.
// A caller that owns its own retry schedule uses RefreshNow.
func (c *Catalog) Refresh(ctx context.Context) error {
	c.mu.Lock()
	if time.Now().Before(c.cooldown) {
		c.mu.Unlock()
		return fmt.Errorf("provider catalog refresh is cooling down after a recent failure")
	}
	c.mu.Unlock()
	return c.refreshLocked(ctx)
}

// RefreshNow refreshes without consulting the cooldown. It exists for the daemon's
// background retry loop: the catalog has no compiled-in fallback, so that loop is
// the only thing that can recover an empty catalog, and it already spaces its own
// attempts. Without this, one transient failure left the provider list empty for a
// full hour with nothing able to retry it.
func (c *Catalog) RefreshNow(ctx context.Context) error {
	return c.refreshLocked(ctx)
}

func (c *Catalog) refreshLocked(ctx context.Context) error {
	c.mu.RLock()
	url := c.indexURL
	c.mu.RUnlock()

	if url == "" {
		return fmt.Errorf("provider catalog is disabled by %s=off", CatalogEnv)
	}

	v, err, _ := c.sf.Do(url, func() (any, error) {
		return c.fetchIndex(ctx, url)
	})
	if err != nil {
		c.mu.Lock()
		c.lastErr = err
		c.cooldown = time.Now().Add(catalogCooldown)
		c.mu.Unlock()
		return err
	}
	f := v.(catalogFile)

	c.mu.Lock()
	c.data = f
	c.ready = true
	c.source = "remote"
	c.lastErr = nil
	c.mu.Unlock()

	c.writeCache(f)
	return nil
}

func (c *Catalog) fetchIndex(ctx context.Context, indexURL string) (catalogFile, error) {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return catalogFile{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return catalogFile{}, fmt.Errorf("fetch catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return catalogFile{}, fmt.Errorf("fetch catalog: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogMaxBytes+1))
	if err != nil {
		return catalogFile{}, err
	}
	if len(body) > catalogMaxBytes {
		return catalogFile{}, fmt.Errorf("catalog exceeds %d bytes", catalogMaxBytes)
	}
	var f catalogFile
	if err := json.Unmarshal(body, &f); err != nil {
		return catalogFile{}, fmt.Errorf("parse catalog: %w", err)
	}
	if f.Schema != catalogSchema {
		return catalogFile{}, fmt.Errorf("unsupported catalog schema %q", f.Schema)
	}
	if f.Entries == nil {
		f.Entries = []CatalogEntry{}
	}
	for i, e := range f.Entries {
		if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.URL) == "" {
			return catalogFile{}, fmt.Errorf("catalog entry %d needs id and url", i)
		}
	}
	return f, nil
}

// writeCache stores the index for the next start. Failure is logged, not
// fatal: the catalog still works from memory.
func (c *Catalog) writeCache(f catalogFile) {
	if c.cacheDir == "" {
		return
	}
	if err := os.MkdirAll(c.cacheDir, 0o700); err != nil {
		c.log.Printf("warning: cannot create provider catalog cache dir: %v", err)
		return
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(c.cacheDir, catalogFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		c.log.Printf("warning: cannot write provider catalog cache: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

// FetchProfile downloads and validates one entry's profile, verifying the
// digest the index published for it.
func (c *Catalog) FetchProfile(ctx context.Context, id string) (*Profile, string, error) {
	entry, ok := c.Entry(id)
	if !ok {
		return nil, "", fmt.Errorf("provider %q is not in the catalog", id)
	}
	if avail := c.Availability(entry); !avail.Available {
		return nil, "", fmt.Errorf("provider %q cannot be installed: %s", id, avail.Reason)
	}
	u, err := url.Parse(entry.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, "", fmt.Errorf("provider %q has an unsafe profile URL", id)
	}

	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.URL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch provider %q: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch provider %q: unexpected status %d", id, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogMaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > catalogMaxBytes {
		return nil, "", fmt.Errorf("provider profile exceeds %d bytes", catalogMaxBytes)
	}

	profile, err := Parse(body)
	if err != nil {
		return nil, "", err
	}
	if profile.ID != entry.ID {
		return nil, "", fmt.Errorf("catalog entry %q served a profile for %q", entry.ID, profile.ID)
	}
	digest, err := profile.Digest()
	if err != nil {
		return nil, "", err
	}
	// When the index publishes a digest, the downloaded profile must match it:
	// that is what stops a silently changed profile from passing as reviewed.
	if entry.Digest != "" && !strings.EqualFold(entry.Digest, digest) {
		return nil, "", fmt.Errorf("provider %q failed its digest check (index %s, downloaded %s)", id, entry.Digest, digest)
	}
	return profile, digest, nil
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// versionAtLeast reports whether a >= b, comparing dotted numeric segments and
// ignoring a leading "v" and any pre-release or build suffix, so "0.14.0" >=
// "0.14" holds. It reports false when either side is not a release version.
func versionAtLeast(have, want string) bool {
	a, okA := parseVersion(have)
	b, okB := parseVersion(want)
	if !okA || !okB {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return true
}

// parseVersion parses a release version such as "0.14.0" or "v1.2.3-rc.1".
//
// It is deliberately strict. A development build reports a git revision (for
// example "24059cb-dirty"), and a lenient parse would read that as major version
// 24059 and report any minimum version as satisfied. Refusing to parse keeps
// that from silently disabling the check.
func parseVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// Drop a pre-release or build suffix; only the numeric part is compared.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return nil, false
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, false
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out, true
}
