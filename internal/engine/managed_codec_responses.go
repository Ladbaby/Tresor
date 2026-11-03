package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// responsesCodec serves the OpenAI Responses wire format.
//
// The Responses API carries the same conversation as Chat but itemises it: a
// turn is a list of items rather than a message with tool_calls, tool results
// arrive as `function_call_output` items addressed by call id, and reasoning is
// its own item carrying opaque `encrypted_content` that only the provider can
// read. Decoding keeps every construct a tool loop needs — function calls, their
// outputs, images, reasoning effort and encrypted reasoning state — and stores
// the opaque ciphertext verbatim so a later turn can echo it back untouched.
//
// `previous_response_id` is rejected: the loop owns the transcript, so a
// server-side pointer would silently drop the turns this loop has already
// written to its durable history.
type responsesCodec struct{}

func (c *responsesCodec) Name() string   { return "openai-responses.v1" }
func (c *responsesCodec) Format() string { return "openai_responses" }

// responsesItem is one input item. Fields are raw so a shape this codec does not
// model is reported rather than silently dropped.
type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	CallID  string          `json:"call_id"`
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Args    json.RawMessage `json:"arguments"`
	Output  json.RawMessage `json:"output"`
	// EncryptedContent is provider-issued ciphertext for a reasoning item. It is
	// stored and echoed verbatim and never interpreted.
	EncryptedContent string          `json:"encrypted_content"`
	Summary          json.RawMessage `json:"summary"`
}

// responsesPart is one content part of a message item or tool output.
type responsesPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
	Detail   string          `json:"detail"`
	FileID   string          `json:"file_id"`
	Filename string          `json:"filename"`
}

