package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"tresor/internal/engine"
)

// Responses2OpenAI converts OpenAI Responses API requests to Chat Completions format
// and Chat Completions responses back to Responses API format.
type Responses2OpenAI struct{}

// --- Responses API Request Types ---

type responsesRequest struct {
	Model       string                    `json:"model"`
	Instructions string                   `json:"instructions,omitempty"`
	Input       json.RawMessage           `json:"input"`
	Stream      bool                      `json:"stream"`
	Tools       json.RawMessage           `json:"tools,omitempty"`
	ToolChoice  json.RawMessage           `json:"tool_choice,omitempty"`
	Reasoning   *responsesReasoningConfig `json:"reasoning,omitempty"`
	Text        *responsesTextConfig      `json:"text,omitempty"`
}

type responsesReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesTextConfig struct {
	Format json.RawMessage `json:"format,omitempty"`
}

// responsesInputItemRaw is used to disambiguate input items by their fields.
type responsesInputItemRaw struct {
	Role    string          `json:"role,omitempty"`
	Type    string          `json:"type,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    string          `json:"arguments,omitempty"`
	Output  string          `json:"output,omitempty"`
}

type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// --- Responses API Response Types ---

type responsesResponse struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Status  string               `json:"status"`
	Model   string               `json:"model"`
	Output  []responsesOutputItem `json:"output"`
	Usage   *responsesUsage      `json:"usage,omitempty"`
}

type responsesOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Text      string `json:"text,omitempty"`
	Role      string `json:"role,omitempty"`
	Status    string `json:"status,omitempty"`
	Content   []responsesContentPart `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type responsesUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- OpenAI Chat Completion Response Types ---

type chatCompletionChoice struct {
	Index        int               `json:"index"`
	Message      openAIChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type chatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   *chatCompletionUsage   `json:"usage,omitempty"`
}

// --- TransformRequest: Responses API → Chat Completions ---

func (t *Responses2OpenAI) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var respReq responsesRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		return nil, nil, fmt.Errorf("responses2openai: failed to parse request: %w", err)
	}

	oaiBody := map[string]interface{}{
		"model":  respReq.Model,
		"stream": respReq.Stream,
	}

	// Set a default max_tokens
	oaiBody["max_tokens"] = 4096

	messages := make([]map[string]interface{}, 0)

	// Instructions → system message (prepended)
	if respReq.Instructions != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": respReq.Instructions,
		})
	}

	// Parse input items
	if len(respReq.Input) > 0 {
		var items []responsesInputItemRaw
		if err := json.Unmarshal(respReq.Input, &items); err == nil {
			// Collect function calls to merge into the preceding assistant message
			var pendingToolCalls []openAIChatToolCall

			flushToolCalls := func() {
				if len(pendingToolCalls) > 0 && len(messages) > 0 {
					last := messages[len(messages)-1]
					if last["role"] == "assistant" {
						last["tool_calls"] = pendingToolCalls
					} else {
						messages = append(messages, map[string]interface{}{
							"role":       "assistant",
							"content":    nil,
							"tool_calls": pendingToolCalls,
						})
					}
					pendingToolCalls = nil
				}
			}

			for _, item := range items {
				if item.Role != "" {
					flushToolCalls()
					msg := map[string]interface{}{
						"role": item.Role,
					}
					// Map developer to system role
					if item.Role == "developer" {
						msg["role"] = "system"
					}
					if len(item.Content) > 0 {
						// Try string content first
						var s string
						if err := json.Unmarshal(item.Content, &s); err == nil {
							msg["content"] = s
						} else {
							// Try array of content parts
							var parts []responsesContentPart
							if err := json.Unmarshal(item.Content, &parts); err == nil {
								oaiParts := make([]map[string]interface{}, 0, len(parts))
								for _, p := range parts {
									switch p.Type {
									case "input_text":
										oaiParts = append(oaiParts, map[string]interface{}{
											"type": "text",
											"text": p.Text,
										})
									case "input_image":
										oaiParts = append(oaiParts, map[string]interface{}{
											"type": "image_url",
											"image_url": map[string]interface{}{
												"url": p.ImageURL,
											},
										})
									}
								}
								msg["content"] = oaiParts
							}
						}
					}
					messages = append(messages, msg)
				} else if item.Type == "function_call" {
					tc := openAIChatToolCall{
						ID:   item.CallID,
						Type: "function",
					}
					tc.Function.Name = item.Name
					tc.Function.Arguments = item.Args
					pendingToolCalls = append(pendingToolCalls, tc)
				} else if item.Type == "function_call_output" {
					flushToolCalls()
					messages = append(messages, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": item.CallID,
						"content":      item.Output,
					})
				}
			}
			flushToolCalls()
		}
	}

	if len(messages) > 0 {
		oaiBody["messages"] = messages
	}

	// Passthrough tools and tool_choice
	if len(respReq.Tools) > 0 {
		oaiBody["tools"] = respReq.Tools
	}
	if len(respReq.ToolChoice) > 0 {
		oaiBody["tool_choice"] = respReq.ToolChoice
	}

	// Reasoning effort
	if respReq.Reasoning != nil && respReq.Reasoning.Effort != "" {
		oaiBody["reasoning_effort"] = respReq.Reasoning.Effort
	}

	// Text.format → response_format
	if respReq.Text != nil && respReq.Text.Format != nil {
		oaiBody["response_format"] = respReq.Text.Format
	}

	newBody, err := json.Marshal(oaiBody)
	if err != nil {
		return nil, nil, fmt.Errorf("responses2openai: failed to marshal request: %w", err)
	}

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	newReq.URL.Path = "/v1/chat/completions"

	if ctx.TargetDownstream != nil {
		newReq.Header.Set("Authorization", "Bearer "+ctx.TargetDownstream.APIKey)
	}

	return newReq, newBody, nil
}

