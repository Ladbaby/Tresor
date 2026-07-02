package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"tresor/internal/engine"
)

// Anthropic2Gemini converts Anthropic Messages requests to Google Gemini
// generateContent requests and converts the Gemini response (JSON or SSE)
// back to Anthropic Messages format.
type Anthropic2Gemini struct{}

// PluginName returns the stable type name for deduplication.
func (t *Anthropic2Gemini) PluginName() string { return "Anthropic2Gemini" }

// anthropicBlockForGemini is a subset of an Anthropic content block used
// when translating to Gemini. We read it as raw maps for flexibility
// (Anthropic's blocks include image, tool_use, tool_result, document,
// text, thinking, etc., and the engine rarely has all of them).
type anthropicBlockForGemini struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// anthropicMessageForGemini is the input message structure (role + content).
type anthropicMessageForGemini struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicToolForGemini is one entry of Anthropic's `tools` array.
type anthropicToolForGemini struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

// anthropicRequestForGemini is the subset of the Anthropic Messages request
// that the Anthropic2Gemini transformer consumes.
type anthropicRequestForGemini struct {
	Model         string                       `json:"model"`
	MaxTokens      int                          `json:"max_tokens"`
	Messages       []anthropicMessageForGemini  `json:"messages"`
	System         *flexibleContent             `json:"system,omitempty"`
	Temperature    float64                      `json:"temperature,omitempty"`
	TopP           float64                      `json:"top_p,omitempty"`
	TopK           int                          `json:"top_k,omitempty"`
	Stream         bool                         `json:"stream,omitempty"`
	StopSequences  []string                     `json:"stop_sequences,omitempty"`
	Tools          []anthropicToolForGemini     `json:"tools,omitempty"`
	ToolChoice     json.RawMessage              `json:"tool_choice,omitempty"`
	Metadata       json.RawMessage              `json:"metadata,omitempty"`
	Thinking       json.RawMessage              `json:"thinking,omitempty"`
}

