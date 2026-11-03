package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// geminiCodec serves the Google Gemini generateContent wire format.
//
// Gemini differs from the other formats in three ways that matter to the loop.
// The model is carried in the URL rather than the body. A turn is a list of
// contents, each a list of parts, and a tool call is a `functionCall` part whose
// identity is its function NAME — there is no call id. A tool result is a
// `functionResponse` part that names the function it answers, which is how the
// canonical tool_call_id is reconstructed. Thinking is a `thought` flag on a
// text part plus a `thinkingConfig` budget.
//
// Because Gemini has no call id, this codec derives a deterministic id from the
// function name and arguments, so the same call in the same turn always maps to
// the same canonical id and the loop's result binding stays stable.
type geminiCodec struct{}

func (c *geminiCodec) Name() string   { return geminiCodecName }
func (c *geminiCodec) Format() string { return "gemini" }

// geminiPart is one part of a content turn.
type geminiPart struct {
	Text             string          `json:"text"`
	Thought          bool            `json:"thought"`
	ThoughtSignature string          `json:"thoughtSignature"`
	InlineData       json.RawMessage `json:"inlineData"`
	FileData         json.RawMessage `json:"fileData"`
	FunctionCall     *geminiCall     `json:"functionCall"`
	FunctionResponse *geminiResponse `json:"functionResponse"`
	ExecutableCode   json.RawMessage `json:"executableCode"`
	CodeExecutionRes json.RawMessage `json:"codeExecutionResult"`
}

type geminiCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// geminiContent is one turn of the conversation.
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

