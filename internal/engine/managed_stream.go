package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"sync"
	"time"

	"tresor/internal/jsplugins"
)

// managedHeartbeatInterval is the waiting-comment cadence for streaming
// managed responses. A package variable keeps tests fast.
var managedHeartbeatInterval = 15 * time.Second

// managedStreamer relays a managed generation to a streaming client as it is
// produced. The client's stream request governs the transport; the policy
// only shapes content (heartbeats, intermediate turns).
type managedStreamer struct {
	w         *statusCaptureWriter
	heartbeat bool
	live      bool

	mu        sync.Mutex
	committed bool
	finished  bool
	forwarded int
	hold      bytes.Buffer
	holdText  bool
}

func newManagedStreamer(w *statusCaptureWriter, decision jsplugins.Decision, live bool) *managedStreamer {
	return &managedStreamer{w: w, heartbeat: decision.WantsHeartbeat(), live: live}
}

// commit sends the SSE headers before any generation work so the client sees
// progress immediately. Idempotent.
func (s *managedStreamer) commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed || s.finished {
		return
	}
	s.committed = true
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.WriteHeader(http.StatusOK)
	s.w.Flush()
}

// heartbeats emits waiting comments while the bot works. It stops as soon as
// the response finishes so no comment trails the terminal marker.
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
		case <-ticker.C:
			if !s.writeFrame([]byte(": waiting\n\n")) {
				return
			}
		}
	}
}

// onData receives every upstream data payload of one generation. In live
// mode chunks are relayed verbatim; otherwise they are held until the turn is
// known to be the final answer.
func (s *managedStreamer) onData(data string) error {
	if data == "[DONE]" {
		// The streamer owns the terminal marker; finishTurn writes it once the
		// final turn is published. Forwarding [DONE] here would duplicate it.
		return nil
	}
	if s.live {
		if s.writeFrame([]byte("data: " + data + "\n\n")) {
			s.mu.Lock()
			s.forwarded++
			s.mu.Unlock()
		}
		return nil
	}
	var chunk struct {
		Choices []struct {
			Delta  map[string]json.RawMessage `json:"delta"`
			Finish *string                    `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return ErrManagedChat
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	delta := chunk.Choices[0].Delta
	if raw, ok := delta["tool_calls"]; ok && string(raw) != "null" {
		// The turn calls tools, so it is not the final answer: its text must
		// not reach the client unless intermediate turns are shown.
		s.hold.Reset()
		s.holdText = false
		return nil
	}
	for _, key := range []string{"content", "reasoning", "reasoning_content"} {
		if raw, ok := delta[key]; ok && string(raw) != "null" {
			var text string
			if json.Unmarshal(raw, &text) == nil {
				if s.hold.Len()+len(text) > externalCollectionLimit {
					return ErrManagedChat
				}
				s.hold.WriteString(text)
				s.holdText = true
			}
		}
	}
	return nil
}

// abortTurn discards a held intermediate turn.
func (s *managedStreamer) abortTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold.Reset()
	s.holdText = false
}

// finishTurn emits the final answer as SSE and terminates the stream. When
// nothing was held (live mode already relayed the chunks) only the terminal
// marker is written.
func (s *managedStreamer) finishTurn() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return nil
	}
	s.finished = true
	if !s.live && s.holdText {
		root := map[string]any{
			"id":      "chatcmpl-managed",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": s.hold.String()}, "finish_reason": nil}},
		}
		chunk, err := json.Marshal(root)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(s.w, "data: %s\n\n", chunk); err != nil {
			return err
		}
		root["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
		if chunk, err = json.Marshal(root); err != nil {
			return err
		}
		if _, err = fmt.Fprintf(s.w, "data: %s\n\n", chunk); err != nil {
			return err
		}
	}
	s.hold.Reset()
	s.holdText = false
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

// hasLiveOutput reports whether anything of the final turn already reached the
// client. When it did not (hold mode discarded a tool-call turn, or the
// downstream never streamed), the stored projection must carry the response.
func (s *managedStreamer) hasLiveOutput() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forwarded > 0 || (!s.live && s.holdText)
}

// committed reports whether response headers were already sent.
func (s *managedStreamer) isCommitted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
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
