package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"tresor/internal/engine"
)

// repeatReminder is the fixed user message appended when a small model is
// detected repeating its output verbatim.
const repeatReminder = "You have repeated the exact same message three times in a row. " +
	"You appear to be stuck in a loop. Stop repeating yourself and take a different, " +
	"concrete next step (or finish if the task is already done)."

// BreakRepeatPlugin inserts a user reminder into an incoming request when the
// last three assistant messages are all identical, nudging small models out of
// endless repetition loops. It is a request-side transformer and works on the
// raw client request in any of the four supported wire formats (OpenAI Chat
// Completions, OpenAI Responses, Anthropic Messages, Gemini generateContent).
type BreakRepeatPlugin struct{}

// NewBreakRepeatPlugin creates a BreakRepeatPlugin. The behavior is fixed and
// the config block is intentionally unused.
func NewBreakRepeatPlugin(config map[string]interface{}) (*BreakRepeatPlugin, error) {
	return &BreakRepeatPlugin{}, nil
}

// PluginName returns the stable type name for deduplication.
func (p *BreakRepeatPlugin) PluginName() string { return "BreakRepeat" }

// TransformRequest inspects the conversation and, if the last three assistant
// turns are identical, appends a user reminder. Any request we cannot
// understand (unknown format, unparseable body, fewer than three assistant
// turns, or non-repeating turns) is returned unchanged.
func (p *BreakRepeatPlugin) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	format := detectRequestFormat(body)
	if format == "" {
		return req, body, nil
	}

	turns := assistantTurns(body, format)
	if len(turns) < 3 {
		return req, body, nil
	}
	last := turns[len(turns)-3:]
	if last[0] != last[1] || last[1] != last[2] {
		return req, body, nil
	}

	newBody, err := appendReminder(body, format, repeatReminder)
	if err != nil {
		return req, body, nil
	}

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	return newReq, newBody, nil
}

// TransformResponse is a no-op for this plugin.
func (p *BreakRepeatPlugin) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	return body, nil
}

// TransformStreamChunk passes the chunk through unchanged.
func (p *BreakRepeatPlugin) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	return chunk, nil
}

// detectRequestFormat identifies the API format of an inbound request body by
// its top-level conversation field. Returns "openai", "openai_responses",
// "anthropic", "gemini", or "" when unrecognized.
func detectRequestFormat(body []byte) string {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	switch {
	case len(env["contents"]) > 0:
		return "gemini"
	case len(env["input"]) > 0:
		return "openai_responses"
	case len(env["messages"]) > 0:
		// Both OpenAI Chat and Anthropic use top-level "messages". Anthropic
		// content is always an array of typed blocks; OpenAI commonly uses a
		// plain string. A string content anywhere means OpenAI.
		var msgs []map[string]json.RawMessage
		if err := json.Unmarshal(env["messages"], &msgs); err != nil {
			return "openai"
		}
		for _, m := range msgs {
			var s string
			if err := json.Unmarshal(m["content"], &s); err == nil {
				return "openai"
			}
		}
		return "anthropic"
	default:
		return ""
	}
}

// assistantRole returns the role value that marks assistant turns in a format.
func assistantRole(format string) string {
	if format == "gemini" {
		return "model"
	}
	return "assistant"
}