func (c *geminiCodec) Decode(raw []byte, path string) (DecodedRequest, error) {
	var wire struct {
		Contents          json.RawMessage `json:"contents"`
		SystemInstruction json.RawMessage `json:"systemInstruction"`
		Tools             json.RawMessage `json:"tools"`
		ToolConfig        json.RawMessage `json:"toolConfig"`
		GenerationConfig  json.RawMessage `json:"generationConfig"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Contents) == 0 {
		return DecodedRequest{}, fmt.Errorf("%w: invalid Gemini request", ErrManagedChat)
	}
	model := geminiModelFromRequestPath(path)
	if model == "" {
		return DecodedRequest{}, fmt.Errorf("%w: model missing from path", ErrManagedChat)
	}
	var contents []geminiContent
	if err := json.Unmarshal(wire.Contents, &contents); err != nil {
		return DecodedRequest{}, fmt.Errorf("%w: invalid contents", ErrManagedChat)
	}
	messages := []ChatMessage{}
	// systemInstruction is a content object whose parts are joined into the
	// leading canonical system turn.
	if text := geminiSystemText(wire.SystemInstruction); text != "" {
		messages = append(messages, systemMessage(text))
	}
	// A function response names the function it answers, never the call, so it is
	// resolved against the calls the preceding model turn actually made.
	// answered records the ids already paired, letting repeated calls to one
	// function pair with their responses in order.
	var lastCalls []json.RawMessage
	answered := map[string]bool{}
	for _, content := range contents {
		converted, calls, err := decodeGeminiContent(content, lastCalls, answered)
		if err != nil {
			return DecodedRequest{}, err
		}
		if strings.EqualFold(content.Role, "model") || strings.EqualFold(content.Role, "assistant") {
			lastCalls, answered = calls, map[string]bool{}
		}
		messages = append(messages, converted...)
	}
	if len(messages) == 0 {
		return DecodedRequest{}, fmt.Errorf("%w: empty contents", ErrManagedChat)
	}
	tools, err := geminiToolsToCanonical(wire.Tools)
	if err != nil {
		return DecodedRequest{}, err
	}
	// A Gemini client always streams over generateContent or streamGenerateContent;
	// the transport the loop uses still follows only the client's own choice,
	// which the route already implies. The request body has no stream flag, so
	// it is read from the path action.
	stream := strings.HasSuffix(path, ":streamGenerateContent")
	// Only canonical names are forwarded: the loop sends Raw to an OpenAI
	// downstream, so Gemini's own field names must not survive.
	rawOut := map[string]json.RawMessage{"model": json.RawMessage(quoteJSON(model))}
	if gen := geminiGenerationConfig(wire.GenerationConfig); len(gen) > 0 {
		for key, value := range gen {
			rawOut[key] = value
		}
	}
	if tc := geminiToolConfig(wire.ToolConfig); tc != nil {
		rawOut["tool_choice"] = tc
	}
	if len(tools) > 0 {
		rawOut["tools"], _ = json.Marshal(tools)
	}
	return DecodedRequest{Messages: messages, Model: model, Stream: stream, Raw: rawOut}, nil
}

// decodeGeminiContent turns one content turn into canonical messages. It also
// returns the canonical tool calls the turn made, so a later turn's function
// responses can be resolved to them by function name.
func decodeGeminiContent(content geminiContent, lastCalls []json.RawMessage, answered map[string]bool) ([]ChatMessage, []json.RawMessage, error) {
	// seen numbers identical calls within this turn so each receives the distinct
	// id the recorded call was given.
	seen := &geminiOccurrences{counts: map[string]int{}}
	role := content.Role
	if role == "" {
		role = "user"
	}
	// A turn's functionResponse parts answer calls from the preceding model
	// turn, so they become canonical tool messages ahead of any prose.
	var results []ChatMessage
	var images []json.RawMessage
	texts := []string{}
	calls := []json.RawMessage{}
	thoughts := []json.RawMessage{}
	for _, part := range content.Parts {
		switch {
		case part.FunctionCall != nil:
			args := "{}"
			if len(part.FunctionCall.Args) > 0 && string(part.FunctionCall.Args) != "null" {
				args = string(part.FunctionCall.Args)
			}
			call, err := json.Marshal(map[string]any{
				"id": seen.FunctionCall(part.FunctionCall.Name, args), "type": "function",
				"function": map[string]any{"name": part.FunctionCall.Name, "arguments": args},
			})
			if err != nil {
				return nil, nil, fmt.Errorf("%w: malformed functionCall", ErrManagedChat)
			}
			calls = append(calls, call)
		case part.FunctionResponse != nil:
			if part.FunctionResponse.Name == "" {
				return nil, nil, fmt.Errorf("%w: functionResponse requires a name", ErrManagedChat)
			}
			// Gemini names the function rather than the call, so the id is
			// resolved against the call this response answers. The loop binds
			// results by canonical id, so an unresolved name cannot be served.
			id := geminiResolveCallID(part.FunctionResponse.Name, lastCalls, answered)
			if id == "" {
				return nil, nil, fmt.Errorf("%w: functionResponse %q answers no call in this turn", ErrManagedChat, part.FunctionResponse.Name)
			}
			results = append(results, canonicalToolResult(id, geminiResponseText(part.FunctionResponse), false))
		case part.InlineData != nil:
			source, err := geminiInlineSource(part.InlineData)
			if err != nil {
				return nil, nil, err
			}
			images = append(images, source)
		case part.FileData != nil:
			source, err := geminiFileSource(part.FileData)
			if err != nil {
				return nil, nil, err
			}
			images = append(images, source)
		case part.ExecutableCode != nil || part.CodeExecutionRes != nil:
			return nil, nil, fmt.Errorf("%w: code execution parts are not supported", ErrManagedChat)
		case part.Thought:
			raw, err := json.Marshal(map[string]any{
				"type": "gemini_thought", "text": part.Text, "thoughtSignature": part.ThoughtSignature,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("%w: malformed thought", ErrManagedChat)
			}
			thoughts = append(thoughts, raw)
		default:
			texts = append(texts, part.Text)
		}
	}
	switch role {
	case "user":
		// Tool results directly follow the turn that requested them; user text
		// from the same turn comes after.
		out := append([]ChatMessage{}, results...)
		if len(texts) > 0 || len(images) > 0 {
			out = append(out, canonicalContentMessage("user", texts, images))
		}
		if len(out) == 0 {
			return nil, nil, fmt.Errorf("%w: empty user turn", ErrManagedChat)
		}
		return out, calls, nil
	case "model", "assistant":
		message := canonicalContentMessage("assistant", texts, images)
		if len(calls) > 0 {
			message["tool_calls"], _ = json.Marshal(calls)
		}
		if len(thoughts) > 0 {
			message["thinking"], _ = json.Marshal(thoughts)
		}
		return []ChatMessage{message}, calls, nil
	default:
		return nil, nil, fmt.Errorf("%w: unsupported role %q", ErrManagedChat, role)
	}
}

// geminiCallID derives the canonical call id for a Gemini function call. Gemini
// carries no id, so name and arguments identify the call.
func geminiCallID(name, args string) string {
	// The id is derived from the call's meaning, not its spelling: a client may
	// re-serialize the same arguments with different key order or spacing
	// between turns, and an id that tracked the raw bytes would stop matching the
	// call it answers.
	if canonical, err := canonicalJSON([]byte(args)); err == nil {
		args = string(canonical)
	}
	sum := sha256.Sum256([]byte(name + "\x00" + args))
	return "call_" + hex.EncodeToString(sum[:8])
}

// geminiResolveCallID finds the call a function response answers. Gemini names
// the function instead of the call, so the id is taken from the call the model
// made in the preceding turn. `used` records the ids already answered in this
// turn, so two calls to the same function pair with their responses in order
// rather than both resolving to the first call.
func geminiResolveCallID(name string, calls []json.RawMessage, used map[string]bool) string {
	for _, raw := range calls {
		var call struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &call) == nil && call.Function.Name == name && !used[call.ID] {
			used[call.ID] = true
			return call.ID
		}
	}
	// The answered call was made in an earlier request, so it is not in this
	// one. The loop has already bound that result by id; an id this codec cannot
	// recover is refused rather than guessed, because a wrong id would silently
	// answer a different call.
	return ""
}

// geminiResponseText renders a functionResponse payload as tool output text.
func geminiResponseText(response *geminiResponse) string {
	if len(response.Response) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(response.Response, &text) == nil {
		return text
	}
	var payload struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(response.Response, &payload) == nil && payload.Output != "" {
		return payload.Output
	}
	return string(response.Response)
}

// geminiInlineSource turns inlineData into the canonical image descriptor.
func geminiInlineSource(raw json.RawMessage) (json.RawMessage, error) {
	var data struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.Data == "" || data.MimeType == "" {
		return nil, fmt.Errorf("%w: inlineData requires mimeType and data", ErrManagedChat)
	}
	encoded, err := json.Marshal(map[string]any{
		"type": "base64", "media_type": data.MimeType, "data": data.Data,
	})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// geminiFileSource turns fileData (a URI reference) into the canonical image
// descriptor.
func geminiFileSource(raw json.RawMessage) (json.RawMessage, error) {
	var data struct {
		MimeType string `json:"mimeType"`
		FileURI  string `json:"fileUri"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.FileURI == "" {
		return nil, fmt.Errorf("%w: fileData requires fileUri", ErrManagedChat)
	}
	encoded, err := json.Marshal(map[string]any{"type": "url", "url": data.FileURI})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// geminiSystemText joins the parts of a systemInstruction object.
func geminiSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var instruction struct {
		Parts []geminiPart `json:"parts"`
	}
	if json.Unmarshal(raw, &instruction) != nil {
		return ""
	}
	texts := []string{}
	for _, part := range instruction.Parts {
		texts = append(texts, part.Text)
	}
	return strings.Join(nonEmpty(texts), "\n")
}

// geminiModelFromRequestPath reads the model out of
// /v1beta/models/<model>:<action>.
func geminiModelFromRequestPath(path string) string {
	if !strings.HasPrefix(path, geminiPathPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, geminiPathPrefix)
	if rest == "" {
		return ""
	}
	if index := strings.Index(rest, ":"); index >= 0 {
		rest = rest[:index]
	}
	return strings.TrimSuffix(rest, "/")
}

// geminiGenerationConfig maps generationConfig onto canonical names. A
// thinkingConfig budget becomes the canonical reasoning effort.
func geminiGenerationConfig(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var config struct {
		Temperature      json.RawMessage `json:"temperature"`
		TopP             json.RawMessage `json:"topP"`
		TopK             json.RawMessage `json:"topK"`
		MaxOutputTokens  json.RawMessage `json:"maxOutputTokens"`
		StopSequences    json.RawMessage `json:"stopSequences"`
		ResponseMimeType json.RawMessage `json:"responseMimeType"`
		ThinkingConfig   json.RawMessage `json:"thinkingConfig"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for key, value := range map[string]json.RawMessage{
		"temperature": config.Temperature, "top_p": config.TopP, "top_k": config.TopK,
		"max_tokens": config.MaxOutputTokens, "stop": config.StopSequences,
	} {
		if len(value) > 0 && string(value) != "null" {
			out[key] = value
		}
	}
	// A JSON mime type is the Gemini spelling of a JSON response format.
	if len(config.ResponseMimeType) > 0 {
		var mime string
		if json.Unmarshal(config.ResponseMimeType, &mime) == nil && mime == "application/json" {
			out["response_format"], _ = json.Marshal(map[string]any{"type": "json_object"})
		}
	}
	if effort := geminiThinkingEffort(config.ThinkingConfig); effort != "" {
		out["reasoning_effort"], _ = json.Marshal(effort)
	}
	return out
}

// geminiThinkingEffort maps a thinkingConfig onto a canonical reasoning effort,
// mirroring levelForGeminiThinking/budgetToEffort in the native converter so
// the managed and non-managed paths send the same effort for one request.
func geminiThinkingEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var config struct {
		IncludeThoughts bool `json:"includeThoughts"`
		ThinkingBudget  *int `json:"thinkingBudget"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return ""
	}
	budget := config.ThinkingBudget
	switch {
	case budget == nil:
		return ""
	case *budget == 0 && !config.IncludeThoughts:
		return "none"
	case *budget == 0:
		return "low"
	case *budget < 0:
		return "medium"
	case *budget <= 1024:
		return "minimal"
	case *budget <= 2048:
		return "low"
	case *budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// geminiToolsToCanonical flattens functionDeclarations into canonical function
// tools. Tool kinds the loop cannot dispatch are refused.
func geminiToolsToCanonical(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tools []struct {
		FunctionDeclarations []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"functionDeclarations"`
		GoogleSearch    json.RawMessage `json:"googleSearch"`
		CodeExecution   json.RawMessage `json:"codeExecution"`
		GoogleSearchRet json.RawMessage `json:"googleSearchRetrieval"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("%w: invalid tools", ErrManagedChat)
	}
	out := []any{}
	for _, tool := range tools {
		if len(tool.GoogleSearch) > 0 || len(tool.CodeExecution) > 0 || len(tool.GoogleSearchRet) > 0 {
			return nil, fmt.Errorf("%w: built-in gemini tools are not supported", ErrManagedChat)
		}
		for _, declaration := range tool.FunctionDeclarations {
			if declaration.Name == "" {
				return nil, fmt.Errorf("%w: function declaration requires a name", ErrManagedChat)
			}
			function := map[string]any{"name": declaration.Name}
			if declaration.Description != "" {
				function["description"] = declaration.Description
			}
			if len(declaration.Parameters) > 0 && string(declaration.Parameters) != "null" {
				function["parameters"] = json.RawMessage(declaration.Parameters)
			} else {
				function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, map[string]any{"type": "function", "function": function})
		}
	}
	return out, nil
}

// geminiToolConfig maps functionCallingConfig onto canonical tool_choice.
func geminiToolConfig(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var config struct {
		FunctionCallingConfig struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return nil
	}
	calling := config.FunctionCallingConfig
	switch calling.Mode {
	case "NONE":
		return json.RawMessage(quoteJSON("none"))
	case "ANY":
		if len(calling.AllowedFunctionNames) == 1 {
			encoded, err := json.Marshal(map[string]any{
				"type": "function", "function": map[string]any{"name": calling.AllowedFunctionNames[0]},
			})
			if err != nil {
				return nil
			}
			return encoded
		}
		return json.RawMessage(quoteJSON("required"))
	case "AUTO", "":
		return nil
	}
	return nil
}

func (c *geminiCodec) AcceptsOpaque(key string, value json.RawMessage) bool {
	if key != "thinking" {
		return false
	}
	var items []struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(value, &items) == nil
}

// geminiRenderer writes Gemini stream chunks. The streaming API sends
// successive JSON objects, each carrying candidates[].content.parts[].
type geminiRenderer struct {
	model string
}

func (r *geminiRenderer) FrameStart() []byte { return nil }

// FrameBlocks reports that Gemini deltas carry their own part type, so no block
// has to be opened around them.
func (r *geminiRenderer) FrameBlocks() bool { return false }

// FrameAlwaysTerminates reports that every response ends with a chunk carrying
// its finishReason, even after the answer streamed.
func (r *geminiRenderer) FrameAlwaysTerminates() bool { return true }

// FrameContent renders answer text as a text part.
func (r *geminiRenderer) FrameContent(text string) []byte {
	return geminiChunk(r.model, map[string]any{"text": text})
}

// FrameProgress renders a status delta as a thought part, so clients show it as
// reasoning rather than as answer text.
func (r *geminiRenderer) FrameProgress(text string) []byte {
	return geminiChunk(r.model, map[string]any{"text": text, "thought": true})
}

// FrameFinish renders the final chunk with its stop reason and usage.
func (r *geminiRenderer) FrameFinish(usage json.RawMessage) []byte {
	payload := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{}},
			"finishReason": "STOP",
			"index":        0,
		}},
	}
	if mapped := geminiUsage(usage); mapped != nil {
		payload["usageMetadata"] = mapped
	}
	return geminiChunkRaw(r.model, payload)
}

