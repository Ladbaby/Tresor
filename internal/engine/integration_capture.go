package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"tresor/internal/inspect"
)

// integrationCapture is request-local and optional. A long tool loop can never
// retain unbounded inspection data, even before its log entry is published.
type integrationCapture struct {
	response                         boundedResponseCapture
	events                           []inspect.Event
	bytes                            int
	truncated                        bool
	taskID, executionID, inferenceID string
}

// accumulateManagedUsage sums separate upstream generations; Merge alone would
// overwrite earlier rounds (its ordinary purpose is joining SSE usage fields).
func accumulateManagedUsage(entry *RequestLogEntry, usage *UsageBlock) {
	if entry.Usage == nil {
		entry.Usage = &UsageBlock{}
	}
	add := func(dst **int64, src *int64) {
		if src == nil {
			return
		}
		n := *src
		if *dst != nil {
			n += **dst
		}
		*dst = &n
	}
	add(&entry.Usage.InputTokens, usage.InputTokens)
	add(&entry.Usage.OutputTokens, usage.OutputTokens)
	add(&entry.Usage.CacheCreationTokens, usage.CacheCreationTokens)
	add(&entry.Usage.CacheReadTokens, usage.CacheReadTokens)
	add(&entry.Usage.CachedTokens, usage.CachedTokens)
}

func (c *integrationCapture) projection(kind string, generation int, stored []byte, replayed bool) {
	// Decode only the public wire representation; never expose stored headers or
	// the base64-encoded internal replay envelope in the inspector.
	var envelope managedProjectionEnvelope
	if json.Unmarshal(stored, &envelope) != nil {
		return
	}
	body, ct := stored, "application/json"
	if envelope.Version == 1 {
		body = envelope.Wire
		if envelope.Streaming {
			ct = "text/event-stream"
		}
	}
	c.add(inspect.Event{Kind: kind, Generation: generation, Replayed: replayed, ContentType: ct, Body: string(body)})
}

const maxFlowEvents = 256

func (c *integrationCapture) add(event inspect.Event) {
	if c == nil {
		return
	}
	if len(c.events) >= maxFlowEvents {
		c.truncated = true
		return
	}
	// Include metadata in the budget, not only potentially large payloads.
	size := len(event.Body) + len(event.Kind) + len(event.ToolName) + len(event.CallID) + len(event.ContentType) + len(event.Error) + 128
	room := inspect.MaxBodyBytes - c.bytes
	if size > room {
		metadata := size - len(event.Body)
		if room < metadata {
			c.truncated = true
			return
		}
		event.Body = event.Body[:room-metadata]
		event.Truncated = true
		size = room
		c.truncated = true
	}
	// Detach a shortened string from the potentially much larger source buffer.
	event.Body = strings.Clone(event.Body)
	c.events = append(c.events, event)
	c.bytes += size
}

type inspectionWriter struct {
	http.ResponseWriter
	capture *boundedResponseCapture
}

func (w *inspectionWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	_, _ = w.capture.Write(p[:n])
	return n, err
}
func (w *inspectionWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func taskSummaryUsage(body []byte, contentType string) *UsageBlock {
	parse := func(raw []byte) *UsageBlock {
		var envelope struct {
			Bot struct {
				Scope string          `json:"usage_scope"`
				Usage json.RawMessage `json:"usage"`
			} `json:"tresorbot"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.Bot.Scope != "task" || len(envelope.Bot.Usage) == 0 || string(envelope.Bot.Usage) == "null" {
			return nil
		}
		wrapped, _ := json.Marshal(map[string]json.RawMessage{"usage": envelope.Bot.Usage})
		usage, _ := UsageFromResponseBody(wrapped)
		return usage
	}
	if strings.HasPrefix(contentType, "text/event-stream") {
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "data:") {
				if usage := parse([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))); usage != nil {
					return usage
				}
			}
		}
		return nil
	}
	return parse(body)
}

// beginIntegrationCapture observes actual client output, including generated
// errors and projected SSE, without replacing the existing encoder or flushing.
func (e *Engine) beginIntegrationCapture(w *statusCaptureWriter, r *http.Request, result *modelResult, entry *RequestLogEntry) func(bool) {
	if e.payloadStore == nil {
		return func(bool) {}
	}
	c := &integrationCapture{}
	if ex := executionFromRequest(r); ex != nil {
		c.executionID, c.inferenceID = ex.ExecutionID, ex.InferenceID
	}
	entry.integrationCapture = c
	original := w.ResponseWriter
	w.ResponseWriter = &inspectionWriter{ResponseWriter: original, capture: &c.response}
	return func(handled bool) {
		w.ResponseWriter = original
		if !handled {
			entry.integrationCapture = nil
			return
		}
		if entry.Status == 0 {
			entry.Status = w.status
		}
		if entry.Error != "" {
			c.add(inspect.Event{Kind: "error", Status: entry.Status, Error: entry.Error})
		}
		if usage := taskSummaryUsage(c.response.Bytes(), w.Header().Get("Content-Type")); usage != nil {
			entry.Usage = usage
			entry.UsageScope = "task"
		}
		e.recordAndCapture(entry, result.originalBody, c.response.Bytes(), r.Header.Get("Content-Type"), w.Header().Get("Content-Type"), c.response.truncated, detectInputFormat(r.URL.Path), detectInputFormat(r.URL.Path))
	}
}