func (c *responsesCodec) Decode(raw []byte, path string) (DecodedRequest, error) {
	var wire struct {
		Model              json.RawMessage `json:"model"`
		Input              json.RawMessage `json:"input"`
		Instructions       json.RawMessage `json:"instructions"`
		Stream             json.RawMessage `json:"stream"`
		Tools              json.RawMessage `json:"tools"`
		ToolChoice         json.RawMessage `json:"tool_choice"`
		Reasoning          json.RawMessage `json:"reasoning"`
		Text               json.RawMessage `json:"text"`
		MaxOutputTokens    json.RawMessage `json:"max_output_tokens"`
		Temperature        json.RawMessage `json:"temperature"`
		TopP               json.RawMessage `json:"top_p"`
		PreviousResponseID json.RawMessage `json:"previous_response_id"`
		Store              json.RawMessage `json:"store"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Input) == 0 {
		return DecodedRequest{}, fmt.Errorf("%w: invalid Responses request", ErrManagedChat)
	}
	if len(wire.PreviousResponseID) > 0 && string(wire.PreviousResponseID) != "null" {
		return DecodedRequest{}, fmt.Errorf("%w: previous_response_id is not supported", ErrManagedChat)
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
	messages := []ChatMessage{}
	// The system prompt becomes the leading canonical system message.
	if text := responsesText(wire.Instructions); text != "" {
		messages = append(messages, systemMessage(text))
	}
	// `input` is either a bare string or a list of items.
	var text string
	if json.Unmarshal(wire.Input, &text) == nil {
		messages = append(messages, canonicalMessage("user", text))
	} else {
		var items []responsesItem
		if err := json.Unmarshal(wire.Input, &items); err != nil {
			return DecodedRequest{}, fmt.Errorf("%w: invalid input items", ErrManagedChat)
		}
		converted, err := decodeResponsesItems(items)
		if err != nil {
			return DecodedRequest{}, err
		}
		messages = append(messages, converted...)
	}
	if len(messages) == 0 {
		return DecodedRequest{}, fmt.Errorf("%w: empty input", ErrManagedChat)
	}
	// Tools map onto canonical function tools.
	tools, err := responsesToolsToCanonical(wire.Tools)
	if err != nil {
		return DecodedRequest{}, err
	}
	// Only canonical field names are forwarded: the loop rebuilds the message
	// list and sends Raw upstream, so a Responses-only field such as "input"
	// would reach an OpenAI downstream as an unknown key.
	rawOut := map[string]json.RawMessage{"model": wire.Model}
	// The canonical request speaks OpenAI Chat names, so Responses-only field
	// names are translated rather than forwarded.
	if len(wire.MaxOutputTokens) > 0 && string(wire.MaxOutputTokens) != "null" {
		rawOut["max_tokens"] = wire.MaxOutputTokens
	}
	for key, value := range map[string]json.RawMessage{"temperature": wire.Temperature, "top_p": wire.TopP} {
		if len(value) > 0 && string(value) != "null" {
			rawOut[key] = value
		}
	}
	if effort := responsesReasoningEffort(wire.Reasoning); effort != "" {
		rawOut["reasoning_effort"], _ = json.Marshal(effort)
	}
	if format := responsesTextFormat(wire.Text); len(format) > 0 {
		rawOut["response_format"] = format
	}
	if tc := responsesToolChoice(wire.ToolChoice); tc != nil {
		rawOut["tool_choice"] = tc
	}
	if len(tools) > 0 {
		rawOut["tools"], _ = json.Marshal(tools)
	}
	return DecodedRequest{Messages: messages, Model: model, Stream: stream, Raw: rawOut}, nil
}

// decodeResponsesItems turns an item list into canonical messages. Ordering
// follows what the loop's reconstruction requires: a turn's tool results
// directly follow the assistant turn that requested them, and any user text from
// the same turn comes after those results.
func decodeResponsesItems(items []responsesItem) ([]ChatMessage, error) {
	out := []ChatMessage{}
	// calls accumulates one assistant turn's function calls, which arrive as
	// consecutive items rather than as fields of a single message.
	var pending []json.RawMessage
	var pendingText []string
	var pendingThinking []json.RawMessage
	flushAssistant := func() {
		if len(pending) == 0 && len(pendingThinking) == 0 && len(pendingText) == 0 {
			return
		}
		message := canonicalContentMessage("assistant", pendingText, nil)
		if len(pending) > 0 {
			message["tool_calls"], _ = json.Marshal(pending)
		}
		if len(pendingThinking) > 0 {
			message["thinking"], _ = json.Marshal(pendingThinking)
		}
		out = append(out, message)
		pending, pendingText, pendingThinking = nil, nil, nil
	}
	var results []ChatMessage
	var resultImages []json.RawMessage
	flushResults := func() {
		if len(results) > 0 {
			out = append(out, results...)
			results = nil
		}
		if len(resultImages) > 0 {
			out = append(out, canonicalContentMessage("user", nil, resultImages))
			resultImages = nil
		}
	}
	for _, item := range items {
		switch item.Type {
		case "", "message":
			role := item.Role
			if role == "" {
				role = "user"
			}
			texts, images, err := responsesContentParts(item.Content)
			if err != nil {
				return nil, err
			}
			if role == "assistant" {
				flushResults()
				pendingText = append(pendingText, texts...)
				continue
			}
			if role != "user" && role != "system" && role != "developer" {
				return nil, fmt.Errorf("%w: unsupported role %q", ErrManagedChat, role)
			}
			// A developer/system item is a second instructions block; it is kept
			// as its own canonical message rather than merged.
			flushAssistant()
			flushResults()
			if role != "user" {
				role = "system"
			}
			out = append(out, canonicalContentMessage(role, texts, images))
		case "function_call":
			if item.CallID == "" || item.Name == "" {
				return nil, fmt.Errorf("%w: function_call requires call_id and name", ErrManagedChat)
			}
			// A second round of calls may only begin once the previous
			// round's results sit directly after the assistant that
			// requested them. Results with no assistant yet (an output that
			// precedes its call) are left for the end-of-input flush.
			if len(results) > 0 && len(out) > 0 {
				var lastRole string
				_ = json.Unmarshal(out[len(out)-1]["role"], &lastRole)
				if lastRole == "assistant" {
					flushResults()
				}
			}
			// arguments is a JSON-encoded string per spec, but is accepted as a
			// raw object too, matching the existing converter.
			args := "{}"
			if len(item.Args) > 0 {
				if json.Unmarshal(item.Args, &args) != nil {
					args = string(item.Args)
				}
			}
			call, err := json.Marshal(map[string]any{
				"id": item.CallID, "type": "function",
				"function": map[string]any{"name": item.Name, "arguments": args},
			})
			if err != nil {
				return nil, fmt.Errorf("%w: malformed function_call", ErrManagedChat)
			}
			pending = append(pending, call)
		case "reasoning":
			// Opaque provider state is preserved exactly; the loop never reads it.
			entry := map[string]any{"type": "reasoning"}
			if item.EncryptedContent != "" {
				entry["encrypted_content"] = item.EncryptedContent
			}
			if len(item.Summary) > 0 {
				entry["summary"] = json.RawMessage(item.Summary)
			}
			raw, err := json.Marshal(entry)
			if err != nil {
				return nil, fmt.Errorf("%w: malformed reasoning item", ErrManagedChat)
			}
			pendingThinking = append(pendingThinking, raw)
		case "function_call_output":
			if item.CallID == "" {
				return nil, fmt.Errorf("%w: function_call_output requires call_id", ErrManagedChat)
			}
			flushAssistant()
			text, images, err := responsesToolOutput(item.Output)
			if err != nil {
				return nil, err
			}
			results = append(results, canonicalToolResult(item.CallID, text, false))
			resultImages = append(resultImages, images...)
		default:
			return nil, fmt.Errorf("%w: unsupported input item %q", ErrManagedChat, item.Type)
		}
	}
	flushAssistant()
	flushResults()
	return out, nil
}

// responsesContentParts splits content into text and image descriptors. Content
// is either a plain string or a list of typed parts.
func responsesContentParts(raw json.RawMessage) ([]string, []json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []string{text}, nil, nil
	}
	var parts []responsesPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, fmt.Errorf("%w: invalid content parts", ErrManagedChat)
	}
	var texts []string
	var images []json.RawMessage
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text", "summary_text":
			texts = append(texts, part.Text)
		case "input_image":
			source := canonicalResponsesImage(part.ImageURL, part.Detail)
			if source == nil {
				return nil, nil, fmt.Errorf("%w: input_image without image_url", ErrManagedChat)
			}
			images = append(images, source)
		case "input_file":
			return nil, nil, fmt.Errorf("%w: input_file is not supported", ErrManagedChat)
		default:
			return nil, nil, fmt.Errorf("%w: unsupported content part %q", ErrManagedChat, part.Type)
		}
	}
	return texts, images, nil
}

// canonicalResponsesImage turns an Responses image reference into the canonical
// image descriptor. image_url is a string in the flat form and an object with a
// url field in the nested form; both are accepted.
func canonicalResponsesImage(raw json.RawMessage, detail string) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var url string
	media := map[string]any{}
	if json.Unmarshal(raw, &url) != nil {
		var nested struct {
			URL    string `json:"url"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &nested) != nil || nested.URL == "" {
			return nil
		}
		url = nested.URL
		if detail == "" {
			detail = nested.Detail
		}
	}
	media["type"] = "url"
	media["url"] = url
	if detail != "" {
		media["detail"] = detail
	}
	encoded, err := json.Marshal(media)
	if err != nil {
		return nil
	}
	return encoded
}