// TransformRequest converts an Anthropic Messages request into a Gemini
// generateContent request.
func (t *Anthropic2Gemini) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var anth anthropicRequestForGemini
	if err := json.Unmarshal(body, &anth); err != nil {
		return nil, nil, fmt.Errorf("anthropic2gemini: failed to parse request: %w", err)
	}

	geminiModel := anth.Model
	if geminiModel == "" && ctx.TargetDownstream != nil {
		geminiModel = ctx.TargetDownstream.ID
	}

	geminiBody := map[string]interface{}{}

	// --- system → systemInstruction ---
	if anth.System != nil && anth.System.Text != "" {
		geminiBody["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": normalizeAnthropicBillingHeader(anth.System.Text)},
			},
		}
	}

	// --- messages → contents ---
	var contents []map[string]interface{}
	for _, msg := range anth.Messages {
		role := msg.Role
		if role != "user" && role != "assistant" {
			// Gemini accepts only user/model roles in contents[]. Map "system"
			// (already pulled into systemInstruction above) and any unknown
			// role to "user" so we don't silently drop the message.
			role = "user"
		}

		// Try plain string content first.
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			contents = append(contents, map[string]interface{}{
				"role":  role,
				"parts": []map[string]interface{}{{"text": s}},
			})
			continue
		}

		// Try array of content blocks.
		var blocks []anthropicBlockForGemini
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			// Unknown shape — pass through as text.
			contents = append(contents, map[string]interface{}{
				"role":  role,
				"parts": []map[string]interface{}{{"text": string(msg.Content)}},
			})
			continue
		}

		if role == "assistant" {
			parts := anthropicBlocksToGeminiModelParts(blocks)
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]interface{}{
				"role":  "model",
				"parts": parts,
			})
			continue
		}

		// User role: walk blocks; tool_result blocks become functionResponse parts.
		var userParts []map[string]interface{}
		var plainTextParts []map[string]interface{}
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				toolName := b.ToolUseID
				var response map[string]interface{}
				// Content can be string or array of blocks.
				var innerStr string
				if json.Unmarshal(b.Content, &innerStr) == nil {
					if err := json.Unmarshal([]byte(innerStr), &response); err != nil {
						response = map[string]interface{}{"output": innerStr}
					}
				} else {
					// Array of blocks → join text content.
					var innerBlocks []anthropicBlockForGemini
					if err := json.Unmarshal(b.Content, &innerBlocks); err == nil {
						var joined string
						for _, ib := range innerBlocks {
							if ib.Type == "text" {
								if joined != "" {
									joined += "\n"
								}
								joined += ib.Text
							}
						}
						response = map[string]interface{}{"output": joined}
					} else {
						response = map[string]interface{}{}
					}
				}
				userParts = append(userParts, map[string]interface{}{
					"functionResponse": map[string]interface{}{
						"name":     toolName,
						"response": response,
					},
				})
			case "text":
				if b.Text != "" {
					plainTextParts = append(plainTextParts, map[string]interface{}{"text": b.Text})
				}
			case "image":
				if block, ok := anthropicImageToGeminiInline(b.Source); ok {
					userParts = append(userParts, block)
				}
			default:
				// Unknown block type — skip.
			}
		}
		// Gemini expects user/model roles to carry parts inline; tool_result
		// must be the only part in a user message in some clients. Concatenate
		// plain text + tool result parts into a single user entry; tool result
		// parts win over plain text if both are present (typical pattern).
		if len(userParts) > 0 {
			contents = append(contents, map[string]interface{}{
				"role":  "user",
				"parts": userParts,
			})
		}
		if len(plainTextParts) > 0 {
			contents = append(contents, map[string]interface{}{
				"role":  "user",
				"parts": plainTextParts,
			})
		}
	}

	if len(contents) == 0 {
		contents = append(contents, map[string]interface{}{
			"role":  "user",
			"parts": []map[string]interface{}{{"text": "Hello"}},
		})
	}
	geminiBody["contents"] = contents

	// --- tools → functionDeclarations ---
	if len(anth.Tools) > 0 {
		decls := make([]map[string]interface{}, 0, len(anth.Tools))
		for _, at := range anth.Tools {
			if at.Name == "" {
				continue
			}
			decl := map[string]interface{}{"name": at.Name}
			if at.Description != "" {
				decl["description"] = at.Description
			}
			if at.InputSchema != nil {
				decl["parameters"] = at.InputSchema
			}
			decls = append(decls, decl)
		}
		if len(decls) > 0 {
			geminiBody["tools"] = []map[string]interface{}{
				{"functionDeclarations": decls},
			}
		}
	}

	// --- tool_choice → toolConfig ---
	if len(anth.ToolChoice) > 0 {
		var tc struct {
			Type string `json:"type"`
			Name string `json:"name,omitempty"`
		}
		if err := json.Unmarshal(anth.ToolChoice, &tc); err == nil {
			mode := "AUTO"
			switch tc.Type {
			case "auto":
				mode = "AUTO"
			case "any":
				mode = "ANY"
			case "tool":
				mode = "ANY"
			case "none":
				mode = "NONE"
			}
			tcCfg := map[string]interface{}{"mode": mode}
			if tc.Type == "tool" && tc.Name != "" {
				tcCfg["allowedFunctionNames"] = []string{tc.Name}
			}
			geminiBody["toolConfig"] = map[string]interface{}{
				"functionCallingConfig": tcCfg,
			}
		}
	}

	// --- generationConfig ---
	genCfg := map[string]interface{}{}
	if anth.MaxTokens > 0 {
		genCfg["maxOutputTokens"] = anth.MaxTokens
	}
	if anth.Temperature > 0 {
		genCfg["temperature"] = anth.Temperature
	}
	if anth.TopP > 0 {
		genCfg["topP"] = anth.TopP
	}
	if anth.TopK > 0 {
		genCfg["topK"] = anth.TopK
	}
	if len(anth.StopSequences) > 0 {
		genCfg["stopSequences"] = anth.StopSequences
	}
	if len(anth.Thinking) > 0 {
		var thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		}
		if err := json.Unmarshal(anth.Thinking, &thinking); err == nil && thinking.Type == "enabled" {
			budget := thinking.BudgetTokens
			if budget <= 0 {
				budget = 10000
			}
			genCfg["thinkingConfig"] = map[string]interface{}{"thinkingBudget": budget}
		}
	}
	if len(genCfg) > 0 {
		geminiBody["generationConfig"] = genCfg
	}

	newBody, err := json.Marshal(geminiBody)
	if err != nil {
		return nil, nil, fmt.Errorf("anthropic2gemini: failed to marshal request: %w", err)
	}

	action := "generateContent"
	if anth.Stream {
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
	if anth.Stream {
		newReq.URL.RawQuery = "alt=sse"
	}

	if ctx.TargetDownstream != nil && ctx.TargetDownstream.APIKey != "" {
		if newReq.Header.Get("x-goog-api-key") == "" {
			newReq.Header.Set("x-goog-api-key", ctx.TargetDownstream.APIKey)
		}
	}

	return newReq, newBody, nil
}

