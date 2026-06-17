package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tresor/internal/engine"
)

// OpenAI2Responses converts OpenAI Chat Completion requests to Responses API format
// and Responses API responses back to Chat Completion format.
type OpenAI2Responses struct{}

// --- Streaming state types ---

type o2rStreamState struct {
	ResponseID string
	Model      string
	Created    int64
	sentRole   bool
	ToolCallID string // current tool call ID being accumulated
	ToolName   string // current tool call name
}

// PluginName returns the stable type name for deduplication.
func (t *OpenAI2Responses) PluginName() string { return "OpenAI2Responses" }

// --- TransformRequest: Chat Completions → Responses API ---

func (t *OpenAI2Responses) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var request map[string]interface{}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, nil, fmt.Errorf("openai2responses: failed to parse request: %w", err)
	}

	downstream := ctx.TargetDownstream
	respBody := make(map[string]interface{})

	// Copy model and stream
	if model, ok := request["model"].(string); ok {
		respBody["model"] = model
	}
	if stream, ok := request["stream"]; ok {
		respBody["stream"] = stream
	}

	// Process messages
	var instructions string
	var inputItems []map[string]interface{}

	if messages, ok := request["messages"].([]interface{}); ok {
		for _, msg := range messages {
			m, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)

			switch role {
			case "system":
				content := extractStringContent(m["content"])
				if instructions != "" {
					instructions += "\n\n" + content
				} else {
					instructions = content
				}
			case "user":
				items := buildRoleInputItem("user", m["content"])
				inputItems = append(inputItems, items...)
			case "assistant":
				content := m["content"]
				if content != nil && content != "" {
					items := buildRoleInputItem("assistant", content)
					inputItems = append(inputItems, items...)
				}
				// Handle tool_calls in assistant message
				if tcs, ok := m["tool_calls"].([]interface{}); ok {
					for _, tc := range tcs {
						tcMap, _ := tc.(map[string]interface{})
						if tcMap == nil {
							continue
						}
						fcItem := map[string]interface{}{
							"type": "function_call",
						}
						if id, ok := tcMap["id"].(string); ok {
							fcItem["call_id"] = id
						}
						if fn, ok := tcMap["function"].(map[string]interface{}); ok {
							if name, ok := fn["name"].(string); ok {
								fcItem["name"] = name
							}
							if args, ok := fn["arguments"].(string); ok {
								fcItem["arguments"] = args
							}
						}
						inputItems = append(inputItems, fcItem)
					}
				}
			case "tool":
				inputItems = append(inputItems, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": m["tool_call_id"],
					"output":  extractStringContent(m["content"]),
				})
			}
		}
	}

	if instructions != "" {
		respBody["instructions"] = instructions
	}
	respBody["input"] = inputItems

	// Passthrough tools and tool_choice
	if tools, ok := request["tools"]; ok {
		respBody["tools"] = tools
	}
	if tc, ok := request["tool_choice"]; ok {
		respBody["tool_choice"] = tc
	}

	// Map reasoning_effort → reasoning.effort
	if effort, ok := request["reasoning_effort"].(string); ok {
		respBody["reasoning"] = map[string]interface{}{
			"effort": effort,
		}
	}

	// Map response_format → text.format
	if rf, ok := request["response_format"]; ok {
		respBody["text"] = map[string]interface{}{
			"format": rf,
		}
	}

	newBody, err := json.Marshal(respBody)
	if err != nil {
		return nil, nil, fmt.Errorf("openai2responses: failed to serialize: %w", err)
	}

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.URL.Path = "/v1/responses"
	newReq.Header.Set("Content-Type", "application/json")
	newReq.Header.Del("x-api-key")
	newReq.Header.Set("Authorization", "Bearer "+downstream.APIKey)

	return newReq, newBody, nil
}

// --- TransformResponse (non-streaming): Responses API → Chat Completions ---

