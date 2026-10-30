package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sync"
	"time"

	"tresor/internal/jsplugins"
)

// managedHeartbeatInterval is the waiting-comment cadence for streaming
// managed responses. A package variable keeps tests fast.
var managedHeartbeatInterval = 5 * time.Second

// managedHoldCommitChars is how much answer text a turn may buffer before the
// streamer stops treating it as possibly-intermediate planning and streams it
// live. Intermediate turns that go on to call tools are short (observed
// ~160 chars), while real answers run to hundreds; committing after this many
// characters keeps the answer progressive without leaking planning text.
var managedHoldCommitChars = 256

// managedStreamer relays a managed generation to a streaming client as it is
// produced. The client's stream request governs the transport; the policy
// only shapes content (heartbeats, intermediate turns).
type managedStreamer struct {
	w         *statusCaptureWriter
	heartbeat bool
	// live means the policy shows intermediate turns: every chunk is relayed
	// verbatim from the first byte.
	live bool

	mu        sync.Mutex
	committed bool
	finished  bool
	// turnLive commits the current turn to live streaming once its held text
	// proves large enough to be an answer rather than planning.
	turnLive bool
	// output records whether any of the final answer reached the client, so the
	// publish path knows not to re-send the stored wire.
	output bool
	hold   bytes.Buffer
	// usage carries the downstream usage block (if any) so a synthesized
	// finish still reports token statistics.
	usage json.RawMessage
	// streamed is closed the first time answer content is written, so the
	// heartbeat goroutine stops signalling "working" once the reply is flowing.
	streamed chan struct{}
	once     sync.Once
	// atLineStart tracks whether the next progress delta begins on a fresh
	// line, so a status never glues itself onto the trailing liveness dots.
	atLineStart bool
	// template renders the progress line, e.g. "Waiting for {label}…".
	template string
	// activity is the current human-readable status shown in progress frames.
	activity string
}

func newManagedStreamer(w *statusCaptureWriter, decision jsplugins.Decision, live bool) *managedStreamer {
	return &managedStreamer{
		w:           w,
		heartbeat:   decision.WantsHeartbeat(),
		live:        live,
		streamed:    make(chan struct{}),
		template:    decision.ActivityTemplate(),
		activity:    "Thinking…",
		atLineStart: true,
	}
}

// setActivity records what the bot is doing now and wakes the heartbeat
// goroutine so the new status is emitted at once. An empty status is ignored.
func (s *managedStreamer) setActivity(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || !s.committed || s.activity == text {
		if !s.finished {
			s.activity = text
		}
		return
	}
	s.activity = text
	if !s.heartbeat {
		return
	}
	// Emit at once so the new step is visible without waiting for the next
	// scheduled tick.
	if frame := s.heartbeatFrame(text); frame != nil {
		_, _ = s.w.Write(frame)
		s.w.Flush()
	}
}

// renderActivity fills the progress template with the given tool name/label.
func (s *managedStreamer) renderActivity(tool, label string) string {
	text := s.template
	if label == "" {
		label = tool
	}
	text = strings.ReplaceAll(text, "{label}", label)
	text = strings.ReplaceAll(text, "{tool}", tool)
	return text
}

// toolActivity renders the progress line for a tool step.
func (s *managedStreamer) toolActivity(name, label string) string {
	return s.renderActivity(name, label)
}

// markStreamed signals that answer content is now flowing, which retires the
// heartbeat indicator. Safe to call many times.
func (s *managedStreamer) markStreamed() {
	s.once.Do(func() { close(s.streamed) })
}

// currentActivity returns the status line to show in progress frames.
func (s *managedStreamer) currentActivity() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activity == "" {
		return "Working…"
	}
	return s.activity
}

// heartbeatFrame builds the wire frame emitted while the bot is working. It is
// a real chat.completion.chunk rather than an SSE comment: comments are
// ignored by SSE clients, so a chat client could never surface them. The mark
// travels in reasoning_content so clients render it as a "thinking" indicator
// and it never mixes into the answer text.
func (s *managedStreamer) heartbeatFrame(text string) []byte {
	if text == "" {
		return nil
	}
	// Each status occupies its own line: clients append reasoning deltas to one
	// buffer, so without a trailing break consecutive steps run together and
	// become unreadable. When liveness dots are pending on the current line, a
	// leading break keeps the new status from being glued onto them.
	if !s.atLineStart {
		text = "\n" + text
	}
	s.atLineStart = true
	return s.progressFrame(text + "\n")
}

// progressFrame renders one progress delta.
func (s *managedStreamer) progressFrame(text string) []byte {
	chunk, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-managed",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"reasoning_content": text},
			"finish_reason": nil,
		}},
	})
	if err != nil {
		return nil
	}
	return []byte("data: " + string(chunk) + "\n\n")
}

// commit sends the SSE headers before any generation work so the client sees
// progress immediately. When heartbeats are enabled it also emits one
// heartbeat frame right away, so there is an immediate sign of life instead of
// silence until the first interval elapses. Idempotent.
func (s *managedStreamer) commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed || s.finished {
		return
	}
	s.committed = true
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.WriteHeader(http.StatusOK)
	if s.heartbeat {
		if frame := s.heartbeatFrame(s.activity); frame != nil {
			_, _ = s.w.Write(frame)
		}
	}
	s.w.Flush()
}

// heartbeats emits a visible progress frame while the bot works. It stops as
// soon as the response finishes so no frame trails the terminal marker.
func (s *managedStreamer) heartbeats(stop <-chan struct{}) {
	if !s.heartbeat {
		return
	}
	ticker := time.NewTicker(managedHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.streamed:
			// The answer is flowing; a "working" indicator would now be noise.
			return
		case <-ticker.C:
			if !s.writePulse() {
				return
			}
		}
	}
}

