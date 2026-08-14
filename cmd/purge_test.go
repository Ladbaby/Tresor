package cmd

import (
	"os"
	"testing"
	"time"

	"tresor/internal/store"
)

func TestPurgeOldUsageStats_RemovesOldKeepsRecent(t *testing.T) {
	f, err := os.CreateTemp("", "tresor-purge-*.db")
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

	// Anchor time: pretend the daemon is starting up at this moment.
	// (purgeOldUsageStats uses time.Now() internally; we can't pin it
	// without refactoring, so we use boundary fixtures that are
	// obviously old or obviously new relative to "now".)
	now := time.Now().UTC()

	// 18 months ago → must be purged (older than 12 months).
	eighteen := now.AddDate(0, -18, 0).Format("2006-01-02T15:00:00Z")
	// 6 months ago → must be kept (within the 12-month window).
	six := now.AddDate(0, -6, 0).Format("2006-01-02T15:00:00Z")
	// 1 hour ago → must be kept.
	recent := now.Add(-time.Hour).Format("2006-01-02T15:00:00Z")

	if err := s.RecordUsageStats(eighteen, "anthropic", "old-model", 100, 50, 0, 0); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := s.RecordUsageStats(six, "anthropic", "recent-model", 100, 50, 0, 0); err != nil {
		t.Fatalf("seed recent: %v", err)
	}
	if err := s.RecordUsageStats(recent, "anthropic", "newest-model", 100, 50, 0, 0); err != nil {
		t.Fatalf("seed newest: %v", err)
	}

	if err := purgeOldUsageStats(s); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// Query the whole history and check which models survived.
	from := now.AddDate(-5, 0, 0)
	to := now.Add(time.Hour)
	rows, err := s.AggregateStats(store.StatsQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Model] = true
	}
	if seen["old-model"] {
		t.Error("old-model (18mo ago) should have been purged")
	}
	if !seen["recent-model"] {
		t.Error("recent-model (6mo ago) should still be present")
	}
	if !seen["newest-model"] {
		t.Error("newest-model (1h ago) should still be present")
	}
}

func TestPurgeOldUsageStats_NoOldRows(t *testing.T) {
	f, err := os.CreateTemp("", "tresor-purge-empty-*.db")
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

	// Should be a safe no-op.
	if err := purgeOldUsageStats(s); err != nil {
		t.Fatalf("purge empty: %v", err)
	}
}

func TestUsageStatsRetentionMonths_IsTwelve(t *testing.T) {
	// Pin the documented retention window so anyone changing it has to update
	// this test (and the associated plan / docs).
	if usageStatsRetentionMonths != 12 {
		t.Errorf("usageStatsRetentionMonths: got %d, want 12", usageStatsRetentionMonths)
	}
}