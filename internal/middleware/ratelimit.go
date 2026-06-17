package middleware

import (
	"sync"
	"time"
)

// rateLimiter enforces a sliding-window rate limit per key (e.g., client IP).
// MaxAttempts failures are allowed within WindowDuration before the key is blocked.
type rateLimiter struct {
	mu    sync.Mutex
	entries map[string]*rateEntry
	max    int
	window time.Duration
	done   chan struct{}
	wg     sync.WaitGroup
}

type rateEntry struct {
	Attempts []time.Time
}

// newRateLimiter creates a rate limiter. caller specifies max attempts and time window.
func newRateLimiter(max int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		entries: make(map[string]*rateEntry),
		max:     max,
		window:  window,
		done:    make(chan struct{}),
	}
	// Clean stale entries every 5 minutes
	rl.wg.Add(1)
	go func() {
		defer rl.wg.Done()
		rl.cleanupLoop()
	}()
	return rl
}

// Stop signals the cleanup goroutine to exit and waits for it to finish.
func (rl *rateLimiter) Stop() {
	close(rl.done)
	rl.wg.Wait()
}

// record records a failed attempt for the given key. Returns true if the key is now blocked.
func (rl *rateLimiter) record(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	e, ok := rl.entries[key]
	if !ok {
		e = &rateEntry{}
		rl.entries[key] = e
	}

	now := time.Now()
	e.Attempts = append(e.Attempts, now)

	// Prune entries outside the window
	cutoff := now.Add(-rl.window)
	pruned := make([]time.Time, 0, len(e.Attempts))
	for _, t := range e.Attempts {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	e.Attempts = pruned

	return len(e.Attempts) > rl.max
}

// cleanupLoop removes stale entries periodically. Exits when rl.done is closed.
func (rl *rateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.window)
			for k, e := range rl.entries {
				valid := make([]time.Time, 0, len(e.Attempts))
				for _, t := range e.Attempts {
					if t.After(cutoff) {
						valid = append(valid, t)
					}
				}
				if len(valid) == 0 {
					delete(rl.entries, k)
				} else {
					e.Attempts = valid
				}
			}
			rl.mu.Unlock()
		case <-rl.done:
			return
		}
	}
}