// --- TransformResponse: Chat Completions → Responses API ---

func (t *Responses2OpenAI) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "text/event-stream" {
		return t.transformStreamingResponse(body)
	}
	return t.transformJSONResponse(body)
}

func (t *Responses2OpenAI) transformJSONResponse(body []byte) ([]byte, error) {
	var oaiResp chatCompletionResponse
	if err := json.Unmarshal(body, &oaiResp); err != nil {
		return body, nil
	}

	respID := oaiResp.ID
	if respID == "" {
		respID = fmt.Sprintf("resp_%d", time.Now().UnixMilli())
	}

	output := make([]responsesOutputItem, 0)

	for _, choice := range oaiResp.Choices {
		// Text content → output_text item
		if choice.Message.Content != "" {
			output = append(output, responsesOutputItem{
				Type:   "output_text",
				ID:     respID + ".text",
				Text:   choice.Message.Content,
				Role:   "assistant",
				Status: "completed",
			})
		}

		// Tool calls → function_call items
		for _, tc := range choice.Message.ToolCalls {
			output = append(output, responsesOutputItem{
				Type:      "function_call",
				ID:        tc.ID,
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
				Status:    "completed",
			})
		}
	}

	usage := &responsesUsage{}
	if oaiResp.Usage != nil {
		usage.InputTokens = oaiResp.Usage.PromptTokens
		usage.OutputTokens = oaiResp.Usage.CompletionTokens
		usage.TotalTokens = oaiResp.Usage.TotalTokens
	}

	out := responsesResponse{
		ID:     respID,
		Object: "response",
		Status: "completed",
		Model:  oaiResp.Model,
		Output: output,
		Usage:  usage,
	}

	return json.Marshal(out)
}

