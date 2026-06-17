package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"tresor/internal/engine"
)

// Responses2Anthropic converts OpenAI Responses API requests to Anthropic Messages format
// and Anthropic Messages responses back to Responses API format.
type Responses2Anthropic struct{}

// reasoningEffortBudget maps Responses API reasoning effort to Anthropic thinking budget tokens.
var reasoningEffortBudget = map[string]int{
	"low":    1024,
	"medium": 8192,
	"high":   16000,
}

// PluginName returns the stable type name for deduplication.
func (t *Responses2Anthropic) PluginName() string { return "Responses2Anthropic" }

// --- TransformRequest: Responses API → Anthropic Messages ---

func (t *Responses2Anthropic) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var respReq responsesRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		return nil, nil, fmt.Errorf("responses2anthropic: failed to parse request: %w", err)
	}

	anthropicBody := map[string]interface{}{
		"model":      respReq.Model,
		"max_tokens": 8192,
		"stream":     respReq.Stream,
	}

	// Collect system instructions from instructions + system/developer input items
	systemParts := make([]string, 0)
	if respReq.Instructions != "" {
		systemParts = append(systemParts, respReq.Instructions)
	}

	// Build messages from input items
	anthroMessages := make([]map[string]interface{}, 0)

	if len(respReq.Input) > 0 {
		var items []responsesInputItemRaw
		if err := json.Unmarshal(respReq.Input, &items); err == nil {
			// Collect tool_use blocks to merge into preceding assistant message
			var pendingToolUses []map[string]interface{}

			flushToolUses := func() {
				if len(pendingToolUses) > 0 {
					if len(anthroMessages) > 0 {
						last := anthroMessages[len(anthroMessages)-1]
						if last["role"] == "assistant" {
							if content, ok := last["content"].([]map[string]interface{}); ok {
								last["content"] = append(content, pendingToolUses...)
							} else {
								last["content"] = pendingToolUses
							}
						} else {
							anthroMessages = append(anthroMessages, map[string]interface{}{
								"role":    "assistant",
								"content": pendingToolUses,
							})
						}
					} else {
						anthroMessages = append(anthroMessages, map[string]interface{}{
							"role":    "assistant",
							"content": pendingToolUses,
						})
					}
					pendingToolUses = nil
				}
			}

			// Collect tool_result blocks to merge into preceding user message
			flushToolResults := func(toolResults []map[string]interface{}) {
				if len(toolResults) > 0 {
					if len(anthroMessages) > 0 {
						last := anthroMessages[len(anthroMessages)-1]
						if last["role"] == "user" {
							if content, ok := last["content"].([]map[string]interface{}); ok {
								last["content"] = append(content, toolResults...)
							} else {
								last["content"] = toolResults
							}
						} else {
							anthroMessages = append(anthroMessages, map[string]interface{}{
								"role":    "user",
								"content": toolResults,
							})
						}
					} else {
						anthroMessages = append(anthroMessages, map[string]interface{}{
							"role":    "user",
							"content": toolResults,
						})
					}
				}
			}

			for _, item := range items {
				if item.Role != "" {
					flushToolUses()

					switch item.Role {
					case "system", "developer":
						// Collect for system field
						var s string
						if len(item.Content) > 0 {
							json.Unmarshal(item.Content, &s)
						}
						if s != "" {
							systemParts = append(systemParts, s)
						}

					case "user":
						msg := map[string]interface{}{
							"role": "user",
						}
						if len(item.Content) > 0 {
							var s string
							if err := json.Unmarshal(item.Content, &s); err == nil {
								msg["content"] = s
							} else {
								var parts []responsesContentPart
								if err := json.Unmarshal(item.Content, &parts); err == nil {
									blocks := make([]map[string]interface{}, 0, len(parts))
									for _, p := range parts {
										switch p.Type {
										case "input_text":
											blocks = append(blocks, map[string]interface{}{
												"type": "text",
												"text": p.Text,
											})
										case "input_image":
											blocks = append(blocks, map[string]interface{}{
												"type": "image",
												"source": map[string]interface{}{
													"type": "url",
													"url":  p.ImageURL,
												},
											})
										}
									}
									msg["content"] = blocks
								}
							}
						}
						anthroMessages = append(anthroMessages, msg)

					case "assistant":
						msg := map[string]interface{}{
							"role": "assistant",
						}
						if len(item.Content) > 0 {
							var s string
							if json.Unmarshal(item.Content, &s) == nil {
								msg["content"] = s
							}
						}
						anthroMessages = append(anthroMessages, msg)
					}
				} else if item.Type == "function_call" {
					var input interface{} = map[string]interface{}{}
					if item.Args != "" {
						json.Unmarshal([]byte(item.Args), &input)
					}
					toolUse := map[string]interface{}{
						"type":  "tool_use",
						"id":    item.CallID,
						"name":  item.Name,
						"input": input,
					}
					pendingToolUses = append(pendingToolUses, toolUse)
				} else if item.Type == "function_call_output" {
					flushToolUses()
					toolResults := []map[string]interface{}{
						{
							"type":        "tool_result",
							"tool_use_id": item.CallID,
							"content":     item.Output,
						},
					}
					flushToolResults(toolResults)
				}
			}
			flushToolUses()
		} else {
			// Handle string input: the Responses API allows input to be a plain string
			var inputStr string
			if err := json.Unmarshal(respReq.Input, &inputStr); err == nil && inputStr != "" {
				anthroMessages = append(anthroMessages, map[string]interface{}{
					"role":    "user",
					"content": inputStr,
				})
			}
		}
	}

	// Set system field
	if len(systemParts) > 0 {
		systemText := ""
		for i, p := range systemParts {
			if i > 0 {
				systemText += "\n\n"
			}
			systemText += p
		}
		anthropicBody["system"] = systemText
	}

	// Ensure at least one message (Anthropic requires this)
	if len(anthroMessages) == 0 {
		anthroMessages = append(anthroMessages, map[string]interface{}{
			"role":    "user",
			"content": "Hello",
		})
	}
	anthropicBody["messages"] = anthroMessages

	// Convert tools: OpenAI → Anthropic format
	if len(respReq.Tools) > 0 {
		var openaiTools []map[string]interface{}
		if err := json.Unmarshal(respReq.Tools, &openaiTools); err == nil {
			anthroTools := make([]map[string]interface{}, 0, len(openaiTools))
			for _, ot := range openaiTools {
				fn, _ := ot["function"].(map[string]interface{})
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				desc, _ := fn["description"].(string)
				params := fn["parameters"]
				anthroTools = append(anthroTools, map[string]interface{}{
					"name":         name,
					"description":  desc,
					"input_schema": params,
				})
			}
			if len(anthroTools) > 0 {
				anthropicBody["tools"] = anthroTools
			}
		}
	}

	// Convert tool_choice
	if len(respReq.ToolChoice) > 0 {
		var tcRaw json.RawMessage
		if err := json.Unmarshal(respReq.ToolChoice, &tcRaw); err == nil {
			var tcStr string
			if json.Unmarshal(tcRaw, &tcStr) == nil {
				switch tcStr {
				case "auto":
					anthropicBody["tool_choice"] = map[string]interface{}{"type": "auto"}
				case "required", "any":
					anthropicBody["tool_choice"] = map[string]interface{}{"type": "any"}
				case "none":
					// Omit tool_choice
				default:
					anthropicBody["tool_choice"] = tcStr
				}
			} else {
				// Object format: {type: "function", name: "..."}
				var tcObj struct {
					Type     string `json:"type"`
					Function *struct {
						Name string `json:"name"`
					} `json:"function,omitempty"`
					Name string `json:"name,omitempty"`
				}
				if json.Unmarshal(tcRaw, &tcObj) == nil {
					name := tcObj.Name
					if tcObj.Function != nil && name == "" {
						name = tcObj.Function.Name
					}
					if tcObj.Type == "function" && name != "" {
						anthropicBody["tool_choice"] = map[string]interface{}{
							"type": "tool",
							"name": name,
						}
					}
				}
			}
		}
	}

	// Convert reasoning effort → thinking
	if respReq.Reasoning != nil && respReq.Reasoning.Effort != "" {
		budget, ok := reasoningEffortBudget[respReq.Reasoning.Effort]
		if !ok {
			budget = 8192
		}
		anthropicBody["thinking"] = map[string]interface{}{
			"type":         "enabled",
			"budget_tokens": budget,
		}
	}

	newBody, err := json.Marshal(anthropicBody)
	if err != nil {
		return nil, nil, fmt.Errorf("responses2anthropic: failed to marshal request: %w", err)
	}

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	newReq.URL.Path = "/v1/messages"

	if ctx.TargetDownstream != nil {
		newReq.Header.Set("x-api-key", ctx.TargetDownstream.APIKey)
		newReq.Header.Set("anthropic-version", "2023-06-01")
	}

	return newReq, newBody, nil
}

