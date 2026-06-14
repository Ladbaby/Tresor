package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"tresor/internal/engine"
)

// Anthropic2Responses converts Anthropic Messages requests to Responses API format
// and Responses API responses back to Anthropic Messages format.
type Anthropic2Responses struct{}

// --- Streaming state types ---

type a2rStreamState struct {
	ResponseID      string
	Model           string
	ContentBlockIdx int
	sentStart       bool
	pendingTextCBS  bool // whether we've sent content_block_start for current text block
	toolCallBlocks  map[string]*a2rToolCallBlock // output_id -> tool call block
}

type a2rToolCallBlock struct {
	ID        string
	Name      string
	BlockIdx  int
	startSent bool
}

// --- TransformRequest: Anthropic Messages → Responses API ---

func (t *Anthropic2Responses) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var anthropicReq map[string]interface{}
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, nil, fmt.Errorf("anthropic2responses: failed to parse request: %w", err)
	}

	downstream := ctx.TargetDownstream
	respBody := make(map[string]interface{})

	// Model
	if model, ok := anthropicReq["model"].(string); ok {
		respBody["model"] = model
	}

	// Stream
	if stream, ok := anthropicReq["stream"]; ok {
		respBody["stream"] = stream
	}

	// System → instructions
	var instructions string
	if system, ok := anthropicReq["system"]; ok {
		switch v := system.(type) {
		case string:
			instructions = v
		case []interface{}:
			var parts []string
			for _, block := range v {
				if b, ok := block.(map[string]interface{}); ok {
					if text, ok := b["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			instructions = strings.Join(parts, "\n\n")
		}
	}
	if instructions != "" {
		respBody["instructions"] = instructions
	}

	// Messages → input items
	var inputItems []map[string]interface{}
	if messages, ok := anthropicReq["messages"].([]interface{}); ok {
		for _, msg := range messages {
			m, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)

			// Handle content (string or array of blocks)
			switch content := m["content"].(type) {
			case string:
				inputItems = append(inputItems, map[string]interface{}{
					"role":    role,
					"content": content,
				})
			case []interface{}:
				var textParts []string
				for _, block := range content {
					b, ok := block.(map[string]interface{})
					if !ok {
						continue
					}
					switch b["type"] {
					case "text":
						if text, ok := b["text"].(string); ok {
							textParts = append(textParts, text)
						}
					case "image":
						// Convert to Responses API input_image
						imgItem := map[string]interface{}{
							"role":    role,
							"content": buildAnthropicImageContent(b),
						}
						inputItems = append(inputItems, imgItem)
					case "tool_use":
						inputItems = append(inputItems, map[string]interface{}{
							"type":      "function_call",
							"call_id":   b["id"],
							"name":      b["name"],
							"arguments": serializeInput(b["input"]),
						})
					case "tool_result":
						toolResult := map[string]interface{}{
							"type":    "function_call_output",
							"call_id": b["tool_use_id"],
							"output":  extractToolResultContent(b["content"]),
						}
						inputItems = append(inputItems, toolResult)
					}
				}
				if len(textParts) > 0 {
					inputItems = append(inputItems, map[string]interface{}{
						"role":    role,
						"content": strings.Join(textParts, "\n"),
					})
				}
			}
		}
	}
	respBody["input"] = inputItems

	// Tools: Anthropic format → OpenAI format
	if tools, ok := anthropicReq["tools"].([]interface{}); ok && len(tools) > 0 {
		openaiTools := make([]map[string]interface{}, 0, len(tools))
		for _, tool := range tools {
			t, ok := tool.(map[string]interface{})
			if !ok {
				continue
			}
			openaiTools = append(openaiTools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t["name"],
					"description": t["description"],
					"parameters":  t["input_schema"],
				},
			})
		}
		respBody["tools"] = openaiTools
	}

	// Tool choice: Anthropic format → OpenAI format
	if tc, ok := anthropicReq["tool_choice"].(map[string]interface{}); ok {
		tcType, _ := tc["type"].(string)
		switch tcType {
		case "auto":
			respBody["tool_choice"] = "auto"
		case "any":
			respBody["tool_choice"] = "required"
		case "tool":
			if name, ok := tc["name"].(string); ok {
				respBody["tool_choice"] = map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name": name,
					},
				}
			}
		}
	}

	// Thinking → reasoning.effort
	if thinking, ok := anthropicReq["thinking"].(map[string]interface{}); ok {
		if thinkingType, _ := thinking["type"].(string); thinkingType == "enabled" || thinkingType == "adaptive" {
			if budget, ok := thinking["budget_tokens"].(float64); ok {
				if effort := budgetToEffort(int(budget)); effort != "" {
					respBody["reasoning"] = map[string]interface{}{
						"effort": effort,
					}
				}
			}
		}
	}

	// max_tokens → default (not directly mapped to Responses API)

	newBody, err := json.Marshal(respBody)
	if err != nil {
		return nil, nil, fmt.Errorf("anthropic2responses: failed to serialize: %w", err)
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

// --- TransformResponse (non-streaming): Responses API → Anthropic Messages ---

func (t *Anthropic2Responses) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return body, nil
	}

	var respResp responsesResponse
	if err := json.Unmarshal(body, &respResp); err != nil {
		return nil, fmt.Errorf("anthropic2responses: failed to parse responses response: %w", err)
	}

	var content []anthropicContent
	for _, item := range respResp.Output {
		switch item.Type {
		case "output_text":
			content = append(content, anthropicContent{
				Type: "text",
				Text: item.Text,
			})
		case "function_call":
			// Parse arguments JSON string into input
			var input json.RawMessage
			if item.Arguments != "" {
				// Try to parse as JSON object; fall back to raw string
				if err := json.Unmarshal([]byte(item.Arguments), &input); err != nil {
					input = json.RawMessage(item.Arguments)
				}
			} else {
				input = json.RawMessage("{}")
			}
			content = append(content, anthropicContent{
				Type:  "tool_use",
				ID:    item.CallID,
				Name:  item.Name,
				Input: input,
			})
		}
	}

	stopReason := mapAnthropicStopReason(respResp.Status)

	response := anthropicResponse{
		ID:    respResp.ID,
		Model: respResp.Model,
		Content: content,
		StopReason: stopReason,
	}
	if respResp.Usage != nil {
		response.Usage.InputTokens = respResp.Usage.InputTokens
		response.Usage.OutputTokens = respResp.Usage.OutputTokens
	}

	// Marshal with the type field for Anthropic compliance
	out := map[string]interface{}{
		"id":          response.ID,
		"type":        "message",
		"role":        "assistant",
		"content":     response.Content,
		"model":       response.Model,
		"stop_reason": response.StopReason,
		"usage": map[string]interface{}{
			"input_tokens":  response.Usage.InputTokens,
			"output_tokens": response.Usage.OutputTokens,
		},
	}
	if out["stop_reason"] == "" {
		out["stop_reason"] = nil
	}
	if out["usage"].(map[string]interface{})["input_tokens"] == 0 && out["usage"].(map[string]interface{})["output_tokens"] == 0 {
		delete(out, "usage")
	}

	return json.Marshal(out)
}