// anthropicBlocksToGeminiModelParts converts Anthropic assistant content
// blocks (text, thinking, tool_use, image) into Gemini model-role parts.
func anthropicBlocksToGeminiModelParts(blocks []anthropicBlockForGemini) []map[string]interface{} {
	parts := []map[string]interface{}{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, map[string]interface{}{"text": b.Text})
			}
		case "thinking":
			if b.Thinking != "" {
				parts = append(parts, map[string]interface{}{
					"text":    b.Thinking,
					"thought": true,
				})
			}
		case "tool_use":
			var input interface{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			parts = append(parts, map[string]interface{}{
				"functionCall": map[string]interface{}{
					"name": b.Name,
					"args": input,
				},
			})
		case "image":
			if block, ok := anthropicImageToGeminiInline(b.Source); ok {
				parts = append(parts, block)
			}
		}
	}
	return parts
}

// anthropicImageToGeminiInline converts an Anthropic image source block
// into a Gemini inlineData part. Source shape:
//   { "type": "base64", "media_type": "image/png", "data": "..." }
// or { "type": "url", "url": "https://..." } (best-effort fetch).
func anthropicImageToGeminiInline(source json.RawMessage) (map[string]interface{}, bool) {
	if len(source) == 0 {
		return nil, false
	}
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(source, &src); err != nil {
		return nil, false
	}
	if src.Type == "base64" && src.Data != "" {
		return map[string]interface{}{
			"inlineData": map[string]interface{}{
				"mimeType": src.MediaType,
				"data":     src.Data,
			},
		}, true
	}
	if src.URL != "" {
		resp, err := http.Get(src.URL)
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
	return nil, false
}

// TransformResponse converts a Gemini GenerateContentResponse into an
// Anthropic Messages response. For streaming responses (text/event-stream)
// it converts the collected SSE body into Anthropic streaming chunks.
func (t *Anthropic2Gemini) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "text/event-stream" {
		return t.transformStreamingResponse(body)
	}
	return t.transformJSONResponse(body)
}

func (t *Anthropic2Gemini) transformJSONResponse(body []byte) ([]byte, error) {
	var geminiResp geminiGenerateContentResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return body, nil
	}

	anthResp := map[string]interface{}{
		"id":         "msg-" + geminiResp.ModelVersion,
		"type":       "message",
		"role":       "assistant",
		"model":      geminiResp.ModelVersion,
		"stop_reason": mapGeminiFinishReasonToAnthropic("", "", ""),
		"content":    []map[string]interface{}{},
	}

	if len(geminiResp.Candidates) > 0 {
		candidate := geminiResp.Candidates[0]
		var contentBlocks []map[string]interface{}
		var toolCalls []map[string]interface{}
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				blockType := "text"
				if part.Thought {
					blockType = "thinking"
				}
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type": blockType,
					"text": part.Text,
				})
				continue
			}
			if part.FunctionCall != nil {
				toolCalls = append(toolCalls, map[string]interface{}{
					"type": "tool_use",
					"id":   "toolu_" + part.FunctionCall.Name,
					"name": part.FunctionCall.Name,
					"input": func() interface{} {
						if part.FunctionCall.Args != nil {
							return part.FunctionCall.Args
						}
						return map[string]interface{}{}
					}(),
				})
			}
		}
		anthResp["content"] = contentBlocks
		// Anthropic expects tool_use blocks inside content[], so concatenate.
		anthResp["content"] = append(contentBlocks, toolCalls...)
		anthResp["stop_reason"] = mapGeminiFinishReasonToAnthropic(candidate.FinishReason, "", "")
	}

	if geminiResp.UsageMetadata != nil {
		anthResp["usage"] = map[string]interface{}{
			"input_tokens":  geminiResp.UsageMetadata.PromptTokenCount,
			"output_tokens": geminiResp.UsageMetadata.CandidatesTokenCount,
		}
	}

	return json.Marshal(anthResp)
}

