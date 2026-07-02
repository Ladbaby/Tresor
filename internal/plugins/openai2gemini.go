package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"tresor/internal/engine"
)

// OpenAI2Gemini converts OpenAI Chat Completion requests to Google Gemini
// generateContent requests and converts the Gemini response (JSON or SSE)
// back to OpenAI Chat Completion format.
//
// Gemini's generateContent endpoint URL is
//   POST /v1beta/models/{model}:generateContent
// (and :streamGenerateContent?alt=sse when stream=true). Authentication is
// via the x-goog-api-key header (or ?key= query param). The body shape is:
//
//   {
//     "contents": [{"role": "user"|"model", "parts": [...]}],
//     "systemInstruction": {"parts": [{"text": "..."}]},
//     "tools": [{"functionDeclarations": [{"name", "description", "parameters"}]}],
//     "toolConfig": {"functionCallingConfig": {"mode": "AUTO"|"NONE"|"ANY"}},
//     "generationConfig": {
//        "temperature": ..., "topP": ..., "topK": ...,
//        "maxOutputTokens": ..., "stopSequences": [...],
//        "responseMimeType": ..., "responseSchema": {...}
//     }
//   }
type OpenAI2Gemini struct{}

// PluginName returns the stable type name for deduplication.
func (t *OpenAI2Gemini) PluginName() string { return "OpenAI2Gemini" }