// responsesToolOutput reads a function_call_output payload. It is a plain string
// in the common case, or a list of parts when it carries images.
func responsesToolOutput(raw json.RawMessage) (string, []json.RawMessage, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var parts []responsesPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, fmt.Errorf("%w: invalid function_call_output", ErrManagedChat)
	}
	var texts []string
	var images []json.RawMessage
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			texts = append(texts, part.Text)
		case "input_image":
			source := canonicalResponsesImage(part.ImageURL, part.Detail)
			if source == nil {
				return "", nil, fmt.Errorf("%w: input_image without image_url", ErrManagedChat)
			}
			images = append(images, source)
		default:
			return "", nil, fmt.Errorf("%w: unsupported tool output part %q", ErrManagedChat, part.Type)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

func (c *responsesCodec) AcceptsOpaque(key string, value json.RawMessage) bool {
	switch key {
	case "thinking":
		// The reasoning items this codec stores, each holding ciphertext.
		var items []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		}
		return json.Unmarshal(value, &items) == nil
	}
	return false
}

// responsesText renders an instructions/string field.
func responsesText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return ""
}

// responsesReasoningEffort reads reasoning.effort, the Responses spelling of the
// canonical reasoning effort.
func responsesReasoningEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var config struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return ""
	}
	// "off" is a Responses spelling the Chat downstream rejects; the
	// responses2openai converter normalizes it the same way.
	if config.Effort == "off" {
		return "none"
	}
	return config.Effort
}

// responsesTextFormat reads text.format, the Responses spelling of
// response_format.
func responsesTextFormat(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var config struct {
		Format json.RawMessage `json:"format"`
	}
	if json.Unmarshal(raw, &config) != nil || len(config.Format) == 0 || string(config.Format) == "null" {
		return nil
	}
	return config.Format
}