func (t *Anthropic2Gemini) transformStreamingResponse(body []byte) ([]byte, error) {
	var out bytes.Buffer
	var id, model string

	emit := func(eventType string, data interface{}) {
		writeAnthropicSSE(&out, eventType, data)
	}

	parseGeminiSSE(body, func(data []byte) bool {
		var chunk geminiGenerateContentResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			return true
		}
		if id == "" && chunk.ModelVersion != "" {
			id = "msg-" + chunk.ModelVersion
			model = chunk.ModelVersion
		}

		// Emit message_start once.
		if out.Len() == 0 {
			startData := map[string]interface{}{
				"type": "message_start",
				"message": map[string]interface{}{
					"id":      id,
					"type":    "message",
					"role":    "assistant",
					"model":   model,
					"content": []map[string]interface{}{},
					"stop_reason": nil,
					"usage": map[string]interface{}{
						"input_tokens":  0,
						"output_tokens": 0,
					},
				},
			}
			emit("message_start", startData)
			emit("ping", map[string]interface{}{"type": "ping"})
		}

		if len(chunk.Candidates) == 0 {
			return true
		}
		candidate := chunk.Candidates[0]

		for i, part := range candidate.Content.Parts {
			if part.Text != "" {
				if part.Thought {
					emit("content_block_start", map[string]interface{}{
						"index": i,
						"content_block": map[string]interface{}{
							"type": "thinking",
							"thinking": part.Text,
						},
					})
				} else {
					emit("content_block_start", map[string]interface{}{
						"index": i,
						"content_block": map[string]interface{}{
							"type": "text",
							"text": "",
						},
					})
					emit("content_block_delta", map[string]interface{}{
						"index": i,
						"delta": map[string]interface{}{
							"type": "text_delta",
							"text": part.Text,
						},
					})
				}
			}
			if part.FunctionCall != nil {
				emit("content_block_start", map[string]interface{}{
					"index": i,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    "toolu_" + part.FunctionCall.Name,
						"name":  part.FunctionCall.Name,
						"input": map[string]interface{}{},
					},
				})
				args := "{}"
				if part.FunctionCall.Args != nil {
					b, err := json.Marshal(part.FunctionCall.Args)
					if err == nil {
						args = string(b)
					}
				}
				emit("content_block_delta", map[string]interface{}{
					"index": i,
					"delta": map[string]interface{}{
						"type":        "input_json_delta",
						"partial_json": args,
					},
				})
			}
		}

		if candidate.FinishReason != "" {
			emit("message_delta", map[string]interface{}{
				"delta": map[string]interface{}{
					"stop_reason":   mapGeminiFinishReasonToAnthropic(candidate.FinishReason, "", ""),
					"stop_sequence": nil,
				},
				"usage": map[string]interface{}{
					"output_tokens": chunk.UsageMetadata.CandidatesTokenCount,
				},
			})
			emit("message_stop", map[string]interface{}{"type": "message_stop"})
		}
		return true
	})

	return out.Bytes(), nil
}

// mapGeminiFinishReasonToAnthropic translates a Gemini finishReason to
// Anthropic's stop_reason vocabulary.
func mapGeminiFinishReasonToAnthropic(r, _, _ string) interface{} {
	switch r {
	case "STOP", "":
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "refusal"
	default:
		if r == "" {
			return "end_turn"
		}
		return "end_turn"
	}
}

