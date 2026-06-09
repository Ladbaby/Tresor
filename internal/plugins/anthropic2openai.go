package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"tresor/internal/engine"
)

// Anthropic2OpenAI converts Anthropic Messages requests to OpenAI Chat Completion format.
type Anthropic2OpenAI struct{}

type anthropicRequest2 struct {
	Model       string               `json:"model"`
	MaxTokens   int                  `json:"max_tokens"`
	Messages    []AnthropicMessage   `json:"messages"`
	System      *flexibleContent     `json:"system,omitempty"`
	Temperature float64              `json:"temperature,omitempty"`
	Stream      bool                 `json:"stream,omitempty"`
}

// TransformRequest converts an Anthropic Messages request into an OpenAI Chat Completion request.
func (t *Anthropic2OpenAI) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var anthropicReq anthropicRequest2
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, nil, fmt.Errorf("anthropic2openai: failed to parse request: %w", err)
	}

	// Build OpenAI request
	openAIReq := openAIChatRequest{
		Model:       mapModelReverse(anthropicReq.Model),
		Messages:    make([]openAIChatMessage, 0),
		MaxTokens:   anthropicReq.MaxTokens,
		Temperature: anthropicReq.Temperature,
		Stream:      anthropicReq.Stream,
	}

	// If there's a system prompt, add it as a system message at the start
	if anthropicReq.System != nil && anthropicReq.System.Text != "" {
		openAIReq.Messages = append(openAIReq.Messages, openAIChatMessage{
			Role:    "system",
			Content: anthropicReq.System.Text,
		})
	}

	// Convert Anthropic messages to OpenAI format
	for _, msg := range anthropicReq.Messages {
		openAIReq.Messages = append(openAIReq.Messages, openAIChatMessage{
			Role:    msg.Role,
			Content: msg.Content.Text,
		})
	}

	newBody, err := json.Marshal(openAIReq)
	if err != nil {
		return nil, nil, fmt.Errorf("anthropic2openai: failed to marshal request: %w", err)
	}

	// Update request to point to OpenAI endpoint
	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	newReq.URL.Path = "/v1/chat/completions"

	// Update downstream context
	if ctx.TargetDownstream != nil {
		newReq.Header.Set("Authorization", "Bearer "+ctx.TargetDownstream.APIKey)
	}

	return newReq, newBody, nil
}

// TransformResponse converts an OpenAI Chat Completion response into an Anthropic Messages response.
func (t *Anthropic2OpenAI) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "text/event-stream" {
		return t.transformStreamingResponse(body)
	}

	return t.transformJSONResponse(body)
}

func (t *Anthropic2OpenAI) transformStreamingResponse(body []byte) ([]byte, error) {
	// Convert OpenAI chat completion chunks to Anthropic SSE events
	var id, model string
	var out bytes.Buffer
	inContentBlock := false
	messageStarted := false

	parseOpenAISSE(body, func(data []byte) bool {
		var chunk openAIChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return false
		}

		if !messageStarted {
			id = chunk.ID
			model = chunk.Model
			messageStarted = true
			// Emit message_start to match Anthropic SSE protocol
			msg := struct {
				Type    string `json:"type"`
				Message struct {
					ID    string `json:"id"`
					Model string `json:"model"`
				} `json:"message"`
			}{
				Type: "message_start",
			}
			msg.Message.ID = id
			msg.Message.Model = model
			writeAnthropicSSE(&out, "message_start", msg)
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Role == "assistant" && !inContentBlock {
				// Start of assistant response
				msg := struct {
					Type    string `json:"type"`
					Content struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
					Index int `json:"index"`
				}{
					Type:  "content_block_start",
					Index: choice.Index,
				}
				msg.Content.Type = "text"
				msg.Content.Text = choice.Delta.Content
				writeAnthropicSSE(&out, "content_block_start", msg)
				inContentBlock = true
			} else if choice.Delta.Content != "" {
				delta := struct {
					Type  string `json:"type"`
					Index int    `json:"index"`
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}{
					Type:  "content_block_delta",
					Index: choice.Index,
				}
				delta.Delta.Type = "text_delta"
				delta.Delta.Text = choice.Delta.Content
				writeAnthropicSSE(&out, "content_block_delta", delta)
			}

			if choice.FinishReason != nil {
				stopReason := *choice.FinishReason
				if stopReason == "stop" {
					stopReason = "end_turn"
				}
				msgDelta := struct {
					Type  string `json:"type"`
					Delta struct {
						StopReason   string `json:"stop_reason"`
						StopSequence string `json:"stop_sequence"`
					} `json:"delta"`
				}{
					Type: "message_delta",
				}
				msgDelta.Delta.StopReason = stopReason
				writeAnthropicSSE(&out, "message_delta", msgDelta)
				writeAnthropicSSE(&out, "message_stop", struct{ Type string `json:"type"` }{Type: "message_stop"})
			}
		}
		return true
	})

	return out.Bytes(), nil
}