// responsesToolsToCanonical maps Responses function tools onto canonical
// function tools. Tool types other than function are refused rather than
// forwarded, because the loop only dispatches functions.
func responsesToolsToCanonical(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("%w: invalid tools", ErrManagedChat)
	}
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "" && tool.Type != "function" {
			return nil, fmt.Errorf("%w: unsupported tool type %q", ErrManagedChat, tool.Type)
		}
		if tool.Name == "" {
			return nil, fmt.Errorf("%w: tool requires a name", ErrManagedChat)
		}
		function := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			function["description"] = tool.Description
		}
		if len(tool.Parameters) > 0 {
			function["parameters"] = json.RawMessage(tool.Parameters)
		} else {
			function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if tool.Strict != nil {
			function["strict"] = *tool.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out, nil
}

// responsesToolChoice maps Responses tool_choice onto the canonical shape. The
// strings (auto, none, required) are shared; the object form names a function.
func responsesToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return raw
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return nil
	}
	if choice.Type == "function" && choice.Name != "" {
		encoded, err := json.Marshal(map[string]any{
			"type": "function", "function": map[string]any{"name": choice.Name},
		})
		if err != nil {
			return nil
		}
		return encoded
	}
	return nil
}

// responsesRenderer writes Responses stream events. The API itemises a response:
// reasoning and text are separate output items, each announced with
// output_item.added and closed with output_item.done, and the turn ends with a
// typed response.completed event. The renderer therefore keeps the index and
// accumulated text of the items it has opened.
type responsesRenderer struct {
	// calls holds the tool calls already rendered as items, so the terminal
	// response carries them and the client can continue the loop.
	calls      []ToolCall
	responseID string
	// reasoningIndex and textIndex are the output indexes of the opened items,
	// or -1 when that item has not been opened yet.
	reasoningIndex int
	textIndex      int
	text           strings.Builder
}

func newResponsesRenderer() *responsesRenderer {
	return &responsesRenderer{responseID: "resp_managed", reasoningIndex: -1, textIndex: -1}
}

func (r *responsesRenderer) FrameStart() []byte {
	return responsesEvent("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": r.responseID, "object": "response", "status": "in_progress",
			"output": []any{},
		},
	})
}

// FrameBlocks reports that output items must be opened before their deltas.
func (r *responsesRenderer) FrameBlocks() bool { return true }

// FrameAlwaysTerminates reports that a response always ends with a terminal
// event carrying its final status.
func (r *responsesRenderer) FrameAlwaysTerminates() bool { return true }

// FrameOpenBlock opens the output item carrying the given kind. Reasoning is
// announced as a reasoning item so clients render it as a thought stream rather
// than as answer text.
func (r *responsesRenderer) FrameOpenBlock(kind string) []byte {
	if kind == "progress" {
		// Once the answer item is open there is no room for a reasoning item,
		// and a dropped frame must not mutate the item indexes.
		if r.reasoningIndex >= 0 || r.textIndex >= 0 {
			return nil
		}
		r.reasoningIndex = 0
		return responsesEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{
				"id": "rs_managed", "type": "reasoning", "summary": []any{},
			},
		})
	}
	if r.textIndex >= 0 {
		return nil
	}
	// A reasoning item, when present, occupies the first output index.
	r.textIndex = 0
	if r.reasoningIndex >= 0 {
		r.textIndex = 1
	}
	return responsesEvent("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": r.textIndex,
		"item": map[string]any{
			"id": "msg_managed", "type": "message", "role": "assistant", "status": "in_progress",
			"content": []any{},
		},
	})
}

// FrameCloseBlock closes whichever item is open. The answer item comes last, so
// closing it is the last thing the turn does before its terminal event.
func (r *responsesRenderer) FrameCloseBlock() []byte {
	switch {
	case r.textIndex >= 0:
		return responsesEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": r.textIndex,
			"item": map[string]any{
				"id": "msg_managed", "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{
					"type": "output_text", "text": r.text.String(), "annotations": []any{},
				}},
			},
		})
	case r.reasoningIndex >= 0:
		return responsesEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": r.reasoningIndex,
			"item": map[string]any{"id": "rs_managed", "type": "reasoning", "summary": []any{}},
		})
	}
	return nil
}

// FrameProgress renders a status delta as a reasoning summary delta.
func (r *responsesRenderer) FrameProgress(text string) []byte {
	index := r.reasoningIndex
	if index < 0 {
		index = 0
	}
	return responsesEvent("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "output_index": index,
		"summary_index": 0, "item_id": "rs_managed", "delta": text,
	})
}

