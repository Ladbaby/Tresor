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
	// EncryptedContent holds the opaque reasoning payload for
	// {type:"reasoning"} items. Codex re-sends prior turns' reasoning
	// back as encrypted blobs — only OpenAI can decode them. We forward
	// the bytes verbatim to backends that accept them, or drop them
	// silently for providers that don't.
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	// Refusal holds the refusal text for {type:"refusal"} parts. The
	// Responses API uses a separate `refusal` field on refusal parts (vs
	// `text` on output_text parts), and Anthropic's refusal blocks use the
	// same split, so we need both fields wired through.
	Refusal string `json:"refusal,omitempty"`
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

// PluginName returns the stable type name for deduplication.
func (t *Responses2OpenAI) PluginName() string { return "Responses2OpenAI" }

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
		} else {
			// Handle string input: the Responses API allows input to be a plain string
			var inputStr string
			if err := json.Unmarshal(respReq.Input, &inputStr); err == nil && inputStr != "" {
				messages = append(messages, map[string]interface{}{
					"role":    "user",
					"content": inputStr,
				})
			}
		}
	}

	if len(messages) > 0 {
		oaiBody["messages"] = messages
	}

	// Convert tools: Responses-API flat format → Chat Completions envelope.
	// Codex (and the Responses API generally) emit tools as
	//   {type:"function", name:"...", description:"...", parameters:{...}, strict:false}
	// while Chat Completions expects
	//   {type:"function", function:{name:"...", description:"...", parameters:{...}, strict?}}
	// `strict: false` is Responses-API-specific — many OpenAI-compat servers
	// (Cherry Studio, llama.cpp, etc.) reject it. Drop it to keep the
	// request acceptable to the broadest set of Chat Completions endpoints.
	if len(respReq.Tools) > 0 {
		oaiBody["tools"] = convertResponsesToolsToChatCompletions(respReq.Tools)
	}
	// tool_choice: Responses API uses {type:"function", name:"..."} while
	// Chat Completions uses {type:"function", function:{name:"..."}}.
	// String forms ("auto", "required", "none") are identical.
	if len(respReq.ToolChoice) > 0 {
		oaiBody["tool_choice"] = convertResponsesToolChoiceToChatCompletions(respReq.ToolChoice)
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

	output := make([]map[string]any, 0)
	msgContent := make([]map[string]any, 0)
	var reasoningItems []map[string]any

	for _, choice := range oaiResp.Choices {
		// Surface reasoning text emitted by an OpenAI Chat Completions upstream
		// (message.reasoning_content) as a Responses-style reasoning output
		// item with summary_text, mirroring what the client receives from
		// first-party OpenAI Responses API. Without this the thinking block
		// gets silently dropped on the responses2openai hop.
		if choice.Message.ReasoningContent != "" {
			reasoningItems = append(reasoningItems, map[string]any{
				"id":   respID + "_reasoning_0",
				"type": "reasoning",
				"summary": []any{
					map[string]any{
						"type": "summary_text",
						"text": choice.Message.ReasoningContent,
					},
				},
			})
		}

		if choice.Message.Content.Text != "" {
			msgContent = append(msgContent, map[string]any{
				"type":        "output_text",
				"text":        choice.Message.Content.Text,
				"annotations": []any{},
			})
		}

		for _, tc := range choice.Message.ToolCalls {
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        tc.ID,
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"status":    "completed",
			})
		}
	}

	// Reasoning items come first (so they precede the assistant message in
	// the output array), then the message item, then any function_call items.
	// This matches the first-party Responses API output ordering.
	final := make([]map[string]any, 0, len(reasoningItems)+len(output)+1)
	final = append(final, reasoningItems...)
	if len(msgContent) > 0 {
		final = append(final, map[string]any{
			"type":    "message",
			"id":      respID + "_msg_0",
			"status":  "completed",
			"role":    "assistant",
			"content": msgContent,
		})
	}
	final = append(final, output...)
	output = final

	usage := map[string]any{
		"input_tokens":  0,
		"output_tokens": 0,
		"total_tokens":  0,
	}
	if oaiResp.Usage != nil {
		usage["input_tokens"] = oaiResp.Usage.PromptTokens
		usage["output_tokens"] = oaiResp.Usage.CompletionTokens
		usage["total_tokens"] = oaiResp.Usage.TotalTokens
	}

	out := map[string]any{
		"id":     respID,
		"object": "response",
		"status": "completed",
		"model":  oaiResp.Model,
		"output": output,
		"usage":  usage,
	}

	return json.Marshal(out)
}

