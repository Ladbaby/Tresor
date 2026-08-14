package store

import (
	"math"
	"testing"
	"time"
)

func TestRecordUsageStats_Aggregates(t *testing.T) {
	s := newTestStore(t)

	// Two requests for the same bucket/downstream/model with cache hits.
	bucket := "2026-06-27T15:00:00Z"
	if err := s.RecordUsageStats(bucket, "anthropic", "claude-opus-4-7",
		100, 50, 10, 30); err != nil {
		t.Fatalf("record stats 1: %v", err)
	}
	if err := s.RecordUsageStats(bucket, "anthropic", "claude-opus-4-7",
		200, 75, 0, 0); err != nil {
		t.Fatalf("record stats 2: %v", err)
	}

	// Query back via AggregateStats.
	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateStats(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate stats: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.DownstreamID != "anthropic" || r.Model != "claude-opus-4-7" {
		t.Errorf("unexpected identity: %+v", r)
	}
	if r.InputTokens != 300 {
		t.Errorf("input_tokens: got %d, want 300", r.InputTokens)
	}
	if r.OutputTokens != 125 {
		t.Errorf("output_tokens: got %d, want 125", r.OutputTokens)
	}
	if r.CacheCreation != 10 {
		t.Errorf("cache_creation: got %d, want 10", r.CacheCreation)
	}
	if r.CacheRead != 30 {
		t.Errorf("cache_read: got %d, want 30", r.CacheRead)
	}
	if r.RequestCount != 2 {
		t.Errorf("request_count: got %d, want 2", r.RequestCount)
	}
	if r.CacheHitCount != 1 {
		t.Errorf("cache_hit_count: got %d, want 1 (only the first request had cache_read > 0)", r.CacheHitCount)
	}
}

func TestRecordUsageStats_MissingUsage(t *testing.T) {
	s := newTestStore(t)

	// Zero usage (empty UsageBlock) should still record a request_count of 1.
	bucket := "2026-06-27T15:00:00Z"
	if err := s.RecordUsageStats(bucket, "openai", "gpt-4o", 0, 0, 0, 0); err != nil {
		t.Fatalf("record stats: %v", err)
	}
	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateStats(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate stats: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].RequestCount != 1 {
		t.Errorf("request_count: got %d, want 1", rows[0].RequestCount)
	}
	if rows[0].CacheHitCount != 0 {
		t.Errorf("cache_hit_count: got %d, want 0", rows[0].CacheHitCount)
	}
}

func TestTotalStats_CacheHitRate(t *testing.T) {
	s := newTestStore(t)

	// 1 request: input=70, cache_read=30 → rate = 30 / (70+30) = 0.3
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "claude-opus-4-7",
		70, 50, 0, 30); err != nil {
		t.Fatalf("record stats: %v", err)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	in, out, reqs, rate, err := s.TotalStats(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("total stats: %v", err)
	}
	if in != 70 || out != 50 || reqs != 1 {
		t.Errorf("totals: got in=%d out=%d reqs=%d, want 70/50/1", in, out, reqs)
	}
	if rate == nil {
		t.Fatal("expected non-nil cache hit rate")
	}
	if math.Abs(*rate-0.30) > 1e-9 {
		t.Errorf("cache hit rate: got %f, want 0.30", *rate)
	}
}

func TestTotalStats_NoCacheData(t *testing.T) {
	s := newTestStore(t)

	// No cache_read anywhere → rate should be nil
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "openai", "gpt-4o",
		100, 50, 0, 0); err != nil {
		t.Fatalf("record stats: %v", err)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	_, _, _, rate, err := s.TotalStats(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("total stats: %v", err)
	}
	if rate != nil {
		t.Errorf("expected nil rate when no cache data, got %f", *rate)
	}
}

func TestTimeSeries_DailyBuckets(t *testing.T) {
	s := newTestStore(t)

	// Two requests in different hours of the same day.
	if err := s.RecordUsageStats("2026-06-27T10:00:00Z", "anthropic", "claude-opus-4-7",
		100, 50, 0, 0); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "claude-opus-4-7",
		200, 75, 0, 0); err != nil {
		t.Fatalf("record 2: %v", err)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	series, err := s.TimeSeries(StatsQuery{From: from, To: to}, "day")
	if err != nil {
		t.Fatalf("time series: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 day bucket, got %d", len(series))
	}
	if series[0].InputTokens != 300 {
		t.Errorf("daily input: got %d, want 300", series[0].InputTokens)
	}
	if series[0].OutputTokens != 125 {
		t.Errorf("daily output: got %d, want 125", series[0].OutputTokens)
	}
	if series[0].RequestCount != 2 {
		t.Errorf("daily requests: got %d, want 2", series[0].RequestCount)
	}
}

func TestTimeSeries_HourlyBuckets(t *testing.T) {
	s := newTestStore(t)

	if err := s.RecordUsageStats("2026-06-27T10:00:00Z", "anthropic", "claude-opus-4-7",
		100, 50, 0, 0); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "claude-opus-4-7",
		200, 75, 0, 0); err != nil {
		t.Fatalf("record 2: %v", err)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	series, err := s.TimeSeries(StatsQuery{From: from, To: to}, "hour")
	if err != nil {
		t.Fatalf("time series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 hour buckets, got %d", len(series))
	}
	if series[0].InputTokens != 100 {
		t.Errorf("hour 10 input: got %d, want 100", series[0].InputTokens)
	}
	if series[1].InputTokens != 200 {
		t.Errorf("hour 15 input: got %d, want 200", series[1].InputTokens)
	}
}

func TestAggregateStats_OrderedByTotalTokens(t *testing.T) {
	s := newTestStore(t)

	// Model A: large; Model B: small.  AggregateStats should return A first.
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "big",
		1000, 500, 0, 0); err != nil {
		t.Fatalf("record A: %v", err)
	}
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "openai", "small",
		10, 5, 0, 0); err != nil {
		t.Fatalf("record B: %v", err)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateStats(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate stats: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Model != "big" {
		t.Errorf("first row model: got %q, want %q", rows[0].Model, "big")
	}
	if rows[1].Model != "small" {
		t.Errorf("second row model: got %q, want %q", rows[1].Model, "small")
	}
}

func TestBulkRecordUsageStats_Aggregates(t *testing.T) {
	s := newTestStore(t)

	bucket := "2026-06-27T15:00:00Z"
	entries := []StatsBatchEntry{
		{Bucket: bucket, DownstreamID: "anthropic", Model: "claude-opus-4-7",
			InputTokens: 100, OutputTokens: 50, CacheCreation: 10, CacheRead: 30},
		{Bucket: bucket, DownstreamID: "anthropic", Model: "claude-opus-4-7",
			InputTokens: 200, OutputTokens: 75, CacheCreation: 0, CacheRead: 0},
		{Bucket: bucket, DownstreamID: "openai", Model: "gpt-4o",
			InputTokens: 50, OutputTokens: 25, CacheCreation: 0, CacheRead: 0},
	}
	written, err := s.BulkRecordUsageStats(entries)
	if err != nil {
		t.Fatalf("bulk record: %v", err)
	}
	if written != 3 {
		t.Errorf("written: got %d, want 3", written)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateStats(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 model rows, got %d", len(rows))
	}

	// Sort is by total tokens desc; claude-opus-4-7 (300+125=425) > gpt-4o (50+25=75)
	var opus, gpt UsageStatsRow
	for _, r := range rows {
		if r.Model == "claude-opus-4-7" {
			opus = r
		} else if r.Model == "gpt-4o" {
			gpt = r
		}
	}
	if opus.InputTokens != 300 || opus.OutputTokens != 125 || opus.RequestCount != 2 || opus.CacheHitCount != 1 {
		t.Errorf("opus aggregation wrong: %+v", opus)
	}
	if gpt.RequestCount != 1 || gpt.CacheHitCount != 0 {
		t.Errorf("gpt aggregation wrong: %+v", gpt)
	}
}

func TestBulkRecordUsageStats_Empty(t *testing.T) {
	s := newTestStore(t)
	written, err := s.BulkRecordUsageStats(nil)
	if err != nil {
		t.Fatalf("empty bulk: %v", err)
	}
	if written != 0 {
		t.Errorf("written: got %d, want 0", written)
	}
}

func TestBulkRecordUsageStats_LargeChunk(t *testing.T) {
	// Exercise the multi-statement chunking path (maxRowsPerStmt = 3000).
	s := newTestStore(t)

	const N = 5000
	entries := make([]StatsBatchEntry, N)
	for i := 0; i < N; i++ {
		entries[i] = StatsBatchEntry{
			Bucket:       "2026-06-27T15:00:00Z",
			DownstreamID: "anthropic",
			Model:        "claude-opus-4-7",
			InputTokens:  1, OutputTokens: 1, CacheCreation: 0, CacheRead: 0,
		}
	}
	written, err := s.BulkRecordUsageStats(entries)
	if err != nil {
		t.Fatalf("large bulk: %v", err)
	}
	if written != N {
		t.Errorf("written: got %d, want %d", written, N)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateStats(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestCount != N {
		t.Errorf("expected 1 row with %d requests, got %+v", N, rows)
	}
}

func TestAggregateByProvider_GroupsByDownstream(t *testing.T) {
	s := newTestStore(t)

	// Seed the downstreams table so the LEFT JOIN in AggregateByProvider
	// can resolve human-readable names. Without these rows the test would
	// only exercise the COALESCE fallback path.
	if err := s.CreateDownstream(&Downstream{
		ID:   "anthropic",
		Name: "Anthropic",
	}); err != nil {
		t.Fatalf("seed downstream anthropic: %v", err)
	}
	if err := s.CreateDownstream(&Downstream{
		ID:   "openai",
		Name: "OpenAI",
	}); err != nil {
		t.Fatalf("seed downstream openai: %v", err)
	}

	// Three requests across two models under "anthropic", one under "openai".
	// Per-downstream rollup should produce two rows; anthropic must aggregate
	// across both its models.
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "claude-opus-4-7",
		100, 50, 0, 0); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "claude-sonnet-4",
		200, 75, 0, 30); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "openai", "gpt-4o",
		50, 25, 0, 0); err != nil {
		t.Fatalf("seed 3: %v", err)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateByProvider(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate by provider: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 provider rows, got %d", len(rows))
	}

	// Sorted by total tokens desc: anthropic (300 + 125 = 425) > openai (50 + 25 = 75).
	if rows[0].DownstreamID != "anthropic" {
		t.Errorf("first row downstream: got %q, want %q", rows[0].DownstreamID, "anthropic")
	}
	if rows[0].Name != "Anthropic" {
		t.Errorf("first row name: got %q, want %q", rows[0].Name, "Anthropic")
	}
	if rows[0].InputTokens != 300 || rows[0].OutputTokens != 125 {
		t.Errorf("anthropic tokens: got in=%d out=%d, want in=300 out=125",
			rows[0].InputTokens, rows[0].OutputTokens)
	}
	if rows[0].RequestCount != 2 {
		t.Errorf("anthropic request_count: got %d, want 2", rows[0].RequestCount)
	}
	if rows[0].CacheRead != 30 {
		t.Errorf("anthropic cache_read: got %d, want 30", rows[0].CacheRead)
	}
	if rows[0].CacheHitCount != 1 {
		t.Errorf("anthropic cache_hit_count: got %d, want 1", rows[0].CacheHitCount)
	}

	if rows[1].DownstreamID != "openai" {
		t.Errorf("second row downstream: got %q, want %q", rows[1].DownstreamID, "openai")
	}
	if rows[1].Name != "OpenAI" {
		t.Errorf("second row name: got %q, want %q", rows[1].Name, "OpenAI")
	}
	if rows[1].InputTokens != 50 || rows[1].OutputTokens != 25 {
		t.Errorf("openai tokens: got in=%d out=%d, want in=50 out=25",
			rows[1].InputTokens, rows[1].OutputTokens)
	}
	if rows[1].RequestCount != 1 {
		t.Errorf("openai request_count: got %d, want 1", rows[1].RequestCount)
	}
}

func TestAggregateByProvider_FallsBackToIDWhenDownstreamMissing(t *testing.T) {
	// Defensive case: if a downstream is deleted after usage_stats was
	// written, the LEFT JOIN will produce a NULL name; COALESCE must
	// fall back to the downstream_id so the dashboard still renders
	// something instead of an empty cell.
	s := newTestStore(t)

	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "orphan-downstream", "some-model",
		100, 50, 0, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateByProvider(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate orphan: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].DownstreamID != "orphan-downstream" {
		t.Errorf("downstream_id: got %q, want %q", rows[0].DownstreamID, "orphan-downstream")
	}
	if rows[0].Name != "orphan-downstream" {
		t.Errorf("name should fall back to downstream_id when missing, got %q", rows[0].Name)
	}
}

func TestAggregateByProvider_Empty(t *testing.T) {
	s := newTestStore(t)

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateByProvider(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate empty: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

func TestAggregateByProvider_OutsideRange(t *testing.T) {
	s := newTestStore(t)

	if err := s.RecordUsageStats("2026-06-27T15:00:00Z", "anthropic", "claude-opus-4-7",
		100, 50, 0, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Query a range that excludes the seeded bucket.
	from, _ := time.Parse("2006-01-02T15:04:05Z", "2027-01-01T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2027-01-02T00:00:00Z")
	rows, err := s.AggregateByProvider(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate outside range: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows outside range, got %d", len(rows))
	}
}

func TestPurgeUsageStatsBefore(t *testing.T) {
	s := newTestStore(t)

	// Old rows (should be deleted): 2024-12 and earlier. Each gets a unique
	// model so we can verify the purge by model instead of by time-window
	// overlap (the boundary between old/recent queries could otherwise pick
	// up a recent row).
	oldRows := []struct {
		bucket string
		model  string
	}{
		{"2024-01-15T10:00:00Z", "old-a"},
		{"2024-06-01T00:00:00Z", "old-b"},
		{"2024-12-31T23:00:00Z", "old-c"},
	}
	for _, r := range oldRows {
		if err := s.RecordUsageStats(r.bucket, "anthropic", r.model, 100, 50, 0, 0); err != nil {
			t.Fatalf("seed old %s: %v", r.bucket, err)
		}
	}

	// Recent rows (should be kept): 2025 and later.
	recentRows := []struct {
		bucket string
		model  string
	}{
		{"2025-01-01T00:00:00Z", "new-a"},
		{"2025-06-15T12:00:00Z", "new-b"},
		{"2026-06-27T15:00:00Z", "new-c"},
	}
	for _, r := range recentRows {
		if err := s.RecordUsageStats(r.bucket, "anthropic", r.model, 100, 50, 0, 0); err != nil {
			t.Fatalf("seed recent %s: %v", r.bucket, err)
		}
	}

	// Cutoff: 2025-01-01. Anything strictly less than this is purged.
	cutoff := "2025-01-01T00:00:00Z"
	n, err := s.PurgeUsageStatsBefore(cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != int64(len(oldRows)) {
		t.Errorf("rows deleted: got %d, want %d", n, len(oldRows))
	}

	// Query a wide range covering everything; verify which model rows survive.
	from, _ := time.Parse("2006-01-02T15:04:05Z", "2020-01-01T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2030-01-01T00:00:00Z")
	rows, err := s.AggregateStats(StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	seen := make(map[string]bool)
	for _, r := range rows {
		seen[r.Model] = true
	}
	for _, r := range oldRows {
		if seen[r.model] {
			t.Errorf("old row %q still present after purge", r.model)
		}
	}
	for _, r := range recentRows {
		if !seen[r.model] {
			t.Errorf("recent row %q missing after purge", r.model)
		}
	}
	if len(rows) != len(recentRows) {
		t.Errorf("expected %d rows total, got %d", len(recentRows), len(rows))
	}
}

func TestPurgeUsageStatsBefore_NoMatchingRows(t *testing.T) {
	s := newTestStore(t)

	// No rows at all — purge should be a safe no-op.
	n, err := s.PurgeUsageStatsBefore("2025-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("purge empty: %v", err)
	}
	if n != 0 {
		t.Errorf("rows deleted: got %d, want 0", n)
	}
}