// --- TransformStreamChunk: Responses API SSE → Anthropic SSE ---

func (t *Anthropic2Responses) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	stateRaw, ok := ctx.Variables["a2r_stream"]
	var state *a2rStreamState
	if !ok {
		state = &a2rStreamState{
			toolCallBlocks: make(map[string]*a2rToolCallBlock),
		}
		ctx.Variables["a2r_stream"] = state
	} else {
		state = stateRaw.(*a2rStreamState)
	}

	var buf bytes.Buffer
	writeSSE := func(eventType string, data interface{}) {
		d, _ := json.Marshal(data)
		buf.WriteString("event: ")
		buf.WriteString(eventType)
		buf.WriteByte('\n')
		buf.WriteString("data: ")
		buf.Write(d)
		buf.WriteString("\n\n")
	}

	switch chunk.EventType {
	case "response.created":
		if state.sentStart {
			break
		}
		state.sentStart = true
		var evt struct {
			Response struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"response"`
		}
		json.Unmarshal(chunk.Data, &evt)
		state.ResponseID = evt.Response.ID
		state.Model = evt.Response.Model

		writeSSE("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":      evt.Response.ID,
				"type":    "message",
				"role":    "assistant",
				"content": []interface{}{},
				"model":   evt.Response.Model,
				"stop_reason": nil,
				"stop_sequence": nil,
				"usage": map[string]interface{}{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		})

	case "response.output_text.delta":
		var evt struct {
			OutputID string `json:"output_id"`
			Delta    string `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil || evt.Delta == "" {
			break
		}

		if !state.pendingTextCBS {
			state.pendingTextCBS = true
			idx := state.ContentBlockIdx
			state.ContentBlockIdx++
			writeSSE("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type": "text",
					"text": "",
				},
			})
		}

		writeSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": state.ContentBlockIdx - 1,
			"delta": map[string]interface{}{
				"type": "text_delta",
				"text": evt.Delta,
			},
		})

	case "response.output_text.done":
		if state.pendingTextCBS {
			state.pendingTextCBS = false
			writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": state.ContentBlockIdx - 1,
			})
		}

	case "response.output_item.added":
		var evt struct {
			OutputID string `json:"output_id"`
			Output   struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"output"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil || evt.Output.Type != "function_call" {
			break
		}

		idx := state.ContentBlockIdx
		state.ContentBlockIdx++
		state.toolCallBlocks[evt.OutputID] = &a2rToolCallBlock{
			ID:       evt.Output.CallID,
			Name:     evt.Output.Name,
			BlockIdx: idx,
		}

		writeSSE("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    evt.Output.CallID,
				"name":  evt.Output.Name,
				"input": map[string]interface{}{},
			},
		})

	case "response.function_call_arguments.delta":
		var evt struct {
			OutputID string `json:"output_id"`
			Delta    string `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil || evt.Delta == "" {
			break
		}

		tcb, ok := state.toolCallBlocks[evt.OutputID]
		if !ok {
			// Unknown tool call — assign a new block index
			idx := state.ContentBlockIdx
			state.ContentBlockIdx++
			tcb = &a2rToolCallBlock{BlockIdx: idx}
			state.toolCallBlocks[evt.OutputID] = tcb
			writeSSE("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    evt.OutputID,
					"name":  "unknown",
					"input": map[string]interface{}{},
				},
			})
		}

		writeSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": tcb.BlockIdx,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": evt.Delta,
			},
		})

	case "response.output_item.done":
		var evt struct {
			OutputID string `json:"output_id"`
		}
		json.Unmarshal(chunk.Data, &evt)
		if tcb, ok := state.toolCallBlocks[evt.OutputID]; ok {
			writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": tcb.BlockIdx,
			})
			delete(state.toolCallBlocks, evt.OutputID)
		}

	case "response.completed":
		var evt struct {
			Response struct {
				Status string         `json:"status"`
				Usage  *responsesUsage `json:"usage"`
			} `json:"response"`
		}
		json.Unmarshal(chunk.Data, &evt)

		// Close any open text block
		if state.pendingTextCBS {
			state.pendingTextCBS = false
			writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": state.ContentBlockIdx - 1,
			})
		}

		// Close any open tool call blocks
		for _, tcb := range state.toolCallBlocks {
			writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": tcb.BlockIdx,
			})
		}

		sr := mapAnthropicStopReason(evt.Response.Status)
		usage := map[string]interface{}{
			"output_tokens": 0,
		}
		if evt.Response.Usage != nil {
			usage["output_tokens"] = evt.Response.Usage.OutputTokens
		}
		writeSSE("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   sr,
				"stop_sequence": nil,
			},
			"usage": usage,
		})

		writeSSE("message_stop", map[string]interface{}{
			"type": "message_stop",
		})
	}

	if buf.Len() == 0 {
		return engine.SSEChunk{}, nil
	}
	return engine.SSEChunk{Data: buf.Bytes()}, nil
}

// --- Helpers ---

// budgetToEffort maps Anthropic thinking budget_tokens to Responses API reasoning effort.
func budgetToEffort(budget int) string {
	switch {
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

func mapAnthropicStopReason(status string) string {
	switch status {
	case "completed":
		return "end_turn"
	case "failed":
		return "error"
	default:
		return ""
	}
}

func buildAnthropicImageContent(b map[string]interface{}) []map[string]interface{} {
	result := []map[string]interface{}{
		{"type": "input_image"},
	}
	if source, ok := b["source"].(map[string]interface{}); ok {
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if data != "" {
			result[0]["image_url"] = fmt.Sprintf("data:%s;base64,%s", mediaType, data)
		}
	}
	return result
}

func extractToolResultContent(content interface{}) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, block := range v {
			if b, ok := block.(map[string]interface{}); ok {
				if text, ok := b["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func serializeInput(input interface{}) string {
	if input == nil {
		return "{}"
	}
	d, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(d)
}

// Interface compliance checks.
var _ engine.RequestTransformer = (*Anthropic2Responses)(nil)
var _ engine.ResponseTransformer = (*Anthropic2Responses)(nil)
var _ engine.StreamResponseTransformer = (*Anthropic2Responses)(nil)
