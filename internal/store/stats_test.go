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