// assistantTurns extracts a canonical fingerprint of every assistant turn in
// the conversation, in order. A fingerprint covers both the textual content
// and any tool calls (name + arguments), so that a sequence of tool calls
// with different arguments is never mistaken for a verbatim repeat.
func assistantTurns(body []byte, format string) []string {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	role := assistantRole(format)

	var out []string
	switch format {
	case "gemini":
		contents, ok := payload["contents"].([]interface{})
		if !ok {
			return nil
		}
		for _, c := range contents {
			msg, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			if msg["role"] != role {
				continue
			}
			parts, ok := msg["parts"].([]interface{})
			if !ok {
				out = append(out, canonicalFingerprint(nil))
				continue
			}
			var pieces []interface{}
			for _, p := range parts {
				pm, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				if t, ok := pm["text"].(string); ok {
					pieces = append(pieces, t)
				}
				if fc, ok := pm["functionCall"].(map[string]interface{}); ok {
					pieces = append(pieces, toolCallPiece(fc["name"], fc["args"]))
				}
			}
			out = append(out, canonicalFingerprint(pieces))
		}
	case "openai_responses":
		items, ok := payload["input"].([]interface{})
		if !ok {
			// A plain-string input has no conversation to inspect.
			return nil
		}
		for _, it := range items {
			msg, ok := it.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := msg["type"].(string)
			switch {
			case typ == "reasoning":
				// Reasoning items carry their content in "summary" blocks.
				// Without them, consecutive reasoning-only turns would all
				// fingerprint as empty and false-trigger.
				var pieces []interface{}
				if sum, ok := msg["summary"].([]interface{}); ok {
					for _, s := range sum {
						if sm, ok := s.(map[string]interface{}); ok {
							if t, ok := sm["text"].(string); ok {
								pieces = append(pieces, t)
							}
						}
					}
				}
				out = append(out, canonicalFingerprint(pieces))
			case msg["role"] == role:
				out = append(out, canonicalFingerprint(
					[]interface{}{extractStringContent(msg["content"])}))
			case typ == "function_call" || typ == "tool_call" || typ == "custom_tool_call":
				var args interface{}
				if typ == "custom_tool_call" {
					args = msg["input"]
				} else {
					args = msg["arguments"]
				}
				out = append(out, canonicalFingerprint(
					[]interface{}{toolCallPiece(msg["name"], args)}))
			}
		}
	default: // "openai" and "anthropic" share the "messages" field
		msgs, ok := payload["messages"].([]interface{})
		if !ok {
			return nil
		}
		for _, m := range msgs {
			msg, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			if msg["role"] != role {
				continue
			}
			pieces := []interface{}{extractStringContent(msg["content"])}
			// OpenAI-style tool_calls live in a sibling field.
			if tcs, ok := msg["tool_calls"].([]interface{}); ok {
				for _, tc := range tcs {
					tcm, ok := tc.(map[string]interface{})
					if !ok {
						continue
					}
					fn, _ := tcm["function"].(map[string]interface{})
					if fn == nil {
						continue
					}
					pieces = append(pieces, toolCallPiece(fn["name"], fn["arguments"]))
				}
			}
			// Anthropic-style tool_use and thinking blocks live inside the
			// content array. Thinking blocks are included so that consecutive
			// reasoning turns are not all fingerprinted as empty.
			if arr, ok := msg["content"].([]interface{}); ok {
				for _, b := range arr {
					bm, ok := b.(map[string]interface{})
					if !ok {
						continue
					}
					switch bm["type"] {
					case "tool_use":
						pieces = append(pieces, toolCallPiece(bm["name"], bm["input"]))
					case "thinking":
						pieces = append(pieces, map[string]interface{}{"thinking": bm["thinking"]})
					case "redacted_thinking":
						pieces = append(pieces, map[string]interface{}{"redacted_thinking": bm["data"]})
					}
				}
			}
			out = append(out, canonicalFingerprint(pieces))
		}
	}
	return out
}

// toolCallPiece builds a normalized piece representing a single tool call so
// that calls differing in name or arguments produce distinct fingerprints.
// Non-deterministic fields (call IDs, etc.) are deliberately excluded.
func toolCallPiece(name, args interface{}) interface{} {
	return map[string]interface{}{
		"tool":  canonicalScalar(name),
		"args":  canonicalScalar(args),
	}
}

// canonicalScalar renders a scalar (string / number / bool / nil) as a plain
// string so that JSON-string arguments and native objects are each stable
// across turns. Note: a JSON-string arg and the equivalent native object are
// kept distinct, which is fine in practice because a given request uses one
// representation per field consistently.
func canonicalScalar(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

// canonicalFingerprint renders turn pieces as a deterministic string used for
// repeat comparison.
func canonicalFingerprint(pieces []interface{}) string {
	raw, err := json.Marshal(pieces)
	if err != nil {
		return ""
	}
	return string(raw)
}

// appendReminder returns a new body with a user reminder message appended in
// the correct shape for the given format.
func appendReminder(body []byte, format string, reminder string) ([]byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	switch format {
	case "openai":
		msgs, _ := payload["messages"].([]interface{})
		payload["messages"] = append(msgs, map[string]interface{}{
			"role":    "user",
			"content": reminder,
		})
	case "openai_responses":
		var items []interface{}
		switch in := payload["input"].(type) {
		case string:
			if in != "" {
				items = append(items, map[string]interface{}{"role": "user", "content": in})
			}
		case []interface{}:
			items = append(items, in...)
		}
		items = append(items, map[string]interface{}{"role": "user", "content": reminder})
		payload["input"] = items
	case "anthropic":
		msgs, _ := payload["messages"].([]interface{})
		payload["messages"] = append(msgs, map[string]interface{}{
			"role":    "user",
			"content": []interface{}{map[string]interface{}{"type": "text", "text": reminder}},
		})
	case "gemini":
		contents, _ := payload["contents"].([]interface{})
		payload["contents"] = append(contents, map[string]interface{}{
			"role":  "user",
			"parts": []interface{}{map[string]interface{}{"text": reminder}},
		})
	}

	return json.Marshal(payload)
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*BreakRepeatPlugin)(nil)
var _ engine.ResponseTransformer = (*BreakRepeatPlugin)(nil)
var _ engine.StreamResponseTransformer = (*BreakRepeatPlugin)(nil)