// TransformStreamChunk converts a single Gemini SSE event into an Anthropic
// SSE chunk.
func (t *Anthropic2Gemini) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	state := &anth2geminiStreamState{}
	if existing, ok := ctx.Variables["anth2gem_stream"]; ok {
		state = existing.(*anth2geminiStreamState)
	}
	defer func() { ctx.Variables["anth2gem_stream"] = state }()

	var resp geminiGenerateContentResponse
	if err := json.Unmarshal(chunk.Data, &resp); err != nil {
		return chunk, nil
	}

	if state.ID == "" && resp.ModelVersion != "" {
		state.ID = "msg-" + resp.ModelVersion
		state.Model = resp.ModelVersion
	}

	// First chunk: emit message_start + ping.
	if !state.started {
		state.started = true
		startData := map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":      state.ID,
				"type":    "message",
				"role":    "assistant",
				"model":   state.Model,
				"content": []map[string]interface{}{},
				"stop_reason": nil,
				"usage": map[string]interface{}{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		}
		b, _ := json.Marshal(startData)
		return engine.SSEChunk{EventType: "message_start", Data: b}, nil
	}

	if len(resp.Candidates) == 0 {
		return engine.SSEChunk{}, nil
	}
	candidate := resp.Candidates[0]

	// Walk parts in order — we translate each into a separate Anthropic event.
	// For simplicity, we emit only the first text-bearing part per chunk
	// (chunk-level granularity is coarse; multiple parts in one Gemini chunk
	// is uncommon).
	for i, part := range candidate.Content.Parts {
		if part.Text != "" {
			if part.Thought {
				startB, _ := json.Marshal(map[string]interface{}{
					"index": i,
					"content_block": map[string]interface{}{
						"type":     "thinking",
						"thinking": part.Text,
					},
				})
				return engine.SSEChunk{EventType: "content_block_start", Data: startB}, nil
			}
			if !state.textBlockStarted {
				state.textBlockStarted = true
				startB, _ := json.Marshal(map[string]interface{}{
					"index": i,
					"content_block": map[string]interface{}{
						"type": "text",
						"text": "",
					},
				})
				return engine.SSEChunk{EventType: "content_block_start", Data: startB}, nil
			}
			deltaB, _ := json.Marshal(map[string]interface{}{
				"index": i,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": part.Text,
				},
			})
			return engine.SSEChunk{EventType: "content_block_delta", Data: deltaB}, nil
		}
		if part.FunctionCall != nil {
			args := "{}"
			if part.FunctionCall.Args != nil {
				b, err := json.Marshal(part.FunctionCall.Args)
				if err == nil {
					args = string(b)
				}
			}
			startB, _ := json.Marshal(map[string]interface{}{
				"index": i,
				"content_block": map[string]interface{}{
					"type":        "tool_use",
					"id":          "toolu_" + part.FunctionCall.Name,
					"name":        part.FunctionCall.Name,
					"input":       map[string]interface{}{},
				},
			})
			_ = startB
			deltaB, _ := json.Marshal(map[string]interface{}{
				"index": i,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": args,
				},
			})
			return engine.SSEChunk{EventType: "content_block_delta", Data: deltaB}, nil
		}
	}

	if candidate.FinishReason != "" {
		stopB, _ := json.Marshal(map[string]interface{}{
			"delta": map[string]interface{}{
				"stop_reason":   mapGeminiFinishReasonToAnthropic(candidate.FinishReason, "", ""),
				"stop_sequence": nil,
			},
			"usage": map[string]interface{}{
				"output_tokens": resp.UsageMetadata.CandidatesTokenCount,
			},
		})
		return engine.SSEChunk{EventType: "message_delta", Data: stopB}, nil
	}

	return engine.SSEChunk{}, nil
}

// anth2geminiStreamState tracks state across SSE chunks for a single stream.
type anth2geminiStreamState struct {
	ID                string
	Model             string
	started           bool
	textBlockStarted  bool
}

// Interface compliance.
var (
	_ engine.RequestTransformer        = (*Anthropic2Gemini)(nil)
	_ engine.ResponseTransformer       = (*Anthropic2Gemini)(nil)
	_ engine.StreamResponseTransformer = (*Anthropic2Gemini)(nil)
)