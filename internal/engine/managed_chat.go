package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var ErrManagedChat = errors.New("invalid managed Chat transcript")

// managedValidationReason is an allowlisted diagnostic, never provider text.
type managedValidationReason string

func managedReason(err error) managedValidationReason {
	if err == nil || !errors.Is(err, ErrManagedChat) {
		return "validation_failed"
	}
	// Match complete known errors, not substrings that could contain provider data.
	for message, reason := range map[string]managedValidationReason{
		"request JSON object required":            "request_object_required",
		"opaque reasoning/state unsupported":      "opaque_state_unsupported",
		"messages required":                       "messages_required",
		"one choice required":                     "single_choice_required",
		"unsupported legacy or linked state":      "legacy_state_unsupported",
		"malformed tool calls":                    "malformed_tool_calls",
		"too many tool calls":                     "too_many_tool_calls",
		"empty or duplicate tool call ID":         "invalid_tool_call_id",
		"function arguments must be encoded JSON": "invalid_function_encoding",
		"tool arguments must be a JSON object":    "tool_arguments_object_required",
		"response object required":                "response_object_required",
		"exactly one response choice required":    "response_single_choice_required",
		"response message required":               "response_message_required",
		"invalid or opaque message":               "invalid_or_opaque_message",
		"assistant response required":             "assistant_role_required",
	} {
		if err.Error() == ErrManagedChat.Error()+": "+message {
			return reason
		}
	}
	return "validation_failed"
}

// ChatMessage preserves provider/client fields not interpreted by the codec.
type ChatMessage map[string]json.RawMessage

// ChatRequest is a validated native Chat request. Raw retains the complete request.
type ChatRequest struct {
	Raw      map[string]json.RawMessage
	Messages []ChatMessage
	Model    string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
	HostOwned bool
}

// ParseManagedChatRequest accepts only one-choice Chat input and rejects opaque state/reasoning.
func ParseManagedChatRequest(data []byte) (ChatRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return ChatRequest{}, fmt.Errorf("%w: request JSON object required", ErrManagedChat)
	}
	if hasOpaque(raw) {
		return ChatRequest{}, fmt.Errorf("%w: opaque reasoning/state unsupported", ErrManagedChat)
	}
	var model string
	_ = json.Unmarshal(raw["model"], &model)
	var messages []ChatMessage
	if len(raw["messages"]) == 0 || json.Unmarshal(raw["messages"], &messages) != nil || len(messages) == 0 {
		return ChatRequest{}, fmt.Errorf("%w: messages required", ErrManagedChat)
	}
	if n := raw["n"]; len(n) > 0 && string(n) != "1" {
		return ChatRequest{}, fmt.Errorf("%w: one choice required", ErrManagedChat)
	}
	for _, key := range []string{"functions", "function_call", "previous_response_id"} {
		if len(raw[key]) > 0 {
			return ChatRequest{}, fmt.Errorf("%w: unsupported legacy or linked state", ErrManagedChat)
		}
	}
	for _, m := range messages {
		if hasOpaque(m) {
			return ChatRequest{}, fmt.Errorf("%w: opaque reasoning/state unsupported", ErrManagedChat)
		}
	}
	return ChatRequest{Raw: raw, Messages: messages, Model: model}, nil
}

// textualReasoning permits plain text only, never structured provider state.
func textualReasoning(key string, value json.RawMessage) bool {
	if key != "reasoning" && key != "reasoning_content" && key != "name" {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return true
	}
	var text string
	return json.Unmarshal(value, &text) == nil
}

func hasOpaque(m map[string]json.RawMessage) bool {
	for k, v := range m {
		lk := strings.ToLower(k)
		if textualReasoning(k, v) {
			continue
		}
		if strings.Contains(lk, "reasoning") || lk == "encrypted_content" || lk == "opaque_state" || lk == "state" {
			return true
		}
	}
	return false
}

// ValidateToolNames ensures names are uniquely owned and have no client/host collision.
func ValidateToolNames(clientNames, hostNames []string) error {
	owners := map[string]string{}
	for _, group := range []struct {
		name   string
		values []string
	}{{"client", clientNames}, {"host", hostNames}} {
		for _, name := range group.values {
			if name == "" {
				return fmt.Errorf("%w: empty tool name", ErrManagedChat)
			}
			if prev, ok := owners[name]; ok {
				return fmt.Errorf("tool_name_collision: %s tool %q conflicts with %s", group.name, name, prev)
			}
			owners[name] = group.name
		}
	}
	return nil
}

