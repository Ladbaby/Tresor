package icons

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tresor/internal/proxy"
)

const (
	// maxIconBytes caps the size of any single icon we will accept from the CDN.
	// A typical SVG icon is < 10 KB; 256 KB leaves generous headroom.
	maxIconBytes = 256 * 1024

	// defaultFetchTimeout is the per-request HTTP timeout for icon fetches.
	defaultFetchTimeout = 10 * time.Second

	// memCacheSize bounds the in-memory icon LRU.
	memCacheSize = 100

	// maxRetries is the maximum number of retries for transient HTTP errors.
	maxRetries = 3

	// retryBaseDelay is the initial delay before the first retry.
	retryBaseDelay = 200 * time.Millisecond

	// iconConcurrency caps how many in-flight icon fetches the fetcher
	// will issue at once. Combined with the rate limiter in iconRate,
	// this keeps the cold-start burst from hammering the CDN.
	iconConcurrency = 4

	// defaultRetryAfterFallback is the wait used when the CDN returns
	// 503 without a Retry-After header. jsDelivr historically throttles
	// to ~5s windows during incidents.
	defaultRetryAfterFallback = 5 * time.Second

	// maxRetryAfter caps any parsed Retry-After value.
	maxRetryAfter = 60 * time.Second
)

// Fetcher resolves model IDs to SVG icon bytes, fetching from a public CDN on
// first miss and caching the result in memory and on disk. Concurrent fetches
// for the same URL are collapsed via a singleflight.
type Fetcher struct {
	cacheDir string
	client   *http.Client
	log      *log.Logger
	cdnURL   func(slug string) string // overridable for tests; defaults to CDNURL

	mem  *memLRU  // successful fetches
	miss *memLRU  // CDN 404s we have already confirmed — avoids hammering the CDN for known-missing slugs within a session

	// sfMu guards sfMap. Each inflight entry represents one in-progress
	// network call; later callers for the same URL block on its done channel
	// instead of issuing their own request.
	sfMu  sync.Mutex
	sfMap map[string]*inflight

	// index is the local CDN slug catalog. When Ready(), the Icon() loop
	// skips slugs not present in the index instead of fetching them.
	index *Index

	// iconRate is the steady-state + warm-start throttle.
	iconRate *rateLimiter

	// iconSem bounds concurrent fetches across all URLs (per-process).
	iconSem chan struct{}

	// proxyMode is captured so SetProxyMode can also rebuild the index's
	// http.Client.
	proxyMode proxy.Mode

	// warmingOnce logs the "index not ready, falling back" warning at
	// most once per process lifetime.
	warmingOnce sync.Once
}

// inflight is the in-flight state of a single URL fetch.
type inflight struct {
	done chan struct{}
	data []byte
	ct   string
	miss bool   // true when the CDN has no file at this URL (404); callers fall through
	err  error
}

// NewWithProxyMode constructs a Fetcher whose outbound HTTP client honors the
// given proxy mode. The cache directory is created if missing.
func NewWithProxyMode(cacheDir string, mode proxy.Mode) (*Fetcher, error) {
	transport := &http.Transport{
		Proxy:               proxy.ProxyFunc(mode),
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConns:        25,
		MaxIdleConnsPerHost: 5,
		DisableCompression:  true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   defaultFetchTimeout,
	}
	return New(cacheDir, client, mode)
}

// New constructs a Fetcher with a pre-built HTTP client (useful for tests
// that need a custom Transport). proxyMode defaults to none; pass an
// explicit mode from NewWithProxyMode in production so live /api/config
// PUTs can also rebuild the index's HTTP client.
func New(cacheDir string, client *http.Client, proxyMode ...proxy.Mode) (*Fetcher, error) {
	if cacheDir == "" {
		return nil, fmt.Errorf("icons: cacheDir is required")
	}
	if client == nil {
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("icons: create cache dir: %w", err)
	}
	mode := proxy.ModeNone
	if len(proxyMode) > 0 {
		mode = proxyMode[0]
	}
	logger := log.New(os.Stderr, "icons: ", log.LstdFlags|log.Lmicroseconds)
	f := &Fetcher{
		cacheDir:  cacheDir,
		client:    client,
		log:       logger,
		mem:       newMemLRU(memCacheSize),
		miss:      newMemLRU(memCacheSize),
		sfMap:     make(map[string]*inflight),
		cdnURL:    CDNURL,
		iconRate:  newRateLimiter(),
		iconSem:   make(chan struct{}, iconConcurrency),
		proxyMode: mode,
	}
	// The index needs its own HTTP client (longer timeout) but should
	// honor the same proxy mode.
	idxClient := &http.Client{
		Transport: &http.Transport{
			Proxy:           proxy.ProxyFunc(mode),
			IdleConnTimeout: 30 * time.Second,
		},
		Timeout: indexSyncTimeout,
	}
	f.index = newIndex(cacheDir, mode, idxClient, logger)
	// Load any cached index synchronously (fast: ~100KB disk read).
	if err := f.index.Load(); err != nil {
		logger.Printf("icons: index load: %v", err)
	}
	// Kick off an async refresh if the loaded index is stale or missing.
	f.index.MaybeSync()
	return f, nil
}

