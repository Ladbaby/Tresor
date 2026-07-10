package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	// Args holds the raw JSON bytes of function_call `arguments`. Per
	// the OpenAI Responses API spec the value is a JSON-encoded string,
	// but a permissive decoder is required because (a) Codex's replay
	// path has been seen sending a malformed concatenation of two
	// objects, and (b) we don't want a non-string `arguments` value to
	// poison the outer json.Unmarshal of the input array and silently
	// drop every other item in the request. Plugins that need to
	// forward the bytes as a JSON string (Chat Completions) call
	// string(Args); plugins that need to inspect or replace the inner
	// value (Anthropic Messages) parse the bytes themselves.
	Args             json.RawMessage `json:"arguments,omitempty"`
	Output           string          `json:"output,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
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
					// Chat Completions `function.arguments` is a JSON
					// string. Cast the RawMessage verbatim — for a string
					// value this yields the original JSON text; for a
					// non-object JSON value (e.g. [1,2,3]) it yields the
					// bracketed form, which Chat Completions will pass
					// through to the model as-is.
					tc.Function.Arguments = string(item.Args)
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
	var reasoningID string

	// Per-item output_index tracking. The Responses API requires every
	// output item to have a stable output_index that matches between its
	// `.added` and `.done` events. The previous implementation always
	// used output_index 0 in the close events, which caused Codex (and
	// any other strict Responses-API client) to drop the function_call
	// item because it shared its index with the assistant message.
	var reasoningOutputIdx, messageOutputIdx int
	var nextOutputIdx int
	// sequence_number is a monotonically increasing integer that the
	// OpenAI Responses API assigns to every SSE event. The official
	// OpenAI SDKs (openai-node, openai-python) require it on every event
	// type. We track it locally here for the buffered path; the
	// per-chunk path (TransformStreamChunk) tracks the same counter in
	// its state struct.
	var seq int
	toolCallAcc := make(map[int]*r2oStreamToolCall)
	toolCallOutputIdx := make(map[int]int)
	// finalItems is keyed by output_index. Populated when each
	// output_item.done event is written, then replayed into the
	// `response.output` array of the response.completed payload so the
	// OpenAI Responses SDK can construct a non-empty ParsedResponse.
	// Without this, the SDK returns an empty response to the client —
	// Codex drops the assistant turn, fails to reconstruct conversation
	// history on the next turn, and silently hangs after the first reply.
	finalItems := make(map[int]map[string]any)

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
					reasoningOutputIdx = nextOutputIdx
					nextOutputIdx++
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": reasoningOutputIdx,
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
						"output_index":  reasoningOutputIdx,
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
					"output_index":  reasoningOutputIdx,
					"summary_index": 0,
					"item_id":       reasoningID,
					"delta":         choice.Delta.ReasoningContent,
				})
			}

			if choice.Delta.Content != "" {
				textContent += choice.Delta.Content

				if !msgItemSent {
					msgItemSent = true
					messageOutputIdx = nextOutputIdx
					nextOutputIdx++
					msgID := id + "_msg_0"
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": messageOutputIdx,
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
						"output_index":  messageOutputIdx,
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
					"output_index":  messageOutputIdx,
					"content_index": 0,
				})
			}

			for _, tc := range choice.Delta.ToolCalls {
				acc, exists := toolCallAcc[tc.Index]
				if !exists {
					acc = &r2oStreamToolCall{
						ID:   tc.ID,
						Name: tc.Function.Name,
					}
					toolCallAcc[tc.Index] = acc
				}
				if tc.ID != "" && !acc.ItemSent {
					acc.ItemSent = true
					acc.Name = tc.Function.Name
					// Synthesize an empty assistant message if the
					// upstream emitted a tool_call without any text delta.
					// The Responses API expects every function_call to
					// come from a message item, matching first-party
					// OpenAI behavior.
					if !msgItemSent {
						msgItemSent = true
						messageOutputIdx = nextOutputIdx
						nextOutputIdx++
						msgID := id + "_msg_0"
						writeResponsesSSE(&out, "response.output_item.added", map[string]any{
							"type":         "response.output_item.added",
							"output_index": messageOutputIdx,
							"item": map[string]any{
								"id":      msgID,
								"type":    "message",
								"status":  "in_progress",
								"role":    "assistant",
								"content": []any{},
							},
						})
					}
					if _, seen := toolCallOutputIdx[tc.Index]; !seen {
						toolCallOutputIdx[tc.Index] = nextOutputIdx
						nextOutputIdx++
					}
					// The function_call item MUST include an `arguments`
					// field even on the `added` event (the value is "").
					// Codex's ResponseItem::FunctionCall deserializer marks
					// `arguments` as a non-optional String; if it is
					// missing, serde returns Err, Codex logs a debug
					// message, and the entire tool call is silently
					// dropped. See codex-rs/protocol/src/models.rs:988.
					// We also include `status: "in_progress"` and the
					// `item.id` (== `item.call_id`) so subsequent
					// `function_call_arguments.delta` and `.done` events
					// can be correlated by `item_id`.
					seq++
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":            "response.output_item.added",
						"output_index":    toolCallOutputIdx[tc.Index],
						"sequence_number": seq,
						"item": map[string]any{
							"type":      "function_call",
							"id":        tc.ID,
							"call_id":   tc.ID,
							"name":      tc.Function.Name,
							"arguments": "",
							"status":    "in_progress",
						},
					})
				}
				if tc.Function.Arguments != "" {
					seq++
					writeResponsesSSE(&out, "response.function_call_arguments.delta", map[string]any{
						"type":            "response.function_call_arguments.delta",
						"delta":           tc.Function.Arguments,
						"item_id":         tc.ID,
						"output_index":    toolCallOutputIdx[tc.Index],
						"sequence_number": seq,
					})
				}
			}

			if choice.FinishReason != nil {
				// Close the reasoning item if any reasoning text was streamed.
				if reasoningItemSent && summaryPartSent {
					writeResponsesSSE(&out, "response.reasoning_summary_text.done", map[string]any{
						"type":          "response.reasoning_summary_text.done",
						"output_index":  reasoningOutputIdx,
						"summary_index": 0,
						"item_id":       reasoningID,
						"text":          reasoningContent,
					})
					writeResponsesSSE(&out, "response.reasoning_summary_part.done", map[string]any{
						"type":          "response.reasoning_summary_part.done",
						"output_index":  reasoningOutputIdx,
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
						"output_index": reasoningOutputIdx,
						"item":         reasoningItem,
					})
					finalItems[reasoningOutputIdx] = reasoningItem
				}
				if textContent != "" {
					if contentPartSent {
						writeResponsesSSE(&out, "response.content_part.done", map[string]any{
							"type":          "response.content_part.done",
							"output_index":  messageOutputIdx,
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
						"output_index":  messageOutputIdx,
						"content_index": 0,
					})
				}
				// Close tool call items in their original index order so
				// the .done events match the order of the .added events.
				toolIndices := make([]int, 0, len(toolCallAcc))
				for idx := range toolCallAcc {
					toolIndices = append(toolIndices, idx)
				}
				sort.Ints(toolIndices)
				for _, idx := range toolIndices {
					acc := toolCallAcc[idx]
					seq++
					writeResponsesSSE(&out, "response.function_call_arguments.done", map[string]any{
						"type":            "response.function_call_arguments.done",
						"item_id":         acc.ID,
						"output_index":    toolCallOutputIdx[idx],
						"name":            acc.Name,
						"arguments":       acc.Arguments,
						"sequence_number": seq,
					})
					seq++
					writeResponsesSSE(&out, "response.output_item.done", map[string]any{
						"type":            "response.output_item.done",
						"output_index":    toolCallOutputIdx[idx],
						"sequence_number": seq,
						"item": map[string]any{
							"type":      "function_call",
							"id":        acc.ID,
							"call_id":   acc.ID,
							"name":      acc.Name,
							"arguments": acc.Arguments,
							"status":    "completed",
						},
					})
					finalItems[toolCallOutputIdx[idx]] = map[string]any{
						"type":      "function_call",
						"id":        acc.ID,
						"call_id":   acc.ID,
						"name":      acc.Name,
						"arguments": acc.Arguments,
						"status":    "completed",
					}
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
					msgItem := map[string]any{
						"type":    "message",
						"id":      id + "_msg_0",
						"status":  "completed",
						"role":    "assistant",
						"content": msgContent,
					}
					writeResponsesSSE(&out, "response.output_item.done", map[string]any{
						"type":         "response.output_item.done",
						"output_index": messageOutputIdx,
						"item":         msgItem,
					})
					finalItems[messageOutputIdx] = msgItem
				}

				// Replay the final `output` array (in output_index order) into
				// response.completed so the OpenAI Responses SDK can build a
				// non-empty ParsedResponse. Without this, the SDK returns
				// an empty response — Codex drops the assistant turn, fails
				// to reconstruct conversation history on the next turn, and
				// silently hangs after the first reply.
				output := make([]map[string]any, 0, len(finalItems))
				for i := 0; i < nextOutputIdx; i++ {
					if item, ok := finalItems[i]; ok {
						output = append(output, item)
					}
				}

				writeResponsesSSE(&out, "response.completed", map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id":     id,
						"status": "completed",
						"output": output,
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
	MessageOutputIdx  int // output_index used when the message item was added
	ContentPartSent   bool
	ToolCallAcc       map[int]*r2oStreamToolCall
	ToolCallOutputIdx map[int]int // tool_call index → output_index used when added
	ReasoningContent  string
	ReasoningItemSent bool
	ReasoningOutputIdx int // output_index used when the reasoning item was added
	SummaryPartSent   bool
	// nextOutputIdx is the next free index in the output[] array. We
	// allocate it lazily when a new item is first emitted so the
	// streaming case (where we don't know in advance which items the
	// model will produce) still produces monotonically-increasing
	// output_index values that match between `added` and `done` events.
	nextOutputIdx int
	// FinalItems stores the completed output items (one per output_index)
	// emitted via response.output_item.done events. It is replayed into
	// the response.completed payload as `response.output` so the OpenAI
	// Responses SDK can build a non-empty ParsedResponse from the
	// authoritative `response` field. Without this, the SDK returns an
	// empty response to the client — Codex drops the assistant turn,
	// fails to reconstruct conversation history on the next turn, and
	// silently hangs after the first reply.
	FinalItems map[int]map[string]any
	// sequence_number is a monotonically increasing integer that
	// accompanies every Responses-API SSE event. The official OpenAI
	// SDKs (openai-node, openai-python) mark it as a required field on
	// every event type. Codex's deserializer does not require it, but
	// other Responses-API clients do, and including it costs nothing.
	// It starts at 0 and increments by 1 per emitted event.
	sequenceNumber int
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
						"output_index":  state.ReasoningOutputIdx,
						"summary_index": 0,
						"item_id":       reasoningID,
						"text":          state.ReasoningContent,
					})
					writeResponsesSSE(&out, "response.reasoning_summary_part.done", map[string]any{
						"type":          "response.reasoning_summary_part.done",
						"output_index":  state.ReasoningOutputIdx,
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
					"output_index": state.ReasoningOutputIdx,
					"item":         reasoningItem,
				})
				state.FinalItems[state.ReasoningOutputIdx] = reasoningItem
			}
			if state.TextContent != "" {
				if state.ContentPartSent {
					writeResponsesSSE(&out, "response.content_part.done", map[string]any{
						"type":          "response.content_part.done",
						"output_index":  state.MessageOutputIdx,
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
					"output_index":  state.MessageOutputIdx,
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
				msgItem := map[string]any{
					"type":    "message",
					"id":      state.ResponseID + "_msg_0",
					"status":  "completed",
					"role":    "assistant",
					"content": msgContent,
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": state.MessageOutputIdx,
					"item":         msgItem,
				})
				state.FinalItems[state.MessageOutputIdx] = msgItem
			}
			// Replay the final `output` array (in output_index order) into
			// response.completed so the OpenAI Responses SDK can build a
			// non-empty ParsedResponse from the authoritative `response`
			// payload. Without this, the SDK returns an empty response —
			// Codex drops the assistant turn, fails to reconstruct
			// conversation history on the next turn, and silently hangs
			// after the first reply.
			output := make([]map[string]any, 0, len(state.FinalItems))
			for i := 0; i < state.nextOutputIdx; i++ {
				if item, ok := state.FinalItems[i]; ok {
					output = append(output, item)
				}
			}
			writeResponsesSSE(&out, "response.completed", map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":     state.ResponseID,
					"status": "completed",
					"output": output,
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
		state.ToolCallOutputIdx = make(map[int]int)
		state.FinalItems = make(map[int]map[string]any)

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
				// Reasoning is always the first output item if present, per
				// the first-party Responses API ordering (reasoning →
				// message → tool_calls). Use the next available index
				// (which is 0 unless something else has already claimed it,
				// e.g. when the downstream emits reasoning after a prior
				// message in the same chunk).
				state.ReasoningOutputIdx = state.nextOutputIdx
				state.nextOutputIdx++
				writeResponsesSSE(&out, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": state.ReasoningOutputIdx,
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
					"output_index":  state.ReasoningOutputIdx,
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
				"output_index":  state.ReasoningOutputIdx,
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
				// Message goes after reasoning (if any). The previous
				// implementation always used output_index 0, which
				// collided with reasoning and broke Codex's stream
				// reconstruction.
				state.MessageOutputIdx = state.nextOutputIdx
				state.nextOutputIdx++
				writeResponsesSSE(&out, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": state.MessageOutputIdx,
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
					"output_index":  state.MessageOutputIdx,
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
				"output_index":  state.MessageOutputIdx,
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
				// Some Chat Completions upstreams (e.g. older llama.cpp
				// builds) emit a tool_call without any preceding message
				// delta. The Responses API expects every function_call to
				// be accompanied by a message item, so we synthesize an
				// empty message at the right output_index first. This also
				// matches the first-party OpenAI behavior, where tool
				// calls always come from an assistant message.
				if !state.MessageItemSent {
					state.MessageItemSent = true
					state.MessageOutputIdx = state.nextOutputIdx
					state.nextOutputIdx++
					msgID := state.ResponseID + "_msg_0"
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": state.MessageOutputIdx,
						"item": map[string]any{
							"id":      msgID,
							"type":    "message",
							"status":  "in_progress",
							"role":    "assistant",
							"content": []any{},
						},
					})
				}
				// Each tool call gets its own output_index, allocated
				// lazily from the next free slot. We can't use tc.Index
				// because tool calls may not arrive in dense 0..N order
				// and the Responses API requires monotonically
				// increasing indices.
				if _, seen := state.ToolCallOutputIdx[tc.Index]; !seen {
					state.ToolCallOutputIdx[tc.Index] = state.nextOutputIdx
					state.nextOutputIdx++
				}
				state.sequenceNumber++
				writeResponsesSSE(&out, "response.output_item.added", map[string]any{
					"type":            "response.output_item.added",
					"output_index":    state.ToolCallOutputIdx[tc.Index],
					"sequence_number": state.sequenceNumber,
					"item": map[string]any{
						"type":      "function_call",
						"id":        tc.ID,
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": "",
						"status":    "in_progress",
					},
				})
			}
			if tc.Function.Arguments != "" {
				acc.Arguments += tc.Function.Arguments
				state.sequenceNumber++
				writeResponsesSSE(&out, "response.function_call_arguments.delta", map[string]any{
					"type":            "response.function_call_arguments.delta",
					"delta":           tc.Function.Arguments,
					"item_id":         acc.ID,
					"output_index":    state.ToolCallOutputIdx[tc.Index],
					"sequence_number": state.sequenceNumber,
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
						"output_index":  state.ReasoningOutputIdx,
						"summary_index": 0,
						"item_id":       reasoningID,
						"text":          state.ReasoningContent,
					})
					writeResponsesSSE(&out, "response.reasoning_summary_part.done", map[string]any{
						"type":          "response.reasoning_summary_part.done",
						"output_index":  state.ReasoningOutputIdx,
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
					"output_index": state.ReasoningOutputIdx,
					"item":         reasoningItem,
				})
				state.FinalItems[state.ReasoningOutputIdx] = reasoningItem
			}
			if state.TextContent != "" {
				if state.ContentPartSent {
					writeResponsesSSE(&out, "response.content_part.done", map[string]any{
						"type":          "response.content_part.done",
						"output_index":  state.MessageOutputIdx,
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
					"output_index":  state.MessageOutputIdx,
					"content_index": 0,
				})
			}
			// Close tool call items in their original output_index order
			// so the .done events are emitted in the same order as the
			// .added events. Using a slice of indices keeps the order
			// deterministic regardless of Go's map iteration order.
			toolIndices := make([]int, 0, len(state.ToolCallAcc))
			for idx := range state.ToolCallAcc {
				toolIndices = append(toolIndices, idx)
			}
			sort.Ints(toolIndices)
			for _, idx := range toolIndices {
				acc := state.ToolCallAcc[idx]
				state.sequenceNumber++
				writeResponsesSSE(&out, "response.function_call_arguments.done", map[string]any{
					"type":            "response.function_call_arguments.done",
					"item_id":         acc.ID,
					"output_index":    state.ToolCallOutputIdx[idx],
					"name":            acc.Name,
					"arguments":       acc.Arguments,
					"sequence_number": state.sequenceNumber,
				})
				state.sequenceNumber++
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":            "response.output_item.done",
					"output_index":    state.ToolCallOutputIdx[idx],
					"sequence_number": state.sequenceNumber,
					"item": map[string]any{
						"type":      "function_call",
						"id":        acc.ID,
						"call_id":   acc.ID,
						"name":      acc.Name,
						"arguments": acc.Arguments,
						"status":    "completed",
					},
				})
				state.FinalItems[state.ToolCallOutputIdx[idx]] = map[string]any{
					"type":      "function_call",
					"id":        acc.ID,
					"call_id":   acc.ID,
					"name":      acc.Name,
					"arguments": acc.Arguments,
					"status":    "completed",
				}
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
				msgItem := map[string]any{
					"type":    "message",
					"id":      state.ResponseID + "_msg_0",
					"status":  "completed",
					"role":    "assistant",
					"content": msgContent,
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": state.MessageOutputIdx,
					"item":         msgItem,
				})
				state.FinalItems[state.MessageOutputIdx] = msgItem
			}
			// Replay the final `output` array (in output_index order) into
			// response.completed so the OpenAI Responses SDK can build a
			// non-empty ParsedResponse. Without this, the SDK returns an
			// empty response — Codex drops the assistant turn, fails to
			// reconstruct conversation history on the next turn, and
			// silently hangs after the first reply.
			output := make([]map[string]any, 0, len(state.FinalItems))
			for i := 0; i < state.nextOutputIdx; i++ {
				if item, ok := state.FinalItems[i]; ok {
					output = append(output, item)
				}
			}
			writeResponsesSSE(&out, "response.completed", map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":     state.ResponseID,
					"status": "completed",
					"output": output,
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
//
// Callers are responsible for including a `sequence_number` field on the
// payload (the official OpenAI SDKs mark this required on every
// Responses-API event). The per-chunk streaming transformer tracks a
// monotonic counter in its state; the buffered transformer has its own
// local counter. This helper is intentionally stateless so it can be
// shared between the two paths.
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
