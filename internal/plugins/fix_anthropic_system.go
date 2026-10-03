package plugins

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"tresor/internal/engine"
)

// FixAnthropicSystem accommodates clients (including Claude Cowork) that
// include system-role messages in an Anthropic messages array. Templates such
// as Qwen require all system instructions before the conversation. This opt-in
// plugin moves those instructions to Anthropic's top-level system field.
type FixAnthropicSystem struct{}

func (p *FixAnthropicSystem) PluginName() string { return "FixAnthropicSystem" }

func (p *FixAnthropicSystem) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	if req.URL == nil || strings.TrimRight(req.URL.Path, "/") != "/v1/messages" {
		return req, body, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return req, body, nil
	}
	var messages []json.RawMessage
	if json.Unmarshal(payload["messages"], &messages) != nil {
		return req, body, nil
	}
	kept := make([]json.RawMessage, 0, len(messages))
	var moved []json.RawMessage
	for _, raw := range messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			return req, body, nil
		}
		if msg.Role != "system" {
			kept = append(kept, raw)
			continue
		}
		blocks, ok := systemTextBlocks(msg.Content)
		if !ok {
			return req, body, nil
		}
		moved = appendSystemBlocks(moved, blocks)
	}
	if len(kept) == len(messages) {
		return req, body, nil
	}
	var system []json.RawMessage
	if existing := payload["system"]; len(existing) > 0 && !bytes.Equal(bytes.TrimSpace(existing), []byte("null")) {
		var ok bool
		system, ok = systemTextBlocks(existing)
		if !ok {
			return req, body, nil
		}
	}
	system = appendSystemBlocks(system, moved)
	if system == nil {
		system = []json.RawMessage{}
	}
	payload["system"], _ = json.Marshal(system)
	payload["messages"], _ = json.Marshal(kept)
	out, err := json.Marshal(payload)
	if err != nil {
		return req, body, nil
	}
	clone := req.Clone(req.Context())
	clone.Body = io.NopCloser(bytes.NewReader(out))
	clone.ContentLength = int64(len(out))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(out)), nil }
	return clone, out, nil
}

// Keep original text blocks intact, including cache_control and extension
// fields. Reject unknown content rather than silently deleting instructions.
func systemTextBlocks(raw json.RawMessage) ([]json.RawMessage, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		return []json.RawMessage{block}, true
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	for _, block := range blocks {
		var b struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if json.Unmarshal(block, &b) != nil || b.Type != "text" || b.Text == nil {
			return nil, false
		}
	}
	return blocks, true
}

func appendSystemBlocks(dst, src []json.RawMessage) []json.RawMessage {
	// llama.cpp concatenates text blocks without separators. Separate instruction
	// groups without modifying the original blocks or their cache metadata.
	if len(dst) > 0 && len(src) > 0 {
		dst = append(dst, json.RawMessage(`{"type":"text","text":"\n\n"}`))
	}
	return append(dst, src...)
}

var _ engine.RequestTransformer = (*FixAnthropicSystem)(nil)
