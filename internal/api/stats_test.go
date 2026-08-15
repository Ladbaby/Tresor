package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tresor/internal/store"
)

func TestStats_EmptyResult(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=last_7_days", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		CapturePayloads bool `json:"capture_payloads"`
		Range           struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"range"`
		BucketSize string `json:"bucket_size"`
		Total      struct {
			InputTokens  int64    `json:"input_tokens"`
			OutputTokens int64    `json:"output_tokens"`
			Requests     int64    `json:"requests"`
			CacheHitRate *float64 `json:"cache_hit_rate"`
		} `json:"total"`
		Models []map[string]interface{} `json:"models"`
		Series []map[string]interface{} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total.InputTokens != 0 || resp.Total.OutputTokens != 0 || resp.Total.Requests != 0 {
		t.Errorf("expected zero totals, got %+v", resp.Total)
	}
	if resp.Total.CacheHitRate != nil {
		t.Errorf("expected nil cache_hit_rate, got %v", *resp.Total.CacheHitRate)
	}
	if resp.BucketSize != "day" {
		t.Errorf("bucket_size: got %q, want %q", resp.BucketSize, "day")
	}
	if len(resp.Models) != 0 || len(resp.Series) != 0 {
		t.Errorf("expected empty models/series, got %d/%d", len(resp.Models), len(resp.Series))
	}
}

func TestStats_WithData(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	// Seed the downstreams table so the LEFT JOIN can resolve human-readable
	// names for the providers section.
	if err := router.store.CreateDownstream(&store.Downstream{
		ID:   "anthropic",
		Name: "Anthropic",
	}); err != nil {
		t.Fatalf("seed downstream anthropic: %v", err)
	}
	if err := router.store.CreateDownstream(&store.Downstream{
		ID:   "openai",
		Name: "OpenAI",
	}); err != nil {
		t.Fatalf("seed downstream openai: %v", err)
	}

	// Seed two requests for the same bucket.
	if err := router.store.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "claude-opus-4-7",
		100, 50, 0, 30); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if err := router.store.RecordUsageStats("2026-06-27T15:00:00Z", "openai", "gpt-4o",
		200, 75, 0, 0); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	// Query a range that covers the seeded bucket.
	from := "2026-06-27"
	to := "2026-06-27"
	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=custom&from="+from+"&to="+to, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Total struct {
			InputTokens  int64    `json:"input_tokens"`
			OutputTokens int64    `json:"output_tokens"`
			Requests     int64    `json:"requests"`
			CacheHitRate *float64 `json:"cache_hit_rate"`
		} `json:"total"`
		Models []struct {
			DownstreamID string `json:"downstream_id"`
			Model        string `json:"model"`
			RequestCount int64  `json:"request_count"`
			CacheRead    int64  `json:"cache_read_tokens"`
		} `json:"models"`
		Providers []struct {
			DownstreamID string `json:"downstream_id"`
			Name         string `json:"name"`
			RequestCount int64  `json:"request_count"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			CacheRead    int64  `json:"cache_read_tokens"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total.InputTokens != 300 {
		t.Errorf("input_tokens: got %d, want 300", resp.Total.InputTokens)
	}
	if resp.Total.OutputTokens != 125 {
		t.Errorf("output_tokens: got %d, want 125", resp.Total.OutputTokens)
	}
	if resp.Total.Requests != 2 {
		t.Errorf("requests: got %d, want 2", resp.Total.Requests)
	}
	if resp.Total.CacheHitRate == nil {
		t.Error("expected non-nil cache_hit_rate")
	}
	if len(resp.Models) != 2 {
		t.Fatalf("expected 2 model rows, got %d", len(resp.Models))
	}
	// Sorted by total tokens desc: claude-opus-4-7 (input=100+30=130, output=50 → 180) > gpt-4o (input=200+0=200, output=75 → 275)
	// gpt-4o should be first
	if resp.Models[0].Model != "gpt-4o" {
		t.Errorf("first model: got %q, want %q", resp.Models[0].Model, "gpt-4o")
	}

	// Providers array must collapse the two seeded rows into one per
	// downstream and surface it in token-descending order.
	if len(resp.Providers) != 2 {
		t.Fatalf("expected 2 provider rows, got %d", len(resp.Providers))
	}
	if resp.Providers[0].DownstreamID != "openai" {
		t.Errorf("first provider: got %q, want %q (largest token total)",
			resp.Providers[0].DownstreamID, "openai")
	}
	if resp.Providers[0].Name != "OpenAI" {
		t.Errorf("first provider name: got %q, want %q", resp.Providers[0].Name, "OpenAI")
	}
	if resp.Providers[0].InputTokens != 200 || resp.Providers[0].OutputTokens != 75 {
		t.Errorf("openai provider tokens: got in=%d out=%d, want in=200 out=75",
			resp.Providers[0].InputTokens, resp.Providers[0].OutputTokens)
	}
	if resp.Providers[0].RequestCount != 1 {
		t.Errorf("openai provider request_count: got %d, want 1", resp.Providers[0].RequestCount)
	}
	if resp.Providers[1].DownstreamID != "anthropic" {
		t.Errorf("second provider: got %q, want %q", resp.Providers[1].DownstreamID, "anthropic")
	}
	if resp.Providers[1].Name != "Anthropic" {
		t.Errorf("second provider name: got %q, want %q", resp.Providers[1].Name, "Anthropic")
	}
	if resp.Providers[1].RequestCount != 1 {
		t.Errorf("anthropic provider request_count: got %d, want 1", resp.Providers[1].RequestCount)
	}
	if resp.Providers[1].CacheRead != 30 {
		t.Errorf("anthropic provider cache_read: got %d, want 30", resp.Providers[1].CacheRead)
	}
}

func TestStats_EmptyResult_IncludesProvidersArray(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=last_7_days", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify the providers array is present and empty (not missing/null)
	// so the frontend can blindly iterate without a null check.
	var resp struct {
		Providers []map[string]interface{} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Providers == nil {
		t.Errorf("providers array should be present (even when empty), got nil")
	}
	if len(resp.Providers) != 0 {
		t.Errorf("expected empty providers, got %d entries", len(resp.Providers))
	}
}

func TestStats_EmptyResult_IncludesIPsArray(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=last_7_days", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// The ips array must be present and empty (not missing/null) so the
	// frontend can blindly iterate without a null check.
	var resp struct {
		IPs []map[string]interface{} `json:"ips"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.IPs == nil {
		t.Errorf("ips array should be present (even when empty), got nil")
	}
	if len(resp.IPs) != 0 {
		t.Errorf("expected empty ips, got %d entries", len(resp.IPs))
	}
}