// ParseToolCalls validates IDs and JSON-object arguments and classifies by registered host name.
func ParseToolCalls(message ChatMessage, hostNames []string) ([]ToolCall, error) {
	if len(message["tool_calls"]) == 0 {
		return nil, nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(message["tool_calls"], &calls); err != nil {
		return nil, fmt.Errorf("%w: malformed tool calls", ErrManagedChat)
	}
	if len(calls) > 128 {
		return nil, fmt.Errorf("%w: too many tool calls", ErrManagedChat)
	}
	host := map[string]bool{}
	for _, n := range hostNames {
		host[n] = true
	}
	seen := map[string]bool{}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		if c.ID == "" || seen[c.ID] {
			return nil, fmt.Errorf("%w: empty or duplicate tool call ID", ErrManagedChat)
		}
		seen[c.ID] = true
		var encoded string
		if c.Type != "function" || c.Function.Name == "" || json.Unmarshal(c.Function.Arguments, &encoded) != nil {
			return nil, fmt.Errorf("%w: function arguments must be encoded JSON", ErrManagedChat)
		}
		var args map[string]json.RawMessage
		if len(encoded) == 0 || len(encoded) > 1<<20 || json.Unmarshal([]byte(encoded), &args) != nil || args == nil {
			return nil, fmt.Errorf("%w: tool arguments must be a JSON object", ErrManagedChat)
		}
		out = append(out, ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: json.RawMessage(encoded), HostOwned: host[c.Function.Name]})
	}
	return out, nil
}