// FrameToolCalls renders the calls a stored turn requested as functionCall
// parts, which is how a Gemini client learns which tools it must run.
func (r *geminiRenderer) FrameToolCalls(calls []ToolCall) []byte {
	parts := []any{}
	for _, call := range calls {
		args := json.RawMessage("{}")
		if len(call.Arguments) > 0 && json.Valid(call.Arguments) {
			args = call.Arguments
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{"name": call.Name, "args": args},
		})
	}
	return geminiChunkRaw(r.model, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": parts},
			"index":   0,
		}},
	})
}

// FrameEnd is absent: Gemini's stream simply stops after the final chunk.
func (r *geminiRenderer) FrameEnd() []byte { return nil }

func (r *geminiRenderer) FrameError(code string) []byte {
	return geminiChunkRaw(r.model, map[string]any{
		"error": map[string]any{"code": 500, "message": code, "status": "INTERNAL"},
	})
}

// RenderJSON answers a non-streaming client with a generateContent response.
func (r *geminiRenderer) RenderJSON(canonical []byte) ([]byte, error) {
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
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if len(message["tool_calls"]) > 0 && json.Unmarshal(message["tool_calls"], &calls) != nil {
		return nil, errManagedProjection
	}
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	for _, call := range calls {
		args := json.RawMessage("{}")
		if call.Function.Arguments != "" && json.Valid([]byte(call.Function.Arguments)) {
			args = json.RawMessage(call.Function.Arguments)
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{"name": call.Function.Name, "args": args},
		})
	}
	candidate := map[string]any{
		"content":      map[string]any{"role": "model", "parts": parts},
		"finishReason": "STOP",
		"index":        0,
	}
	response := map[string]any{"candidates": []any{candidate}}
	if mapped := geminiUsage(root["usage"]); mapped != nil {
		response["usageMetadata"] = mapped
	}
	return json.Marshal(response)
}