func (t *OpenAI2Responses) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return body, nil
	}

	var respMap map[string]any
	if err := json.Unmarshal(body, &respMap); err != nil {
		return nil, fmt.Errorf("openai2responses: failed to parse responses response: %w", err)
	}

	respID, _ := respMap["id"].(string)
	model, _ := respMap["model"].(string)
	status, _ := respMap["status"].(string)

	finishReason := mapOpenAIFinishReason(status)
	var content string
	var toolCalls []openAIChatToolCall

	output, _ := respMap["output"].([]any)
	for _, itemRaw := range output {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		switch itemType {
		case "message":
			msgContent, _ := item["content"].([]any)
			for _, partRaw := range msgContent {
				part, ok := partRaw.(map[string]any)
				if !ok {
					continue
				}
				if partType, _ := part["type"].(string); partType == "output_text" {
					if text, _ := part["text"].(string); text != "" {
						if content != "" {
							content += "\n"
						}
						content += text
					}
				}
			}
		case "output_text":
			text, _ := item["text"].(string)
			if text != "" {
				if content != "" {
					content += "\n"
				}
				content += text
			}
		case "function_call":
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			toolCalls = append(toolCalls, openAIChatToolCall{
				ID:   callID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      name,
					Arguments: arguments,
				},
			})
		}
	}

	response := map[string]any{
		"id":     respID,
		"object": "chat.completion",
		"model":  model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": finishReason,
			},
		},
	}

	if usage, ok := respMap["usage"].(map[string]any); ok {
		inputTokens, _ := usage["input_tokens"].(float64)
		outputTokens, _ := usage["output_tokens"].(float64)
		totalTokens, _ := usage["total_tokens"].(float64)
		response["usage"] = map[string]any{
			"prompt_tokens":     int(inputTokens),
			"completion_tokens": int(outputTokens),
			"total_tokens":      int(totalTokens),
		}
	}

	if len(toolCalls) > 0 {
		msg := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		msg["tool_calls"] = toolCalls
	}

	return json.Marshal(response)
}

// --- TransformStreamChunk: Responses API SSE → Chat Completions SSE ---