// ValidateOneChoiceResponse validates native Chat response envelope and rejects multi-choice.
func ValidateOneChoiceResponse(data []byte) (map[string]json.RawMessage, error) {
	var r map[string]json.RawMessage
	if json.Unmarshal(data, &r) != nil || r == nil {
		return nil, fmt.Errorf("%w: response object required", ErrManagedChat)
	}
	var choices []json.RawMessage
	if json.Unmarshal(r["choices"], &choices) != nil || len(choices) != 1 {
		return nil, fmt.Errorf("%w: exactly one response choice required", ErrManagedChat)
	}
	var choice map[string]json.RawMessage
	if json.Unmarshal(choices[0], &choice) != nil || len(choice["message"]) == 0 {
		return nil, fmt.Errorf("%w: response message required", ErrManagedChat)
	}
	if hasOpaque(choice) {
		return nil, fmt.Errorf("%w: opaque reasoning/state unsupported", ErrManagedChat)
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(choice["message"], &msg) != nil || hasOpaque(msg) || hasOpaque(r) {
		return nil, fmt.Errorf("%w: invalid or opaque message", ErrManagedChat)
	}
	var role string
	_ = json.Unmarshal(msg["role"], &role)
	if role != "assistant" {
		return nil, fmt.Errorf("%w: assistant response required", ErrManagedChat)
	}
	return r, nil
}

// ProjectAssistantMessage removes host calls/results and private fields, preserving all other fields.
func ProjectAssistantMessage(message ChatMessage, hostNames []string) (ChatMessage, []ToolCall, error) {
	calls, err := ParseToolCalls(message, hostNames)
	if err != nil {
		return nil, nil, err
	}
	out := ChatMessage{}
	for k, v := range message {
		if hasOpaque(map[string]json.RawMessage{k: v}) {
			continue
		}
		out[k] = append(json.RawMessage(nil), v...)
	}
	client := make([]json.RawMessage, 0, len(calls))
	hosts := make([]ToolCall, 0)
	var original []json.RawMessage
	_ = json.Unmarshal(message["tool_calls"], &original)
	for i, c := range calls {
		if c.HostOwned {
			hosts = append(hosts, c)
		} else {
			client = append(client, original[i])
		}
	}
	if len(client) == 0 {
		delete(out, "tool_calls")
		if string(out["content"]) == "null" {
			out["content"] = json.RawMessage(`""`)
		}
	} else {
		b, _ := json.Marshal(client)
		out["tool_calls"] = b
	}
	return out, hosts, nil
}

// collectManagedSSE buffers a complete native Chat generation before effects.
// collectManagedSSE reassembles one upstream Chat generation into a single
// Chat JSON. The optional forward callback sees every data payload as it
// arrives, so a live streamer can relay chunks before the turn is complete.
func collectManagedSSE(reader io.Reader, observe func([]byte), forward func(data string) error) ([]byte, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, externalCollectionLimit+1))
	scanner.Buffer(make([]byte, 4096), externalValueLimit)
	root := map[string]any{}
	message := map[string]any{"role": "assistant", "content": nil}
	content := ""
	finish := ""
	done := false
	total := 0
	type fragment struct{ ID, Type, Name, Arguments string }
	calls := map[int]*fragment{}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > externalCollectionLimit {
			return nil, ErrManagedChat
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		// Observe every data payload (including the terminal [DONE]) so the
		// decode measurement sees the full stream window and usage.
		if observe != nil {
			observe([]byte(data))
		}
		if forward != nil {
			if err := forward(data); err != nil {
				return nil, err
			}
		}
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			ID      string          `json:"id"`
			Model   string          `json:"model"`
			Created int64           `json:"created"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index  int                        `json:"index"`
				Delta  map[string]json.RawMessage `json:"delta"`
				Finish *string                    `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Error) > 0 {
			return nil, ErrManagedChat
		}
		if len(chunk.Usage) > 0 {
			root["usage"] = chunk.Usage
		}
		if chunk.ID != "" {
			root["id"] = chunk.ID
		}
		if chunk.Model != "" {
			root["model"] = chunk.Model
		}
		root["created"] = chunk.Created
		if len(chunk.Choices) == 0 {
			continue
		}
		if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 {
			return nil, ErrManagedChat
		}
		choice := chunk.Choices[0]
		if hasOpaque(choice.Delta) {
			return nil, ErrManagedChat
		}
		for _, key := range []string{"reasoning", "reasoning_content"} {
			if raw, ok := choice.Delta[key]; ok {
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					if _, exists := message[key]; !exists {
						message[key] = nil
					}
					continue
				}
				var text string
				if json.Unmarshal(raw, &text) != nil {
					return nil, ErrManagedChat
				}
				previous, _ := message[key].(string)
				message[key] = previous + text
			}
		}
		if choice.Finish != nil {
			finish = *choice.Finish
		}
		if raw := choice.Delta["content"]; len(raw) > 0 && string(raw) != "null" {
			var text string
			if json.Unmarshal(raw, &text) != nil {
				return nil, ErrManagedChat
			}
			content += text
			message["content"] = content
		}
		if raw := choice.Delta["tool_calls"]; len(raw) > 0 {
			var parts []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}
			if json.Unmarshal(raw, &parts) != nil {
				return nil, ErrManagedChat
			}
			for _, p := range parts {
				if p.Index < 0 || p.Index > 1024 {
					return nil, ErrManagedChat
				}
				f := calls[p.Index]
				if f == nil {
					f = &fragment{}
					calls[p.Index] = f
				}
				f.ID += p.ID
				f.Type += p.Type
				f.Name += p.Function.Name
				f.Arguments += p.Function.Arguments
			}
		}
	}
	if scanner.Err() != nil || !done || finish == "" {
		return nil, ErrManagedChat
	}
	if len(calls) > 0 {
		tools := []any{}
		for i := 0; i < len(calls); i++ {
			f := calls[i]
			if f == nil {
				return nil, ErrManagedChat
			}
			tools = append(tools, map[string]any{"id": f.ID, "type": f.Type, "function": map[string]any{"name": f.Name, "arguments": f.Arguments}})
		}
		message["tool_calls"] = tools
	}
	root["object"] = "chat.completion"
	root["choices"] = []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}
	return json.Marshal(root)
}

// writeManagedProjection replays a fully validated, durable public generation.
func writeManagedProjection(w http.ResponseWriter, raw []byte, stream bool) error {
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write(raw)
		return err
	}
	root, err := ValidateOneChoiceResponse(raw)
	if err != nil {
		return err
	}
	var choices []map[string]json.RawMessage
	_ = json.Unmarshal(root["choices"], &choices)
	var message map[string]json.RawMessage
	_ = json.Unmarshal(choices[0]["message"], &message)
	var calls []map[string]json.RawMessage
	if len(message["tool_calls"]) > 0 {
		if json.Unmarshal(message["tool_calls"], &calls) != nil {
			return ErrManagedChat
		}
		for i := range calls {
			calls[i]["index"], _ = json.Marshal(i)
		}
		message["tool_calls"], _ = json.Marshal(calls)
	}
	root["object"] = json.RawMessage(`"chat.completion.chunk"`)
	root["choices"], _ = json.Marshal([]any{map[string]any{"index": 0, "delta": message, "finish_reason": nil}})
	chunk, err := json.Marshal(root)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if _, err = fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
		return err
	}
	root["choices"], _ = json.Marshal([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": json.RawMessage(choices[0]["finish_reason"])}})
	chunk, err = json.Marshal(root)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
