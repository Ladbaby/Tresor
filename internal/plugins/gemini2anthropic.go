package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"tresor/internal/engine"
)

// Gemini2Anthropic converts Google Gemini generateContent requests to
// Anthropic Messages requests and converts the Anthropic response (JSON or
// SSE) back to Gemini format.
type Gemini2Anthropic struct{}

// PluginName returns the stable type name for deduplication.
func (t *Gemini2Anthropic) PluginName() string { return "Gemini2Anthropic" }

// TransformRequest converts a Gemini generateContent request into an
// Anthropic Messages request. The URL is rewritten to /v1/messages and the
// model is moved from the URL into the body's "model" field.
func (t *Gemini2Anthropic) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var gem geminiRequestForTranslate
	if err := json.Unmarshal(body, &gem); err != nil {
		return nil, nil, fmt.Errorf("gemini2anthropic: failed to parse request: %w", err)
	}

	model := geminiModelFromPath(req.URL.Path)
	if model == "" && ctx.TargetDownstream != nil {
		model = ctx.TargetDownstream.ID
	}

	maxTokens := 4096
	if gem.GenerationConfig != nil && gem.GenerationConfig.MaxOutputTokens > 0 {
		maxTokens = gem.GenerationConfig.MaxOutputTokens
	}

	anthBody := map[string]interface{}{
		"model":      model,
		"max_tokens": maxTokens,
	}

	if gem.GenerationConfig != nil && gem.GenerationConfig.Temperature > 0 {
		anthBody["temperature"] = gem.GenerationConfig.Temperature
	}
	if gem.GenerationConfig != nil && gem.GenerationConfig.TopP > 0 {
		anthBody["top_p"] = gem.GenerationConfig.TopP
	}
	if gem.GenerationConfig != nil && gem.GenerationConfig.TopK > 0 {
		anthBody["top_k"] = gem.GenerationConfig.TopK
	}
	if gem.GenerationConfig != nil && len(gem.GenerationConfig.StopSequences) > 0 {
		anthBody["stop_sequences"] = gem.GenerationConfig.StopSequences
	}

	// --- systemInstruction → system ---
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
			anthBody["system"] = sysText
		}
	}

	// --- contents → messages ---
	var messages []map[string]interface{}
	for _, c := range gem.Contents {
		role := c.Role
		if role != "user" && role != "model" {
			role = "user"
		}
		anthRole := "user"
		if role == "model" {
			anthRole = "assistant"
		}

		// Walk typed parts.
		var contentBlocks []map[string]interface{}

		// Re-parse raw contents to detect functionResponse (typed struct omits it).
		var rawContents []map[string]interface{}
		_ = json.Unmarshal(body, &struct {
			Contents *[]map[string]interface{} `json:"contents"`
		}{Contents: &rawContents})

		idx := -1
		for i, rc := range rawContents {
			if rc["role"] == c.Role && len(rc["parts"].([]interface{})) == len(c.Parts) {
				idx = i
				break
			}
		}

		if idx >= 0 && idx < len(rawContents) {
			rawParts, _ := rawContents[idx]["parts"].([]interface{})
			for _, rp := range rawParts {
				rpm, _ := rp.(map[string]interface{})
				fr, _ := rpm["functionResponse"].(map[string]interface{})
				if fr != nil {
					name, _ := fr["name"].(string)
					response, _ := fr["response"].(map[string]interface{})
					contentText := ""
					if response != nil {
						if output, ok := response["output"].(string); ok {
							contentText = output
						} else {
							b, err := json.Marshal(response)
							if err == nil {
								contentText = string(b)
							}
						}
					}
					contentBlocks = append(contentBlocks, map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": name,
						"content":     contentText,
					})
				}
			}
		}

		for _, p := range c.Parts {
			if p.Text != "" {
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type": "text",
					"text": p.Text,
				})
				continue
			}
			if p.FunctionCall != nil {
				var input interface{}
				if p.FunctionCall.Args != nil {
					b, err := json.Marshal(p.FunctionCall.Args)
					if err == nil {
						_ = json.Unmarshal(b, &input)
					}
				}
				if input == nil {
					input = map[string]interface{}{}
				}
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    "toolu_" + p.FunctionCall.Name,
					"name":  p.FunctionCall.Name,
					"input": input,
				})
				continue
			}
			if p.InlineData != nil {
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type": "image",
					"source": map[string]interface{}{
						"type":       "base64",
						"media_type": p.InlineData.MimeType,
						"data":       p.InlineData.Data,
					},
				})
			}
		}

		if len(contentBlocks) == 0 {
			continue
		}
		messages = append(messages, map[string]interface{}{
			"role":    anthRole,
			"content": contentBlocks,
		})
	}

	if len(messages) == 0 {
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": []map[string]interface{}{{"type": "text", "text": "Hello"}},
		})
	}
	anthBody["messages"] = messages

	// --- tools → tools[] ---
	if len(gem.Tools) > 0 {
		var anthTools []map[string]interface{}
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
					"name": name,
				}
				if desc, _ := dm["description"].(string); desc != "" {
					tool["description"] = desc
				}
				if params, ok := dm["parameters"]; ok {
					tool["input_schema"] = params
				}
				anthTools = append(anthTools, tool)
			}
		}
		if len(anthTools) > 0 {
			anthBody["tools"] = anthTools
		}
	}

	// --- toolConfig → tool_choice ---
	if gem.ToolConfig != nil && gem.ToolConfig.FunctionCallingConfig != nil {
		mode := gem.ToolConfig.FunctionCallingConfig.Mode
		switch mode {
		case "AUTO", "":
			anthBody["tool_choice"] = map[string]interface{}{"type": "auto"}
		case "NONE":
			anthBody["tool_choice"] = map[string]interface{}{"type": "none"}
		case "ANY":
			if len(gem.ToolConfig.FunctionCallingConfig.AllowedFunctionNames) == 1 {
				anthBody["tool_choice"] = map[string]interface{}{
					"type": "tool",
					"name": gem.ToolConfig.FunctionCallingConfig.AllowedFunctionNames[0],
				}
			} else {
				anthBody["tool_choice"] = map[string]interface{}{"type": "any"}
			}
		}
	}

	// Detect stream from the URL path (engine put it there via openai2gemini):
	// :streamGenerateContent means stream=true.
	if isStreamRequest(req.URL.Path) {
		anthBody["stream"] = true
	}

	newBody, err := json.Marshal(anthBody)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini2anthropic: failed to marshal request: %w", err)
	}

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	newReq.URL.Path = "/v1/messages"
	newReq.URL.RawQuery = ""

	if ctx.TargetDownstream != nil && ctx.TargetDownstream.APIKey != "" {
		if newReq.Header.Get("x-api-key") == "" {
			newReq.Header.Set("x-api-key", ctx.TargetDownstream.APIKey)
		}
		if newReq.Header.Get("anthropic-version") == "" {
			newReq.Header.Set("anthropic-version", "2023-06-01")
		}
	}

	return newReq, newBody, nil
}

