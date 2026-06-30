package icons

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tresor/internal/proxy"
	"golang.org/x/sync/singleflight"
)

const (
	indexMaxAge        = 24 * time.Hour
	indexSyncTimeout   = 30 * time.Second
	indexSyncCooldown  = 1 * time.Hour
	indexFileName      = "index.json"
	indexHTTPUserAgent = "tresor-icon-index/1.0"
)

const indexResolvedURL = "https://data.jsdelivr.com/v1/packages/npm/@lobehub/icons-static-svg/resolved"

const indexFlatURLTpl = "https://data.jsdelivr.com/v1/packages/npm/@lobehub/icons-static-svg@%s?structure=flat"

// indexJSON is the on-disk and wire representation of the index. We keep
// Slugs as a sorted []string rather than a map for compact serialization
// (an 871-entry JSON file with a map is noticeably larger).
type indexJSON struct {
	Version   string    `json:"version"`
	Fetched   time.Time `json:"fetched"`
	Slugs     []string  `json:"slugs"`
	Source    string    `json:"source"`
	LocalTime time.Time `json:"local_time"`
}

// resolvedResponse mirrors the JSON returned by data.jsdelivr.com's
// /resolved endpoint.
type resolvedResponse struct {
	Version string `json:"version"`
}

// flatResponse is the top-level shape of ?structure=flat.
type flatResponse struct {
	Files []flatFile `json:"files"`
}

// flatFile is one entry in the flat file list. We only need Name.
type flatFile struct {
	Name string `json:"name"`
}

// Index holds a local snapshot of the CDN's available icon slugs. The
// hot path (Contains) is an O(1) read under an RLock; all CDN I/O happens
// off the request path via background goroutines.
type Index struct {
	mu       sync.RWMutex
	data     indexJSON
	set      map[string]struct{}
	ready    bool
	lastErr  error
	cooldown time.Time

	cacheDir string
	log      *log.Logger
	client   *http.Client
	sf       singleflight.Group

	// proxyMode is captured so SetProxyMode can rebuild the client.
	proxyMode proxy.Mode

	// resolvedURL and flatURLTpl are the jsDelivr data API endpoints.
	// Tests override these to point at httptest.Server URLs.
	resolvedURL string
	flatURLTpl  string
}

// newIndex constructs an Index. The supplied http.Client is reused for
// all sync traffic; callers may pass nil to get a default client.
//
// resolvedURL and flatURLTpl may be overridden to point at a test server;
// production code leaves them as the package-level constants.
func newIndex(cacheDir string, mode proxy.Mode, client *http.Client, logger *log.Logger) *Index {
	if client == nil {
		client = &http.Client{Timeout: indexSyncTimeout}
	}
	if logger == nil {
		logger = log.New(os.Stderr, "icons: ", log.LstdFlags|log.Lmicroseconds)
	}
	return &Index{
		cacheDir:    cacheDir,
		client:      client,
		log:         logger,
		proxyMode:   mode,
		resolvedURL: indexResolvedURL,
		flatURLTpl:  indexFlatURLTpl,
	}
}

// SetEndpoints overrides the jsDelivr data API endpoints. Intended for
// tests; production code uses the package-level defaults.
func (i *Index) SetEndpoints(resolvedURL, flatURLTpl string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.resolvedURL = resolvedURL
	i.flatURLTpl = flatURLTpl
}

// SetProxyMode rebuilds the http.Client with the new proxy mode. Mirrors
// Fetcher.SetProxyMode so live /api/config PUTs propagate.
func (i *Index) SetProxyMode(mode proxy.Mode) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.proxyMode = mode
	i.client = &http.Client{
		Transport: &http.Transport{
			Proxy:           proxy.ProxyFunc(mode),
			IdleConnTimeout: 30 * time.Second,
		},
		Timeout: indexSyncTimeout,
	}
}

// Ready reports whether the in-memory index is populated and usable.
// False until the first successful Sync (or a valid disk cache).
func (i *Index) Ready() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.ready
}

// Version returns the version string from the most recent successful sync.
func (i *Index) Version() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.data.Version
}