// TransformRequest converts an OpenAI Chat Completion request into a Gemini
// generateContent request. The URL is rewritten to the appropriate Gemini
// model-action endpoint based on the "stream" flag.
func (t *OpenAI2Gemini) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var openAIReq openAIChatRequest
	if err := json.Unmarshal(body, &openAIReq); err != nil {
		return nil, nil, fmt.Errorf("openai2gemini: failed to parse request: %w", err)
	}

	// Map the model. The OpenAI request is rewritten to the Gemini model name;
	// because the engine already substitutes the resolved output model into
	// the body's "model" field, we simply use that value here.
	geminiModel := openAIReq.Model
	if geminiModel == "" && ctx.TargetDownstream != nil {
		// Fallback: keep whatever the path says; an empty model here will
		// cause the downstream to error and surface as 4xx.
		geminiModel = ctx.TargetDownstream.ID
	}

	geminiBody := map[string]interface{}{}

	// --- messages → contents + systemInstruction ---
	var contents []map[string]interface{}
	var systemParts []map[string]interface{}

	flushSystem := func(text string) {
		if text == "" {
			return
		}
		systemParts = append(systemParts, map[string]interface{}{"text": text})
	}

	for _, msg := range openAIReq.Messages {
		if msg.Role == "system" {
			flushSystem(msg.Content.Text)
			continue
		}

		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			// Assistant message with tool_calls → model role with text + functionCall parts
			parts := []map[string]interface{}{}
			if msg.Content.Text != "" {
				parts = append(parts, map[string]interface{}{"text": msg.Content.Text})
			} else if msg.Content.Set && len(msg.Content.Parts) > 0 {
				parts = append(parts, openAIPartsToGeminiParts(msg.Content.Parts)...)
			}
			for _, tc := range msg.ToolCalls {
				var args interface{}
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				} else {
					args = map[string]interface{}{}
				}
				parts = append(parts, map[string]interface{}{
					"functionCall": map[string]interface{}{
						"name": tc.Function.Name,
						"args": args,
					},
				})
			}
			contents = append(contents, map[string]interface{}{
				"role":  "model",
				"parts": parts,
			})
			continue
		}

		if msg.Role == "tool" {
			// Tool result → user role with functionResponse part
			var response map[string]interface{}
			if msg.Content.Text != "" {
				// Best-effort: parse text as JSON, fall back to wrapping in {"output": ...}
				if err := json.Unmarshal([]byte(msg.Content.Text), &response); err != nil {
					response = map[string]interface{}{"output": msg.Content.Text}
				}
			} else if len(msg.Content.Parts) > 0 {
				// Multimodal tool content — embed as a textual response payload.
				response = map[string]interface{}{
					"output": extractStringContent(msg.Content.Parts),
				}
			} else {
				response = map[string]interface{}{}
			}
			contents = append(contents, map[string]interface{}{
				"role": "user",
				"parts": []map[string]interface{}{
					{"functionResponse": map[string]interface{}{
						"name":     msg.ToolCallID,
						"response": response,
					}},
				},
			})
			continue
		}

		// Regular user / assistant / other role text/multimodal message.
		if msg.Role == "assistant" {
			if msg.Content.Set && len(msg.Content.Parts) > 0 {
				contents = append(contents, map[string]interface{}{
					"role":  "model",
					"parts": openAIPartsToGeminiParts(msg.Content.Parts),
				})
			} else {
				contents = append(contents, map[string]interface{}{
					"role":  "model",
					"parts": []map[string]interface{}{{"text": msg.Content.Text}},
				})
			}
		} else {
			// Default: anything that isn't assistant/system/tool is treated as user.
			if msg.Content.Set && len(msg.Content.Parts) > 0 {
				contents = append(contents, map[string]interface{}{
					"role":  "user",
					"parts": openAIPartsToGeminiParts(msg.Content.Parts),
				})
			} else {
				contents = append(contents, map[string]interface{}{
					"role":  "user",
					"parts": []map[string]interface{}{{"text": msg.Content.Text}},
				})
			}
		}
	}

	// Also pick up the OpenAI "system" field if any.
	if openAIReq.System != "" {
		flushSystem(openAIReq.System)
	}

	if len(systemParts) > 0 {
		geminiBody["systemInstruction"] = map[string]interface{}{"parts": systemParts}
	}

	// Ensure at least one content entry — Gemini rejects empty contents.
	if len(contents) == 0 {
		contents = append(contents, map[string]interface{}{
			"role":  "user",
			"parts": []map[string]interface{}{{"text": "Hello"}},
		})
	}
	geminiBody["contents"] = contents

	// --- tools → functionDeclarations ---
	if len(openAIReq.Tools) > 0 {
		var openAITools []map[string]interface{}
		if err := json.Unmarshal(openAIReq.Tools, &openAITools); err == nil {
			decls := []map[string]interface{}{}
			for _, ot := range openAITools {
				fn, _ := ot["function"].(map[string]interface{})
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				decl := map[string]interface{}{"name": name}
				if desc, _ := fn["description"].(string); desc != "" {
					decl["description"] = desc
				}
				if params, ok := fn["parameters"]; ok && params != nil {
					decl["parameters"] = params
				}
				decls = append(decls, decl)
			}
			if len(decls) > 0 {
				geminiBody["tools"] = []map[string]interface{}{
					{"functionDeclarations": decls},
				}
			}
		}
	}

	// --- tool_choice → toolConfig.functionCallingConfig ---
	if len(openAIReq.ToolChoice) > 0 {
		var tcRaw json.RawMessage
		if err := json.Unmarshal(openAIReq.ToolChoice, &tcRaw); err == nil {
			mode := "AUTO"
			var allowedNames []string
			var tcStr string
			if json.Unmarshal(tcRaw, &tcStr) == nil {
				switch tcStr {
				case "auto":
					mode = "AUTO"
				case "none":
					mode = "NONE"
				case "required":
					mode = "ANY"
				}
			} else {
				var tcObj struct {
					Type     string `json:"type"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if json.Unmarshal(tcRaw, &tcObj) == nil && tcObj.Type == "function" {
					mode = "ANY"
					if tcObj.Function.Name != "" {
						allowedNames = []string{tcObj.Function.Name}
					}
				}
			}
			tcCfg := map[string]interface{}{"mode": mode}
			if len(allowedNames) > 0 {
				tcCfg["allowedFunctionNames"] = allowedNames
			}
			geminiBody["toolConfig"] = map[string]interface{}{
				"functionCallingConfig": tcCfg,
			}
		}
	}

	// --- generationConfig ---
	genCfg := map[string]interface{}{}
	if openAIReq.Temperature > 0 {
		genCfg["temperature"] = openAIReq.Temperature
	}
	if openAIReq.MaxTokens > 0 {
		genCfg["maxOutputTokens"] = openAIReq.MaxTokens
	}
	if len(openAIReq.Stop) > 0 {
		genCfg["stopSequences"] = openAIReq.Stop
	}
	// response_format (json_object / json_schema) → responseMimeType + responseSchema
	if rf := openAIRequestField(body, "response_format"); rf != nil {
		switch rfv := rf.(type) {
		case string:
			if rfv == "json_object" {
				genCfg["responseMimeType"] = "application/json"
			}
		case map[string]interface{}:
			if t, _ := rfv["type"].(string); t == "json_object" {
				genCfg["responseMimeType"] = "application/json"
			}
			if schema, ok := rfv["json_schema"]; ok {
				if schemaObj, ok := schema.(map[string]interface{}); ok {
					if s, ok := schemaObj["schema"]; ok {
						genCfg["responseMimeType"] = "application/json"
						genCfg["responseSchema"] = s
					}
				}
			}
		}
	}
	if len(genCfg) > 0 {
		geminiBody["generationConfig"] = genCfg
	}

	// Marshal the new body
	newBody, err := json.Marshal(geminiBody)
	if err != nil {
		return nil, nil, fmt.Errorf("openai2gemini: failed to marshal request: %w", err)
	}

	// URL: /v1beta/models/{model}:generateContent or :streamGenerateContent?alt=sse
	action := "generateContent"
	if openAIReq.Stream {
		action = "streamGenerateContent"
	}
	newPath := fmt.Sprintf("/v1beta/models/%s:%s", geminiModel, action)

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	newReq.URL.Path = newPath
	if openAIReq.Stream {
		newReq.URL.RawQuery = "alt=sse"
	}

	// Auth: set x-goog-api-key if not already set by the engine
	if ctx.TargetDownstream != nil && ctx.TargetDownstream.APIKey != "" {
		if newReq.Header.Get("x-goog-api-key") == "" {
			newReq.Header.Set("x-goog-api-key", ctx.TargetDownstream.APIKey)
		}
	}

	return newReq, newBody, nil
}

// openAIRequestField is a small helper to read a top-level field out of the
// raw OpenAI request body without committing to a typed struct.
func openAIRequestField(body []byte, field string) interface{} {
	if len(body) == 0 {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	return raw[field]
}

// openAIPartsToGeminiParts converts an OpenAI multimodal content-parts array
// into Gemini parts. Supports text and image_url parts.
func openAIPartsToGeminiParts(parts []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(parts))
	for _, p := range parts {
		ptype, _ := p["type"].(string)
		switch ptype {
		case "text":
			if text, _ := p["text"].(string); text != "" {
				out = append(out, map[string]interface{}{"text": text})
			}
		case "image_url":
			iu, _ := p["image_url"].(map[string]interface{})
			url, _ := iu["url"].(string)
			if url == "" {
				continue
			}
			if block, ok := openAIImageToGeminiInline(url); ok {
				out = append(out, block)
			}
		}
	}
	return out
}

// openAIImageToGeminiInline converts an OpenAI image URL (data URI or
// http(s) URL) into a Gemini inline_data part.
func openAIImageToGeminiInline(url string) (map[string]interface{}, bool) {
	if strings.HasPrefix(url, "data:") {
		rest := strings.TrimPrefix(url, "data:")
		semi := strings.Index(rest, ";")
		if semi < 0 || !strings.HasPrefix(rest[semi+1:], "base64,") {
			return nil, false
		}
		mediaType := rest[:semi]
		payload := rest[semi+1+len("base64,"):]
		if payload == "" {
			return nil, false
		}
		return map[string]interface{}{
			"inlineData": map[string]interface{}{
				"mimeType": mediaType,
				"data":     payload,
			},
		}, true
	}
	// External URL: best-effort fetch.
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != 200 {
		return nil, false
	}
	defer resp.Body.Close()
	mediaType := resp.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "image/png"
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return map[string]interface{}{
		"inlineData": map[string]interface{}{
			"mimeType": mediaType,
			"data":     base64Encode(data),
		},
	}, true
}

// base64Encode is a tiny wrapper so we don't need to import encoding/base64
// at the top level (matches style of other helpers in the package).
func base64Encode(b []byte) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out bytes.Buffer
	n := len(b)
	for i := 0; i < n; i += 3 {
		var v uint32
		switch n - i {
		case 1:
			v = uint32(b[i]) << 16
			out.WriteByte(tbl[(v>>18)&0x3f])
			out.WriteByte(tbl[(v>>12)&0x3f])
			out.WriteByte('=')
			out.WriteByte('=')
		case 2:
			v = uint32(b[i])<<16 | uint32(b[i+1])<<8
			out.WriteByte(tbl[(v>>18)&0x3f])
			out.WriteByte(tbl[(v>>12)&0x3f])
			out.WriteByte(tbl[(v>>6)&0x3f])
			out.WriteByte('=')
		default:
			v = uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
			out.WriteByte(tbl[(v>>18)&0x3f])
			out.WriteByte(tbl[(v>>12)&0x3f])
			out.WriteByte(tbl[(v>>6)&0x3f])
			out.WriteByte(tbl[v&0x3f])
		}
	}
	return out.String()
}

// TransformResponse converts a Gemini GenerateContentResponse into an OpenAI
// Chat Completion response. For streaming responses (text/event-stream) it
// converts the collected SSE body into OpenAI streaming chunks.
func (t *OpenAI2Gemini) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "text/event-stream" {
		return t.transformStreamingResponse(body)
	}
	return t.transformJSONResponse(body)
}

func (t *OpenAI2Gemini) transformJSONResponse(body []byte) ([]byte, error) {
	var geminiResp geminiGenerateContentResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		// Pass-through for non-JSON / error pages.
		return body, nil
	}

	openAIResp := openAIChatResponse{
		Object: "chat.completion",
	}

	if len(geminiResp.Candidates) > 0 {
		candidate := geminiResp.Candidates[0]
		msg := openAIChatMessage{Role: "assistant"}

		var content strings.Builder
		var reasoning strings.Builder
		var toolCalls []openAIChatToolCall

		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				if part.Thought {
					reasoning.WriteString(part.Text)
				} else {
					content.WriteString(part.Text)
				}
				continue
			}
			if part.FunctionCall != nil {
				args := "{}"
				if part.FunctionCall.Args != nil {
					b, err := json.Marshal(part.FunctionCall.Args)
					if err == nil {
						args = string(b)
					}
				}
				tc := openAIChatToolCall{
					ID:   fmt.Sprintf("call_%s", part.FunctionCall.Name),
					Type: "function",
				}
				tc.Function.Name = part.FunctionCall.Name
				tc.Function.Arguments = args
				toolCalls = append(toolCalls, tc)
			}
		}

		msg.Content = openAIChatContent{
			Text: content.String(),
			Set:  content.Len() > 0,
		}
		if reasoning.Len() > 0 {
			msg.ReasoningContent = reasoning.String()
		}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}

		finish := mapGeminiFinishReason(candidate.FinishReason)
		openAIResp.Choices = []openAIChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finish,
		}}
		openAIResp.ID = "chatcmpl-" + geminiResp.ModelVersion
		openAIResp.Model = geminiResp.ModelVersion
	}

	if geminiResp.UsageMetadata != nil {
		openAIResp.Usage = openAIChatUsage{
			PromptTokens:     geminiResp.UsageMetadata.PromptTokenCount,
			CompletionTokens: geminiResp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      geminiResp.UsageMetadata.TotalTokenCount,
		}
	}

	return json.Marshal(openAIResp)
}

func (t *OpenAI2Gemini) transformStreamingResponse(body []byte) ([]byte, error) {
	var out bytes.Buffer
	var id, model string
	var roleSent bool

	parseGeminiSSE(body, func(data []byte) bool {
		var chunk geminiGenerateContentResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			return true
		}
		if id == "" && chunk.ModelVersion != "" {
			id = "chatcmpl-" + chunk.ModelVersion
			model = chunk.ModelVersion
		}

		if len(chunk.Candidates) == 0 {
			return true
		}
		candidate := chunk.Candidates[0]
		if !roleSent {
			outChunk := openAIChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Model:   model,
				Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Role: "assistant"}}},
			}
			writeSSEData(&out, outChunk)
			roleSent = true
		}

		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				delta := openAIDelta{Content: part.Text}
				if part.Thought {
					delta.ReasoningContent = part.Text
					delta.Content = ""
				}
				outChunk := openAIChunk{
					ID:      id,
					Object:  "chat.completion.chunk",
					Model:   model,
					Choices: []openAIChunkChoice{{Index: 0, Delta: delta}},
				}
				writeSSEData(&out, outChunk)
			}
			if part.FunctionCall != nil {
				args := "{}"
				if part.FunctionCall.Args != nil {
					b, err := json.Marshal(part.FunctionCall.Args)
					if err == nil {
						args = string(b)
					}
				}
				tc := openAIToolCallDelta{
					Index: 0,
					ID:    fmt.Sprintf("call_%s", part.FunctionCall.Name),
					Type:  "function",
				}
				tc.Function.Name = part.FunctionCall.Name
				tc.Function.Arguments = args
				outChunk := openAIChunk{
					ID:      id,
					Object:  "chat.completion.chunk",
					Model:   model,
					Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{tc}}}},
				}
				writeSSEData(&out, outChunk)
			}
		}

		if candidate.FinishReason != "" {
			finish := mapGeminiFinishReason(candidate.FinishReason)
			outChunk := openAIChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Model:   model,
				Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{}, FinishReason: &finish}},
			}
			writeSSEData(&out, outChunk)
			writeDoneMarker(&out)
		}
		return true
	})

	if !roleSent {
		// Edge case: downstream sent no candidates. Emit a minimal role chunk
		// so OpenAI clients don't error on missing first chunk.
		outChunk := openAIChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Model:   model,
			Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Role: "assistant"}}},
		}
		writeSSEData(&out, outChunk)
		writeDoneMarker(&out)
	}

	return out.Bytes(), nil
}

// mapGeminiFinishReason translates a Gemini finishReason to OpenAI's.
func mapGeminiFinishReason(r string) string {
	switch r {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	case "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "MALFORMED_FUNCTION_CALL", "IMAGE_SAFETY", "LANGUAGE", "OTHER":
		return "stop"
	default:
		return r
	}
}

// TransformStreamChunk converts a single Gemini SSE event (one JSON payload
// per data: line) into an OpenAI SSE chunk.
func (t *OpenAI2Gemini) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	state := &oai2geminiStreamState{}
	if existing, ok := ctx.Variables["oai2gem_stream"]; ok {
		state = existing.(*oai2geminiStreamState)
	}
	defer func() { ctx.Variables["oai2gem_stream"] = state }()

	// Each Gemini data: line is a GenerateContentResponse JSON.
	var resp geminiGenerateContentResponse
	if err := json.Unmarshal(chunk.Data, &resp); err != nil {
		return chunk, nil
	}

	if state.ID == "" && resp.ModelVersion != "" {
		state.ID = "chatcmpl-" + resp.ModelVersion
		state.Model = resp.ModelVersion
	}

	if len(resp.Candidates) == 0 {
		return engine.SSEChunk{}, nil
	}
	candidate := resp.Candidates[0]

	if !state.roleSent {
		state.roleSent = true
		outChunk := openAIChunk{
			ID:      state.ID,
			Object:  "chat.completion.chunk",
			Model:   state.Model,
			Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Role: "assistant"}}},
		}
		data, _ := json.Marshal(outChunk)
		return engine.SSEChunk{EventType: "", Data: data}, nil
	}

	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			delta := openAIDelta{Content: part.Text}
			if part.Thought {
				delta.ReasoningContent = part.Text
				delta.Content = ""
			}
			outChunk := openAIChunk{
				ID:      state.ID,
				Object:  "chat.completion.chunk",
				Model:   state.Model,
				Choices: []openAIChunkChoice{{Index: 0, Delta: delta}},
			}
			data, _ := json.Marshal(outChunk)
			return engine.SSEChunk{EventType: "", Data: data}, nil
		}
		if part.FunctionCall != nil {
			args := "{}"
			if part.FunctionCall.Args != nil {
				b, err := json.Marshal(part.FunctionCall.Args)
				if err == nil {
					args = string(b)
				}
			}
			tc := openAIToolCallDelta{
				Index: 0,
				ID:    fmt.Sprintf("call_%s", part.FunctionCall.Name),
				Type:  "function",
			}
			tc.Function.Name = part.FunctionCall.Name
			tc.Function.Arguments = args
			outChunk := openAIChunk{
				ID:      state.ID,
				Object:  "chat.completion.chunk",
				Model:   state.Model,
				Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{tc}}}},
			}
			data, _ := json.Marshal(outChunk)
			return engine.SSEChunk{EventType: "", Data: data}, nil
		}
	}

	if candidate.FinishReason != "" {
		finish := mapGeminiFinishReason(candidate.FinishReason)
		outChunk := openAIChunk{
			ID:      state.ID,
			Object:  "chat.completion.chunk",
			Model:   state.Model,
			Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{}, FinishReason: &finish}},
		}
		data, _ := json.Marshal(outChunk)
		return engine.SSEChunk{EventType: "", Data: data}, nil
	}

	return engine.SSEChunk{}, nil
}

// oai2geminiStreamState tracks state across SSE chunks for a single stream.
type oai2geminiStreamState struct {
	ID       string
	Model    string
	roleSent bool
}

// Interface compliance.
var (
	_ engine.RequestTransformer         = (*OpenAI2Gemini)(nil)
	_ engine.ResponseTransformer        = (*OpenAI2Gemini)(nil)
	_ engine.StreamResponseTransformer  = (*OpenAI2Gemini)(nil)
)