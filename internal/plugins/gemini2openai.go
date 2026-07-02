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

// geminiModelFromPath extracts the model segment from a Gemini path.
// Examples:
//   /v1beta/models                       → ""
//   /v1beta/models/gemini-2.5-pro        → "gemini-2.5-pro"
//   /v1beta/models/gemini-2.5-pro:generateContent → "gemini-2.5-pro"
// Returns "" for non-Gemini paths. Duplicated from internal/engine so
// plugins can use it without an import cycle.
func geminiModelFromPath(path string) string {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return ""
	}
	if i := strings.Index(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return ""
	}
	return rest
}

// Gemini2OpenAI converts Google Gemini generateContent requests to OpenAI
// Chat Completion requests and converts the OpenAI response (JSON or SSE)
// back to Gemini format.
//
// Auth: OpenAI uses Bearer Authorization. The model moves from the URL path
// to the body's `model` field; the URL is rewritten to /v1/chat/completions.
type Gemini2OpenAI struct{}

// PluginName returns the stable type name for deduplication.
func (t *Gemini2OpenAI) PluginName() string { return "Gemini2OpenAI" }

// geminiRequestForTranslate is the subset of a Gemini request that this
// transformer reads.
type geminiRequestForTranslate struct {
	Contents          []geminiContent          `json:"contents"`
	SystemInstruction *geminiContent           `json:"systemInstruction,omitempty"`
	Tools             []map[string]interface{} `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig        `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenerationConfig  `json:"generationConfig,omitempty"`
}