func (t *Anthropic2OpenAI) transformJSONResponse(body []byte) ([]byte, error) {
	var openAIResp openAIChatResponse
	if err := json.Unmarshal(body, &openAIResp); err != nil {
		return nil, fmt.Errorf("anthropic2openai: failed to parse response: %w", err)
	}

	// Build Anthropic response
	content := make([]anthropicContent, 0)
	text := ""
	for _, choice := range openAIResp.Choices {
		text += choice.Message.Content
	}
	if text != "" {
		content = append(content, anthropicContent{Type: "text", Text: text})
	}

	stopReason := "end_turn"
	if len(openAIResp.Choices) > 0 && openAIResp.Choices[0].FinishReason == "stop" {
		stopReason = "end_turn"
	} else if len(openAIResp.Choices) > 0 && openAIResp.Choices[0].FinishReason == "length" {
		stopReason = "max_tokens"
	}

	anthropicResp := anthropicResponse{
		ID:      openAIResp.ID,
		Model:   mapModelReverse(openAIResp.Model),
		Content: content,
		Usage: struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}{
			InputTokens:  openAIResp.Usage.PromptTokens,
			OutputTokens: openAIResp.Usage.CompletionTokens,
		},
		StopReason: stopReason,
	}

	return json.Marshal(anthropicResp)
}

// TransformStreamChunk converts a single OpenAI SSE data payload to an Anthropic SSE event.
func (t *Anthropic2OpenAI) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	// State tracking across chunks for this stream session
	state := &anthropic2openaiStreamState{}
	if existing, ok := ctx.Variables["a2o_stream"]; ok {
		state = existing.(*anthropic2openaiStreamState)
	}
	defer func() { ctx.Variables["a2o_stream"] = state }()

	var out bytes.Buffer

	// Parse the OpenAI chunk
	var oaiChunk openAIChunk
	if err := json.Unmarshal(chunk.Data, &oaiChunk); err != nil {
		return chunk, fmt.Errorf("anthropic2openai stream: parse chunk: %w", err)
	}

	// Check for [DONE] marker
	if string(bytes.TrimSpace(chunk.Data)) == "[DONE]" {
		// Emit message_stop to close the Anthropic stream
		writeAnthropicSSE(&out, "message_stop", struct{ Type string `json:"type"` }{Type: "message_stop"})
		return engine.SSEChunk{EventType: "", Data: out.Bytes()}, nil
	}

	// First chunk: emit message_start
	if !state.messageStarted {
		state.ID = oaiChunk.ID
		state.Model = oaiChunk.Model
		state.messageStarted = true
		msg := struct {
			Type    string `json:"type"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
		}{Type: "message_start"}
		msg.Message.ID = state.ID
		msg.Message.Model = state.Model
		writeAnthropicSSE(&out, "message_start", msg)
	}

	for _, choice := range oaiChunk.Choices {
		if choice.Delta.Role == "assistant" && !state.inContentBlock {
			// Start of assistant response -> content_block_start
			msg := struct {
				Type    string `json:"type"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Index int `json:"index"`
			}{Type: "content_block_start", Index: choice.Index}
			msg.Content.Type = "text"
			msg.Content.Text = choice.Delta.Content
			writeAnthropicSSE(&out, "content_block_start", msg)
			state.inContentBlock = true
		} else if choice.Delta.Content != "" {
			delta := struct {
				Type  string `json:"type"`
				Index int    `json:"index"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}{Type: "content_block_delta", Index: choice.Index}
			delta.Delta.Type = "text_delta"
			delta.Delta.Text = choice.Delta.Content
			writeAnthropicSSE(&out, "content_block_delta", delta)
		}

		if choice.FinishReason != nil {
			stopReason := *choice.FinishReason
			if stopReason == "stop" {
				stopReason = "end_turn"
			}
			msgDelta := struct {
				Type  string `json:"type"`
				Delta struct {
					StopReason   string `json:"stop_reason"`
					StopSequence string `json:"stop_sequence"`
				} `json:"delta"`
			}{Type: "message_delta"}
			msgDelta.Delta.StopReason = stopReason
			writeAnthropicSSE(&out, "message_delta", msgDelta)
			writeAnthropicSSE(&out, "message_stop", struct{ Type string `json:"type"` }{Type: "message_stop"})
		}
	}

	return engine.SSEChunk{EventType: "", Data: out.Bytes()}, nil
}

// anthropic2openaiStreamState tracks state across SSE chunks for a single stream.
type anthropic2openaiStreamState struct {
	ID             string
	Model          string
	messageStarted bool
	inContentBlock bool
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*Anthropic2OpenAI)(nil)
var _ engine.ResponseTransformer = (*Anthropic2OpenAI)(nil)
var _ engine.StreamResponseTransformer = (*Anthropic2OpenAI)(nil)

// mapModelReverse maps Anthropic model names back to OpenAI equivalents.
func mapModelReverse(anthropicModel string) string {
	modelMap := map[string]string{
		"claude-sonnet-4-20250514":   "gpt-4o",
		"claude-haiku-3-5-20241022": "gpt-4o-mini",
		"claude-opus-4-20250514":    "gpt-4-turbo",
	}
	if mapped, ok := modelMap[anthropicModel]; ok {
		return mapped
	}
	return anthropicModel
}
