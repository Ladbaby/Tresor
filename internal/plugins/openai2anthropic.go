package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"tresor/internal/engine"
)

// OpenAI2Anthropic converts OpenAI Chat Completion requests to Anthropic Messages format.
type OpenAI2Anthropic struct{}

type openAIChatMessage struct {
	Role       string               `json:"role"`
	Content    string               `json:"content"`
	ToolCalls  []openAIChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
}

type openAIChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	System      string              `json:"system,omitempty"`
	Tools       json.RawMessage     `json:"tools,omitempty"`
	ToolChoice  json.RawMessage     `json:"tool_choice,omitempty"`
	Stop        []string            `json:"stop,omitempty"`
}

// PluginName returns the stable type name for deduplication.
func (t *OpenAI2Anthropic) PluginName() string { return "OpenAI2Anthropic" }

// TransformRequest converts an OpenAI Chat Completion request into an Anthropic Messages request.
func (t *OpenAI2Anthropic) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var openAIReq openAIChatRequest
	if err := json.Unmarshal(body, &openAIReq); err != nil {
		return nil, nil, fmt.Errorf("openai2anthropic: failed to parse request: %w", err)
	}

	// Map OpenAI model name to Anthropic model name
	anthropicModel := mapModel(openAIReq.Model)

	// Build Anthropic request body as a map
	anthropicReq := map[string]interface{}{
		"model":       anthropicModel,
		"max_tokens":  openAIReq.MaxTokens,
		"temperature": openAIReq.Temperature,
		"stream":      openAIReq.Stream,
	}

	// Convert stop → stop_sequences
	if len(openAIReq.Stop) > 0 {
		anthropicReq["stop_sequences"] = openAIReq.Stop
	}

	// Convert tool definitions: OpenAI → Anthropic format
	if len(openAIReq.Tools) > 0 {
		var openaiTools []map[string]interface{}
		if err := json.Unmarshal(openAIReq.Tools, &openaiTools); err == nil {
			anthroTools := make([]map[string]interface{}, 0, len(openaiTools))
			for _, ot := range openaiTools {
				fn, _ := ot["function"].(map[string]interface{})
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				desc, _ := fn["description"].(string)
				params := fn["parameters"]
				anthroTool := map[string]interface{}{
					"name":         name,
					"description":  desc,
					"input_schema": params,
				}
				anthroTools = append(anthroTools, anthroTool)
			}
			anthropicReq["tools"] = anthroTools
		}
	}

	// Convert tool_choice: OpenAI → Anthropic format
	if len(openAIReq.ToolChoice) > 0 {
		var tcRaw json.RawMessage
		if err := json.Unmarshal(openAIReq.ToolChoice, &tcRaw); err == nil {
			// Try string format first
			var tcStr string
			if json.Unmarshal(tcRaw, &tcStr) == nil {
				switch tcStr {
				case "auto":
					anthropicReq["tool_choice"] = map[string]interface{}{"type": "auto"}
				case "required", "any":
					anthropicReq["tool_choice"] = map[string]interface{}{"type": "any"}
				default:
					anthropicReq["tool_choice"] = tcStr
				}
			} else {
				// Object format: {"type": "function", "function": {"name": "foo"}}
				var tcObj struct {
					Type     string `json:"type"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if json.Unmarshal(tcRaw, &tcObj) == nil && tcObj.Type == "function" {
					anthropicReq["tool_choice"] = map[string]interface{}{
						"type": "tool",
						"name": tcObj.Function.Name,
					}
				}
			}
		}
	}

	// Convert messages
	systemText := ""
	if openAIReq.System != "" {
		systemText = openAIReq.System
	}

	var anthroMessages []map[string]interface{}

	for _, msg := range openAIReq.Messages {
		if msg.Role == "system" {
			if systemText != "" {
				systemText += "\n"
			}
			systemText += msg.Content
			continue
		}

		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			// Assistant message with tool_calls → content blocks with text + tool_use
			blocks := make([]map[string]interface{}, 0)
			if msg.Content != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				var input interface{}
				json.Unmarshal([]byte(tc.Function.Arguments), &input)
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			anthroMessages = append(anthroMessages, map[string]interface{}{
				"role":    "assistant",
				"content": blocks,
			})
		} else if msg.Role == "tool" {
			// Tool role → tool_result content block
			blocks := []map[string]interface{}{
				{
					"type":        "tool_result",
					"tool_use_id": msg.ToolCallID,
					"content":     msg.Content,
				},
			}
			anthroMessages = append(anthroMessages, map[string]interface{}{
				"role":    "user",
				"content": blocks,
			})
		} else {
			// Regular text message
			anthroMessages = append(anthroMessages, map[string]interface{}{
				"role":    msg.Role,
				"content": msg.Content,
			})
		}
	}

	// Set system prompt if present
	if systemText != "" {
		anthropicReq["system"] = systemText
	}

	// Ensure at least one message
	if len(anthroMessages) == 0 {
		anthroMessages = append(anthroMessages, map[string]interface{}{
			"role":    "user",
			"content": "Hello",
		})
	}
	anthropicReq["messages"] = anthroMessages

	// Ensure max_tokens is set
	if openAIReq.MaxTokens <= 0 {
		anthropicReq["max_tokens"] = 1024
	}

	newBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, nil, fmt.Errorf("openai2anthropic: failed to marshal request: %w", err)
	}

	// Update the URL path for Anthropic
	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	newReq.URL.Path = "/v1/messages"

	// Update downstream context
	if ctx.TargetDownstream != nil {
		newReq.Header.Set("x-api-key", ctx.TargetDownstream.APIKey)
		newReq.Header.Set("anthropic-version", "2023-06-01")
	}

	return newReq, newBody, nil
}

// TransformResponse converts an Anthropic Messages response into an OpenAI Chat Completion response.
func (t *OpenAI2Anthropic) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	// Handle streaming responses (SSE) vs non-streaming
	contentType := resp.Header.Get("Content-Type")
	if contentType == "text/event-stream" {
		return t.transformStreamingResponse(body)
	}

	return t.transformJSONResponse(body)
}

type anthropicContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type anthropicResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Content []anthropicContent `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	StopReason string `json:"stop_reason"`
}

type openAIChatChoice struct {
	Index        int               `json:"index"`
	Message      openAIChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type openAIChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIChatResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Model   string             `json:"model"`
	Choices []openAIChatChoice `json:"choices"`
	Usage   openAIChatUsage    `json:"usage"`
}

func (t *OpenAI2Anthropic) transformJSONResponse(body []byte) ([]byte, error) {
	var anthropicResp anthropicResponse
	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		// Not valid JSON (e.g. downstream error page) — pass through unchanged
		return body, nil
	}

	// Build OpenAI response — extract text and tool calls from content blocks
	content := ""
	var toolCalls []openAIChatToolCall
	for _, c := range anthropicResp.Content {
		if c.Type == "text" {
			content += c.Text
		} else if c.Type == "tool_use" {
			tc := openAIChatToolCall{
				ID:   c.ID,
				Type: "function",
			}
			tc.Function.Name = c.Name
			if c.Input != nil {
				tc.Function.Arguments = string(c.Input)
			} else {
				tc.Function.Arguments = "{}"
			}
			toolCalls = append(toolCalls, tc)
		}
	}

	finishReason := anthropicResp.StopReason
	if finishReason == "end_turn" {
		finishReason = "stop"
	}

	msg := openAIChatMessage{
		Role:    "assistant",
		Content: content,
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	openAIResp := openAIChatResponse{
		ID:     anthropicResp.ID,
		Object: "chat.completion",
		Model:  anthropicResp.Model,
		Choices: []openAIChatChoice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
		Usage: openAIChatUsage{
			PromptTokens:     anthropicResp.Usage.InputTokens,
			CompletionTokens: anthropicResp.Usage.OutputTokens,
			TotalTokens:      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		},
	}

	return json.Marshal(openAIResp)
}

func (t *OpenAI2Anthropic) transformStreamingResponse(body []byte) ([]byte, error) {
	// Convert Anthropic SSE events to OpenAI chat completion chunks
	var id, model string
	var out bytes.Buffer

	parseAnthropicSSE(body, func(eventType string, data []byte) bool {
		switch eventType {
		case "message_start":
			var msg struct {
				Message struct {
					ID    string `json:"id"`
					Model string `json:"model"`
				} `json:"message"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				return false
			}
			id = msg.Message.ID
			model = msg.Message.Model

			// First chunk with role
			chunk := openAIChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Model:   model,
				Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Role: "assistant"}, FinishReason: nil}},
			}
			writeSSEData(&out, chunk)

		case "content_block_start":
			var block struct {
				ContentBlock struct {
					Type string `json:"type"`
					Text string `json:"text"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(data, &block); err != nil {
				return false
			}
			switch block.ContentBlock.Type {
			case "text":
				if block.ContentBlock.Text != "" {
					chunk := openAIChunk{
						ID:      id,
						Object:  "chat.completion.chunk",
						Model:   model,
						Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Content: block.ContentBlock.Text}, FinishReason: nil}},
					}
					writeSSEData(&out, chunk)
				}
			case "tool_use":
				tc := openAIToolCallDelta{
					Index: 0,
					ID:    block.ContentBlock.ID,
					Type:  "function",
				}
				tc.Function.Name = block.ContentBlock.Name
				chunk := openAIChunk{
					ID:      id,
					Object:  "chat.completion.chunk",
					Model:   model,
					Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{tc}}, FinishReason: nil}},
				}
				writeSSEData(&out, chunk)
			}

		case "content_block_delta":
			var delta struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(data, &delta); err != nil {
				return false
			}
			switch delta.Delta.Type {
			case "text_delta":
				if delta.Delta.Text != "" {
					chunk := openAIChunk{
						ID:      id,
						Object:  "chat.completion.chunk",
						Model:   model,
						Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Content: delta.Delta.Text}, FinishReason: nil}},
					}
					writeSSEData(&out, chunk)
				}
			case "input_json_delta":
				if delta.Delta.PartialJSON != "" {
					tc := openAIToolCallDelta{Index: 0}
					tc.Function.Arguments = delta.Delta.PartialJSON
					chunk := openAIChunk{
						ID:      id,
						Object:  "chat.completion.chunk",
						Model:   model,
						Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{tc}}, FinishReason: nil}},
					}
					writeSSEData(&out, chunk)
				}
			}

		case "message_delta":
			var md struct {
				Delta struct {
					StopReason   string `json:"stop_reason"`
					StopSequence string `json:"stop_sequence"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(data, &md); err != nil {
				return false
			}
			finishReason := md.Delta.StopReason
			if finishReason == "end_turn" {
				finishReason = "stop"
			}
			if finishReason == "tool_use" {
				finishReason = "tool_calls"
			}
			chunk := openAIChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Model:   model,
				Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{}, FinishReason: &finishReason}},
			}
			writeSSEData(&out, chunk)

		case "message_stop":
			writeDoneMarker(&out)
		}
		return true
	})

	return out.Bytes(), nil
}

// TransformStreamChunk converts a single Anthropic SSE event to an OpenAI SSE chunk.
func (t *OpenAI2Anthropic) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	// State tracking across chunks for this stream session
	state := &openai2anthropicStreamState{}
	if existing, ok := ctx.Variables["oai2anth_stream"]; ok {
		state = existing.(*openai2anthropicStreamState)
	}
	defer func() { ctx.Variables["oai2anth_stream"] = state }()

	switch chunk.EventType {
	case "message_start":
		var msg struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal(chunk.Data, &msg); err != nil {
			return chunk, nil
		}
		state.ID = msg.Message.ID
		state.Model = msg.Message.Model

		outChunk := openAIChunk{
			ID:      state.ID,
			Object:  "chat.completion.chunk",
			Model:   state.Model,
			Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Role: "assistant"}}},
		}
		data, _ := json.Marshal(outChunk)
		return engine.SSEChunk{EventType: "", Data: data}, nil

	case "content_block_start":
		var block struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				Text string `json:"text"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(chunk.Data, &block); err != nil {
			return chunk, nil
		}
		switch block.ContentBlock.Type {
		case "text":
			if block.ContentBlock.Text != "" {
				outChunk := openAIChunk{
					ID:      state.ID,
					Object:  "chat.completion.chunk",
					Model:   state.Model,
					Choices: []openAIChunkChoice{{Index: block.Index, Delta: openAIDelta{Content: block.ContentBlock.Text}}},
				}
				data, _ := json.Marshal(outChunk)
				return engine.SSEChunk{EventType: "", Data: data}, nil
			}
			return engine.SSEChunk{}, nil
		case "tool_use":
			tc := openAIToolCallDelta{
				Index: 0,
				ID:    block.ContentBlock.ID,
				Type:  "function",
			}
			tc.Function.Name = block.ContentBlock.Name
			outChunk := openAIChunk{
				ID:      state.ID,
				Object:  "chat.completion.chunk",
				Model:   state.Model,
				Choices: []openAIChunkChoice{{Index: block.Index, Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{tc}}}},
			}
			data, _ := json.Marshal(outChunk)
			return engine.SSEChunk{EventType: "", Data: data}, nil
		default:
			return engine.SSEChunk{}, nil
		}

	case "content_block_delta":
		var delta struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &delta); err != nil {
			return chunk, nil
		}
		switch delta.Delta.Type {
		case "text_delta":
			if delta.Delta.Text != "" {
				outChunk := openAIChunk{
					ID:      state.ID,
					Object:  "chat.completion.chunk",
					Model:   state.Model,
					Choices: []openAIChunkChoice{{Index: delta.Index, Delta: openAIDelta{Content: delta.Delta.Text}}},
				}
				data, _ := json.Marshal(outChunk)
				return engine.SSEChunk{EventType: "", Data: data}, nil
			}
			return engine.SSEChunk{}, nil
		case "input_json_delta":
			if delta.Delta.PartialJSON != "" {
				tc := openAIToolCallDelta{Index: 0}
				tc.Function.Arguments = delta.Delta.PartialJSON
				outChunk := openAIChunk{
					ID:      state.ID,
					Object:  "chat.completion.chunk",
					Model:   state.Model,
					Choices: []openAIChunkChoice{{Index: delta.Index, Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{tc}}}},
				}
				data, _ := json.Marshal(outChunk)
				return engine.SSEChunk{EventType: "", Data: data}, nil
			}
			return engine.SSEChunk{}, nil
		default:
			return engine.SSEChunk{}, nil
		}

	case "message_delta":
		var md struct {
			Delta struct {
				StopReason   string `json:"stop_reason"`
				StopSequence string `json:"stop_sequence"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &md); err != nil {
			return chunk, nil
		}
		finishReason := md.Delta.StopReason
		if finishReason == "end_turn" {
			finishReason = "stop"
		}
		if finishReason == "tool_use" {
			finishReason = "tool_calls"
		}
		outChunk := openAIChunk{
			ID:      state.ID,
			Object:  "chat.completion.chunk",
			Model:   state.Model,
			Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{}, FinishReason: &finishReason}},
		}
		data, _ := json.Marshal(outChunk)
		return engine.SSEChunk{EventType: "", Data: data}, nil

	case "message_stop":
		// OpenAI uses "[DONE]" marker — write it as the data payload
		return engine.SSEChunk{EventType: "", Data: []byte("[DONE]")}, nil

	case "content_block_stop":
		// OpenAI format has no block stop events — no output needed
		return engine.SSEChunk{}, nil

	default:
		// Unknown event types (e.g. Anthropic "ping" keepalives) are
		// dropped - they have no meaning in OpenAI format and confuse
		// strict OpenAI client validators.
		return engine.SSEChunk{}, nil
	}
}

// openai2anthropicStreamState tracks state across SSE chunks for a single stream.
type openai2anthropicStreamState struct {
	ID    string
	Model string
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*OpenAI2Anthropic)(nil)
var _ engine.ResponseTransformer = (*OpenAI2Anthropic)(nil)
var _ engine.StreamResponseTransformer = (*OpenAI2Anthropic)(nil)

// mapModel translates common OpenAI model names to Anthropic equivalents.
func mapModel(openAIModel string) string {
	modelMap := map[string]string{
		"gpt-4":           "claude-sonnet-4-20250514",
		"gpt-4o":          "claude-sonnet-4-20250514",
		"gpt-4o-mini":     "claude-haiku-3-5-20241022",
		"gpt-3.5-turbo":   "claude-haiku-3-5-20241022",
		"gpt-4-turbo":     "claude-opus-4-20250514",
		"o1":              "claude-opus-4-20250514",
		"o3-mini":         "claude-sonnet-4-20250514",
	}
	if mapped, ok := modelMap[openAIModel]; ok {
		return mapped
	}
	return openAIModel
}