// geminiToolConfig is the Gemini toolConfig object.
type geminiToolConfig struct {
	FunctionCallingConfig *geminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

// geminiFunctionCallingConfig is the inner functionCallingConfig object.
type geminiFunctionCallingConfig struct {
	Mode                string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// geminiGenerationConfig is the Gemini generationConfig object.
type geminiGenerationConfig struct {
	Temperature      float64  `json:"temperature,omitempty"`
	TopP             float64  `json:"topP,omitempty"`
	TopK             int      `json:"topK,omitempty"`
	MaxOutputTokens  int      `json:"maxOutputTokens,omitempty"`
	StopSequences    []string `json:"stopSequences,omitempty"`
	ResponseMimeType string   `json:"responseMimeType,omitempty"`
	ResponseSchema   interface{} `json:"responseSchema,omitempty"`
}

// TransformRequest converts a Gemini generateContent request into an OpenAI
// Chat Completion request. The URL path is rewritten to /v1/chat/completions
// and the model is moved from the URL into the body's "model" field.
func (t *Gemini2OpenAI) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var gem geminiRequestForTranslate
	if err := json.Unmarshal(body, &gem); err != nil {
		return nil, nil, fmt.Errorf("gemini2openai: failed to parse request: %w", err)
	}

	// Extract the model name from the URL path (the engine placed it there).
	model := geminiModelFromPath(req.URL.Path)
	if model == "" {
		// Best-effort fallback: read ctx.TargetDownstream.ID as a placeholder.
		if ctx.TargetDownstream != nil {
			model = ctx.TargetDownstream.ID
		}
	}

	oaiBody := map[string]interface{}{
		"model": model,
	}

	stream := false
	if gem.GenerationConfig != nil && gem.GenerationConfig.MaxOutputTokens > 0 {
		oaiBody["max_tokens"] = gem.GenerationConfig.MaxOutputTokens
	}
	if gem.GenerationConfig != nil && gem.GenerationConfig.Temperature > 0 {
		oaiBody["temperature"] = gem.GenerationConfig.Temperature
	}
	if gem.GenerationConfig != nil && gem.GenerationConfig.TopP > 0 {
		oaiBody["top_p"] = gem.GenerationConfig.TopP
	}
	if gem.GenerationConfig != nil && len(gem.GenerationConfig.StopSequences) > 0 {
		oaiBody["stop"] = gem.GenerationConfig.StopSequences
	}
	if gem.GenerationConfig != nil && gem.GenerationConfig.ResponseMimeType == "application/json" {
		oaiBody["response_format"] = map[string]interface{}{"type": "json_object"}
		if gem.GenerationConfig.ResponseSchema != nil {
			oaiBody["response_format"] = map[string]interface{}{
				"type": "json_schema",
				"json_schema": map[string]interface{}{
					"name":   "response",
					"schema": gem.GenerationConfig.ResponseSchema,
				},
			}
		}
	}

	// --- messages ---
	var messages []map[string]interface{}

	// System instruction → system message.
	if gem.SystemInstruction != nil {
		var sysText string
		for _, p := range gem.SystemInstruction.Parts {
			if p.Text != "" {
				if sysText != "" {
					sysText += "\n"
				}
				sysText += p.Text
			}
		}
		if sysText != "" {
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": sysText,
			})
		}
	}

	for _, c := range gem.Contents {
		role := c.Role
		if role != "user" && role != "model" {
			role = "user"
		}
		oaiRole := "user"
		if role == "model" {
			oaiRole = "assistant"
		}

		// Collect text parts and tool-related parts separately.
		var textParts []string
		var toolCalls []openAIChatToolCall
		var toolMessage *map[string]interface{}

		for _, p := range c.Parts {
			if p.Text != "" {
				textParts = append(textParts, p.Text)
				continue
			}
			if p.FunctionCall != nil {
				args := "{}"
				if p.FunctionCall.Args != nil {
					b, err := json.Marshal(p.FunctionCall.Args)
					if err == nil {
						args = string(b)
					}
				}
				tc := openAIChatToolCall{
					ID:   fmt.Sprintf("call_%s", p.FunctionCall.Name),
					Type: "function",
				}
				tc.Function.Name = p.FunctionCall.Name
				tc.Function.Arguments = args
				toolCalls = append(toolCalls, tc)
				continue
			}
			// functionResponse parts in user role → tool message.
			// These were written as raw maps in the upstream OpenAI2Gemini
			// (and Anthropic2Gemini) transformers. We look them up via a
			// raw re-parse of the original body to preserve the field.
		}

		// Re-parse the raw body to detect functionResponse parts (the typed
		// geminiPart struct doesn't expose that field).
		var rawContents []map[string]interface{}
		_ = json.Unmarshal(body, &struct {
			Contents *[]map[string]interface{} `json:"contents"`
		}{Contents: &rawContents})

		// Walk this content's index.
		for i, rc := range rawContents {
			if i >= len(gem.Contents) {
				break
			}
			if rc["role"] != c.Role {
				continue
			}
			rawParts, _ := rc["parts"].([]interface{})
			// Find which content index this is; if not match, skip.
			if i != indexOfContent(gem.Contents, c) {
				continue
			}
			for _, rp := range rawParts {
				rpm, _ := rp.(map[string]interface{})
				fr, _ := rpm["functionResponse"].(map[string]interface{})
				if fr == nil {
					continue
				}
				name, _ := fr["name"].(string)
				response, _ := fr["response"].(map[string]interface{})
				content := ""
				if response != nil {
					if output, ok := response["output"].(string); ok {
						content = output
					} else {
						b, err := json.Marshal(response)
						if err == nil {
							content = string(b)
						}
					}
				}
				if content == "" {
					content = "{}"
				}
				tm := map[string]interface{}{
					"role":         "tool",
					"tool_call_id": name,
					"content":      content,
				}
				toolMessage = &tm
			}
			break
		}

		if toolMessage != nil {
			messages = append(messages, *toolMessage)
			continue
		}

		if oaiRole == "assistant" && len(toolCalls) > 0 {
			msg := map[string]interface{}{
				"role": "assistant",
			}
			if len(textParts) > 0 {
				msg["content"] = joinStrings(textParts, "\n")
			}
			msg["tool_calls"] = toolCallsToMaps(toolCalls)
			messages = append(messages, msg)
			continue
		}

		if oaiRole == "assistant" {
			messages = append(messages, map[string]interface{}{
				"role":    "assistant",
				"content": joinStrings(textParts, "\n"),
			})
			continue
		}

		// user role
		if len(textParts) == 0 {
			continue
		}
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": joinStrings(textParts, "\n"),
		})
	}

	if len(messages) == 0 {
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": "Hello",
		})
	}
	oaiBody["messages"] = messages
	_ = stream

	// --- tools ---
	if len(gem.Tools) > 0 {
		decls := []map[string]interface{}{}
		for _, gt := range gem.Tools {
			fd, _ := gt["functionDeclarations"].([]interface{})
			for _, d := range fd {
				dm, _ := d.(map[string]interface{})
				if dm == nil {
					continue
				}
				name, _ := dm["name"].(string)
				if name == "" {
					continue
				}
				tool := map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name": name,
					},
				}
				fn := tool["function"].(map[string]interface{})
				if desc, _ := dm["description"].(string); desc != "" {
					fn["description"] = desc
				}
				if params, ok := dm["parameters"]; ok {
					fn["parameters"] = params
				}
				decls = append(decls, tool)
			}
		}
		if len(decls) > 0 {
			oaiBody["tools"] = decls
		}
	}

	// --- tool_choice ---
	if gem.ToolConfig != nil && gem.ToolConfig.FunctionCallingConfig != nil {
		mode := gem.ToolConfig.FunctionCallingConfig.Mode
		switch mode {
		case "AUTO", "":
			oaiBody["tool_choice"] = "auto"
		case "NONE":
			oaiBody["tool_choice"] = "none"
		case "ANY":
			if len(gem.ToolConfig.FunctionCallingConfig.AllowedFunctionNames) == 1 {
				oaiBody["tool_choice"] = map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name": gem.ToolConfig.FunctionCallingConfig.AllowedFunctionNames[0],
					},
				}
			} else {
				oaiBody["tool_choice"] = "required"
			}
		}
	}

	// Detect stream from the URL path (engine put it there via openai2gemini):
	// :streamGenerateContent means stream=true.
	if isStreamRequest(req.URL.Path) {
		oaiBody["stream"] = true
	}

	newBody, err := json.Marshal(oaiBody)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini2openai: failed to marshal request: %w", err)
	}

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	newReq.URL.Path = "/v1/chat/completions"
	newReq.URL.RawQuery = ""

	if ctx.TargetDownstream != nil && ctx.TargetDownstream.APIKey != "" {
		if newReq.Header.Get("Authorization") == "" {
			newReq.Header.Set("Authorization", "Bearer "+ctx.TargetDownstream.APIKey)
		}
	}

	return newReq, newBody, nil
}