func (t *Responses2OpenAI) transformStreamingResponse(body []byte) ([]byte, error) {
	var id string
	var out bytes.Buffer
	var textContent string
	var messageStarted bool
	var msgItemSent bool
	var contentPartSent bool
	var reasoningContent string
	var reasoningItemSent bool
	var summaryPartSent bool
	const reasoningItemID = "rs_0" // sentinel; replaced with id-prefixed ID below
	var reasoningID string

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
			reasoningID = id + "_reasoning_0"
			messageStarted = true

			writeResponsesSSE(&out, "response.created", map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id":     id,
					"status": "in_progress",
				},
			})

			writeResponsesSSE(&out, "response.in_progress", map[string]any{
				"type": "response.in_progress",
				"response": map[string]any{
					"id":     id,
					"status": "in_progress",
				},
			})
		}

		for _, choice := range chunk.Choices {
			// Reasoning text streamed by an OpenAI Chat Completions upstream
			// (delta.reasoning_content) is surfaced as a Responses-style
			// reasoning item + summary_text.delta so the client (Cherry Studio,
			// codex-proxy, etc.) sees the same chain-of-thought it would from
			// the first-party Responses API. Without this the thinking block
			// gets silently dropped on the responses2openai hop.
			if choice.Delta.ReasoningContent != "" {
				reasoningContent += choice.Delta.ReasoningContent

				if !reasoningItemSent {
					reasoningItemSent = true
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": 0,
						"item": map[string]any{
							"id":      reasoningID,
							"type":    "reasoning",
							"summary": []any{},
						},
					})
				}
				if !summaryPartSent {
					summaryPartSent = true
					writeResponsesSSE(&out, "response.reasoning_summary_part.added", map[string]any{
						"type":          "response.reasoning_summary_part.added",
						"output_index":  0,
						"summary_index": 0,
						"item_id":       reasoningID,
						"part": map[string]any{
							"type": "summary_text",
							"text": "",
						},
					})
				}

				writeResponsesSSE(&out, "response.reasoning_summary_text.delta", map[string]any{
					"type":          "response.reasoning_summary_text.delta",
					"output_index":  0,
					"summary_index": 0,
					"item_id":       reasoningID,
					"delta":         choice.Delta.ReasoningContent,
				})
			}

			if choice.Delta.Content != "" {
				textContent += choice.Delta.Content

				if !msgItemSent {
					msgItemSent = true
					msgID := id + "_msg_0"
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": 0,
						"item": map[string]any{
							"id":      msgID,
							"type":    "message",
							"status":  "in_progress",
							"role":    "assistant",
							"content": []any{},
						},
					})
				}
				if !contentPartSent {
					contentPartSent = true
					writeResponsesSSE(&out, "response.content_part.added", map[string]any{
						"type":          "response.content_part.added",
						"output_index":  0,
						"content_index": 0,
						"item_id":       id + "_msg_0",
						"part": map[string]any{
							"type":        "output_text",
							"text":        "",
							"annotations": []any{},
						},
					})
				}

				writeResponsesSSE(&out, "response.output_text.delta", map[string]any{
					"type":          "response.output_text.delta",
					"delta":         choice.Delta.Content,
					"item_id":       id + "_msg_0",
					"output_index":  0,
					"content_index": 0,
				})
			}

			for _, tc := range choice.Delta.ToolCalls {
				if tc.ID != "" {
					if !msgItemSent {
						msgItemSent = true
						msgID := id + "_msg_0"
						writeResponsesSSE(&out, "response.output_item.added", map[string]any{
							"type":         "response.output_item.added",
							"output_index": 0,
							"item": map[string]any{
								"id":      msgID,
								"type":    "message",
								"status":  "in_progress",
								"role":    "assistant",
								"content": []any{},
							},
						})
					}
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": tc.Index + 1,
						"item": map[string]any{
							"type":    "function_call",
							"id":      tc.ID,
							"call_id": tc.ID,
							"name":    tc.Function.Name,
						},
					})
				}
				if tc.Function.Arguments != "" {
					writeResponsesSSE(&out, "response.function_call_arguments.delta", map[string]any{
						"type":         "response.function_call_arguments.delta",
						"delta":        tc.Function.Arguments,
						"call_id":      tc.ID,
						"output_index": tc.Index + 1,
					})
				}
			}

			if choice.FinishReason != nil {
				// Close the reasoning item if any reasoning text was streamed.
				if reasoningItemSent && summaryPartSent {
					writeResponsesSSE(&out, "response.reasoning_summary_text.done", map[string]any{
						"type":          "response.reasoning_summary_text.done",
						"output_index":  0,
						"summary_index": 0,
						"item_id":       reasoningID,
						"text":          reasoningContent,
					})
					writeResponsesSSE(&out, "response.reasoning_summary_part.done", map[string]any{
						"type":          "response.reasoning_summary_part.done",
						"output_index":  0,
						"summary_index": 0,
						"item_id":       reasoningID,
						"part": map[string]any{
							"type": "summary_text",
							"text": reasoningContent,
						},
					})
				}
				if reasoningItemSent {
					reasoningItem := map[string]any{
						"id":      reasoningID,
						"type":    "reasoning",
						"summary": []any{},
					}
					if reasoningContent != "" {
						reasoningItem["summary"] = []any{
							map[string]any{
								"type": "summary_text",
								"text": reasoningContent,
							},
						}
					}
					writeResponsesSSE(&out, "response.output_item.done", map[string]any{
						"type":         "response.output_item.done",
						"output_index": 0,
						"item":         reasoningItem,
					})
				}
				if textContent != "" {
					if contentPartSent {
						writeResponsesSSE(&out, "response.content_part.done", map[string]any{
							"type":          "response.content_part.done",
							"output_index":  0,
							"content_index": 0,
							"item_id":       id + "_msg_0",
							"part": map[string]any{
								"type":        "output_text",
								"text":        textContent,
								"annotations": []any{},
							},
						})
					}
					writeResponsesSSE(&out, "response.output_text.done", map[string]any{
						"type":          "response.output_text.done",
						"text":          textContent,
						"item_id":       id + "_msg_0",
						"output_index":  0,
						"content_index": 0,
					})
				}
				if msgItemSent {
					msgContent := []map[string]any{}
					if textContent != "" {
						msgContent = append(msgContent, map[string]any{
							"type":        "output_text",
							"text":        textContent,
							"annotations": []any{},
						})
					}
					writeResponsesSSE(&out, "response.output_item.done", map[string]any{
						"type":         "response.output_item.done",
						"output_index": 0,
						"item": map[string]any{
							"type":    "message",
							"id":      id + "_msg_0",
							"status":  "completed",
							"role":    "assistant",
							"content": msgContent,
						},
					})
				}

				writeResponsesSSE(&out, "response.completed", map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id":     id,
						"status": "completed",
						"usage":  map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
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
	ResponseID        string
	Model             string
	Created           bool
	Terminated        bool // set when finish-reason events have been emitted
	TextContent       string
	MessageItemSent   bool
	ContentPartSent   bool
	ToolCallAcc       map[int]*r2oStreamToolCall
	ReasoningContent  string
	ReasoningItemSent bool
	SummaryPartSent   bool
}

func (t *Responses2OpenAI) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	state := &r2oStreamState{}
	if existing, ok := ctx.Variables["r2o_stream"]; ok {
		state = existing.(*r2oStreamState)
	}
	defer func() { ctx.Variables["r2o_stream"] = state }()

	// Handle [DONE] marker — if already terminated by finish reason, skip.
	if string(bytes.TrimSpace(chunk.Data)) == "[DONE]" {
		if state.Terminated {
			return engine.SSEChunk{}, nil
		}
		if state.Created {
			var out bytes.Buffer
			if state.ReasoningItemSent {
				reasoningID := state.ResponseID + "_reasoning_0"
				if state.SummaryPartSent {
					writeResponsesSSE(&out, "response.reasoning_summary_text.done", map[string]any{
						"type":          "response.reasoning_summary_text.done",
						"output_index":  0,
						"summary_index": 0,
						"item_id":       reasoningID,
						"text":          state.ReasoningContent,
					})
					writeResponsesSSE(&out, "response.reasoning_summary_part.done", map[string]any{
						"type":          "response.reasoning_summary_part.done",
						"output_index":  0,
						"summary_index": 0,
						"item_id":       reasoningID,
						"part": map[string]any{
							"type": "summary_text",
							"text": state.ReasoningContent,
						},
					})
				}
				reasoningItem := map[string]any{
					"id":      reasoningID,
					"type":    "reasoning",
					"summary": []any{},
				}
				if state.ReasoningContent != "" {
					reasoningItem["summary"] = []any{
						map[string]any{
							"type": "summary_text",
							"text": state.ReasoningContent,
						},
					}
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item":         reasoningItem,
				})
			}
			if state.TextContent != "" {
				if state.ContentPartSent {
					writeResponsesSSE(&out, "response.content_part.done", map[string]any{
						"type":          "response.content_part.done",
						"output_index":  0,
						"content_index": 0,
						"item_id":       state.ResponseID + "_msg_0",
						"part": map[string]any{
							"type":        "output_text",
							"text":        state.TextContent,
							"annotations": []any{},
						},
					})
				}
				writeResponsesSSE(&out, "response.output_text.done", map[string]any{
					"type":          "response.output_text.done",
					"text":          state.TextContent,
					"item_id":       state.ResponseID + "_msg_0",
					"output_index":  0,
					"content_index": 0,
				})
			}
			if state.MessageItemSent {
				msgContent := []map[string]any{}
				if state.TextContent != "" {
					msgContent = append(msgContent, map[string]any{
						"type":        "output_text",
						"text":        state.TextContent,
						"annotations": []any{},
					})
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item": map[string]any{
						"type":    "message",
						"id":      state.ResponseID + "_msg_0",
						"status":  "completed",
						"role":    "assistant",
						"content": msgContent,
					},
				})
			}
			writeResponsesSSE(&out, "response.completed", map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":     state.ResponseID,
					"status": "completed",
					"usage":  map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
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

		writeResponsesSSE(&out, "response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":     state.ResponseID,
				"status": "in_progress",
			},
		})

		writeResponsesSSE(&out, "response.in_progress", map[string]any{
			"type": "response.in_progress",
			"response": map[string]any{
				"id":     state.ResponseID,
				"status": "in_progress",
			},
		})
	}

	for _, choice := range oaiChunk.Choices {
		// Reasoning text streamed by an OpenAI Chat Completions upstream
		// (delta.reasoning_content) is surfaced as a Responses-style
		// reasoning item + summary_text.delta. Without this the thinking
		// block gets silently dropped on the responses2openai hop.
		if choice.Delta.ReasoningContent != "" {
			state.ReasoningContent += choice.Delta.ReasoningContent
			reasoningID := state.ResponseID + "_reasoning_0"

			if !state.ReasoningItemSent {
				state.ReasoningItemSent = true
				writeResponsesSSE(&out, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": 0,
					"item": map[string]any{
						"id":      reasoningID,
						"type":    "reasoning",
						"summary": []any{},
					},
				})
			}
			if !state.SummaryPartSent {
				state.SummaryPartSent = true
				writeResponsesSSE(&out, "response.reasoning_summary_part.added", map[string]any{
					"type":          "response.reasoning_summary_part.added",
					"output_index":  0,
					"summary_index": 0,
					"item_id":       reasoningID,
					"part": map[string]any{
						"type": "summary_text",
						"text": "",
					},
				})
			}

			writeResponsesSSE(&out, "response.reasoning_summary_text.delta", map[string]any{
				"type":          "response.reasoning_summary_text.delta",
				"output_index":  0,
				"summary_index": 0,
				"item_id":       reasoningID,
				"delta":         choice.Delta.ReasoningContent,
			})
		}

		// Content delta
		if choice.Delta.Content != "" {
			state.TextContent += choice.Delta.Content

			if !state.MessageItemSent {
				state.MessageItemSent = true
				msgID := state.ResponseID + "_msg_0"
				writeResponsesSSE(&out, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": 0,
					"item": map[string]any{
						"id":      msgID,
						"type":    "message",
						"status":  "in_progress",
						"role":    "assistant",
						"content": []any{},
					},
				})
			}
			if !state.ContentPartSent {
				state.ContentPartSent = true
				writeResponsesSSE(&out, "response.content_part.added", map[string]any{
					"type":          "response.content_part.added",
					"output_index":  0,
					"content_index": 0,
					"item_id":       state.ResponseID + "_msg_0",
					"part": map[string]any{
						"type":        "output_text",
						"text":        "",
						"annotations": []any{},
					},
				})
			}

			writeResponsesSSE(&out, "response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"delta":         choice.Delta.Content,
				"item_id":       state.ResponseID + "_msg_0",
				"output_index":  0,
				"content_index": 0,
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
				if !state.MessageItemSent {
					state.MessageItemSent = true
					msgID := state.ResponseID + "_msg_0"
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": 0,
						"item": map[string]any{
							"id":      msgID,
							"type":    "message",
							"status":  "in_progress",
							"role":    "assistant",
							"content": []any{},
						},
					})
				}
				writeResponsesSSE(&out, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": tc.Index + 1,
					"item": map[string]any{
						"type":    "function_call",
						"id":      tc.ID,
						"call_id": tc.ID,
						"name":    tc.Function.Name,
					},
				})
			}
			if tc.Function.Arguments != "" {
				acc.Arguments += tc.Function.Arguments
				writeResponsesSSE(&out, "response.function_call_arguments.delta", map[string]any{
					"type":         "response.function_call_arguments.delta",
					"delta":        tc.Function.Arguments,
					"call_id":      acc.ID,
					"output_index": tc.Index + 1,
				})
			}
		}

		// Finish reason
		if choice.FinishReason != nil {
			// Close the reasoning item if any reasoning text was streamed.
			if state.ReasoningItemSent {
				reasoningID := state.ResponseID + "_reasoning_0"
				if state.SummaryPartSent {
					writeResponsesSSE(&out, "response.reasoning_summary_text.done", map[string]any{
						"type":          "response.reasoning_summary_text.done",
						"output_index":  0,
						"summary_index": 0,
						"item_id":       reasoningID,
						"text":          state.ReasoningContent,
					})
					writeResponsesSSE(&out, "response.reasoning_summary_part.done", map[string]any{
						"type":          "response.reasoning_summary_part.done",
						"output_index":  0,
						"summary_index": 0,
						"item_id":       reasoningID,
						"part": map[string]any{
							"type": "summary_text",
							"text": state.ReasoningContent,
						},
					})
				}
				reasoningItem := map[string]any{
					"id":      reasoningID,
					"type":    "reasoning",
					"summary": []any{},
				}
				if state.ReasoningContent != "" {
					reasoningItem["summary"] = []any{
						map[string]any{
							"type": "summary_text",
							"text": state.ReasoningContent,
						},
					}
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item":         reasoningItem,
				})
			}
			if state.TextContent != "" {
				if state.ContentPartSent {
					writeResponsesSSE(&out, "response.content_part.done", map[string]any{
						"type":          "response.content_part.done",
						"output_index":  0,
						"content_index": 0,
						"item_id":       state.ResponseID + "_msg_0",
						"part": map[string]any{
							"type":        "output_text",
							"text":        state.TextContent,
							"annotations": []any{},
						},
					})
				}
				writeResponsesSSE(&out, "response.output_text.done", map[string]any{
					"type":          "response.output_text.done",
					"text":          state.TextContent,
					"item_id":       state.ResponseID + "_msg_0",
					"output_index":  0,
					"content_index": 0,
				})
			}
			// Close tool call items
			for _, acc := range state.ToolCallAcc {
				writeResponsesSSE(&out, "response.function_call_arguments.done", map[string]any{
					"type":      "response.function_call_arguments.done",
					"call_id":   acc.ID,
					"name":      acc.Name,
					"arguments": acc.Arguments,
				})
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item": map[string]any{
						"type":    "function_call",
						"id":      acc.ID,
						"call_id": acc.ID,
						"status":  "completed",
					},
				})
			}
			if state.MessageItemSent {
				msgContent := []map[string]any{}
				if state.TextContent != "" {
					msgContent = append(msgContent, map[string]any{
						"type":        "output_text",
						"text":        state.TextContent,
						"annotations": []any{},
					})
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": 0,
					"item": map[string]any{
						"type":    "message",
						"id":      state.ResponseID + "_msg_0",
						"status":  "completed",
						"role":    "assistant",
						"content": msgContent,
					},
				})
			}
			writeResponsesSSE(&out, "response.completed", map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":     state.ResponseID,
					"status": "completed",
					"usage":  map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
				},
			})
			state.Terminated = true
		}
	}

	if out.Len() == 0 {
		return engine.SSEChunk{}, nil
	}
	return engine.SSEChunk{Data: out.Bytes()}, nil
}