func (t *OpenAI2Responses) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	stateRaw, ok := ctx.Variables["o2r_stream"]
	var state *o2rStreamState
	if !ok {
		state = &o2rStreamState{Created: time.Now().Unix()}
		ctx.Variables["o2r_stream"] = state
	} else {
		state = stateRaw.(*o2rStreamState)
	}

	var buf bytes.Buffer
	writeChunk := func(data map[string]interface{}) {
		d, _ := json.Marshal(data)
		buf.WriteString("data: ")
		buf.Write(d)
		buf.WriteString("\n\n")
	}

	switch chunk.EventType {
	case "response.created":
		var evt struct {
			Response struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"response"`
		}
		json.Unmarshal(chunk.Data, &evt)
		state.ResponseID = evt.Response.ID
		state.Model = evt.Response.Model

	case "response.in_progress":
		if !state.sentRole {
			state.sentRole = true
			writeChunk(map[string]interface{}{
				"id":      state.ResponseID,
				"object":  "chat.completion.chunk",
				"created": state.Created,
				"model":   state.Model,
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"delta":         map[string]interface{}{"role": "assistant"},
						"finish_reason": nil,
					},
				},
			})
		}

	case "response.output_text.delta":
		var evt struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil || evt.Delta == "" {
			break
		}
		if !state.sentRole {
			state.sentRole = true
			writeChunk(map[string]interface{}{
				"id":      state.ResponseID,
				"object":  "chat.completion.chunk",
				"created": state.Created,
				"model":   state.Model,
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"delta": map[string]interface{}{
							"role":    "assistant",
							"content": evt.Delta,
						},
						"finish_reason": nil,
					},
				},
			})
		} else {
			writeChunk(map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"delta": map[string]interface{}{
							"content": evt.Delta,
						},
						"finish_reason": nil,
					},
				},
			})
		}

	case "response.output_item.added":
		var evt struct {
			Output struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"output"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil {
			break
		}
		if evt.Output.Type == "function_call" {
			state.ToolCallID = evt.Output.CallID
			state.ToolName = evt.Output.Name

			// Send tool call header chunk (id + name)
			writeChunk(map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []interface{}{
								map[string]interface{}{
									"index": 0,
									"id":    evt.Output.CallID,
									"type":  "function",
									"function": map[string]interface{}{
										"name":      evt.Output.Name,
										"arguments": "",
									},
								},
							},
						},
						"finish_reason": nil,
					},
				},
			})
		}

	case "response.function_call_arguments.delta":
		var evt struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil || evt.Delta == "" {
			break
		}
		writeChunk(map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"delta": map[string]interface{}{
						"tool_calls": []interface{}{
							map[string]interface{}{
								"index": 0,
								"function": map[string]interface{}{
									"arguments": evt.Delta,
								},
							},
						},
					},
					"finish_reason": nil,
				},
			},
		})

	case "response.completed":
		var evt struct {
			Response struct {
				Status string         `json:"status"`
				Usage  *responsesUsage `json:"usage"`
			} `json:"response"`
		}
		json.Unmarshal(chunk.Data, &evt)

		fr := mapOpenAIFinishReason(evt.Response.Status)
		finalChunk := map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": fr,
				},
			},
		}
		if evt.Response.Usage != nil {
			finalChunk["usage"] = map[string]interface{}{
				"prompt_tokens":     evt.Response.Usage.InputTokens,
				"completion_tokens": evt.Response.Usage.OutputTokens,
				"total_tokens":      evt.Response.Usage.TotalTokens,
			}
		}
		writeChunk(finalChunk)
		buf.WriteString("data: [DONE]\n\n")
	}

	if buf.Len() == 0 {
		return engine.SSEChunk{}, nil
	}
	return engine.SSEChunk{Data: buf.Bytes()}, nil
}

// --- Helpers ---

func mapOpenAIFinishReason(status string) *string {
	switch status {
	case "completed":
		s := "stop"
		return &s
	default:
		return nil
	}
}

// buildRoleInputItem converts Chat Completions message content to Responses API input items.
func buildRoleInputItem(role string, content interface{}) []map[string]interface{} {
	if content == nil {
		return []map[string]interface{}{{role: role, "content": ""}}
	}

	switch v := content.(type) {
	case string:
		return []map[string]interface{}{{"role": role, "content": v}}
	case []interface{}:
		hasImage := false
		for _, part := range v {
			if p, ok := part.(map[string]interface{}); ok && p["type"] == "image_url" {
				hasImage = true
				break
			}
		}
		if !hasImage {
			var texts []string
			for _, part := range v {
				if p, ok := part.(map[string]interface{}); ok {
					if text, ok := p["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
			return []map[string]interface{}{{"role": role, "content": strings.Join(texts, "\n")}}
		}
		// Has images: convert to Responses API content parts
		var parts []map[string]interface{}
		for _, part := range v {
			if p, ok := part.(map[string]interface{}); ok {
				switch p["type"] {
				case "text":
					if text, ok := p["text"].(string); ok {
						parts = append(parts, map[string]interface{}{
							"type": "input_text",
							"text": text,
						})
					}
				case "image_url":
					parts = append(parts, extractImagePart(p["image_url"]))
				}
			}
		}
		return []map[string]interface{}{{"role": role, "content": parts}}
	}
	return []map[string]interface{}{{"role": role, "content": ""}}
}

func extractStringContent(content interface{}) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, part := range v {
			if p, ok := part.(map[string]interface{}); ok {
				if text, ok := p["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func extractImagePart(imageURL interface{}) map[string]interface{} {
	part := map[string]interface{}{"type": "input_image"}
	switch v := imageURL.(type) {
	case string:
		part["image_url"] = v
	case map[string]interface{}:
		if url, ok := v["url"].(string); ok {
			part["image_url"] = url
		}
	}
	return part
}

// Interface compliance checks.
var _ engine.RequestTransformer = (*OpenAI2Responses)(nil)
var _ engine.ResponseTransformer = (*OpenAI2Responses)(nil)
var _ engine.StreamResponseTransformer = (*OpenAI2Responses)(nil)