// --- TransformResponse: Anthropic Messages → Responses API ---

func (t *Responses2Anthropic) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "text/event-stream" {
		return t.transformStreamingResponse(body)
	}
	return t.transformJSONResponse(body)
}

func (t *Responses2Anthropic) transformJSONResponse(body []byte) ([]byte, error) {
	var anthropicResp anthropicResponse
	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		return body, nil
	}

	respID := anthropicResp.ID
	if respID == "" {
		respID = fmt.Sprintf("resp_%s", anthropicResp.ID)
	}

	output := make([]map[string]any, 0)
	msgContent := make([]map[string]any, 0)

	for _, c := range anthropicResp.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				msgContent = append(msgContent, map[string]any{
					"type":        "output_text",
					"text":        c.Text,
					"annotations": []any{},
				})
			}
		case "tool_use":
			args := "{}"
			if c.Input != nil {
				args = string(c.Input)
			}
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        c.ID,
				"call_id":   c.ID,
				"name":      c.Name,
				"arguments": args,
				"status":    "completed",
			})
		}
	}

	if len(msgContent) > 0 {
		msgItem := map[string]any{
			"type":    "message",
			"id":      respID + "_msg_0",
			"status":  "completed",
			"role":    "assistant",
			"content": msgContent,
		}
		output = append([]map[string]any{msgItem}, output...)
	}

	usage := map[string]any{
		"input_tokens":  anthropicResp.Usage.InputTokens,
		"output_tokens": anthropicResp.Usage.OutputTokens,
		"total_tokens":  anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
	}

	out := map[string]any{
		"id":     respID,
		"object": "response",
		"status": "completed",
		"model":  anthropicResp.Model,
		"output": output,
		"usage":  usage,
	}

	return json.Marshal(out)
}