// --- Helpers ---

// writeResponsesSSE writes a Responses-API SSE event. Framing is identical to
// Anthropic SSE (event: / data: / blank line), so it delegates.
func writeResponsesSSE(buf *bytes.Buffer, eventType string, data interface{}) {
	writeAnthropicSSE(buf, eventType, data)
}

// convertResponsesToolsToChatCompletions turns the Responses-API flat tool
// shape into the Chat Completions envelope shape.
//
// Responses API (Codex, OpenAI Responses, etc.):
//
//	{ "type": "function", "name": "...", "description": "...",
//	  "parameters": {...}, "strict": false }
//
// Chat Completions API:
//
//	{ "type": "function", "function": { "name": "...", "description": "...",
//	  "parameters": {...}, "strict": true|false } }
//
// If the input is already in Chat Completions envelope form (i.e. has a
// `function` key) it is preserved as-is. Tools that are neither the flat
// Responses shape nor a recognized function envelope — e.g. Codex-internal
// `type: "custom"`, `type: "namespace"`, `type: "tool_search"` — are
// dropped: Chat Completions has no equivalent concept and most servers
// reject unknown tool types with a 400.
func convertResponsesToolsToChatCompletions(raw json.RawMessage) []map[string]interface{} {
	var tools []map[string]interface{}
	if err := json.Unmarshal(raw, &tools); err != nil {
		// Could not parse as an array — return nil so the caller omits tools
		// entirely (preserves prior behavior on garbage input).
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, tool := range tools {
		converted, keep := convertResponsesToolToChatCompletions(tool)
		if keep {
			out = append(out, converted)
		}
	}
	return out
}

func convertResponsesToolToChatCompletions(tool map[string]interface{}) (map[string]interface{}, bool) {
	toolType, _ := tool["type"].(string)
	// Already in Chat Completions envelope form — preserve as long as it's a function.
	if _, hasFn := tool["function"]; hasFn {
		if toolType == "" || toolType == "function" {
			return tool, true
		}
		return nil, false
	}
	// Only the flat Responses shape with type:"function" can be converted;
	// any other type (custom, namespace, tool_search, ...) has no
	// Chat Completions equivalent and would be rejected by the downstream.
	if toolType != "function" {
		return nil, false
	}

	fn := map[string]interface{}{}
	if name, ok := tool["name"]; ok {
		fn["name"] = name
	}
	if desc, ok := tool["description"]; ok {
		fn["description"] = desc
	}
	if params, ok := tool["parameters"]; ok {
		fn["parameters"] = params
	}
	// Only forward `strict` when it is explicitly true. `strict: false` is
	// the Responses-API default and many OpenAI-compatible Chat Completions
	// servers (Cherry Studio, llama.cpp, etc.) reject it as an unknown
	// field. Omitting it matches the behavior of those servers and is
	// semantically equivalent to false.
	if strict, ok := tool["strict"]; ok {
		if b, isBool := strict.(bool); isBool && b {
			fn["strict"] = true
		}
	}

	return map[string]interface{}{
		"type":     "function",
		"function": fn,
	}, true
}

// convertResponsesToolChoiceToChatCompletions converts a Responses-API
// tool_choice value into the Chat Completions equivalent.
//
// String forms ("auto", "required", "none") are identical in both APIs and
// pass through unchanged. The object form differs:
//
//	Responses:   { "type": "function", "name": "shell_command" }
//	Chat Comp:   { "type": "function", "function": { "name": "shell_command" } }
//
// If the input cannot be parsed or doesn't match either known shape, it
// is returned verbatim so providers with custom tool_choice semantics
// (e.g. hosted classifiers) keep working.
func convertResponsesToolChoiceToChatCompletions(raw json.RawMessage) interface{} {
	// String form — both APIs share the same vocabulary.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Object form — Responses uses `name`, Chat Completions uses `function.name`.
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return json.RawMessage(raw) // pass through unchanged
	}
	if _, hasFn := obj["function"]; hasFn {
		// Already Chat Completions-shaped.
		return obj
	}
	if name, ok := obj["name"]; ok {
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": name},
		}
	}
	return obj
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*Responses2OpenAI)(nil)
var _ engine.ResponseTransformer = (*Responses2OpenAI)(nil)
var _ engine.StreamResponseTransformer = (*Responses2OpenAI)(nil)
