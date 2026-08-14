package engine

import (
	"os"
	"testing"
	"time"

	"tresor/internal/store"
)

// newTestEngineForStats creates a fresh engine backed by a temp SQLite DB,
// suitable for testing the stats-buffer / flusher plumbing.
func newTestEngineForStats(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	f, err := os.CreateTemp("", "tresor-engine-stats-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	s, err := store.Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return New(s), s
}

func TestEngine_BufferStatsAndFlush(t *testing.T) {
	eng, s := newTestEngineForStats(t)
	defer eng.Stop()

	// Buffer several entries directly (without going through the full
	// proxy pipeline) so we can verify the flusher writes them in bulk.
	for i := 0; i < 5; i++ {
		eng.bufferStats(store.StatsBatchEntry{
			Bucket:       "2026-06-27T15:00:00Z",
			DownstreamID: "anthropic",
			Model:        "claude-opus-4-7",
			InputTokens:  100, OutputTokens: 50, CacheCreation: 10, CacheRead: 30,
		})
	}

	// Buffer should have 5 entries; nothing on disk yet.
	eng.statsBufMu.Lock()
	bufLen := len(eng.statsBuf)
	eng.statsBufMu.Unlock()
	if bufLen != 5 {
		t.Fatalf("buffer len: got %d, want 5", bufLen)
	}

	// Manually flush and confirm the store sees the aggregated rows.
	eng.flushStatsNow()

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateStats(store.StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 model row, got %d", len(rows))
	}
	r := rows[0]
	if r.InputTokens != 500 || r.OutputTokens != 250 {
		t.Errorf("aggregation wrong: in=%d out=%d", r.InputTokens, r.OutputTokens)
	}
	if r.RequestCount != 5 {
		t.Errorf("request_count: got %d, want 5", r.RequestCount)
	}
	if r.CacheHitCount != 5 {
		t.Errorf("cache_hit_count: got %d, want 5", r.CacheHitCount)
	}

	// Buffer should now be empty.
	eng.statsBufMu.Lock()
	bufLen = len(eng.statsBuf)
	eng.statsBufMu.Unlock()
	if bufLen != 0 {
		t.Errorf("buffer should be drained, got %d", bufLen)
	}
}

func TestEngine_FlushEmptyBuffer(t *testing.T) {
	eng, _ := newTestEngineForStats(t)
	defer eng.Stop()

	// Flushing an empty buffer must be a safe no-op.
	eng.flushStatsNow()
	eng.flushStatsNow() // idempotent
}

func TestEngine_StopDrainsBuffer(t *testing.T) {
	eng, s := newTestEngineForStats(t)

	// Buffer some entries but don't flush — Stop() must drain them.
	for i := 0; i < 3; i++ {
		eng.bufferStats(store.StatsBatchEntry{
			Bucket:       "2026-06-27T15:00:00Z",
			DownstreamID: "openai",
			Model:        "gpt-4o",
			InputTokens:  10, OutputTokens: 5, CacheCreation: 0, CacheRead: 0,
		})
	}

	eng.Stop()

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateStats(store.StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate after stop: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestCount != 3 {
		t.Errorf("expected 3 rows persisted after Stop, got %+v", rows)
	}
}

func TestEngine_StopIsIdempotent(t *testing.T) {
	eng, _ := newTestEngineForStats(t)
	eng.Stop()
	// A second call must not panic or deadlock.
	eng.Stop()
}

func TestStatsFlushInterval_IsSixtySeconds(t *testing.T) {
	// Pin the documented value so anyone tightening it has to update this test.
	if statsFlushInterval != 60*time.Second {
		t.Errorf("statsFlushInterval: got %v, want 60s", statsFlushInterval)
	}
}

// TestEngine_BufferStatsIsAllocationLazy verifies that bufferStats works on
// a fresh engine without panicking — the underlying nil slice is fine for
// append. Engines that never see a payload-captured request still pay the
// goroutine cost of the flusher, but no per-request allocation happens.
func TestEngine_BufferStatsOnFreshEngine(t *testing.T) {
	eng, s := newTestEngineForStats(t)
	defer eng.Stop()

	// First bufferStats call should not panic even though statsBuf is nil.
	eng.bufferStats(store.StatsBatchEntry{
		Bucket: "2026-06-27T15:00:00Z", DownstreamID: "x", Model: "y",
		InputTokens: 1, OutputTokens: 1,
	})
	eng.flushStatsNow()

	from, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T00:00:00Z")
	to, _ := time.Parse("2006-01-02T15:04:05Z", "2026-06-27T23:59:59Z")
	rows, err := s.AggregateStats(store.StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestCount != 1 {
		t.Errorf("expected 1 row, got %+v", rows)
	}
}