// SetCDNURL overrides the CDN URL builder. Intended for tests; production
// code uses the default (CDNURL).
func (f *Fetcher) SetCDNURL(fn func(slug string) string) {
	f.cdnURL = fn
}

// SetProxyMode rebuilds the http.Client Transport with the given proxy mode.
// The current in-flight fetches are not interrupted; new fetches use the new
// Transport. This is wired up so /api/config PUTs propagate live.
func (f *Fetcher) SetProxyMode(mode proxy.Mode) {
	transport := &http.Transport{
		Proxy:               proxy.ProxyFunc(mode),
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConns:        25,
		MaxIdleConnsPerHost: 5,
		DisableCompression:  true,
	}
	f.client = &http.Client{
		Transport: transport,
		Timeout:   defaultFetchTimeout,
	}
	f.proxyMode = mode
	if f.index != nil {
		f.index.SetProxyMode(mode)
	}
}

// RefreshIndex triggers an out-of-band index sync. Used by the
// /api/icons/refresh admin endpoint.
func (f *Fetcher) RefreshIndex(ctx context.Context) (string, int, error) {
	if f.index == nil {
		return "", 0, fmt.Errorf("icons: index not initialized")
	}
	v, n, err := f.index.Sync(ctx)
	if err == nil {
		// Reset the warm-start window so the next 30 fetches get paced.
		f.iconRate.activateWarmStart()
	}
	return v, n, err
}

// StartPeriodicRefresh launches a background goroutine that syncs the
// index on the configured interval. The returned function cancels the
// goroutine; callers should defer it on shutdown.
func (f *Fetcher) StartPeriodicRefresh(ctx context.Context) func() {
	if f.index == nil {
		return func() {}
	}
	return f.index.StartPeriodic(ctx)
}

// Icon returns the icon bytes and content type for the given model ID.
// If no candidate slug can be resolved (neither a pattern match nor the
// first-segment fallback), returns ("", "", nil) — the caller should treat
// this as "no icon available" and respond with 404.
//
// Resolution order:
//  1. Pattern table (e.g. "gpt-4o" -> "openai").
//  2. First-segment fallback: take the substring before the first "-",
//     lowercase, and use it as a literal slug (e.g. "MiniMax-M2.5" ->
//     "minimax"). This catches brand-new vendors not yet in the table.
//
// For each candidate slug, we go through the unified mem + disk + singleflight
// + fetch path. If a candidate 404s at the CDN, we fall through to the next
// candidate WITHOUT caching the negative result — a transient gap in the CDN
// catalog should not break icons permanently.
//
// Index integration: when the local Index is Ready, candidates not in the
// index are skipped silently — no fetch, no log line. This eliminates the
// 503-flood that occurs when the CDN is degraded, since "unknown slug"
// never reaches the network. While the index is warming up (first start
// before the first sync completes), candidates are tried unfiltered; a
// one-shot warning is logged.
func (f *Fetcher) Icon(modelID string) ([]byte, string, error) {
	candidates := CandidateSlugs(modelID)
	if len(candidates) == 0 {
		return nil, "", nil
	}

	// Filter against the local index when it's ready. This is the main
	// defense against CDN-outage log floods.
	if f.index != nil && f.index.Ready() {
		filtered := candidates[:0]
		for _, s := range candidates {
			if f.index.Contains(s) {
				filtered = append(filtered, s)
			}
		}
		candidates = filtered
		if len(candidates) == 0 {
			// Index says none of these slugs exist on the CDN. Quietly
			// return no-icon; the HTTP handler will serve the dummy.
			return nil, "", nil
		}
	} else if f.index != nil {
		// Index exists but hasn't completed its first sync. Log once
		// and fall through with unfiltered candidates so the user still
		// sees icons during the warm-up window.
		f.warmingOnce.Do(func() {
			f.log.Printf("icons: index not ready, falling back to direct CDN fetches until first sync completes")
		})
		f.index.MaybeSync()
	}

	for _, slug := range candidates {
		if slug == "" {
			continue
		}
		url := f.cdnURL(slug)

		// In-memory cache lookup
		if data, ct, ok := f.mem.Get(url); ok {
			return data, ct, nil
		}
		// Skip if we've already confirmed this slug is missing this session
		if _, _, ok := f.miss.Get(url); ok {
			continue
		}

		// Token-bucket pacing — caps steady-state rate.
		if err := f.iconRate.wait(context.Background()); err != nil {
			return nil, "", err
		}
		// Concurrency cap — at most iconConcurrency fetches in flight.
		select {
		case f.iconSem <- struct{}{}:
		default:
			// All slots busy; wait for one. We can't return without a
			// result, and we don't want to issue a free unbounded fetch.
			f.iconSem <- struct{}{}
		}
		data, ct, miss, err := f.singleflightDo(url)
		<-f.iconSem
		if err != nil {
			return nil, "", err
		}
		if miss {
			// Remember the miss for this session so subsequent requests
			// for the same model ID don't repeatedly hit the CDN. We do
			// NOT write to disk — a CDN upload of this slug later should
			// resolve without manual cache invalidation.
			f.miss.Put(url, nil, "")
			continue
		}
		f.mem.Put(url, data, ct)
		return data, ct, nil
	}
	return nil, "", nil
}