// SlugCount returns the number of slugs in the index.
func (i *Index) SlugCount() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.data.Slugs)
}

// Age returns the time since the last successful sync.
func (i *Index) Age() time.Duration {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.data.Fetched.IsZero() {
		return time.Duration(math.MaxInt64)
	}
	return time.Since(i.data.Fetched)
}

// Contains reports whether slug is in the index. Returns false when the
// index is not yet ready.
func (i *Index) Contains(slug string) bool {
	if slug == "" {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.ready {
		return false
	}
	_, ok := i.set[slug]
	return ok
}

// Load reads the on-disk index file and populates the in-memory state.
// ENOENT is not an error (returns nil, ready=false). Corrupt JSON removes
// the file and returns the parse error so the next sync writes a fresh one.
func (i *Index) Load() error {
	path := filepath.Join(i.cacheDir, indexFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	var data indexJSON
	if err := json.Unmarshal(b, &data); err != nil {
		// Best-effort cleanup; ignore remove errors.
		_ = os.Remove(path)
		return fmt.Errorf("parse %s: %w", path, err)
	}
	set := make(map[string]struct{}, len(data.Slugs))
	for _, s := range data.Slugs {
		if s == "" {
			continue
		}
		set[s] = struct{}{}
	}
	i.mu.Lock()
	i.data = data
	i.set = set
	i.ready = true
	i.mu.Unlock()
	i.log.Printf("icons: index loaded from disk: version=%s slugs=%d local_time=%s",
		data.Version, len(data.Slugs), data.LocalTime.Format(time.RFC3339))
	return nil
}

// Sync fetches the latest directory listing from jsDelivr and updates the
// in-memory state. Concurrent calls coalesce via singleflight. The returned
// version and slugCount reflect the new state on success.
func (i *Index) Sync(ctx context.Context) (version string, slugCount int, err error) {
	v, err, _ := i.sf.Do("sync", func() (any, error) {
		return i.doSync(ctx)
	})
	if err != nil {
		return "", 0, err
	}
	r := v.(syncResult)
	return r.Version, r.Count, nil
}

type syncResult struct {
	Version string
	Count   int
}

func (i *Index) doSync(ctx context.Context) (syncResult, error) {
	i.mu.RLock()
	if !i.cooldown.IsZero() && time.Now().Before(i.cooldown) {
		remaining := time.Until(i.cooldown)
		i.mu.RUnlock()
		return syncResult{}, fmt.Errorf("index sync in cooldown, retry in %s", remaining.Round(time.Second))
	}
	resolvedURL := i.resolvedURL
	flatURLTpl := i.flatURLTpl
	i.mu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, indexSyncTimeout)
	defer cancel()

	// 1) Resolve current version
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolvedURL, nil)
	if err != nil {
		return syncResult{}, fmt.Errorf("build resolved request: %w", err)
	}
	req.Header.Set("User-Agent", indexHTTPUserAgent)

	resp, err := i.client.Do(req)
	if err != nil {
		i.markFailure(err)
		return syncResult{}, fmt.Errorf("fetch resolved: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
	if err != nil {
		i.markFailure(err)
		return syncResult{}, fmt.Errorf("read resolved body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		i.markFailure(fmt.Errorf("resolved: status %d", resp.StatusCode))
		return syncResult{}, fmt.Errorf("resolved: status %d", resp.StatusCode)
	}
	var r resolvedResponse
	if err := json.Unmarshal(body, &r); err != nil {
		i.markFailure(err)
		return syncResult{}, fmt.Errorf("parse resolved: %w", err)
	}
	if r.Version == "" {
		i.markFailure(fmt.Errorf("resolved: empty version"))
		return syncResult{}, fmt.Errorf("resolved: empty version")
	}

	// 2) Fetch flat listing pinned to that version
	flatURL := fmt.Sprintf(flatURLTpl, r.Version)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, flatURL, nil)
	if err != nil {
		i.markFailure(err)
		return syncResult{}, fmt.Errorf("build flat request: %w", err)
	}
	req.Header.Set("User-Agent", indexHTTPUserAgent)

	resp, err = i.client.Do(req)
	if err != nil {
		i.markFailure(err)
		return syncResult{}, fmt.Errorf("fetch flat: %w", err)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	resp.Body.Close()
	if err != nil {
		i.markFailure(err)
		return syncResult{}, fmt.Errorf("read flat body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Honor Retry-After on 503 etc.
		if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			i.markFailure(fmt.Errorf("flat: status %d (retry-after %s)", resp.StatusCode, ra))
		} else {
			i.markFailure(fmt.Errorf("flat: status %d", resp.StatusCode))
		}
		return syncResult{}, fmt.Errorf("flat: status %d", resp.StatusCode)
	}
	var fr flatResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		i.markFailure(err)
		return syncResult{}, fmt.Errorf("parse flat: %w", err)
	}

	slugs := make([]string, 0, len(fr.Files))
	for _, f := range fr.Files {
		name := f.Name
		const prefix = "/icons/"
		const suffix = ".svg"
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		s := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
		if s == "" {
			continue
		}
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	// Dedup
	dedup := slugs[:0]
	var last string
	for _, s := range slugs {
		if s == last {
			continue
		}
		dedup = append(dedup, s)
		last = s
	}
	slugs = dedup

	data := indexJSON{
		Version:   r.Version,
		Fetched:   time.Now().UTC(),
		Slugs:     slugs,
		Source:    flatURL,
		LocalTime: time.Now().UTC(),
	}

	// Build the lookup set under a write lock, then swap.
	set := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		set[s] = struct{}{}
	}

	// Detect version drift between the previously-loaded index and the
	// freshly synced one. We log a warning but still apply the new data.
	i.mu.Lock()
	if i.ready && i.data.Version != "" && i.data.Version != data.Version {
		i.log.Printf("icons: index version drift: previous=%s new=%s", i.data.Version, data.Version)
	}
	i.data = data
	i.set = set
	i.ready = true
	i.lastErr = nil
	i.cooldown = time.Time{}
	i.mu.Unlock()

	if err := i.writeAtomic(data); err != nil {
		i.log.Printf("icons: index write to disk: %v", err)
	}

	i.log.Printf("icons: index ready: version=%s slugs=%d", data.Version, len(slugs))
	return syncResult{Version: data.Version, Count: len(slugs)}, nil
}

// markFailure records the failure and arms the cooldown.
func (i *Index) markFailure(err error) {
	i.mu.Lock()
	i.lastErr = err
	i.cooldown = time.Now().Add(indexSyncCooldown)
	i.mu.Unlock()
}

// writeAtomic writes the index JSON to disk via temp+rename, mirroring
// Fetcher.writeDisk's pattern.
func (i *Index) writeAtomic(data indexJSON) error {
	if err := os.MkdirAll(i.cacheDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache dir: %w", err)
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp, err := os.CreateTemp(i.cacheDir, "index.*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	final := filepath.Join(i.cacheDir, indexFileName)
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// MaybeSync kicks off a background sync if the index is stale or missing.
// Never blocks; never returns an error.
func (i *Index) MaybeSync() {
	i.mu.RLock()
	stale := !i.ready || time.Since(i.data.Fetched) > indexMaxAge
	inCooldown := !i.cooldown.IsZero() && time.Now().Before(i.cooldown)
	i.mu.RUnlock()
	if !stale || inCooldown {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), indexSyncTimeout)
		defer cancel()
		_, _, _ = i.Sync(ctx)
	}()
}

// StartPeriodic kicks off a sync immediately (if needed) and then once
// every indexMaxAge. The returned function cancels the periodic timer.
func (i *Index) StartPeriodic(ctx context.Context) func() {
	pctx, cancel := context.WithCancel(ctx)
	go func() {
		i.MaybeSync()
		t := time.NewTicker(indexMaxAge)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
				i.MaybeSync()
			}
		}
	}()
	return cancel
}

// parseRetryAfter parses a Retry-After header value as integer seconds.
// Returns 0 on missing/unparseable input.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil {
		return 0
	}
	if secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}