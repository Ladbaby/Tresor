package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicCodec serves the Anthropic Messages wire format.
//
// Decoding preserves what a Claude-style tool loop needs to keep working:
// thinking blocks (with their signatures) are carried into the canonical
// transcript so a later tool-result turn can echo them back, and images and
// thinking budgets survive the translation. Nothing is silently dropped: a
// block shape this codec cannot represent fails the request.
type anthropicCodec struct{}

func (c *anthropicCodec) Name() string   { return "anthropic-messages.v1" }
func (c *anthropicCodec) Format() string { return "anthropic" }

// anthropicBlock is one Anthropic content block. Fields are pointers/raw so an
// absent field is distinguishable from an empty one.
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (c *anthropicCodec) Decode(raw []byte, path string) (DecodedRequest, error) {
	var wire struct {
		Model         json.RawMessage `json:"model"`
		MaxTokens     json.RawMessage `json:"max_tokens"`
		System        json.RawMessage `json:"system"`
		Messages      json.RawMessage `json:"messages"`
		Tools         json.RawMessage `json:"tools"`
		ToolChoice    json.RawMessage `json:"tool_choice"`
		Thinking      json.RawMessage `json:"thinking"`
		Stream        json.RawMessage `json:"stream"`
		Temperature   json.RawMessage `json:"temperature"`
		TopP          json.RawMessage `json:"top_p"`
		TopK          json.RawMessage `json:"top_k"`
		StopSequences json.RawMessage `json:"stop_sequences"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Messages) == 0 {
		return DecodedRequest{}, fmt.Errorf("%w: invalid Anthropic request", ErrManagedChat)
	}
	var model string
	if json.Unmarshal(wire.Model, &model) != nil || model == "" {
		return DecodedRequest{}, fmt.Errorf("%w: model required", ErrManagedChat)
	}
	var stream bool
	if len(wire.Stream) > 0 {
		if err := json.Unmarshal(wire.Stream, &stream); err != nil {
			return DecodedRequest{}, fmt.Errorf("%w: stream must be boolean", ErrManagedChat)
		}
	}
	var anthropicMessages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(wire.Messages, &anthropicMessages); err != nil || len(anthropicMessages) == 0 {
		return DecodedRequest{}, fmt.Errorf("%w: messages required", ErrManagedChat)
	}
	messages := make([]ChatMessage, 0, len(anthropicMessages)+1)
	// The system prompt becomes the leading canonical system message.
	if text := anthropicSystemText(wire.System); text != "" {
		messages = append(messages, systemMessage(text))
	}
	for _, m := range anthropicMessages {
		converted, err := c.decodeMessage(m.Role, m.Content)
		if err != nil {
			return DecodedRequest{}, err
		}
		messages = append(messages, converted...)
	}
	// Anthropic tool declarations map onto canonical function tools.
	tools, err := anthropicToolsToCanonical(wire.Tools)
	if err != nil {
		return DecodedRequest{}, err
	}
	// Forward the client's sampling intent under canonical names. The loop sends
	// Raw upstream, so anything omitted here never reaches the model. top_k is
	// forwarded as-is, exactly as anthropic2openai does.
	rawOut := map[string]json.RawMessage{"model": wire.Model, "messages": wire.Messages}
	for key, value := range map[string]json.RawMessage{
		"max_tokens": wire.MaxTokens, "temperature": wire.Temperature, "top_p": wire.TopP,
		"top_k": wire.TopK,
	} {
		if len(value) > 0 && string(value) != "null" {
			rawOut[key] = value
		}
	}
	// The canonical request speaks OpenAI's names: stop_sequences becomes stop,
	// and Anthropic's tool_choice object is invalid for the OpenAI downstream,
	// so it is mapped the way anthropic2openai maps it instead of forwarded.
	if len(wire.StopSequences) > 0 && string(wire.StopSequences) != "null" {
		rawOut["stop"] = wire.StopSequences
	}
	if tc := anthropicToolChoice(wire.ToolChoice); tc != nil {
		rawOut["tool_choice"] = tc
	}
	// A thinking budget becomes the canonical reasoning effort so the loop
	// forwards the client's intent to the downstream model.
	if effort := anthropicThinkingEffort(wire.Thinking); effort != "" {
		rawOut["reasoning_effort"], _ = json.Marshal(effort)
	}
	if len(tools) > 0 {
		rawOut["tools"], _ = json.Marshal(tools)
	}
	return DecodedRequest{Messages: messages, Model: model, Stream: stream, Raw: rawOut}, nil
}

// AcceptsOpaque validates provider-echoed blocks. Anthropic thinking carries a
// signature that must be echoed back with tool results, so the shape is
// accepted and preserved verbatim; it is never interpreted or regenerated.
func (c *anthropicCodec) AcceptsOpaque(key string, value json.RawMessage) bool {
	switch key {
	case "thinking", "signature", "redacted_thinking", "data":
	default:
		return false
	}
	trimmed := strings.TrimSpace(string(value))
	// Either a plain string (thinking text, signature, redacted payload) or a
	// block that carries one.
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		return json.Unmarshal(value, &s) == nil
	}
	return false
}

// decodeMessage converts one Anthropic message into canonical messages. A
// tool_result block becomes its own canonical tool message, because that is how
// the loop matches results to calls.
func (c *anthropicCodec) decodeMessage(role string, content json.RawMessage) ([]ChatMessage, error) {
	if role != "user" && role != "assistant" {
		return nil, fmt.Errorf("%w: unsupported role %q", ErrManagedChat, role)
	}
	// Plain string content is the common case.
	var text string
	if json.Unmarshal(content, &text) == nil {
		return []ChatMessage{canonicalMessage(role, text)}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, fmt.Errorf("%w: invalid content blocks", ErrManagedChat)
	}
	if role == "user" {
		return decodeUserBlocks(blocks)
	}
	return decodeAssistantBlocks(blocks)
}

// decodeUserBlocks handles a user turn: tool results become canonical tool
// messages, everything else stays user content.
func decodeUserBlocks(blocks []anthropicBlock) ([]ChatMessage, error) {
	var results []ChatMessage
	texts := []string{}
	images := []json.RawMessage{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			if len(b.Source) == 0 {
				return nil, fmt.Errorf("%w: image block without source", ErrManagedChat)
			}
			images = append(images, b.Source)
		case "tool_result":
			if b.ToolUseID == "" {
				return nil, fmt.Errorf("%w: tool_result without id", ErrManagedChat)
			}
			results = append(results, canonicalToolResult(b.ToolUseID, anthropicToolResultText(b), b.IsError))
		case "thinking", "redacted_thinking":
			// Client-echoed thinking is preserved on the assistant side; in a
			// user turn it carries no meaning for the loop.
		default:
			return nil, fmt.Errorf("%w: unsupported user block %q", ErrManagedChat, b.Type)
		}
	}
	// Tool results must directly follow the assistant turn that requested
	// them: reconstructManaged requires pending results at the anchor's next
	// positions, and the OpenAI downstream requires tool messages right after
	// the tool_calls they answer. User text from the same turn comes after.
	out := append([]ChatMessage{}, results...)
	// A turn that is only tool results contributes no user text.
	if len(texts) > 0 || len(images) > 0 {
		out = append(out, canonicalContentMessage("user", texts, images))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: empty user turn", ErrManagedChat)
	}
	return out, nil
}

// decodeAssistantBlocks handles an assistant turn: text and images become
// content, tool_use becomes canonical tool_calls, and thinking blocks are
// preserved so a later request can echo them back.
func decodeAssistantBlocks(blocks []anthropicBlock) ([]ChatMessage, error) {
	texts := []string{}
	images := []json.RawMessage{}
	thinking := []json.RawMessage{}
	calls := []json.RawMessage{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			if len(b.Source) == 0 {
				return nil, fmt.Errorf("%w: image block without source", ErrManagedChat)
			}
			images = append(images, b.Source)
		case "tool_use":
			if b.ID == "" || b.Name == "" {
				return nil, fmt.Errorf("%w: tool_use requires id and name", ErrManagedChat)
			}
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			call, err := json.Marshal(map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": string(args)},
			})
			if err != nil {
				return nil, fmt.Errorf("%w: malformed tool_use", ErrManagedChat)
			}
			calls = append(calls, call)
		case "thinking":
			// Preserve text and signature verbatim for echo-back.
			raw, err := json.Marshal(map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": b.Signature})
			if err != nil {
				return nil, fmt.Errorf("%w: malformed thinking block", ErrManagedChat)
			}
			thinking = append(thinking, raw)
		case "redacted_thinking":
			if b.Data == "" {
				return nil, fmt.Errorf("%w: redacted_thinking without data", ErrManagedChat)
			}
			raw, err := json.Marshal(map[string]any{"type": "redacted_thinking", "data": b.Data})
			if err != nil {
				return nil, fmt.Errorf("%w: malformed redacted_thinking", ErrManagedChat)
			}
			thinking = append(thinking, raw)
		default:
			return nil, fmt.Errorf("%w: unsupported assistant block %q", ErrManagedChat, b.Type)
		}
	}
	message := canonicalContentMessage("assistant", texts, images)
	if len(calls) > 0 {
		message["tool_calls"], _ = json.Marshal(calls)
	}
	// Thinking travels under a canonical key the store round-trips untouched.
	if len(thinking) > 0 {
		message["thinking"], _ = json.Marshal(thinking)
	}
	return []ChatMessage{message}, nil
}

func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	texts := []string{}
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func anthropicToolResultText(b anthropicBlock) string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var inner []anthropicBlock
	if json.Unmarshal(b.Content, &inner) != nil {
		return string(b.Content)
	}
	texts := []string{}
	for _, ib := range inner {
		if ib.Type == "text" {
			texts = append(texts, ib.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// anthropicToolsToCanonical maps Anthropic tool declarations to canonical
// function tools, preserving the input schema.
func anthropicToolsToCanonical(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("%w: invalid tool declarations", ErrManagedChat)
	}
	out := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "" {
			return nil, fmt.Errorf("%w: tool requires a name", ErrManagedChat)
		}
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		encoded, err := json.Marshal(map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": tool.Name, "description": tool.Description, "parameters": schema,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("%w: malformed tool declaration", ErrManagedChat)
		}
		out = append(out, encoded)
	}
	return out, nil
}

// anthropicToolChoice maps Anthropic's tool_choice object onto the OpenAI
// form, mirroring anthropic2openai: auto -> "auto", any -> "required",
// tool -> a function object, any other declared type passes through as a
// string. An unparseable value is dropped, as the converter drops it, rather
// than forwarded in a shape the downstream rejects.
func anthropicToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil || tc.Type == "" {
		return nil
	}
	switch tc.Type {
	case "auto":
		return json.RawMessage(`"auto"`)
	case "any":
		return json.RawMessage(`"required"`)
	case "tool":
		mapped, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}})
		return mapped
	default:
		mapped, _ := json.Marshal(tc.Type)
		return mapped
	}
}

// anthropicThinkingEffort buckets a thinking budget into a canonical level. The
// wire value is an effort hint, never the raw budget, because the downstream
// model decides its own budget.
func anthropicThinkingEffort(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var cfg struct {
		Type         string          `json:"type"`
		BudgetTokens json.RawMessage `json:"budget_tokens"`
		Effort       string          `json:"effort"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	switch cfg.Type {
	case "disabled":
		return "none"
	case "adaptive":
		if cfg.Effort != "" {
			return cfg.Effort
		}
		return "medium"
	case "enabled":
		var budget int64
		if json.Unmarshal(cfg.BudgetTokens, &budget) != nil || budget <= 0 {
			return "medium"
		}
		switch {
		case budget <= 4096:
			return "low"
		case budget <= 16384:
			return "medium"
		default:
			return "high"
		}
	}
	return ""
}

// Renderer returns the Anthropic SSE renderer.
func (c *anthropicCodec) Renderer() StreamRenderer { return &anthropicRenderer{} }

// ClientCallID keeps the downstream's call id: tool_use blocks carry it, and a
// tool_result echoes the same id on the turn that answers the call.
func (c *anthropicCodec) ClientCallID(call ToolCall, occurrence int) string { return call.ID }

// anthropicRenderer emits Anthropic's typed SSE events. Anthropic streams are a
// named event sequence, not bare data frames, so every frame carries its event
// name. Progress travel as thinking deltas so a client shows them in its
// reasoning panel and never in the answer text.
// anthropicRenderer writes Anthropic's named events. It records whether it has
// emitted tool_use blocks, because the turn's stop_reason depends on it.
type anthropicRenderer struct{ toolCalls bool }

const anthropicMessageID = "msg_managed"

func anthropicEvent(name string, payload any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []byte("event: " + name + "\ndata: " + string(encoded) + "\n\n")
}

func (r *anthropicRenderer) FrameStart() []byte {
	return anthropicEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": anthropicMessageID, "type": "message", "role": "assistant",
			"model": "managed", "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// FrameBlocks reports that this format requires an opened block before any
// delta, and an explicit close when the block changes.
func (r *anthropicRenderer) FrameBlocks() bool { return true }

// FrameOpenBlock opens the block carrying the given kind. Status travels in a
// thinking block at index 0 and the answer in a text block at index 1, so a
// client renders the two separately.
func (r *anthropicRenderer) FrameOpenBlock(kind string) []byte {
	if kind == "progress" {
		return anthropicEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})
	}
	return anthropicEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 1,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// FrameAlwaysTerminates reports that a message must end with its stop reason,
// so the closing frames are written even after a live answer.
func (r *anthropicRenderer) FrameAlwaysTerminates() bool { return true }

// FrameCloseBlock closes whichever block the streamer currently has open.
func (r *anthropicRenderer) FrameCloseBlock() []byte {
	return anthropicEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	})
}

