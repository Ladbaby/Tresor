package icons

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tresor/internal/proxy"
)

// indexTestServer is a stub for the jsDelivr data API. It serves:
//   - /v1/packages/npm/@lobehub/icons-static-svg/resolved -> the resolved version
//   - /v1/packages/npm/@lobehub/icons-static-svg@<v>?structure=flat -> the flat
//     file listing.
//
// Tests can override the returned version, the slug list, and the HTTP status
// code per endpoint to simulate failures and version drift.
type indexTestServer struct {
	*httptest.Server

	version string
	slugs   []string

	// Optional: return non-OK status for resolved or flat endpoints.
	resolvedStatus int
	flatStatus     int
	// Optional: latency injected before each response.
	latency time.Duration

	hits int32 // both endpoints count toward this counter
}

func newIndexTestServer(t *testing.T) *indexTestServer {
	its := &indexTestServer{
		version: "1.91.0",
		slugs:   []string{"openai", "claude-color", "qwen-color", "grok", "kimi"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/resolved", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&its.hits, 1)
		if its.latency > 0 {
			time.Sleep(its.latency)
		}
		if its.resolvedStatus != 0 && its.resolvedStatus != http.StatusOK {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(its.resolvedStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"version": its.version})
	})
	mux.HandleFunc("/flat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&its.hits, 1)
		if its.latency > 0 {
			time.Sleep(its.latency)
		}
		if its.flatStatus != 0 && its.flatStatus != http.StatusOK {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(its.flatStatus)
			return
		}
		files := make([]map[string]any, 0, len(its.slugs))
		for _, s := range its.slugs {
			files = append(files, map[string]any{
				"name": "/icons/" + s + ".svg",
				"hash": "fakehash",
				"size": 256,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"type":    "npm",
			"name":    "@lobehub/icons-static-svg",
			"version": its.version,
			"files":   files,
		})
	})
	its.Server = httptest.NewServer(mux)
	t.Cleanup(its.Close)
	return its
}

// indexTestClient returns an *http.Client whose transport is wired to the
// test server, so request URLs resolve to localhost.
func (its *indexTestServer) indexTestClient() *http.Client {
	return its.Client()
}

// indexTestEndpoints returns the (resolvedURL, flatURLTpl) pair that
// point at this test server. The %s in flatURLTpl is filled with the
// resolved version, just like the production endpoint.
func (its *indexTestServer) indexTestEndpoints() (string, string) {
	return its.URL + "/resolved", its.URL + "/flat?version=%s"
}

func TestIndex_LoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	idx := newIndex(dir, proxy.ModeNone, &http.Client{Timeout: 5 * time.Second}, nil)
	if err := idx.Load(); err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if idx.Ready() {
		t.Errorf("Ready() should be false when no index file exists")
	}
	if idx.Version() != "" {
		t.Errorf("Version() should be empty, got %q", idx.Version())
	}
}

func TestIndex_LoadCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, indexFileName), []byte("not json{{{"), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	idx := newIndex(dir, proxy.ModeNone, &http.Client{Timeout: 5 * time.Second}, nil)
	err := idx.Load()
	if err == nil {
		t.Fatalf("Load on corrupt file should error")
	}
	if idx.Ready() {
		t.Errorf("Ready() should be false after a corrupt file load")
	}
	// Confirm the corrupt file was removed.
	if _, err := os.Stat(filepath.Join(dir, indexFileName)); !os.IsNotExist(err) {
		t.Errorf("corrupt index.json should be removed; stat err = %v", err)
	}
}

func TestIndex_SyncSuccess(t *testing.T) {
	its := newIndexTestServer(t)
	dir := t.TempDir()
	idx := newIndex(dir, proxy.ModeNone, its.indexTestClient(), nil)
	resolvedURL, flatURLTpl := its.indexTestEndpoints()
	idx.SetEndpoints(resolvedURL, flatURLTpl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, n, err := idx.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if v != "1.91.0" {
		t.Errorf("expected version 1.91.0, got %q", v)
	}
	if n != 5 {
		t.Errorf("expected 5 slugs, got %d", n)
	}
	if !idx.Ready() {
		t.Errorf("Ready() should be true after a successful sync")
	}
	if !idx.Contains("openai") || !idx.Contains("claude-color") {
		t.Errorf("Contains() should report true for synced slugs")
	}
	if idx.Contains("not-a-real-slug") {
		t.Errorf("Contains() should report false for unknown slugs")
	}
	// File on disk
	data, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		t.Fatalf("read on-disk index: %v", err)
	}
	var onDisk indexJSON
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("parse on-disk index: %v", err)
	}
	if onDisk.Version != "1.91.0" || len(onDisk.Slugs) != 5 {
		t.Errorf("on-disk index wrong: %+v", onDisk)
	}
}