func (t *Responses2Anthropic) transformStreamingResponse(body []byte) ([]byte, error) {
	var id string
	var out bytes.Buffer
	var textContent string
	var msgItemSent bool
	var contentPartSent bool
	var toolOutputIdx int = 1
	openToolCalls := make(map[string]*r2aStreamToolCall)

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
				return true
			}
			id = msg.Message.ID

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

		case "content_block_start":
			var block struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					Text string `json:"text,omitempty"`
					ID   string `json:"id,omitempty"`
					Name string `json:"name,omitempty"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(data, &block); err != nil {
				return true
			}
			switch block.ContentBlock.Type {
			case "tool_use":
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
				oidx := toolOutputIdx
				toolOutputIdx++
				tc := &r2aStreamToolCall{
					CallID: block.ContentBlock.ID,
					Name:   block.ContentBlock.Name,
				}
				openToolCalls[block.ContentBlock.ID] = tc
				writeResponsesSSE(&out, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": oidx,
					"item": map[string]any{
						"type":    "function_call",
						"id":      block.ContentBlock.ID,
						"call_id": block.ContentBlock.ID,
						"name":    block.ContentBlock.Name,
					},
				})
			}

		case "content_block_delta":
			var delta struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text,omitempty"`
					PartialJSON string `json:"partial_json,omitempty"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(data, &delta); err != nil {
				return true
			}
			switch delta.Delta.Type {
			case "text_delta":
				if delta.Delta.Text != "" {
					textContent += delta.Delta.Text

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
						"type":  "response.output_text.delta",
						"delta": delta.Delta.Text,
					})
				}
			case "input_json_delta":
				if delta.Delta.PartialJSON != "" {
					for _, tc := range openToolCalls {
						tc.Arguments += delta.Delta.PartialJSON
						writeResponsesSSE(&out, "response.function_call_arguments.delta", map[string]any{
							"type":         "response.function_call_arguments.delta",
							"delta":        delta.Delta.PartialJSON,
							"call_id":      tc.CallID,
							"output_index": delta.Index + 1,
						})
						break
					}
				}
			}

		case "content_block_stop":
			for _, tc := range openToolCalls {
				writeResponsesSSE(&out, "response.function_call_arguments.done", map[string]any{
					"type":      "response.function_call_arguments.done",
					"call_id":   tc.CallID,
					"name":      tc.Name,
					"arguments": tc.Arguments,
				})
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type": "response.output_item.done",
					"item": map[string]any{
						"type":    "function_call",
						"id":      tc.CallID,
						"call_id": tc.CallID,
						"status":  "completed",
					},
				})
			}
			openToolCalls = make(map[string]*r2aStreamToolCall)

		case "message_delta":

		case "message_stop":
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
					"type": "response.output_text.done",
					"text": textContent,
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
		return true
	})

	return out.Bytes(), nil
}

// --- StreamResponseTransformer: per-chunk Anthropic SSE → Responses API ---

type r2aStreamToolCall struct {
	CallID    string
	Name      string
	Arguments string
	ItemSent  bool
}

type r2aStreamState struct {
	ResponseID      string
	Model           string
	Created         bool
	TextContent     string
	TextBlockOpen   bool
	MessageItemSent bool
	ContentPartSent bool
	ToolOutputIdx   int
	ToolCallAcc     map[string]*r2aStreamToolCall
}

func (t *Responses2Anthropic) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	state := &r2aStreamState{}
	if existing, ok := ctx.Variables["r2a_stream"]; ok {
		state = existing.(*r2aStreamState)
	}
	defer func() { ctx.Variables["r2a_stream"] = state }()

	var out bytes.Buffer

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
		state.ResponseID = msg.Message.ID
		state.Model = msg.Message.Model
		state.Created = true
		state.ToolCallAcc = make(map[string]*r2aStreamToolCall)
		state.ToolOutputIdx = 1

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

	case "content_block_start":
		var block struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				Text string `json:"text,omitempty"`
				ID   string `json:"id,omitempty"`
				Name string `json:"name,omitempty"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(chunk.Data, &block); err != nil {
			return chunk, nil
		}
		switch block.ContentBlock.Type {
		case "text":
			state.TextBlockOpen = true
			if block.ContentBlock.Text != "" {
				state.TextContent += block.ContentBlock.Text
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
					"type":  "response.output_text.delta",
					"delta": block.ContentBlock.Text,
				})
			}
		case "tool_use":
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
			oidx := state.ToolOutputIdx
			state.ToolOutputIdx++
			tc := &r2aStreamToolCall{
				CallID: block.ContentBlock.ID,
				Name:   block.ContentBlock.Name,
			}
			state.ToolCallAcc[block.ContentBlock.ID] = tc
			writeResponsesSSE(&out, "response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": oidx,
				"item": map[string]any{
					"type":    "function_call",
					"id":      block.ContentBlock.ID,
					"call_id": block.ContentBlock.ID,
					"name":    block.ContentBlock.Name,
				},
			})
		}

	case "content_block_delta":
		var delta struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text,omitempty"`
				PartialJSON string `json:"partial_json,omitempty"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &delta); err != nil {
			return chunk, nil
		}
		switch delta.Delta.Type {
		case "text_delta":
			if delta.Delta.Text != "" {
				state.TextContent += delta.Delta.Text
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
					"type":  "response.output_text.delta",
					"delta": delta.Delta.Text,
				})
			}
		case "input_json_delta":
			if delta.Delta.PartialJSON != "" {
				for _, tc := range state.ToolCallAcc {
					tc.Arguments += delta.Delta.PartialJSON
					writeResponsesSSE(&out, "response.function_call_arguments.delta", map[string]any{
						"type":         "response.function_call_arguments.delta",
						"delta":        delta.Delta.PartialJSON,
						"call_id":      tc.CallID,
						"output_index": delta.Index + 1,
					})
					break
				}
			}
		}

	case "content_block_stop":
		for _, tc := range state.ToolCallAcc {
			writeResponsesSSE(&out, "response.function_call_arguments.done", map[string]any{
				"type":      "response.function_call_arguments.done",
				"call_id":   tc.CallID,
				"name":      tc.Name,
				"arguments": tc.Arguments,
			})
			writeResponsesSSE(&out, "response.output_item.done", map[string]any{
				"type": "response.output_item.done",
				"item": map[string]any{
					"type":    "function_call",
					"id":      tc.CallID,
					"call_id": tc.CallID,
					"status":  "completed",
				},
			})
		}
		state.ToolCallAcc = make(map[string]*r2aStreamToolCall)
		state.TextBlockOpen = false

	case "message_delta":

	case "message_stop":
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
				"type": "response.output_text.done",
				"text": state.TextContent,
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

	default:
		return engine.SSEChunk{}, nil
	}

	if out.Len() == 0 {
		return engine.SSEChunk{}, nil
	}
	return engine.SSEChunk{Data: out.Bytes()}, nil
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*Responses2Anthropic)(nil)
var _ engine.ResponseTransformer = (*Responses2Anthropic)(nil)
var _ engine.StreamResponseTransformer = (*Responses2Anthropic)(nil)