func indexOfContent(contents []geminiContent, target geminiContent) int {
	for i, c := range contents {
		// Compare by role + parts length (best-effort identity check)
		if c.Role == target.Role && len(c.Parts) == len(target.Parts) {
			return i
		}
	}
	return -1
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func toolCallsToMaps(tcs []openAIChatToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(tcs))
	for _, tc := range tcs {
		out = append(out, map[string]interface{}{
			"id":       tc.ID,
			"type":     tc.Type,
			"function": map[string]interface{}{
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		})
	}
	return out
}

func isStreamRequest(path string) bool {
	return bytes.Contains([]byte(path), []byte(":streamGenerateContent"))
}

// TransformResponse converts an OpenAI Chat Completion response into a Gemini
// generateContentResponse. For SSE responses it converts the collected body.
func (t *Gemini2OpenAI) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "text/event-stream" {
		return t.transformStreamingResponse(body)
	}
	return t.transformJSONResponse(body)
}

func (t *Gemini2OpenAI) transformJSONResponse(body []byte) ([]byte, error) {
	var oaiResp openAIChatResponse
	if err := json.Unmarshal(body, &oaiResp); err != nil {
		return body, nil
	}

	geminiResp := geminiGenerateContentResponse{
		ModelVersion: oaiResp.Model,
	}

	if len(oaiResp.Choices) > 0 {
		choice := oaiResp.Choices[0]
		parts := []geminiPart{}
		if choice.Message.Content.Text != "" {
			parts = append(parts, geminiPart{Text: choice.Message.Content.Text})
		}
		if choice.Message.ReasoningContent != "" {
			parts = append(parts, geminiPart{
				Text:    choice.Message.ReasoningContent,
				Thought: true,
			})
		}
		for _, tc := range choice.Message.ToolCalls {
			parts = append(parts, geminiPart{
				FunctionCall: &geminiFunctionCall{
					Name: tc.Function.Name,
					Args: func() map[string]interface{} {
						var args map[string]interface{}
						if tc.Function.Arguments != "" {
							_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
						}
						if args == nil {
							args = map[string]interface{}{}
						}
						return args
					}(),
				},
			})
		}
		geminiResp.Candidates = []geminiCandidate{{
			Content: geminiContent{
				Role:  "model",
				Parts: parts,
			},
			FinishReason: mapOpenAIFinishReasonToGemini(choice.FinishReason),
			Index:        0,
		}}
	}

	if oaiResp.Usage.TotalTokens > 0 {
		geminiResp.UsageMetadata = &geminiUsageMetadata{
			PromptTokenCount:     oaiResp.Usage.PromptTokens,
			CandidatesTokenCount: oaiResp.Usage.CompletionTokens,
			TotalTokenCount:      oaiResp.Usage.TotalTokens,
		}
	}

	return json.Marshal(geminiResp)
}

