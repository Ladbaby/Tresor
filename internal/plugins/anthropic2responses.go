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

	// Reasoning tracking. When the upstream emits a reasoning output item
	// (response.output_item.added with type:"reasoning"), we register a
	// thinking content block at the next available ContentBlockIdx and route
	// reasoning_summary_text.delta events into thinking_delta deltas on that
	// block. The empty signature emitted on close satisfies the Anthropic
	// SDK's discriminated-union schema variant for thinking blocks.
	pendingThinkingCBS    bool
	thinkingBlockIdx      int
	thinkingBlockOutputID string
}

type a2rToolCallBlock struct {
	ID        string
	Name      string
	BlockIdx  int
	startSent bool
}

// PluginName returns the stable type name for deduplication.
func (t *Anthropic2Responses) PluginName() string { return "Anthropic2Responses" }

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

	var respMap map[string]any
	if err := json.Unmarshal(body, &respMap); err != nil {
		return nil, fmt.Errorf("anthropic2responses: failed to parse responses response: %w", err)
	}

	respID, _ := respMap["id"].(string)
	model, _ := respMap["model"].(string)
	status, _ := respMap["status"].(string)

	var content []anthropicContent
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
						content = append(content, anthropicContent{
							Type: "text",
							Text: text,
						})
					}
				}
			}
		case "output_text":
			text, _ := item["text"].(string)
			if text != "" {
				content = append(content, anthropicContent{
					Type: "text",
					Text: text,
				})
			}
		case "function_call":
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			var input json.RawMessage
			if arguments != "" {
				if err := json.Unmarshal([]byte(arguments), &input); err != nil {
					input = json.RawMessage(arguments)
				}
			} else {
				input = json.RawMessage("{}")
			}
			content = append(content, anthropicContent{
				Type:  "tool_use",
				ID:    callID,
				Name:  name,
				Input: input,
			})
		case "reasoning":
			// Reasoning items carry human-readable summaries in summary[]
			// (each entry is {type:"summary_text", text:"..."}). Concatenate
			// them as the Anthropic thinking block's text content. The empty
			// signature satisfies the Anthropic SDK's discriminated-union
			// schema that requires the `signature` field on every thinking
			// content block.
			var thinking strings.Builder
			if summaries, ok := item["summary"].([]any); ok {
				for _, s := range summaries {
					if sMap, ok := s.(map[string]any); ok {
						if t, _ := sMap["text"].(string); t != "" {
							if thinking.Len() > 0 {
								thinking.WriteString("\n\n")
							}
							thinking.WriteString(t)
						}
					}
				}
			}
			if thinking.Len() > 0 {
				content = append(content, anthropicContent{
					Type:      "thinking",
					Thinking:  thinking.String(),
					Signature: "",
				})
			}
		}
	}

	stopReason := mapAnthropicStopReason(status)

	response := anthropicResponse{
		ID:         respID,
		Model:      model,
		Content:    content,
		StopReason: stopReason,
	}

	var inputTokens, outputTokens int
	if usage, ok := respMap["usage"].(map[string]any); ok {
		if it, ok := usage["input_tokens"].(float64); ok {
			inputTokens = int(it)
		}
		if ot, ok := usage["output_tokens"].(float64); ok {
			outputTokens = int(ot)
		}
	}
	response.Usage.InputTokens = inputTokens
	response.Usage.OutputTokens = outputTokens

	out := map[string]any{
		"id":          response.ID,
		"type":        "message",
		"role":        "assistant",
		"content":     response.Content,
		"model":       response.Model,
		"stop_reason": response.StopReason,
		"usage": map[string]any{
			"input_tokens":  response.Usage.InputTokens,
			"output_tokens": response.Usage.OutputTokens,
		},
	}
	if out["stop_reason"] == "" {
		out["stop_reason"] = nil
	}
	if inputTokens == 0 && outputTokens == 0 {
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
			OutputIndex int `json:"output_index"`
			Item        struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil {
			break
		}
		switch evt.Item.Type {
		case "function_call":
			idx := state.ContentBlockIdx
			state.ContentBlockIdx++
			state.toolCallBlocks[evt.Item.ID] = &a2rToolCallBlock{
				ID:       evt.Item.CallID,
				Name:     evt.Item.Name,
				BlockIdx: idx,
			}
			writeSSE("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    evt.Item.CallID,
					"name":  evt.Item.Name,
					"input": map[string]interface{}{},
				},
			})
		case "reasoning":
			// Register a thinking block at the next available index so the
			// Anthropic SDK accepts the discriminated-union schema variant
			// for thinking content. The Vercel AI SDK / Anthropic SDK
			// content_block_start schema for type:"thinking" is strict:
			// only `type` and `thinking` fields are allowed in the
			// content_block — no `signature` (which arrives via a
			// separate signature_delta). Subsequent
			// reasoning_summary_text.delta events will be routed here as
			// thinking_delta deltas.
			idx := state.ContentBlockIdx
			state.ContentBlockIdx++
			state.pendingThinkingCBS = true
			state.thinkingBlockIdx = idx
			state.thinkingBlockOutputID = evt.Item.ID
			writeSSE("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":     "thinking",
					"thinking": "",
				},
			})
		}

	case "response.function_call_arguments.delta":
		var evt struct {
			OutputIndex int    `json:"output_index"`
			ItemID      string `json:"item_id"`
			CallID      string `json:"call_id"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil || evt.Delta == "" {
			break
		}

		// The Responses API may use either call_id or item_id depending on
		// the upstream implementation. item_id is the canonical Responses
		// API field; call_id is the legacy OpenAI field.
		key := evt.CallID
		if key == "" {
			key = evt.ItemID
		}
		if key == "" {
			break
		}
		tcb, ok := state.toolCallBlocks[key]
		if !ok {
			// Unknown tool call — assign a new block index
			idx := state.ContentBlockIdx
			state.ContentBlockIdx++
			tcb = &a2rToolCallBlock{BlockIdx: idx}
			state.toolCallBlocks[key] = tcb
			writeSSE("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    key,
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

	case "response.reasoning_summary_text.delta":
		// Human-readable reasoning summary deltas. Map each one to a
		// thinking_delta on the currently-open reasoning block so the
		// Anthropic SDK surfaces the model's reasoning to the user.
		var evt struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &evt); err != nil || evt.Delta == "" {
			break
		}
		if !state.pendingThinkingCBS {
			break
		}
		writeSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": state.thinkingBlockIdx,
			"delta": map[string]interface{}{
				"type":     "thinking_delta",
				"thinking": evt.Delta,
			},
		})

	case "response.output_item.done":
		var evt struct {
			OutputIndex int    `json:"output_index"`
			ItemID      string `json:"item_id"`
			Item        struct {
				ID string `json:"id"`
			} `json:"item"`
		}
		json.Unmarshal(chunk.Data, &evt)
		// Prefer item.id (the canonical Responses API identifier) but fall
		// back to item_id for older implementations.
		key := evt.Item.ID
		if key == "" {
			key = evt.ItemID
		}
		if key == "" {
			break
		}
		if tcb, ok := state.toolCallBlocks[key]; ok {
			writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": tcb.BlockIdx,
			})
			delete(state.toolCallBlocks, key)
		} else if state.pendingThinkingCBS && key == state.thinkingBlockOutputID {
			// Close the reasoning block with an empty signature_delta (the
			// Anthropic SDK requires `signature` on every thinking block)
			// followed by content_block_stop.
			writeSSE("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": state.thinkingBlockIdx,
				"delta": map[string]interface{}{
					"type":      "signature_delta",
					"signature": "",
				},
			})
			writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": state.thinkingBlockIdx,
			})
			state.pendingThinkingCBS = false
			state.thinkingBlockOutputID = ""
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

		// Close any open reasoning block (the upstream may have ended the
		// stream without emitting response.output_item.done for the
		// reasoning item).
		if state.pendingThinkingCBS {
			writeSSE("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": state.thinkingBlockIdx,
				"delta": map[string]interface{}{
					"type":      "signature_delta",
					"signature": "",
				},
			})
			writeSSE("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": state.thinkingBlockIdx,
			})
			state.pendingThinkingCBS = false
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
