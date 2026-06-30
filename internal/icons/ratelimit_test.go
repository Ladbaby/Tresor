package icons

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tresor/internal/proxy"
)

// TestRateLimiter_BucketPacesRequests verifies the token-bucket layer:
// requests issued faster than the configured rate must wait. We set a
// very tight limit (2/s, burst 1) so the test runs fast.
func TestRateLimiter_BucketPacesRequests(t *testing.T) {
	rl := newRateLimiter()
	rl.cfg = iconRateConfig{Limit: 2, Burst: 1}
	rl.warm = warmStartConfig{Count: 0, Interval: time.Hour, Timeout: time.Hour}
	rl.bucket = rateNewLimiterFromCfg(rl.cfg)

	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := rl.wait(context.Background()); err != nil {
			t.Fatalf("wait #%d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	// 4 requests at 2/s with burst 1: first is immediate, then ~1500ms total.
	if elapsed < 1200*time.Millisecond {
		t.Errorf("expected ~1.5s elapsed for 4 reqs at 2/s burst 1, got %s", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("rate limiter waited too long: %s", elapsed)
	}
}

// TestRateLimiter_WarmStartOpens confirms that after activating the
// warm-start window, the first few wait() calls each take ~Interval
// instead of returning immediately from the burst bucket.
func TestRateLimiter_WarmStartOpens(t *testing.T) {
	rl := newRateLimiter()
	rl.warm = warmStartConfig{Count: 5, Interval: 40 * time.Millisecond, Timeout: time.Second}
	rl.bucket = rateNewLimiterFromCfg(iconRateConfig{Limit: 100, Burst: 100})

	rl.activateWarmStart()
	start := time.Now()
	// First call consumes the first warm token (immediately available).
	if err := rl.wait(context.Background()); err != nil {
		t.Fatalf("wait #0: %v", err)
	}
	// Subsequent calls each wait ~Interval (40 ms) for the next token.
	for i := 1; i < 5; i++ {
		if err := rl.wait(context.Background()); err != nil {
			t.Fatalf("wait #%d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	// 5 warm tokens spaced 40ms apart → ~160ms minimum.
	if elapsed < 120*time.Millisecond {
		t.Errorf("warm-start should pace requests ~40ms apart, total elapsed=%s", elapsed)
	}
}

// TestRateLimiter_ActivateIsIdempotent: a second activateWarmStart call
// while the window is open is a no-op (not a reset), and the window
// closes on its own once the producer goroutine drains.
func TestRateLimiter_ActivateIsIdempotent(t *testing.T) {
	rl := newRateLimiter()
	rl.warm = warmStartConfig{Count: 5, Interval: 30 * time.Millisecond, Timeout: 5 * time.Second}
	rl.bucket = rateNewLimiterFromCfg(iconRateConfig{Limit: 100, Burst: 100})

	rl.activateWarmStart()
	rl.activateWarmStart() // should NOT reset

	// Consume all the warm tokens.
	for i := 0; i < 5; i++ {
		_ = rl.wait(context.Background())
	}
	// Producer goroutine + watcher should close the window shortly after.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rl.mu.Lock()
		active := rl.warmActive
		rl.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("warm-start window did not close after producer drained")
}

// TestRateLimiter_WarmStartTimeoutCloses: even if fewer than Count
// requests happen, the window closes after Timeout.
func TestRateLimiter_WarmStartTimeoutCloses(t *testing.T) {
	rl := newRateLimiter()
	rl.warm = warmStartConfig{Count: 100, Interval: time.Second, Timeout: 100 * time.Millisecond}
	rl.bucket = rateNewLimiterFromCfg(iconRateConfig{Limit: 100, Burst: 100})

	rl.activateWarmStart()
	time.Sleep(250 * time.Millisecond)
	rl.mu.Lock()
	active := rl.warmActive
	rl.mu.Unlock()
	if active {
		t.Errorf("warm-start window should have closed by timeout")
	}
}

// TestFetcher_RateLimitingSlowsBurst ensures the Fetcher respects the
// rate limit by measuring the spread between first and last CDN hits
// when issuing many concurrent Icon() calls for distinct URLs.
func TestFetcher_RateLimitingSlowsBurst(t *testing.T) {
	var hits int32
	var firstHit, lastHit atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		now := time.Now().UnixNano()
		if n == 1 {
			firstHit.Store(now)
		}
		lastHit.Store(now)
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(makeSVG(24))
	}))
	defer cdn.Close()

	f := newTestFetcher(t, cdn)
	// Force a tighter rate so the test runs fast.
	f.iconRate.cfg = iconRateConfig{Limit: 10, Burst: 2}
	f.iconRate.bucket = rateNewLimiterFromCfg(f.iconRate.cfg)
	// Empty index (not Ready) so candidates pass through unfiltered.
	f.index = newIndex(t.TempDir(), proxy.ModeNone, cdn.Client(), nil)

	// Each model ID has a unique first segment (no trailing digit),
	// yielding 8 unique candidate URLs and 8 distinct CDN hits.
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		i := i
		go func() {
			model := fmt.Sprintf("ratemodel%c-xyz", 'a'+rune(i))
			_, _, _ = f.Icon(model)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}

	spread := time.Duration(lastHit.Load() - firstHit.Load())
	// 8 unique URLs at limit=10/s burst=2: first 2 immediate, then ~100ms each.
	// Expect at least ~400ms spread.
	if spread < 300*time.Millisecond {
		t.Errorf("expected fetcher to pace 8 unique URLs over at least ~300ms with limit=10 burst=2, got spread=%s hits=%d", spread, hits)
	}
	if got := atomic.LoadInt32(&hits); got < 8 {
		t.Errorf("expected 8 CDN hits (distinct URLs), got %d", got)
	}
}