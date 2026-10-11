package engine

import (
	"log"
	"time"
	"tresor/internal/store"
)

// statsFlushInterval is how often the engine drains its in-memory stats
// buffer to SQLite. 60s keeps WAL checkpoint pressure low while still
// surfacing fresh data on the Dashboard within a minute of activity.
const statsFlushInterval = 60 * time.Second

// statsBufferWarnThreshold emits a debug log when the in-memory buffer
// exceeds this many entries — a sign the flush cadence isn't keeping up
// with the request rate and might need tuning.
const statsBufferWarnThreshold = 50_000

// Stop flushes any buffered stats and stops the background flusher goroutine.
// Safe to call once; subsequent calls are no-ops. The daemon should call this
// on graceful shutdown so no in-flight stats are lost.
func (e *Engine) Stop() {
	if e.statsStop == nil {
		return
	}
	select {
	case <-e.statsStop:
		// already closed
	default:
		close(e.statsStop)
	}
	if e.statsDone != nil {
		<-e.statsDone
	}
}

// flushStatsNow drains and writes the current buffer immediately. Used by
// the periodic flusher and by Stop(). Errors are logged, never returned —
// losing a batch of stats must not abort the daemon.
func (e *Engine) flushStatsNow() {
	e.statsBufMu.Lock()
	if len(e.statsBuf) == 0 {
		e.statsBufMu.Unlock()
		return
	}
	batch := e.statsBuf
	e.statsBuf = nil
	e.statsBufMu.Unlock()

	if e.store == nil {
		return
	}
	written, err := e.store.BulkRecordUsageStats(batch)
	if err != nil {
		log.Printf("stats flush failed (%d entries dropped): %v", len(batch), err)
		return
	}
	if written > 0 {
		log.Printf("stats flush: wrote %d usage_stats rows", written)
	}

	// Per-client-IP rollup, best-effort like the main table above: a failed
	// IP flush must not abort the daemon. Entries without a ClientIP are
	// skipped inside the store call.
	if ipWritten, ipErr := e.store.BulkRecordIPUsageStats(batch); ipErr != nil {
		log.Printf("ip stats flush failed (%d entries dropped): %v", len(batch), ipErr)
	} else if ipWritten > 0 {
		log.Printf("stats flush: wrote %d ip_usage_stats rows", ipWritten)
	}
}

// runStatsFlusher drains the in-memory stats buffer every statsFlushInterval
// and on engine shutdown. The interval is the upper bound on how stale the
// Dashboard can read; in practice, the dashboard sees fresh data within
// (flush interval) seconds of the last request.
func (e *Engine) runStatsFlusher() {
	defer close(e.statsDone)
	ticker := time.NewTicker(statsFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.statsStop:
			e.flushStatsNow() // final flush on shutdown
			return
		case <-ticker.C:
			e.flushStatsNow()
		}
	}
}

// bufferStats appends one entry to the in-memory stats buffer. The buffer is
// flushed by runStatsFlusher every statsFlushInterval (and on shutdown).
// Allocated on first use so engines that never see a payload-captured
// request pay nothing.
func (e *Engine) bufferStats(entry store.StatsBatchEntry) {
	e.statsBufMu.Lock()
	e.statsBuf = append(e.statsBuf, entry)
	overThreshold := len(e.statsBuf) >= statsBufferWarnThreshold
	e.statsBufMu.Unlock()
	if overThreshold {
		log.Printf("warning: stats buffer at %d entries — flush interval may be too long for current request rate", statsBufferWarnThreshold)
	}
}