func (t *Responses2OpenAI) transformStreamingResponse(body []byte) ([]byte, error) {
	var id string
	var out bytes.Buffer
	var textContent string
	var messageStarted bool

	parseOpenAISSE(body, func(data []byte) bool {
		if string(bytes.TrimSpace(data)) == "[DONE]" {
			return false
		}

		var chunk openAIChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return true
		}

		if !messageStarted {
			id = chunk.ID
			messageStarted = true

			// Emit response.created
			writeResponsesSSE(&out, "response.created", map[string]interface{}{
				"type": "response.created",
				"response": map[string]interface{}{
					"id":     id,
					"status": "in_progress",
				},
			})

			// Emit response.in_progress
			writeResponsesSSE(&out, "response.in_progress", map[string]interface{}{
				"type": "response.in_progress",
				"response": map[string]interface{}{
					"id":     id,
					"status": "in_progress",
				},
			})
		}

		for _, choice := range chunk.Choices {
			// Content delta
			if choice.Delta.Content != "" {
				textContent += choice.Delta.Content
				writeResponsesSSE(&out, "response.output_text.delta", map[string]interface{}{
					"type":  "response.output_text.delta",
					"delta": choice.Delta.Content,
				})
			}

			// Tool calls in streaming delta
			for _, tc := range choice.Delta.ToolCalls {
				if tc.ID != "" {
					// New tool call — emit output_item.added
					writeResponsesSSE(&out, "response.output_item.added", map[string]interface{}{
						"type":         "response.output_item.added",
						"output_index": tc.Index,
						"item": map[string]interface{}{
							"type":    "function_call",
							"id":      tc.ID,
							"call_id": tc.ID,
							"name":    tc.Function.Name,
						},
					})
				}
				if tc.Function.Arguments != "" {
					writeResponsesSSE(&out, "response.function_call_arguments.delta", map[string]interface{}{
						"type":      "response.function_call_arguments.delta",
						"delta":     tc.Function.Arguments,
						"call_id":   tc.ID,
						"output_index": tc.Index,
					})
				}
			}

			// Finish reason
			if choice.FinishReason != nil {
				if textContent != "" {
					writeResponsesSSE(&out, "response.output_text.done", map[string]interface{}{
						"type": "response.output_text.done",
						"text": textContent,
					})
				}

				writeResponsesSSE(&out, "response.completed", map[string]interface{}{
					"type": "response.completed",
					"response": map[string]interface{}{
						"id":     id,
						"status": "completed",
						"output": []interface{}{},
						"usage": map[string]interface{}{
							"input_tokens":  0,
							"output_tokens": 0,
							"total_tokens":  0,
						},
					},
				})
			}
		}
		return true
	})

	return out.Bytes(), nil
}

// --- StreamResponseTransformer: per-chunk Chat Completions → Responses API ---

type r2oStreamToolCall struct {
	ID            string
	Name          string
	Arguments     string
	ItemSent      bool
	ArgsDeltaSent bool
}

type r2oStreamState struct {
	ResponseID  string
	Model       string
	Created     bool
	TextContent string
	ToolCallAcc map[int]*r2oStreamToolCall
}