func TestIndex_SyncCooldown(t *testing.T) {
	its := newIndexTestServer(t)
	its.resolvedStatus = http.StatusInternalServerError
	dir := t.TempDir()
	idx := newIndex(dir, proxy.ModeNone, its.indexTestClient(), nil)
	resolvedURL, flatURLTpl := its.indexTestEndpoints()
	idx.SetEndpoints(resolvedURL, flatURLTpl)

	// Force a cooldown. We do this by directly calling Sync with a 5xx.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := idx.Sync(ctx); err == nil {
		t.Fatalf("Sync should fail when resolved returns 500")
	}

	// Now the cooldown is armed. A second Sync call should fail without
	// hitting the server — the marker is `cooldown > now`.
	beforeHits := atomic.LoadInt32(&its.hits)
	_, _, err := idx.Sync(ctx)
	if err == nil {
		t.Fatalf("Sync during cooldown should fail")
	}
	if !strings.Contains(err.Error(), "cooldown") {
		t.Errorf("expected cooldown error, got %v", err)
	}
	afterHits := atomic.LoadInt32(&its.hits)
	if beforeHits != afterHits {
		t.Errorf("cooldown should prevent the network call; hits before=%d after=%d", beforeHits, afterHits)
	}
}

func TestIndex_AtomicWrite(t *testing.T) {
	its := newIndexTestServer(t)
	dir := t.TempDir()
	idx := newIndex(dir, proxy.ModeNone, its.indexTestClient(), nil)
	resolvedURL, flatURLTpl := its.indexTestEndpoints()
	idx.SetEndpoints(resolvedURL, flatURLTpl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := idx.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Confirm no leftover .tmp files in the cache dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after sync: %s", e.Name())
		}
	}
}

func TestIndex_LoadFromDisk(t *testing.T) {
	its := newIndexTestServer(t)
	dir := t.TempDir()
	resolvedURL, flatURLTpl := its.indexTestEndpoints()

	// First instance syncs to disk.
	idx1 := newIndex(dir, proxy.ModeNone, its.indexTestClient(), nil)
	idx1.SetEndpoints(resolvedURL, flatURLTpl)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := idx1.Sync(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Second instance loads from disk without touching the network.
	idx2 := newIndex(dir, proxy.ModeNone, its.indexTestClient(), nil)
	idx2.SetEndpoints(resolvedURL, flatURLTpl)
	beforeHits := atomic.LoadInt32(&its.hits)
	if err := idx2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	afterHits := atomic.LoadInt32(&its.hits)
	if beforeHits != afterHits {
		t.Errorf("Load should not hit the network; hits before=%d after=%d", beforeHits, afterHits)
	}
	if !idx2.Ready() || idx2.Version() != "1.91.0" || idx2.SlugCount() != 5 {
		t.Errorf("Load did not populate from disk: ready=%v version=%q count=%d",
			idx2.Ready(), idx2.Version(), idx2.SlugCount())
	}
}

// TestFetcher_IndexFiltersMissing: when the index is ready and the
// requested model resolves to a slug NOT in the index, the fetcher must
// return (nil, "", nil) without issuing any CDN request.
func TestFetcher_IndexFiltersMissing(t *testing.T) {
	// We need a CDN server that records all hits. The fetcher's index
	// will be pre-populated with only "openai", so "claude-opus" (which
	// resolves to "claude-color") must be filtered out.
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		t.Errorf("CDN should not be called when index filters the slug; got path=%s", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	// Replace the index with one that only knows "openai".
	f.index = newIndex(t.TempDir(), proxy.ModeNone, cdn.Client(), nil)
	seedReadyIndex(t, f.index, []string{"openai"})

	data, ct, err := f.Icon("claude-opus-4-7")
	if err != nil {
		t.Fatalf("Icon: %v", err)
	}
	if data != nil || ct != "" {
		t.Errorf("expected empty data/ct, got data=%d ct=%q", len(data), ct)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("expected zero CDN hits when index filters slug, got %d", got)
	}
}

// TestFetcher_IndexWarmingFallsBack: when the index exists but is not
// ready (cold start before first sync), Icon() should still try the
// candidates via the CDN.
func TestFetcher_IndexWarmingFallsBack(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(makeSVG(32))
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	// Index exists but is not ready (empty slugs).
	f.index = newIndex(t.TempDir(), proxy.ModeNone, cdn.Client(), nil)
	if f.index.Ready() {
		t.Fatalf("test fixture: index should not be ready initially")
	}
	if _, _, err := f.Icon("claude-opus-4-7"); err != nil {
		t.Fatalf("Icon: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Errorf("expected at least 1 CDN hit during warming fallback, got 0")
	}
}

// seedReadyIndex installs the supplied slug list as a fully-loaded,
// ready=true Index. Used to test fetcher behavior in isolation from the
// network-based sync path.
func seedReadyIndex(t *testing.T, idx *Index, slugs []string) {
	t.Helper()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.data = indexJSON{
		Version:   "test-seed",
		Fetched:   time.Now().UTC(),
		Slugs:     append([]string{}, slugs...),
		Source:    "test",
		LocalTime: time.Now().UTC(),
	}
	set := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		set[s] = struct{}{}
	}
	idx.set = set
	idx.ready = true
}

func TestIndex_ParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"  ", 0},
		{"30", 30 * time.Second},
		{"  60 ", 60 * time.Second},
		{"abc", defaultRetryAfterFallback},
		{"-5", defaultRetryAfterFallback},
		{"0", 0},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q", c.in), func(t *testing.T) {
			got := retryAfterWait(c.in)
			if got != c.want {
				t.Errorf("retryAfterWait(%q) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}