// singleflightDo ensures only one goroutine actually fetches a given URL;
// concurrent callers block on the same inflight call.
//
// Returns (data, ct, miss, err):
//   - data != nil and err == nil on success
//   - miss == true when the CDN returned 404 for this URL; the caller should
//     try the next candidate slug WITHOUT caching
//   - err != nil for transport or non-404 HTTP errors
func (f *Fetcher) singleflightDo(url string) ([]byte, string, bool, error) {
	f.sfMu.Lock()
	if existing, ok := f.sfMap[url]; ok {
		f.sfMu.Unlock()
		<-existing.done
		return existing.data, existing.ct, existing.miss, existing.err
	}
	c := &inflight{done: make(chan struct{})}
	f.sfMap[url] = c
	f.sfMu.Unlock()

	defer func() {
		f.sfMu.Lock()
		delete(f.sfMap, url)
		f.sfMu.Unlock()
		close(c.done)
	}()

	// Try disk cache first
	if data, ct, ok := f.readDisk(url); ok {
		c.data, c.ct = data, ct
		return data, ct, false, nil
	}

	// Fetch from CDN
	data, ct, miss, err := f.fetch(url)
	if err != nil {
		c.err = err
		return nil, "", false, err
	}
	if miss {
		// Negative result — do NOT cache to disk. A later CDN add should
		// still resolve on a future request.
		c.miss = true
		return nil, "", true, nil
	}

	// Persist to disk (best-effort; failure to write is logged but not fatal)
	if werr := f.writeDisk(url, data, ct); werr != nil {
		f.log.Printf("write cache for %s: %v", url, werr)
	}

	c.data, c.ct = data, ct
	return data, ct, false, nil
}

func (f *Fetcher) fetch(url string) ([]byte, string, bool, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := retryBaseDelay * time.Duration(1<<(attempt-1))
			if d := retryAfterFromErr(lastErr); d > delay {
				delay = d
			}
			if delay > maxRetryAfter {
				delay = maxRetryAfter
			}
			time.Sleep(delay)
		}

		data, ct, miss, err := f.doFetch(url)
		if err == nil {
			return data, ct, miss, nil
		}

		lastErr = err

		// Retryable: network errors and 5xx server errors.
		isRetryable := false
		if attempt < maxRetries {
			if strings.Contains(err.Error(), "context deadline exceeded") {
				isRetryable = true
			} else if strings.Contains(err.Error(), "status 503") {
				isRetryable = true
			} else if strings.Contains(err.Error(), "status 5") { // 500-599
				isRetryable = true
			}
		}

		if !isRetryable {
			return nil, "", false, err
		}
	}
	return nil, "", false, lastErr
}

// errRetryAfter wraps an underlying error with the duration the CDN
// asked us to wait. retryAfterFromErr extracts it; defaultRetryAfterFallback
// is used when no header was sent.
type errRetryAfter struct {
	err error
	d   time.Duration
}

func (e *errRetryAfter) Error() string { return e.err.Error() }
func (e *errRetryAfter) Unwrap() error { return e.err }