func (r *anthropicRenderer) FrameProgress(text string) []byte {
	return anthropicEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

// FrameContent closes the thinking block and opens the text block before the
// first answer delta, so the client renders them separately.
func (r *anthropicRenderer) FrameContent(text string) []byte {
	return anthropicEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 1,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (r *anthropicRenderer) FrameFinish(usage json.RawMessage) []byte {
	// A turn that asked the client to run tools must say so: Claude clients key
	// on stop_reason to decide whether to execute them, and handing back
	// end_turn makes them end the conversation instead.
	stop := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": r.stopReason(), "stop_sequence": nil},
	}
	if mapped := anthropicUsage(usage); mapped != nil {
		stop["usage"] = mapped
	}
	return anthropicEvent("message_delta", stop)
}

// anthropicUsage renames the canonical (OpenAI) token counters. Passing them
// through unchanged would leave a client reading input_tokens/output_tokens with
// zeroes, because those keys never appear in a Chat usage block.
func anthropicUsage(usage json.RawMessage) map[string]any {
	if len(usage) == 0 {
		return nil
	}
	var counts struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
	}
	if json.Unmarshal(usage, &counts) != nil {
		return nil
	}
	return map[string]any{"input_tokens": counts.Prompt, "output_tokens": counts.Completion}
}