// TransformResponse converts an Anthropic Messages response into a Gemini
// generateContentResponse. For SSE responses, converts the collected body.
func (t *Gemini2Anthropic) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "text/event-stream" {
		return t.transformStreamingResponse(body)
	}
	return t.transformJSONResponse(body)
}

func (t *Gemini2Anthropic) transformJSONResponse(body []byte) ([]byte, error) {
	var anthResp struct {
		ID         string                 `json:"id"`
		Model      string                 `json:"model"`
		StopReason string                 `json:"stop_reason"`
		Content    []map[string]interface{} `json:"content"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &anthResp); err != nil {
		return body, nil
	}

	geminiResp := geminiGenerateContentResponse{
		ModelVersion: anthResp.Model,
	}

	var parts []geminiPart
	for _, block := range anthResp.Content {
		btype, _ := block["type"].(string)
		switch btype {
		case "text":
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, geminiPart{Text: text})
			}
		case "thinking":
			if thinking, _ := block["thinking"].(string); thinking != "" {
				parts = append(parts, geminiPart{
					Text:    thinking,
					Thought: true,
				})
			}
		case "tool_use":
			name, _ := block["name"].(string)
			input, _ := block["input"].(map[string]interface{})
			if input == nil {
				input = map[string]interface{}{}
			}
			parts = append(parts, geminiPart{
				FunctionCall: &geminiFunctionCall{
					Name: name,
					Args: input,
				},
			})
		}
	}

	geminiResp.Candidates = []geminiCandidate{{
		Content:      geminiContent{Role: "model", Parts: parts},
		FinishReason: mapAnthropicFinishReasonToGemini(anthResp.StopReason),
		Index:        0,
	}}

	if anthResp.Usage.InputTokens > 0 || anthResp.Usage.OutputTokens > 0 {
		geminiResp.UsageMetadata = &geminiUsageMetadata{
			PromptTokenCount:     anthResp.Usage.InputTokens,
			CandidatesTokenCount: anthResp.Usage.OutputTokens,
			TotalTokenCount:      anthResp.Usage.InputTokens + anthResp.Usage.OutputTokens,
		}
	}

	return json.Marshal(geminiResp)
}

func (t *Gemini2Anthropic) transformStreamingResponse(body []byte) ([]byte, error) {
	var out bytes.Buffer
	var model string

	parseAnthropicSSE(body, func(eventType string, data []byte) bool {
		switch eventType {
		case "message_start":
			var msg struct {
				Message struct {
					Model string `json:"model"`
				} `json:"message"`
			}
			if err := json.Unmarshal(data, &msg); err == nil {
				model = msg.Message.Model
			}
		case "content_block_start":
			// Open as a new candidate (only the first block contributes text deltas).
		case "content_block_delta":
			var d struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(data, &d); err != nil {
				return true
			}
			if d.Delta.Type == "text_delta" && d.Delta.Text != "" {
				writeSSEData(&out, geminiGenerateContentResponse{
					ModelVersion: model,
					Candidates: []geminiCandidate{{
						Content: geminiContent{
							Role:  "model",
							Parts: []geminiPart{{Text: d.Delta.Text}},
						},
						Index: 0,
					}},
				})
			}
		case "message_delta":
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(data, &d); err == nil {
				if d.Delta.StopReason != "" {
					writeSSEData(&out, geminiGenerateContentResponse{
						ModelVersion: model,
						Candidates: []geminiCandidate{{
							Content:      geminiContent{Role: "model"},
							FinishReason: mapAnthropicFinishReasonToGemini(d.Delta.StopReason),
							Index:        0,
						}},
					})
				}
			}
		}
		return true
	})

	return out.Bytes(), nil
}

func mapAnthropicFinishReasonToGemini(r string) string {
	switch r {
	case "end_turn", "":
		return "STOP"
	case "max_tokens":
		return "MAX_TOKENS"
	case "refusal":
		return "SAFETY"
	case "stop_sequence", "tool_use":
		return "STOP"
	default:
		return "STOP"
	}
}

// TransformStreamChunk converts a single Anthropic SSE event into a Gemini
// SSE chunk (one JSON object per data: line).
func (t *Gemini2Anthropic) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	switch chunk.EventType {
	case "content_block_delta":
		var d struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &d); err != nil {
			return chunk, nil
		}
		if d.Delta.Type == "text_delta" && d.Delta.Text != "" {
			resp := geminiGenerateContentResponse{
				Candidates: []geminiCandidate{{
					Content: geminiContent{
						Role:  "model",
						Parts: []geminiPart{{Text: d.Delta.Text}},
					},
					Index: 0,
				}},
			}
			data, _ := json.Marshal(resp)
			return engine.SSEChunk{EventType: "", Data: data}, nil
		}
		return engine.SSEChunk{}, nil
	case "message_delta":
		var d struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &d); err == nil && d.Delta.StopReason != "" {
			resp := geminiGenerateContentResponse{
				Candidates: []geminiCandidate{{
					Content:      geminiContent{Role: "model"},
					FinishReason: mapAnthropicFinishReasonToGemini(d.Delta.StopReason),
					Index:        0,
				}},
			}
			data, _ := json.Marshal(resp)
			return engine.SSEChunk{EventType: "", Data: data}, nil
		}
		return engine.SSEChunk{}, nil
	default:
		// message_start / message_stop / content_block_start / ping are dropped.
		return engine.SSEChunk{}, nil
	}
}

// Interface compliance.
var (
	_ engine.RequestTransformer        = (*Gemini2Anthropic)(nil)
	_ engine.ResponseTransformer       = (*Gemini2Anthropic)(nil)
	_ engine.StreamResponseTransformer = (*Gemini2Anthropic)(nil)
)