// FrameContent renders answer text as an output_text delta.
func (r *responsesRenderer) FrameContent(text string) []byte {
	r.text.WriteString(text)
	index := r.textIndex
	if index < 0 {
		index = 0
	}
	return responsesEvent("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": index,
		"content_index": 0, "item_id": "msg_managed", "delta": text, "logprobs": []any{},
	})
}

func (r *responsesRenderer) FrameFinish(usage json.RawMessage) []byte {
	response := map[string]any{
		"id": r.responseID, "object": "response", "status": "completed",
		"output": []any{},
	}
	items := []any{}
	if output := r.text.String(); output != "" {
		items = append(items, map[string]any{
			"id": "msg_managed", "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": output, "annotations": []any{},
			}},
		})
	}
	// A turn that asked the client to run tools must carry those items in the
	// terminal response too: SDK clients read them from there, not from the
	// deltas, and without them the loop cannot continue.
	for _, call := range r.calls {
		items = append(items, map[string]any{
			"id": "fc_" + call.ID, "type": "function_call", "status": "completed",
			"call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments),
		})
	}
	response["output"] = items
	if mapped := responsesUsage(usage); mapped != nil {
		response["usage"] = mapped
	}
	return responsesEvent("response.completed", map[string]any{
		"type": "response.completed", "response": response,
	})
}

// FrameToolCalls renders the calls a stored turn requested as function_call
// items, which is how a Responses client learns which tools it must run.
func (r *responsesRenderer) FrameToolCalls(calls []ToolCall) []byte {
	r.calls = append(r.calls, calls...)
	out := []byte{}
	for index, call := range calls {
		output := r.textIndex + 1 + index
		if r.textIndex < 0 {
			output = index
		}
		out = append(out, responsesEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": output,
			"item": map[string]any{
				"id": "fc_" + call.ID, "type": "function_call", "status": "in_progress",
				"call_id": call.ID, "name": call.Name, "arguments": "",
			},
		})...)
		out = append(out, responsesEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "output_index": output,
			"item_id": "fc_" + call.ID, "delta": string(call.Arguments),
		})...)
		out = append(out, responsesEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": output,
			"item": map[string]any{
				"id": "fc_" + call.ID, "type": "function_call", "status": "completed",
				"call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments),
			},
		})...)
	}
	return out
}

func (r *responsesRenderer) FrameEnd() []byte { return nil }

func (r *responsesRenderer) FrameError(code string) []byte {
	return responsesEvent("error", map[string]any{
		"type": "error", "code": "api_error", "message": code,
	})
}

// RenderJSON answers a non-streaming client with a Responses response object
// built from the stored canonical turn.
func (r *responsesRenderer) RenderJSON(canonical []byte) ([]byte, error) {
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
	output := []any{}
	if text != "" {
		output = append(output, map[string]any{
			"id": "msg_managed", "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": text, "annotations": []any{},
			}},
		})
	}
	for _, call := range calls {
		output = append(output, map[string]any{
			"id": "fc_" + call.ID, "type": "function_call",
			"call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments,
		})
	}
	// A turn that called tools is a completed response whose output asks the
	// client for tool results; "incomplete" is reserved for truncation
	// (incomplete_details), and the streaming path reports "completed" too.
	response := map[string]any{
		"id": r.responseID, "object": "response", "status": "completed", "output": output,
	}
	if mapped := responsesUsage(root["usage"]); mapped != nil {
		response["usage"] = mapped
	}
	return json.Marshal(response)
}

// responsesUsage renames the canonical token counters to the Responses names.
func responsesUsage(usage json.RawMessage) map[string]any {
	if len(usage) == 0 {
		return nil
	}
	var counts struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Total      int `json:"total_tokens"`
	}
	if json.Unmarshal(usage, &counts) != nil {
		return nil
	}
	return map[string]any{
		"input_tokens": counts.Prompt, "output_tokens": counts.Completion,
		"total_tokens": counts.Total,
	}
}

// responsesEvent renders one named SSE event.
func responsesEvent(name string, payload any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []byte("event: " + name + "\ndata: " + string(encoded) + "\n\n")
}

func (c *responsesCodec) Renderer() StreamRenderer { return newResponsesRenderer() }

// ClientCallID keeps the downstream's call id: function_call items carry it as
// call_id, and a function_call_output echoes the same value.
func (c *responsesCodec) ClientCallID(call ToolCall, occurrence int) string { return call.ID }
