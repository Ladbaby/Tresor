package icons

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// iconRateConfig holds the limits used by rateLimiter. These are the
// production defaults; tests can override per-fetcher.
type iconRateConfig struct {
	// rate is the steady-state requests-per-second cap. Default 4.
	rate.Limit
	// burst is the maximum number of requests allowed in a short burst.
	// Default 8.
	Burst int
}

var defaultIconRate = iconRateConfig{
	Limit: rate.Limit(4),
	Burst: 8,
}

// warmStartConfig controls the one-shot pacing after each successful
// index sync: the first N fetches are paced to one every Interval, then
// the steady-state rate limiter takes over.
type warmStartConfig struct {
	// Count is the number of fetches that go through the pacing path
	// before the steady-state limiter resumes. Default 30.
	Count int
	// Interval is the gap between paced fetches. Default 50ms.
	Interval time.Duration
	// Timeout caps how long the warm-start window can stay open even
	// when fewer than Count fetches happen. Default 5s.
	Timeout time.Duration
}

var defaultWarmStart = warmStartConfig{
	Count:    30,
	Interval: 50 * time.Millisecond,
	Timeout:  5 * time.Second,
}

// rateLimiter combines a token bucket (rate.Limiter) with a one-shot
// warm-start pacing channel. The warm-start path fires immediately after
// each successful index sync; it smooths the first burst of icon requests
// so they don't all hit the CDN in the same millisecond.
type rateLimiter struct {
	mu sync.Mutex

	cfg iconRateConfig
	warm warmStartConfig

	bucket *rate.Limiter

	// warmActive is true while the warm-start window is open. Once false,
	// the bucket alone handles pacing.
	warmActive bool
	warmEnd    time.Time
	// warmTokens is a buffered channel that yields one token per Interval.
	// Closed by warmCloseCh when the window expires (by count or timeout).
	warmTokens   <-chan struct{}
	warmCloseCh  chan struct{}
	warmDoneCh   chan struct{} // closed when the producer goroutine exits
	warmRemaining int
}

// newRateLimiter returns a rateLimiter initialized with the production
// defaults. Tests can override fields directly after construction.
func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		cfg:    defaultIconRate,
		warm:   defaultWarmStart,
		bucket: rate.NewLimiter(defaultIconRate.Limit, defaultIconRate.Burst),
	}
}

// activateWarmStart opens a new warm-start window. The next waitWarmStart
// calls will be paced to one per Interval until Count tokens are consumed
// or Timeout elapses. Safe to call concurrently; subsequent calls within
// the active window are no-ops.
func (r *rateLimiter) activateWarmStart() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warmActive {
		return
	}
	ch := make(chan struct{}, r.warm.Count)
	closeCh := make(chan struct{})
	doneCh := make(chan struct{})
	ticker := time.NewTicker(r.warm.Interval)
	go func() {
		defer close(doneCh)
		defer ticker.Stop()
		for i := 0; i < r.warm.Count; i++ {
			select {
			case <-closeCh:
				return
			case <-ticker.C:
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	// Background watcher closes the window when the producer finishes
	// or the timeout elapses, whichever comes first.
	go func() {
		select {
		case <-doneCh:
		case <-time.After(r.warm.Timeout):
		case <-closeCh:
			return
		}
		r.closeWarmStart()
	}()
	r.warmActive = true
	r.warmEnd = time.Now().Add(r.warm.Timeout)
	r.warmTokens = ch
	r.warmCloseCh = closeCh
	r.warmDoneCh = doneCh
	r.warmRemaining = r.warm.Count
}

// closeWarmStart releases the goroutine backing the warm-start ticker.
func (r *rateLimiter) closeWarmStart() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.warmActive {
		return
	}
	close(r.warmCloseCh)
	r.warmActive = false
	r.warmTokens = nil
	r.warmCloseCh = nil
	r.warmDoneCh = nil
	r.warmRemaining = 0
}

// wait blocks until the caller is allowed to make one more request, or
// ctx is canceled. Honors the warm-start window if active; otherwise
// uses the token bucket.
//
// Returns ctx.Err() on cancellation.
func (r *rateLimiter) wait(ctx context.Context) error {
	// Try to consume a warm-start token first if available.
	r.mu.Lock()
	if r.warmActive {
		if time.Now().After(r.warmEnd) || r.warmRemaining <= 0 {
			r.mu.Unlock()
			r.closeWarmStart()
		} else {
			tokens := r.warmTokens
			done := r.warmDoneCh
			r.mu.Unlock()
			select {
			case <-tokens:
				r.mu.Lock()
				r.warmRemaining--
				// If we just drained the last token AND the producer
				// has finished, close the window.
				if r.warmRemaining <= 0 && r.warmDoneCh != nil {
					select {
					case <-done:
						// producer already finished; safe to close
					default:
					}
				}
				r.mu.Unlock()
				return nil
			case <-done:
				// Producer exited (count reached). Close the window
				// and fall through to the bucket.
				r.closeWarmStart()
			case <-time.After(time.Until(r.warmEnd)):
				r.closeWarmStart()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	} else {
		r.mu.Unlock()
	}
	return r.bucket.Wait(ctx)
}

// retryAfterSleep sleeps for d or until ctx is canceled. Used to honor
// Retry-After headers from the CDN.
func retryAfterSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// rateNewLimiterFromCfg builds a rate.Limiter from the given config. It
// lives here so tests in this package can construct one without reaching
// into rate directly.
func rateNewLimiterFromCfg(cfg iconRateConfig) *rate.Limiter {
	return rate.NewLimiter(cfg.Limit, cfg.Burst)
}