func (t *Gemini2OpenAI) transformStreamingResponse(body []byte) ([]byte, error) {
	var out bytes.Buffer

	parseOpenAISSE(body, func(data []byte) bool {
		var chunk openAIChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return true
		}
		if len(chunk.Choices) == 0 {
			return true
		}
		choice := chunk.Choices[0]
		parts := []geminiPart{}
		if choice.Delta.Content != "" {
			parts = append(parts, geminiPart{Text: choice.Delta.Content})
		}
		if choice.Delta.ReasoningContent != "" {
			parts = append(parts, geminiPart{
				Text:    choice.Delta.ReasoningContent,
				Thought: true,
			})
		}
		for _, tc := range choice.Delta.ToolCalls {
			args := map[string]interface{}{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
			parts = append(parts, geminiPart{
				FunctionCall: &geminiFunctionCall{
					Name: tc.Function.Name,
					Args: args,
				},
			})
		}

		resp := geminiGenerateContentResponse{
			ModelVersion: chunk.Model,
			Candidates: []geminiCandidate{{
				Content: geminiContent{
					Role:  "model",
					Parts: parts,
				},
				Index: 0,
			}},
		}
		if choice.FinishReason != nil {
			resp.Candidates[0].FinishReason = mapOpenAIFinishReasonToGemini(*choice.FinishReason)
		}
		writeSSEData(&out, resp)
		return true
	})

	return out.Bytes(), nil
}

// mapOpenAIFinishReasonToGemini translates OpenAI's finish_reason to Gemini's.
func mapOpenAIFinishReasonToGemini(r string) string {
	switch r {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "tool_calls":
		return "STOP"
	default:
		return "STOP"
	}
}

// TransformStreamChunk converts a single OpenAI SSE chunk into a Gemini
// SSE chunk (one JSON object per data: line).
func (t *Gemini2OpenAI) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	// The OpenAI upstream may send a [DONE] terminator (and the engine may
	// also emit a synthetic [DONE] if the upstream didn't). The Gemini
	// protocol does NOT use [DONE] — it ends the stream with a final chunk
	// carrying finishReason, which we already emit for the upstream's last
	// meaningful chunk. Drop [DONE] silently so it doesn't leak to Gemini
	// clients (who would treat it as a malformed data: payload).
	if bytes.Equal(bytes.TrimSpace(chunk.Data), []byte("[DONE]")) {
		return engine.SSEChunk{}, nil
	}
	var oaiChunk openAIChunk
	if err := json.Unmarshal(chunk.Data, &oaiChunk); err != nil {
		return chunk, nil
	}
	if len(oaiChunk.Choices) == 0 {
		return engine.SSEChunk{}, nil
	}
	choice := oaiChunk.Choices[0]

	parts := []geminiPart{}
	if choice.Delta.Content != "" {
		parts = append(parts, geminiPart{Text: choice.Delta.Content})
	}
	if choice.Delta.ReasoningContent != "" {
		parts = append(parts, geminiPart{
			Text:    choice.Delta.ReasoningContent,
			Thought: true,
		})
	}
	for _, tc := range choice.Delta.ToolCalls {
		args := map[string]interface{}{}
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
		parts = append(parts, geminiPart{
			FunctionCall: &geminiFunctionCall{
				Name: tc.Function.Name,
				Args: args,
			},
		})
	}

	resp := geminiGenerateContentResponse{
		ModelVersion: oaiChunk.Model,
		Candidates: []geminiCandidate{{
			Content: geminiContent{Role: "model", Parts: parts},
			Index:    0,
		}},
	}
	if choice.FinishReason != nil {
		resp.Candidates[0].FinishReason = mapOpenAIFinishReasonToGemini(*choice.FinishReason)
	}
	data, _ := json.Marshal(resp)
	return engine.SSEChunk{EventType: "", Data: data}, nil
}

// Interface compliance.
var (
	_ engine.RequestTransformer        = (*Gemini2OpenAI)(nil)
	_ engine.ResponseTransformer       = (*Gemini2OpenAI)(nil)
	_ engine.StreamResponseTransformer = (*Gemini2OpenAI)(nil)
)