// stopReason reports how the turn ended. A turn that asked the client to run
// tools ends with tool_use; prose alone ends with end_turn.
func (r *anthropicRenderer) stopReason() string {
	if r.toolCalls {
		return "tool_use"
	}
	return "end_turn"
}

// FrameToolCalls renders the calls a stored turn requested as tool_use blocks,
// which is how a Claude client learns which tools it must run.
func (r *anthropicRenderer) FrameToolCalls(calls []ToolCall) []byte {
	if len(calls) > 0 {
		r.toolCalls = true
	}
	out := []byte{}
	for index, call := range calls {
		input := json.RawMessage("{}")
		if len(call.Arguments) > 0 && json.Valid(call.Arguments) {
			input = call.Arguments
		}
		out = append(out, anthropicEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": 1 + index,
			"content_block": map[string]any{
				"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{},
			},
		})...)
		out = append(out, anthropicEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 1 + index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)},
		})...)
		out = append(out, anthropicEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": 1 + index,
		})...)
	}
	return out
}

func (r *anthropicRenderer) FrameEnd() []byte {
	return anthropicEvent("message_stop", map[string]any{"type": "message_stop"})
}

func (r *anthropicRenderer) FrameError(code string) []byte {
	return anthropicEvent("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": code},
	})
}

// RenderJSON answers a non-streaming client with an Anthropic Messages
// response object built from the stored canonical turn. The stored wire is
// canonical Chat and is never written verbatim to this format.
func (r *anthropicRenderer) RenderJSON(canonical []byte) ([]byte, error) {
	root, err := ValidateOneChoiceResponse(canonical)
	if err != nil {
		return nil, err
	}
	message, err := managedResponseMessage(root)
	if err != nil {
		return nil, err
	}
	var text string
	if raw, ok := message["content"]; ok {
		_ = json.Unmarshal(raw, &text)
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if len(message["tool_calls"]) > 0 && json.Unmarshal(message["tool_calls"], &calls) != nil {
		return nil, errManagedProjection
	}
	blocks := []any{}
	if text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	for _, call := range calls {
		input := json.RawMessage(`{}`)
		if call.Function.Arguments != "" && json.Valid([]byte(call.Function.Arguments)) {
			input = json.RawMessage(call.Function.Arguments)
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input,
		})
	}
	var choices []map[string]json.RawMessage
	_ = json.Unmarshal(root["choices"], &choices)
	var finish string
	if len(choices) > 0 {
		_ = json.Unmarshal(choices[0]["finish_reason"], &finish)
	}
	stopReason := "end_turn"
	switch finish {
	case "tool_calls":
		stopReason = "tool_use"
	case "length":
		stopReason = "max_tokens"
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if raw := root["usage"]; len(raw) > 0 {
		var oai struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}
		if json.Unmarshal(raw, &oai) == nil {
			usage["input_tokens"] = oai.PromptTokens
			usage["output_tokens"] = oai.CompletionTokens
		}
	}
	var model string
	_ = json.Unmarshal(root["model"], &model)
	if model == "" {
		model = "managed"
	}
	return json.Marshal(map[string]any{
		"id": anthropicMessageID, "type": "message", "role": "assistant",
		"model": model, "content": blocks,
		"stop_reason": stopReason, "stop_sequence": nil, "usage": usage,
	})
}