// geminiUsage renames the canonical token counters to Gemini's usage metadata.
func geminiUsage(usage json.RawMessage) map[string]any {
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
		"promptTokenCount":     counts.Prompt,
		"candidatesTokenCount": counts.Completion,
		"totalTokenCount":      counts.Total,
	}
}

// geminiChunk renders one streaming chunk carrying a single part.
func geminiChunk(model string, part map[string]any) []byte {
	return geminiChunkRaw(model, map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{part}},
			"index":   0,
		}},
	})
}

func geminiChunkRaw(model string, payload map[string]any) []byte {
	if model != "" {
		payload["modelVersion"] = model
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []byte("data: " + string(encoded) + "\n\n")
}

// Renderer supplies a renderer bound to the model being served, which Gemini
// clients expect to see echoed in each chunk.
func (c *geminiCodec) Renderer() StreamRenderer { return &geminiRenderer{} }

// ClientCallID derives the id Gemini can reproduce. A functionCall carries only
// a name and args, so the id must be a function of those two values; the
// following request re-derives it from the functionResponse's name and resolves
// it against the call it answers.
func (c *geminiCodec) ClientCallID(call ToolCall, occurrence int) string {
	return geminiOccurrenceCallID(call.Name, string(call.Arguments), occurrence)
}

// geminiOccurrenceCallID qualifies the derived id with the call's position among
// identical calls in the same turn, so two identical calls stay distinguishable
// on a wire that carries no id.
func geminiOccurrenceCallID(name, args string, occurrence int) string {
	if occurrence == 0 {
		return geminiCallID(name, args)
	}
	return geminiCallID(name, fmt.Sprintf("%s\x00%d", args, occurrence))
}

// geminiOccurrences numbers identical calls within one turn. It mirrors the
// occurrence counter the loop uses when it records a call, so the id derived
// while decoding matches the id that was recorded.
type geminiOccurrences struct{ counts map[string]int }

// FunctionCall returns the id for a call, qualified by its position among
// identical calls in the same turn.
func (o *geminiOccurrences) FunctionCall(name, args string) string {
	key := name + "\x00" + args
	occurrence := o.counts[key]
	o.counts[key]++
	return geminiOccurrenceCallID(name, args, occurrence)
}
