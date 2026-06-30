package icons

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// makeSVG returns a tiny but valid-looking SVG body of arbitrary size.
func makeSVG(size int) []byte {
	// Real SVGs from the CDN start with <?xml or <svg — emulate that.
	header := []byte(`<?xml version="1.0" encoding="UTF-8"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	if size <= len(header) {
		return header[:size]
	}
	out := make([]byte, 0, size)
	out = append(out, header...)
	out = append(out, make([]byte, size-len(header))...)
	return out
}

// newTestFetcher creates a Fetcher whose CDN points at the given httptest server.
// The cache dir is t.TempDir(). The Fetcher's stdlib log is redirected to
// os.Stderr to keep test output clean unless the test fails.
func newTestFetcher(t *testing.T, cdn *httptest.Server) *Fetcher {
	t.Helper()
	f, err := New(t.TempDir(), cdn.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.SetCDNURL(func(slug string) string {
		return fmt.Sprintf("%s/icons/%s.svg", cdn.URL, slug)
	})
	return f
}

func TestFetcher_MemoryCacheHit(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(makeSVG(64))
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)

	data, ct, err := f.Icon("gpt-4o")
	if err != nil {
		t.Fatalf("first Icon: %v", err)
	}
	if ct != "image/svg+xml" || len(data) == 0 {
		t.Fatalf("unexpected first response: ct=%q len=%d", ct, len(data))
	}

	data2, ct2, err := f.Icon("gpt-4o")
	if err != nil {
		t.Fatalf("second Icon: %v", err)
	}
	if ct2 != ct || string(data2) != string(data) {
		t.Fatalf("second call returned different bytes (should be mem-cache hit)")
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected exactly 1 CDN hit, got %d", got)
	}
}

func TestFetcher_DiskPersistence(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(makeSVG(64))
	}))
	defer cdn.Close()

	dir := t.TempDir()

	// First fetcher populates the cache.
	f1, err := New(dir, cdn.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f1.SetCDNURL(func(slug string) string {
		return fmt.Sprintf("%s/icons/%s.svg", cdn.URL, slug)
	})
	if _, _, err := f1.Icon("claude-sonnet-4-20250514"); err != nil {
		t.Fatalf("first Icon: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 hit during priming, got %d", got)
	}

	// Confirm the .svg and .meta files are on disk.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	svgCount, metaCount := 0, 0
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".svg"):
			svgCount++
		case strings.HasSuffix(e.Name(), ".meta"):
			metaCount++
		}
	}
	if svgCount != 1 || metaCount != 1 {
		t.Fatalf("expected 1 .svg and 1 .meta, got svg=%d meta=%d", svgCount, metaCount)
	}

	// Second fetcher with same cache dir should NOT hit the network.
	f2, err := New(dir, cdn.Client())
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}
	f2.SetCDNURL(func(slug string) string {
		return fmt.Sprintf("%s/icons/%s.svg", cdn.URL, slug)
	})
	if _, _, err := f2.Icon("claude-sonnet-4-20250514"); err != nil {
		t.Fatalf("second fetcher Icon: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("second fetcher should not hit the network; total hits = %d", got)
	}
}

// TestFetcher_UnmatchedModel asserts that an empty model ID never reaches
// the network. (Models like "my-totally-unknown-model-xyz" now fall through
// to the first-segment fallback; the dedicated tests for that path live in
// TestFetcher_FallbackHit and TestFetcher_FallbackMiss below.)
func TestFetcher_UnmatchedModel(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		t.Errorf("CDN should not be called for empty model")
		w.WriteHeader(500)
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	data, ct, err := f.Icon("")
	if err != nil {
		t.Fatalf("Icon: %v", err)
	}
	if data != nil || ct != "" {
		t.Errorf("empty model should return empty data and ct, got data=%d ct=%q", len(data), ct)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("expected 0 CDN hits for empty model, got %d", got)
	}
}

// TestFetcher_FallbackHit: a model ID that doesn't match any pattern triggers
// the first-segment fallback. The CDN serves the SVG at the fallback URL.
func TestFetcher_FallbackHit(t *testing.T) {
	var hits int32
	var seenPaths []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenPaths = append(seenPaths, r.URL.Path)
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(makeSVG(32))
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	data, ct, err := f.Icon("MiniMax-M2.5")
	if err != nil {
		t.Fatalf("Icon: %v", err)
	}
	if ct != "image/svg+xml" || len(data) == 0 {
		t.Fatalf("expected SVG body, got ct=%q len=%d", ct, len(data))
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 CDN hit, got %d (paths=%v)", got, seenPaths)
	}
	// The fallback URL must end with /minimax-color.svg (color twin of
	// first segment "minimax") — the color variant is preferred.
	foundColor := false
	for _, p := range seenPaths {
		if p == "/icons/minimax-color.svg" {
			foundColor = true
		}
	}
	if !foundColor {
		t.Errorf("expected fallback to fetch /icons/minimax-color.svg first, got paths=%v", seenPaths)
	}
}

// TestFetcher_FallbackMiss: a model whose first segment is also missing from
// the CDN returns empty (no error, no exception) so the browser can render
// the onerror fallback.
func TestFetcher_FallbackMiss(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	data, ct, err := f.Icon("brandnewmodel-X")
	if err != nil {
		t.Fatalf("Icon: %v", err)
	}
	if data != nil || ct != "" {
		t.Errorf("unresolved fallback should return empty data and ct, got data=%d ct=%q", len(data), ct)
	}
}

// TestFetcher_FallbackSecondCallSkipsCDN: after the first fallback chain
// 404s (color then flat), the in-session miss cache should prevent the
// same URLs from being re-fetched. The chain produces 2 unique CDN hits
// (color variant + flat variant), and subsequent calls add 0 more.
func TestFetcher_FallbackSecondCallSkipsCDN(t *testing.T) {
	var hits int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	for i := 0; i < 3; i++ {
		if _, _, err := f.Icon("brandnewmodel-X"); err != nil {
			t.Fatalf("Icon #%d: %v", i, err)
		}
	}
	// First call: tries brandnewmodel-color.svg (404 → miss cached),
	// then brandnewmodel.svg (404 → miss cached). = 2 hits.
	// Subsequent calls: both already in miss cache. = 0 additional.
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("expected exactly 2 CDN hits (miss cache), got %d", got)
	}
}

// TestFetcher_FallbackSkippedWhenPrimaryMatches: a model that matches the
// pattern table with a hard-coded color slug should NOT trigger the
// non-color fallback. ("claude-sonnet-..." resolves to "claude-color";
// the first segment "claude" is just the color twin of the primary, so
// the dedupe rule in CandidateSlugs drops the fallback entirely.)
func TestFetcher_FallbackSkippedWhenPrimaryMatches(t *testing.T) {
	var hits int32
	var seenPaths []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenPaths = append(seenPaths, r.URL.Path)
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(makeSVG(24))
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	if _, _, err := f.Icon("claude-sonnet-4-20250514"); err != nil {
		t.Fatalf("Icon: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 CDN hit, got %d", got)
	}
	for _, p := range seenPaths {
		if p != "/icons/claude-color.svg" {
			t.Errorf("only /icons/claude-color.svg should be fetched, got %v", seenPaths)
		}
	}
}

// TestFetcher_ColorPreferredOverFlat: when both the color and the flat
// variant of a first-segment slug exist on the CDN, the color one is tried
// first and wins. Uses a model name with no hard-coded pattern so we
// exercise the fallback path purely.
func TestFetcher_ColorPreferredOverFlat(t *testing.T) {
	var hits int32
	var seenPaths []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenPaths = append(seenPaths, r.URL.Path)
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(makeSVG(32))
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	// "MiniMax-M2.5" → no hard-coded primary, fb = "minimax".
	// We expect /icons/minimax-color.svg FIRST, the flat variant only on miss.
	if _, _, err := f.Icon("MiniMax-M2.5"); err != nil {
		t.Fatalf("Icon: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 CDN hit (color won on first try), got %d (paths=%v)", got, seenPaths)
	}
	if len(seenPaths) != 1 || seenPaths[0] != "/icons/minimax-color.svg" {
		t.Errorf("/icons/minimax-color.svg should be the only request, got %v", seenPaths)
	}
}

// TestFetcher_ColorFallthroughToFlat: when the color slug 404s at the CDN,
// the chain falls through to the flat slug for the same segment.
func TestFetcher_ColorFallthroughToFlat(t *testing.T) {
	var hits int32
	var seenPaths []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenPaths = append(seenPaths, r.URL.Path)
		if r.URL.Path == "/icons/minimax-color.svg" {
			// Simulate the real CDN: this vendor doesn't ship a color
			// variant for this slug.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(makeSVG(32))
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	data, ct, err := f.Icon("MiniMax-M2.5")
	if err != nil {
		t.Fatalf("Icon: %v", err)
	}
	if ct != "image/svg+xml" || len(data) == 0 {
		t.Fatalf("expected SVG body from flat fallback, got ct=%q len=%d", ct, len(data))
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected 2 CDN hits (color miss + flat hit), got %d (paths=%v)", got, seenPaths)
	}
	want := []string{"/icons/minimax-color.svg", "/icons/minimax.svg"}
	if len(seenPaths) != len(want) {
		t.Fatalf("expected paths %v, got %v", want, seenPaths)
	}
	for i, p := range seenPaths {
		if p != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, p, want[i])
		}
	}
}

func TestFetcher_FetchError(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	_, _, err := f.Icon("gpt-4o")
	if err == nil {
		t.Fatalf("expected error from 500 response")
	}
	// Ensure no file was written to disk.
	dir := t.TempDir()
	f.cacheDir = dir
	_, _, _ = f.Icon("gpt-4o")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no disk writes on fetch error, found %d", len(entries))
	}
}

func TestFetcher_RejectsLargeResponse(t *testing.T) {
	// Serve 1 MB of "svg" — well over the 256 KB cap.
	big := make([]byte, 1024*1024)
	for i := range big {
		big[i] = 'A'
	}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(big)
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	_, _, err := f.Icon("gpt-4o")
	if err == nil {
		t.Fatalf("expected error for oversized response")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' in error, got %v", err)
	}
}

func TestFetcher_RejectsBadContentType(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not an icon</html>"))
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	_, _, err := f.Icon("gpt-4o")
	if err == nil {
		t.Fatalf("expected error for non-SVG content type")
	}
}

func TestFetcher_ProxyModeHonored(t *testing.T) {
	// We can't easily prove Transport.Proxy was called without a network,
	// so we just confirm NewWithProxyMode does not panic and produces a
	// working Fetcher.
	dir := t.TempDir()
	f, err := NewWithProxyMode(dir, "none")
	if err != nil {
		t.Fatalf("NewWithProxyMode: %v", err)
	}
	if f == nil || f.client == nil {
		t.Fatal("expected non-nil Fetcher with non-nil client")
	}
	f.SetProxyMode("env")
	if f.client == nil {
		t.Fatal("client should remain non-nil after SetProxyMode")
	}
}

func TestFetcher_AtomicDiskWrite(t *testing.T) {
	// Verify writeDisk + readDisk round-trips.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Write(makeSVG(128))
	}))
	defer cdn.Close()

	dir := t.TempDir()
	f, err := New(dir, cdn.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.SetCDNURL(func(slug string) string {
		return fmt.Sprintf("%s/icons/%s.svg", cdn.URL, slug)
	})
	if _, _, err := f.Icon("gemini-2.5-pro"); err != nil {
		t.Fatalf("Icon: %v", err)
	}

	// Both files should exist with the right content.
	entries, _ := os.ReadDir(dir)
	var foundSVG, foundMeta bool
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".svg") {
			foundSVG = true
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read svg: %v", err)
			}
			if !strings.HasPrefix(string(data), "<?xml") {
				t.Errorf("svg content looks wrong: %q", string(data[:min(20, len(data))]))
			}
		}
		if strings.HasSuffix(name, ".meta") {
			foundMeta = true
		}
	}
	if !foundSVG || !foundMeta {
		t.Errorf("expected both .svg and .meta, got svg=%v meta=%v", foundSVG, foundMeta)
	}
}

func TestFetcher_Timeout(t *testing.T) {
	// Server sleeps longer than the client timeout. Default timeout is 10s
	// which is too long for a unit test, so build a Fetcher with a 100ms timeout.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write(makeSVG(16))
	}))
	defer cdn.Close()

	dir := t.TempDir()
	f, err := New(dir, cdn.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.client.Timeout = 100 * time.Millisecond
	f.SetCDNURL(func(slug string) string {
		return fmt.Sprintf("%s/icons/%s.svg", cdn.URL, slug)
	})
	_, _, err = f.Icon("gpt-4o")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