func (t *Responses2OpenAI) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	state := &r2oStreamState{}
	if existing, ok := ctx.Variables["r2o_stream"]; ok {
		state = existing.(*r2oStreamState)
	}
	defer func() { ctx.Variables["r2o_stream"] = state }()

	// Handle [DONE] marker — no output, engine terminates stream
	if string(bytes.TrimSpace(chunk.Data)) == "[DONE]" {
		if state.Created {
			// Emit final output_text.done + completed
			var out bytes.Buffer
			if state.TextContent != "" {
				writeResponsesSSE(&out, "response.output_text.done", map[string]interface{}{
					"type": "response.output_text.done",
					"text": state.TextContent,
				})
			}
			writeResponsesSSE(&out, "response.completed", map[string]interface{}{
				"type": "response.completed",
				"response": map[string]interface{}{
					"id":     state.ResponseID,
					"status": "completed",
					"output": []interface{}{},
					"usage":  map[string]interface{}{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
				},
			})
			return engine.SSEChunk{Data: out.Bytes()}, nil
		}
		return engine.SSEChunk{}, nil
	}

	var oaiChunk openAIChunk
	if err := json.Unmarshal(chunk.Data, &oaiChunk); err != nil {
		return chunk, nil
	}

	var out bytes.Buffer

	// First chunk: emit lifecycle events
	if !state.Created {
		state.ResponseID = oaiChunk.ID
		if state.ResponseID == "" {
			state.ResponseID = fmt.Sprintf("resp_%d", time.Now().UnixMilli())
		}
		state.Model = oaiChunk.Model
		state.Created = true
		state.ToolCallAcc = make(map[int]*r2oStreamToolCall)

		writeResponsesSSE(&out, "response.created", map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id":     state.ResponseID,
				"status": "in_progress",
			},
		})

		writeResponsesSSE(&out, "response.in_progress", map[string]interface{}{
			"type": "response.in_progress",
			"response": map[string]interface{}{
				"id":     state.ResponseID,
				"status": "in_progress",
			},
		})
	}

	for _, choice := range oaiChunk.Choices {
		// Content delta
		if choice.Delta.Content != "" {
			state.TextContent += choice.Delta.Content
			writeResponsesSSE(&out, "response.output_text.delta", map[string]interface{}{
				"type":  "response.output_text.delta",
				"delta": choice.Delta.Content,
			})
		}

		// Tool calls in streaming delta
		for _, tc := range choice.Delta.ToolCalls {
			acc, exists := state.ToolCallAcc[tc.Index]
			if !exists {
				acc = &r2oStreamToolCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
				}
				state.ToolCallAcc[tc.Index] = acc
			}
			if tc.ID != "" && !acc.ItemSent {
				acc.ItemSent = true
				acc.Name = tc.Function.Name
				writeResponsesSSE(&out, "response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": tc.Index,
					"item": map[string]interface{}{
						"type":    "function_call",
						"id":      tc.ID,
						"call_id": tc.ID,
						"name":    tc.Function.Name,
					},
				})
			}
			if tc.Function.Arguments != "" {
				acc.Arguments += tc.Function.Arguments
				writeResponsesSSE(&out, "response.function_call_arguments.delta", map[string]interface{}{
					"type":      "response.function_call_arguments.delta",
					"delta":     tc.Function.Arguments,
					"call_id":   acc.ID,
					"output_index": tc.Index,
				})
			}
		}

		// Finish reason
		if choice.FinishReason != nil {
			if state.TextContent != "" {
				writeResponsesSSE(&out, "response.output_text.done", map[string]interface{}{
					"type": "response.output_text.done",
					"text": state.TextContent,
				})
			}
			// Close tool call items
			for _, acc := range state.ToolCallAcc {
				writeResponsesSSE(&out, "response.function_call_arguments.done", map[string]interface{}{
					"type":      "response.function_call_arguments.done",
					"call_id":   acc.ID,
					"name":      acc.Name,
					"arguments": acc.Arguments,
				})
				writeResponsesSSE(&out, "response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item": map[string]interface{}{
						"type":    "function_call",
						"id":      acc.ID,
						"call_id": acc.ID,
						"status":  "completed",
					},
				})
			}
			writeResponsesSSE(&out, "response.completed", map[string]interface{}{
				"type": "response.completed",
				"response": map[string]interface{}{
					"id":     state.ResponseID,
					"status": "completed",
					"usage":  map[string]interface{}{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
				},
			})
		}
	}

	if out.Len() == 0 {
		return engine.SSEChunk{}, nil
	}
	return engine.SSEChunk{Data: out.Bytes()}, nil
}

// --- Helpers ---

func writeResponsesSSE(buf *bytes.Buffer, eventType string, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(buf, "event: %s\n", eventType)
	buf.WriteString("data: ")
	buf.Write(payload)
	buf.WriteString("\n\n")
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*Responses2OpenAI)(nil)
var _ engine.ResponseTransformer = (*Responses2OpenAI)(nil)
var _ engine.StreamResponseTransformer = (*Responses2OpenAI)(nil)
