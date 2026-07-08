package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LogLevel represents the verbosity of request logging.
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

var logLevelNames = []string{"debug", "info", "warn", "error"}

func (l LogLevel) String() string {
	if l < 0 || l >= LogLevel(len(logLevelNames)) {
		return "info"
	}
	return logLevelNames[l]
}

// ParseLogLevel returns a LogLevel from its string name.
func ParseLogLevel(s string) (LogLevel, error) {
	s = strings.ToLower(s)
	for i, name := range logLevelNames {
		if s == name {
			return LogLevel(i), nil
		}
	}
	return LogLevelInfo, fmt.Errorf("unknown log level %q; must be one of: %s", s, strings.Join(logLevelNames, ", "))
}

// DurationMs is a time.Duration that marshals to/from milliseconds in JSON.
type DurationMs time.Duration

func (d DurationMs) MarshalJSON() ([]byte, error) {
	ms := float64(time.Duration(d)) / float64(time.Millisecond)
	return json.Marshal(ms)
}

func (d *DurationMs) UnmarshalJSON(data []byte) error {
	var ms float64
	if err := json.Unmarshal(data, &ms); err != nil {
		return err
	}
	*d = DurationMs(time.Duration(ms) * time.Millisecond)
	return nil
}

// RequestLogEntry captures a single gateway request.
type RequestLogEntry struct {
	ID             int       `json:"id"`
	Timestamp      time.Time `json:"timestamp"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	Model          string    `json:"model"`
	ResolvedModel  string    `json:"resolved_model,omitempty"`
	DownstreamID   string    `json:"downstream_id"`
	DownstreamName string    `json:"downstream_name"`
	AliasGroup     string    `json:"alias_group,omitempty"`
	RuleIDs        []string  `json:"rule_ids,omitempty"`
	ClientIP       string    `json:"client_ip,omitempty"`
	Status         int       `json:"status"`
	Duration       DurationMs `json:"duration"`
	Error          string    `json:"error,omitempty"`
	Level          string    `json:"level,omitempty"`
	Message        string    `json:"message,omitempty"`
}

// logSeverity returns the severity level of a log entry based on status code and type.
func (e RequestLogEntry) logSeverity() LogLevel {
	if e.Level == "debug" {
		return LogLevelDebug
	}
	if e.Error != "" {
		return LogLevelError
	}
	if e.Status >= 500 {
		return LogLevelError
	}
	if e.Status >= 400 {
		return LogLevelWarn
	}
	return LogLevelInfo
}

const maxLogEntries = 500

// RequestLogger collects gateway request logs in a circular buffer
// and supports SSE streaming for real-time UI updates.
type RequestLogger struct {
	mu       sync.RWMutex
	level    LogLevel
	entries  []RequestLogEntry
	nextID   int
	writers  []logWriter // SSE subscribers
}

type logWriter struct {
	flusher http.Flusher
	writer  http.ResponseWriter
}

// NewRequestLogger creates a new RequestLogger.
func NewRequestLogger() *RequestLogger {
	return &RequestLogger{
		level:   LogLevelInfo,
		entries: make([]RequestLogEntry, 0, maxLogEntries),
	}
}

// SetLevel updates the log level filter.
func (l *RequestLogger) SetLevel(level LogLevel) {
	l.mu.Lock()
	l.level = level
	l.mu.Unlock()
}

// Level returns the current log level.
func (l *RequestLogger) Level() LogLevel {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.level
}

// Debug records a debug-level system message. These entries are filtered
// out when the log level is above LogLevelDebug, and display as system
// messages in the web UI rather than traffic entries.
func (l *RequestLogger) Debug(format string, args ...interface{}) {
	if l == nil {
		return
	}
	l.Record(&RequestLogEntry{
		Timestamp: time.Now(),
		Level:     "debug",
		Message:   fmt.Sprintf(format, args...),
	})
}

// Record adds a new log entry and notifies SSE subscribers.
// The entry's ID field is assigned by Record (atomically incrementing
// nextID) so the caller can read the assigned id back from the same
// pointer. Callers that need the assigned id (e.g. the inspector store)
// must pass a pointer; the helper recordAndCapture relies on this.
func (l *RequestLogger) Record(entry *RequestLogEntry) {
	if l == nil || entry == nil {
		return
	}
	l.mu.Lock()
	entry.ID = l.nextID
	l.nextID++

	// Check if this entry passes the level filter
	if entry.logSeverity() < l.level {
		l.mu.Unlock()
		return
	}

	l.entries = append(l.entries, *entry)
	if len(l.entries) > maxLogEntries {
		l.entries = l.entries[len(l.entries)-maxLogEntries:]
	}

	// Snapshot writers under lock to avoid race with Subscribe/Unsubscribe
	writers := make([]logWriter, len(l.writers))
	copy(writers, l.writers)
	l.mu.Unlock()

	// Iterate the snapshot without holding the lock. We pass *entry so the
	// serialized JSON includes the assigned id.
	var failed []logWriter
	for _, w := range writers {
		if err := sendSSEEvent(w.writer, w.flusher, "log", *entry); err != nil {
			failed = append(failed, w)
		}
	}

	// Batch-remove failed writers under lock
	if len(failed) > 0 {
		l.mu.Lock()
		for _, f := range failed {
			for i, sw := range l.writers {
				if sw.writer == f.writer {
					l.writers = append(l.writers[:i], l.writers[i+1:]...)
					break
				}
			}
		}
		l.mu.Unlock()
	}
}

// RecentEntries returns the last n entries, newest first.
// Entries are already filtered by level at insertion time in Record().
func (l *RequestLogger) RecentEntries(n int) []RequestLogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	start := len(l.entries) - n
	if start < 0 {
		start = 0
	}
	filtered := make([]RequestLogEntry, 0, len(l.entries)-start)
	for i := start; i < len(l.entries); i++ {
		filtered = append(filtered, l.entries[i])
	}
	// Return newest first (reverse chronological order)
	for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	return filtered
}

// Subscribe adds an SSE writer. Call this when a new client connects to the stream endpoint.
func (l *RequestLogger)Subscribe(w http.ResponseWriter, flusher http.Flusher) {
	l.mu.Lock()
	l.writers = append(l.writers, logWriter{flusher: flusher, writer: w})
	l.mu.Unlock()
}

// Unsubscribe removes an SSE writer.
func (l *RequestLogger)Unsubscribe(w http.ResponseWriter) {
	l.mu.Lock()
	for i, sw := range l.writers {
		if sw.writer == w {
			l.writers = append(l.writers[:i], l.writers[i+1:]...)
			break
		}
	}
	l.mu.Unlock()
}

// StreamLogs serves an SSE stream of log entries.
// It first sends recent entries as a batch, then streams new ones in real-time.
func (l *RequestLogger) StreamLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send initial batch of recent entries
	recent := l.RecentEntries(50)
	for _, entry := range recent {
		if err := sendSSEEvent(w, flusher, "log", entry); err != nil {
			log.Printf("SSE write error: %v", err)
			return
		}
	}

	// Subscribe to new entries
	l.Subscribe(w, flusher)
	defer l.Unsubscribe(w)

	// Send keep-alive comments to prevent premature disconnect
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Wait for client disconnect via request context
	ctx := r.Context()

	// Broadcast current level and config on connect
	sendSSEEvent(w, flusher, "config", map[string]interface{}{
		"level": l.Level().String(),
	})

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Write SSE comment to keep connection alive. SafeFlush swallows
			// panics from a closed/hijacked client so the logger goroutine
			// doesn't crash the request that called into Record/Debug.
			func() {
				defer func() { _ = recover() }()
				fmt.Fprint(w, ": heartbeat\n\n")
			}()
			SafeFlush(flusher)
		}
	}
}

func sendSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "event:", event)
	fmt.Fprintln(w, "data:", string(jsonData))
	fmt.Fprintln(w, "") // empty line terminates SSE event
	if !SafeFlush(flusher) {
		return fmt.Errorf("sse flush failed (client gone)")
	}
	return nil
}