func TestStats_BadRange(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=garbage", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown preset, got %d", w.Code)
	}
}

func TestStats_CustomMissingDates(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=custom", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for custom without dates, got %d", w.Code)
	}
}

func TestStats_TimeSeriesHourlyAutoBuckets(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	// Two distinct hours in the same day → should produce 2 daily-bucketed
	// entries when range is ≥ 4 days, or 2 hourly entries when ≤ 3 days.
	if err := router.store.RecordUsageStats("2026-06-27T10:00:00Z", "anthropic", "claude-opus-4-7",
		100, 50, 0, 0); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if err := router.store.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "claude-opus-4-7",
		200, 75, 0, 0); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	// Custom range of 1 day → hour buckets
	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=custom&from=2026-06-27&to=2026-06-27", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		BucketSize string `json:"bucket_size"`
		Series     []struct {
			Bucket string `json:"bucket"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.BucketSize != "hour" {
		t.Errorf("bucket_size: got %q, want hour", resp.BucketSize)
	}
	if len(resp.Series) != 2 {
		t.Errorf("expected 2 hour buckets, got %d", len(resp.Series))
	}
}

func TestStats_Presets(t *testing.T) {
	// Sanity-check that the basic presets are wired correctly.
	router := newTestRouter(t)
	handler := router.Handler()

	presets := []string{"today", "yesterday", "last_7_days", "last_14_days",
		"last_4_weeks", "this_month", "last_month", "this_year", "last_year"}
	for _, p := range presets {
		req := httptest.NewRequest(http.MethodGet, "/api/stats?range="+p, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("preset %q: expected 200, got %d: %s", p, w.Code, w.Body.String())
		}
	}
}

func TestStats_CapturePayloadsFlagLeaked(t *testing.T) {
	router := newTestRouter(t)
	handler := router.Handler()

	// Toggle the runtime flag (mirrors what the Settings tab does).
	InitRuntimeConfig("auto", nil, "", "downstreams", "info", true, false)

	req := httptest.NewRequest(http.MethodGet, "/api/stats?range=today", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		CapturePayloads bool `json:"capture_payloads"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.CapturePayloads {
		t.Errorf("expected capture_payloads=true in response")
	}

	// Reset for other tests
	InitRuntimeConfig("auto", nil, "", "downstreams", "info", false, false)
}

func TestStatsQueryBuilder_Today(t *testing.T) {
	// Pin the clock to a known time so the test is deterministic.
	now := time.Date(2026, 6, 27, 14, 30, 0, 0, time.UTC)
	from, to, bs, ok := statsRangeParam("today", now)
	if !ok {
		t.Fatal("today preset not recognized")
	}
	if bs != "hour" {
		t.Errorf("today bucket: got %q, want hour", bs)
	}
	if from.Day() != 27 || to.Day() != 27 {
		t.Errorf("today range: got %s..%s, want 2026-06-27..2026-06-27", from, to)
	}
	if !from.Before(to) {
		t.Errorf("from must precede to: %s >= %s", from, to)
	}
}