// onData receives every upstream data payload of one generation. With the
// show-intermediate policy every chunk is relayed verbatim. Otherwise a turn
// is held until either it proves large enough to be the final answer (then the
// hold is flushed and the rest streams live) or it calls tools (then the hold
// is discarded as planning text).
func (s *managedStreamer) onData(data string) error {
	if data == "[DONE]" {
		// The streamer owns the terminal marker; finishTurn writes it once the
		// final turn is published. Forwarding [DONE] here would duplicate it.
		return nil
	}
	if s.live {
		s.relay(data)
		return nil
	}
	var chunk struct {
		Choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return ErrManagedChat
	}
	if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
		s.mu.Lock()
		s.usage = chunk.Usage
		s.mu.Unlock()
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	delta := chunk.Choices[0].Delta
	if raw, ok := delta["tool_calls"]; ok && string(raw) != "null" {
		// The turn calls tools, so it is not the final answer: drop any held
		// planning text and stop treating the rest of the turn as an answer.
		s.mu.Lock()
		s.hold.Reset()
		s.turnLive = false
		s.mu.Unlock()
		return nil
	}
	// Only visible answer text counts toward the answer. The model's reasoning
	// is internal: it must never be re-emitted as answer content, because the
	// client would render planning text as part of the reply.
	var text strings.Builder
	if raw, ok := delta["content"]; ok && string(raw) != "null" {
		var piece string
		if json.Unmarshal(raw, &piece) == nil {
			text.WriteString(piece)
		}
	}
	if text.Len() == 0 {
		return nil
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return nil
	}
	if s.turnLive {
		s.mu.Unlock()
		return s.writeContent([]byte(text.String()))
	}
	if s.hold.Len()+text.Len() > externalCollectionLimit {
		s.mu.Unlock()
		return ErrManagedChat
	}
	s.hold.WriteString(text.String())
	if s.hold.Len() < managedHoldCommitChars {
		s.mu.Unlock()
		return nil
	}
	// The turn has produced enough answer text to be the final reply, not short
	// planning: flush what we held as the first progressive chunk, then keep
	// streaming the remaining content deltas.
	pending := append([]byte(nil), s.hold.Bytes()...)
	s.hold.Reset()
	s.turnLive = true
	s.output = true
	s.mu.Unlock()
	return s.writeContent(pending)
}

// relay forwards one upstream chunk verbatim, recording that the client saw
// answer content.
func (s *managedStreamer) relay(data string) {
	s.mu.Lock()
	if s.finished || !s.committed {
		s.mu.Unlock()
		return
	}
	s.output = true
	s.markStreamed()
	_, err := s.w.Write([]byte("data: " + data + "\n\n"))
	if err == nil {
		s.w.Flush()
	}
	s.mu.Unlock()
}

// writeContent emits a synthesized content chunk and flushes it.
func (s *managedStreamer) writeContent(text []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return nil
	}
	chunk, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-managed",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": string(text)}, "finish_reason": nil}},
	})
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(s.w, "data: %s\n\n", chunk); err != nil {
		return err
	}
	s.w.Flush()
	s.markStreamed()
	return nil
}

// abortTurn discards a held intermediate turn.
func (s *managedStreamer) abortTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold.Reset()
	s.turnLive = false
}

// finishTurn flushes any short final answer that never crossed the commit
// threshold, then terminates the stream with the single [DONE] marker. Longer
// answers already streamed live, so only the terminal marker is missing.
func (s *managedStreamer) finishTurn() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return nil
	}
	s.finished = true
	if !s.live && !s.turnLive && s.hold.Len() > 0 {
		chunk, err := json.Marshal(map[string]any{
			"id":      "chatcmpl-managed",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": s.hold.String()}, "finish_reason": nil}},
		})
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(s.w, "data: %s\n\n", chunk); err != nil {
			return err
		}
		stop := map[string]any{
			"id":      "chatcmpl-managed",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		}
		if len(s.usage) > 0 {
			stop["usage"] = s.usage
		}
		if chunk, err = json.Marshal(stop); err != nil {
			return err
		}
		if _, err = fmt.Fprintf(s.w, "data: %s\n\n", chunk); err != nil {
			return err
		}
		s.output = true
	}
	s.hold.Reset()
	if _, err := fmt.Fprint(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	s.w.Flush()
	return nil
}

// writeError reports a post-commit failure as a documented error object and
// terminates without a normal successful finish. Idempotent: only the first
// terminal write wins, so a later finish or repeated error is suppressed.
func (s *managedStreamer) writeError(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": code}})
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", payload)
	s.w.Flush()
}

// stop silences the streamer without writing: heartbeats cease and any later
// terminal write is suppressed. Used before writing a stored wire directly so
// no comment can trail its terminal marker and no heartbeat races the write.
func (s *managedStreamer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = true
}

// hasLiveOutput reports whether the final answer already reached the client.
// When it did not (hold mode discarded a tool-call turn, or the downstream
// never streamed), the stored projection must carry the response.
func (s *managedStreamer) hasLiveOutput() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output
}

// committed reports whether response headers were already sent.
func (s *managedStreamer) isCommitted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
}

// writePulse emits the periodic liveness dot and records that the line is no
// longer empty.
func (s *managedStreamer) writePulse() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return false
	}
	if _, err := s.w.Write(s.progressFrame(".")); err != nil {
		return false
	}
	s.atLineStart = false
	s.w.Flush()
	return true
}

// writeFrame emits one wire frame and flushes it. Reports false once the
// response has finished, so heartbeats stop before the terminal marker.
func (s *managedStreamer) writeFrame(frame []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return false
	}
	if _, err := s.w.Write(frame); err != nil {
		return false
	}
	s.w.Flush()
	return true
}