func retryAfterFromErr(err error) time.Duration {
	var ra *errRetryAfter
	if errors.As(err, &ra) {
		return ra.d
	}
	return 0
}

// retryAfterWait parses an HTTP Retry-After header value and returns the
// duration to wait. Returns 0 for missing/empty input (caller should NOT
// sleep). Returns defaultRetryAfterFallback when the value can't be parsed
// as integer seconds (matches the conservative behavior the spec suggests).
func retryAfterWait(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil || secs < 0 {
		return defaultRetryAfterFallback
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

func (f *Fetcher) doFetch(url string) ([]byte, string, bool, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "image/svg+xml,*/*;q=0.8")
	req.Header.Set("User-Agent", "tresor-icon-fetcher/1.0")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	// 404 (and friends) signals that the CDN has no icon at this slug.
	// Treat it as a non-error miss so the Icon() loop can fall through to
	// the next candidate rather than surfacing a hard error to the caller.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, "", true, nil
	}
	if resp.StatusCode != http.StatusOK {
		baseErr := fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
		if d := retryAfterWait(resp.Header.Get("Retry-After")); d > 0 {
			return nil, "", false, &errRetryAfter{err: baseErr, d: d}
		}
		return nil, "", false, baseErr
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "image/svg") {
		return nil, "", false, fmt.Errorf("unexpected content-type %q for %s", ct, url)
	}

	// LimitReader guards against a hostile or buggy CDN sending an unbounded body.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIconBytes+1))
	if err != nil {
		return nil, "", false, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxIconBytes {
		return nil, "", false, fmt.Errorf("icon too large (>%d bytes) for %s", maxIconBytes, url)
	}

	if ct == "" {
		ct = "image/svg+xml"
	}
	return body, ct, false, nil
}

// diskName returns the cache filename for a given URL.
func (f *Fetcher) diskName(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

func (f *Fetcher) svgPath(url string) string {
	return filepath.Join(f.cacheDir, f.diskName(url)+".svg")
}

func (f *Fetcher) metaPath(url string) string {
	return filepath.Join(f.cacheDir, f.diskName(url)+".meta")
}

// readDisk returns the cached bytes for a URL if present, valid, and small.
func (f *Fetcher) readDisk(url string) ([]byte, string, bool) {
	svgPath := f.svgPath(url)
	info, err := os.Stat(svgPath)
	if err != nil || info.Size() == 0 || info.Size() > maxIconBytes {
		return nil, "", false
	}
	data, err := os.ReadFile(svgPath)
	if err != nil {
		return nil, "", false
	}

	// Best-effort sidecar read for the original content-type
	ct := "image/svg+xml"
	if mf, err := os.ReadFile(f.metaPath(url)); err == nil {
		var meta struct {
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
		}
		if json.Unmarshal(mf, &meta) == nil && meta.ContentType != "" {
			ct = meta.ContentType
		}
	}
	return data, ct, true
}

// writeDisk atomically writes the SVG bytes and a small JSON sidecar.
func (f *Fetcher) writeDisk(url string, data []byte, ct string) error {
	svgPath := f.svgPath(url)
	metaPath := f.metaPath(url)

	// Atomic write: temp file + rename
	tmp, err := os.CreateTemp(f.cacheDir, f.diskName(url)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, svgPath); err != nil {
		return err
	}

	meta := struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
		Fetched     string `json:"fetched"`
	}{
		URL:         url,
		ContentType: ct,
		Fetched:     time.Now().UTC().Format(time.RFC3339),
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, mb, 0o644)
}

// memLRU is a tiny fixed-capacity LRU keyed by string. We do not pull in a
// third-party LRU library for a 100-entry map. Eviction is FIFO (O(n) on
// insert when full, but n=100 is trivial).
type memLRU struct {
	mu    sync.Mutex
	cap   int
	items map[string]memEntry
	order []string
}

type memEntry struct {
	data []byte
	ct   string
}

func newMemLRU(capacity int) *memLRU {
	return &memLRU{
		cap:   capacity,
		items: make(map[string]memEntry, capacity),
	}
}

func (l *memLRU) Get(key string) ([]byte, string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.items[key]
	if !ok {
		return nil, "", false
	}
	return e.data, e.ct, true
}

func (l *memLRU) Put(key string, data []byte, ct string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.items[key]; !exists {
		if len(l.items) >= l.cap {
			// Evict oldest entry (FIFO).
			oldest := l.order[0]
			l.order = l.order[1:]
			delete(l.items, oldest)
		}
		l.order = append(l.order, key)
	}
	l.items[key] = memEntry{data: data, ct: ct}
}
