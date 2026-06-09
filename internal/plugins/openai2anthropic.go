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
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	System      string              `json:"system,omitempty"`
}

type anthropicRequest struct {
	Model       string               `json:"model"`
	MaxTokens   int                  `json:"max_tokens"`
	Messages    []AnthropicMessage   `json:"messages"`
	System      *flexibleContent     `json:"system,omitempty"`
	Temperature float64              `json:"temperature,omitempty"`
	Stream      bool                 `json:"stream,omitempty"`
}

// TransformRequest converts an OpenAI Chat Completion request into an Anthropic Messages request.
func (t *OpenAI2Anthropic) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var openAIReq openAIChatRequest
	if err := json.Unmarshal(body, &openAIReq); err != nil {
		return nil, nil, fmt.Errorf("openai2anthropic: failed to parse request: %w", err)
	}

	// Map OpenAI model name to Anthropic model name
	anthropicModel := mapModel(openAIReq.Model)

	// Convert messages and extract system prompt
	anthropicReq := anthropicRequest{
		Model:       anthropicModel,
		MaxTokens:   openAIReq.MaxTokens,
		Temperature: openAIReq.Temperature,
		Stream:      openAIReq.Stream,
	}

	for _, msg := range openAIReq.Messages {
		if msg.Role == "system" {
			anthropicReq.System = &flexibleContent{Text: msg.Content}
			continue
		}
		anthropicReq.Messages = append(anthropicReq.Messages, AnthropicMessage{
			Role: msg.Role,
			Content: &contentBlockArray{
				Text: msg.Content,
			},
		})
	}

	// Ensure at least one message
	if len(anthropicReq.Messages) == 0 {
		anthropicReq.Messages = []AnthropicMessage{{Role: "user", Content: &contentBlockArray{Text: "Hello"}}}
	}
	if anthropicReq.MaxTokens <= 0 {
		anthropicReq.MaxTokens = 1024
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
	Type string `json:"type"`
	Text string `json:"text"`
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
	Index        int             `json:"index"`
	Message      openAIChatMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
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

	// Build OpenAI response
	content := ""
	for _, c := range anthropicResp.Content {
		if c.Type == "text" {
			content += c.Text
		}
	}

	finishReason := anthropicResp.StopReason
	if finishReason == "end_turn" {
		finishReason = "stop"
	}

	openAIResp := openAIChatResponse{
		ID:     anthropicResp.ID,
		Object: "chat.completion",
		Model:  anthropicResp.Model,
		Choices: []openAIChatChoice{
			{
				Index: 0,
				Message: openAIChatMessage{
					Role:    "assistant",
					Content: content,
				},
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
				} `json:"content_block"`
			}
			if err := json.Unmarshal(data, &block); err != nil {
				return false
			}
			if block.ContentBlock.Type == "text" && block.ContentBlock.Text != "" {
				chunk := openAIChunk{
					ID:      id,
					Object:  "chat.completion.chunk",
					Model:   model,
					Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Content: block.ContentBlock.Text}, FinishReason: nil}},
				}
				writeSSEData(&out, chunk)
			}

		case "content_block_delta":
			var delta struct {
				Index int `json:"index"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(data, &delta); err != nil {
				return false
			}
			if delta.Delta.Type == "text_delta" && delta.Delta.Text != "" {
				chunk := openAIChunk{
					ID:      id,
					Object:  "chat.completion.chunk",
					Model:   model,
					Choices: []openAIChunkChoice{{Index: 0, Delta: openAIDelta{Content: delta.Delta.Text}, FinishReason: nil}},
				}
				writeSSEData(&out, chunk)
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
			} `json:"content_block"`
		}
		if err := json.Unmarshal(chunk.Data, &block); err != nil {
			return chunk, nil
		}
		if block.ContentBlock.Type == "text" && block.ContentBlock.Text != "" {
			outChunk := openAIChunk{
				ID:      state.ID,
				Object:  "chat.completion.chunk",
				Model:   state.Model,
				Choices: []openAIChunkChoice{{Index: block.Index, Delta: openAIDelta{Content: block.ContentBlock.Text}}},
			}
			data, _ := json.Marshal(outChunk)
			return engine.SSEChunk{EventType: "", Data: data}, nil
		}
		// Non-text block or empty — pass through silently (no output)
		return chunk, nil

	case "content_block_delta":
		var delta struct {
			Index int `json:"index"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &delta); err != nil {
			return chunk, nil
		}
		if delta.Delta.Type == "text_delta" && delta.Delta.Text != "" {
			outChunk := openAIChunk{
				ID:      state.ID,
				Object:  "chat.completion.chunk",
				Model:   state.Model,
				Choices: []openAIChunkChoice{{Index: delta.Index, Delta: openAIDelta{Content: delta.Delta.Text}}},
			}
			data, _ := json.Marshal(outChunk)
			return engine.SSEChunk{EventType: "", Data: data}, nil
		}
		return chunk, nil

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

	default:
		// Unknown event — pass through unchanged
		return chunk, nil
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
