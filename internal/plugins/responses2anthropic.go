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

			// droppedToolUseIDs records call_ids whose corresponding
			// tool_use was suppressed because the Responses-API client
			// sent arguments that couldn't be forwarded safely to
			// Anthropic (malformed JSON, empty string, or a non-object
			// value). When the matching function_call_output comes in,
			// it must also be dropped — otherwise Anthropic sees a
			// tool_result whose tool_use_id has no preceding tool_use
			// and rejects the request with 400 ("unexpected
			// tool_use_id").
			droppedToolUseIDs := make(map[string]struct{})

			// Collect redacted_thinking blocks (from Responses API
			// reasoning items) to merge into the assistant message they
			// logically precede. Codex re-sends prior turns' reasoning
			// back as encrypted blobs; Anthropic accepts them as
			// redacted_thinking content blocks carrying an opaque `data`
			// field, so the bytes round-trip safely even though neither
			// Tresor nor Anthropic can decrypt them.
			var pendingReasoning []map[string]interface{}

			flushReasoning := func() {
				if len(pendingReasoning) == 0 {
					return
				}
				if len(anthroMessages) > 0 {
					last := anthroMessages[len(anthroMessages)-1]
					if last["role"] == "assistant" {
						if content, ok := last["content"].([]map[string]interface{}); ok {
							last["content"] = append(content, pendingReasoning...)
						} else {
							last["content"] = pendingReasoning
						}
					} else {
						anthroMessages = append(anthroMessages, map[string]interface{}{
							"role":    "assistant",
							"content": pendingReasoning,
						})
					}
				} else {
					anthroMessages = append(anthroMessages, map[string]interface{}{
						"role":    "assistant",
						"content": pendingReasoning,
					})
				}
				pendingReasoning = nil
			}

			flushToolUses := func() {
				if len(pendingToolUses) > 0 {
					// Tool calls must come after any pending reasoning
					// in the same assistant message: flush reasoning
					// first so the assistant content block order is
					// [redacted_thinking..., tool_use...].
					flushReasoning()
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
				if len(toolResults) == 0 {
					return
				}

				// Anthropic requires every tool_result to appear in a
				// user message that IMMEDIATELY follows the assistant
				// message containing the matching tool_use — a
				// "tool call result does not follow tool call" 400
				// otherwise. Codex's Responses-API stream can interleave
				// interim assistant text between a function_call and its
				// function_call_output (the "I'll help you use Nora..."
				// text in input items [6] of codex_request.txt, which
				// sits between the function_call [5] and its tool result
				// [7]). Our first-pass conversion would emit that text as
				// a standalone assistant message and leave the tool_result
				// stranded two messages later, which Anthropic rejects.
				//
				// Before emitting the tool_result-bearing user message,
				// scan backwards over `anthroMessages` and absorb any
				// text-only assistant messages that were emitted between
				// the tool_use-bearing assistant message and the current
				// tail. Those text blocks are inserted at the end of the
				// tool_use-bearing assistant message but BEFORE the
				// tool_use, so the resulting block order is
				// `thinking → text → tool_use` — Anthropic's required
				// ordering for an assistant message that calls tools.
				toolUseMsgIdx := -1
				for idx := len(anthroMessages) - 1; idx >= 0; idx-- {
					m := anthroMessages[idx]
					if m["role"] != "assistant" {
						break
					}
					blocks, ok := m["content"].([]map[string]interface{})
					if !ok {
						break
					}
					match := false
					for _, b := range blocks {
						if b["type"] == "tool_use" {
							id, _ := b["id"].(string)
							for _, tr := range toolResults {
								tuid, _ := tr["tool_use_id"].(string)
								if id == tuid {
									match = true
									break
								}
							}
							if match {
								break
							}
						}
					}
					if match {
						toolUseMsgIdx = idx
						break
					}
				}

				if toolUseMsgIdx >= 0 {
					// Pull every assistant message after the tool_use
					// one (up to the current tail) into a single
					// intermediate-slice to absorb.
					toAbsorb := anthroMessages[toolUseMsgIdx+1:]
					anthroMessages = anthroMessages[:toolUseMsgIdx+1]

					// Extract text blocks from each absorbed assistant
					// message and insert them into the tool_use-bearing
					// assistant message right before the first tool_use
					// block, preserving Anthropic's required
					// `thinking → text → tool_use` ordering.
					var textBlocks []map[string]interface{}
					for _, m := range toAbsorb {
						if m["role"] != "assistant" {
							continue
						}
						switch c := m["content"].(type) {
						case []map[string]interface{}:
							for _, b := range c {
								if b["type"] == "text" {
									textBlocks = append(textBlocks, b)
								}
							}
						case string:
							if c != "" {
								textBlocks = append(textBlocks, map[string]interface{}{
									"type": "text",
									"text": c,
								})
							}
						}
					}
					if len(textBlocks) > 0 {
						toolUseMsg := anthroMessages[toolUseMsgIdx]
						blocks, _ := toolUseMsg["content"].([]map[string]interface{})
						// Find the index of the first tool_use block.
						insertAt := len(blocks)
						for i, b := range blocks {
							if b["type"] == "tool_use" {
								insertAt = i
								break
							}
						}
						newBlocks := make([]map[string]interface{}, 0, len(blocks)+len(textBlocks))
						newBlocks = append(newBlocks, blocks[:insertAt]...)
						newBlocks = append(newBlocks, textBlocks...)
						newBlocks = append(newBlocks, blocks[insertAt:]...)
						toolUseMsg["content"] = newBlocks
					}
				}

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

			for _, item := range items {
				if item.Role != "" {
					// Pending reasoning blocks (Responses API encrypted
					// reasoning blobs) belong with the assistant message
					// they logically precede:
					//   - if the next role-bearing item is "assistant",
					//     prepend them to the new assistant message's
					//     content so redacted_thinking blocks come before
					//     text/refusal/tool_use in Anthropic's required
					//     block order;
					//   - otherwise materialize them as a standalone
					//     assistant message now, since dangling reasoning
					//     with no anchor would be lost.
					if item.Role == "assistant" && len(pendingReasoning) > 0 {
						// Merge happens inline in the assistant case below
						// by prepending pendingReasoning to the new msg's
						// content. pendingReasoning is consumed there.
					} else {
						flushReasoning()
					}
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
							} else {
								// Array-of-parts: Responses API represents prior
								// assistant turns as [{type:"output_text",
								// text:"..."}] (and occasionally
								// [{type:"refusal", refusal:"..."}]). Without
								// this fallback the conversion silently emits
								// an assistant message with no content field,
								// which Anthropic rejects with HTTP 400
								// ("input json is empty").
								var parts []responsesContentPart
								if err := json.Unmarshal(item.Content, &parts); err == nil {
									blocks := make([]map[string]interface{}, 0, len(parts))
									for _, p := range parts {
										switch p.Type {
										case "output_text":
											if p.Text != "" {
												blocks = append(blocks, map[string]interface{}{
													"type": "text",
													"text": p.Text,
												})
											}
										case "refusal":
											if p.Refusal != "" {
												blocks = append(blocks, map[string]interface{}{
													"type":    "refusal",
													"refusal": p.Refusal,
												})
											}
										}
									}
									if len(blocks) > 0 {
										msg["content"] = blocks
									}
								}
							}
						}
						// If reasoning blocks were buffered for this assistant
						// message (typically from a prior turn's
						// {type:"reasoning", encrypted_content:"..."} item),
						// prepend them. Anthropic requires the block order
						// redacted_thinking → text → tool_use inside an
						// assistant message, so reasoning must come first.
						if len(pendingReasoning) > 0 {
							switch existing := msg["content"].(type) {
							case string:
								// Plain-string content + pending reasoning
								// becomes a block array. Wrap the string in
								// a text block alongside the redacted_thinking.
								wrapped := []map[string]interface{}{pendingReasoning[0]}
								if existing != "" {
									wrapped = append(wrapped, map[string]interface{}{
										"type": "text",
										"text": existing,
									})
								}
								if len(pendingReasoning) > 1 {
									wrapped = append(wrapped, pendingReasoning[1:]...)
								}
								msg["content"] = wrapped
							case []map[string]interface{}:
								msg["content"] = append(pendingReasoning, existing...)
							case nil:
								msg["content"] = pendingReasoning
							}
							pendingReasoning = nil
						}
						anthroMessages = append(anthroMessages, msg)
					}
				} else if item.Type == "function_call" {
					// Parse arguments strictly. The Responses-API
					// function_call `arguments` field is a JSON-encoded
					// STRING per OpenAI's spec — its decoded value is the
					// tool's input JSON. Codex (and other Responses-API
					// clients) sometimes send malformed values here — e.g.
					// two concatenated JSON objects from a stale
					// tool_use_id replay like
					// `{"command": "..."}{"command": "..."}`, or a bare
					// value that's not a JSON object. The previous
					// implementation silently swallowed the parse error
					// and forwarded `input: {}` to Anthropic, which then
					// rejected the request with HTTP 400 "invalid params,
					// 400 (2013)" the moment any downstream tool's
					// input_schema declared required fields (e.g.
					// shell_command requires a "command" property).
					//
					// On any failure path (empty arguments, inner JSON
					// doesn't parse, or inner JSON isn't an object), drop
					// the tool_use entirely and remember its call_id so
					// the matching function_call_output is also skipped —
					// that avoids Anthropic's 400 "unexpected tool_use_id"
					// when the result block references a tool_use we
					// didn't emit.
					//
					// item.Args is json.RawMessage. Two shapes are valid
					// in the wild:
					//
					//   - OpenAI Responses-API spec: a JSON-encoded string,
					//     e.g. `"arguments": "{\"command\":\"ls\"}"`.
					//     json.Unmarshal yields a Go string holding the
					//     inner JSON, which must be parsed again.
					//   - Non-conforming clients: a JSON object directly,
					//     e.g. `"arguments": {"command":"ls"}`.
					//     json.Unmarshal yields a map already.
					//
					// We accept both and normalise to the map form.
					argsStr := strings.TrimSpace(string(item.Args))
					if argsStr == "" || argsStr == "null" {
						droppedToolUseIDs[item.CallID] = struct{}{}
					} else {
						var input interface{}
						if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
							droppedToolUseIDs[item.CallID] = struct{}{}
						} else if s, ok := input.(string); ok {
							// Case A: the value was a JSON-encoded
							// string. Parse the inner JSON.
							inner := strings.TrimSpace(s)
							if err := json.Unmarshal([]byte(inner), &input); err != nil {
								droppedToolUseIDs[item.CallID] = struct{}{}
							}
						}
						if _, ok := input.(map[string]interface{}); !ok {
							// Anthropic's tool_use.input must be a JSON
							// object. A bare string/number/array/bool/null
							// from a non-conforming client would be
							// rejected, so drop those cases too.
							droppedToolUseIDs[item.CallID] = struct{}{}
						} else {
							toolUse := map[string]interface{}{
								"type":  "tool_use",
								"id":    item.CallID,
								"name":  item.Name,
								"input": input,
							}
							pendingToolUses = append(pendingToolUses, toolUse)
						}
					}
				} else if item.Type == "function_call_output" {
					if _, dropped := droppedToolUseIDs[item.CallID]; dropped {
						// Companion of a dropped tool_use above. Skip so
						// we don't emit a tool_result pointing at a
						// tool_use_id that doesn't exist in this turn's
						// messages.
						continue
					}
					flushToolUses()
					toolResults := []map[string]interface{}{
						{
							"type":        "tool_result",
							"tool_use_id": item.CallID,
							"content":     item.Output,
						},
					}
					flushToolResults(toolResults)
				} else if item.Type == "reasoning" && item.EncryptedContent != "" {
					// Codex-style encrypted reasoning. Buffer the bytes
					// as an Anthropic redacted_thinking block; the block
					// will be merged into the next assistant message's
					// content (or flushed as a standalone assistant
					// message if no assistant message follows).
					pendingReasoning = append(pendingReasoning, map[string]interface{}{
						"type": "redacted_thinking",
						"data": item.EncryptedContent,
					})
				}
			}
			flushToolUses()
			// Any reasoning left dangling after the loop has no assistant
			// message to attach to — flush it as a standalone assistant
			// message so the bytes aren't silently lost.
			flushReasoning()
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

	// Convert tools: Responses-API flat format or Chat Completions envelope
	// → Anthropic {name, description, input_schema}.
	//
	// Codex (and the Responses API generally) emits tools as
	//   {type:"function", name:"...", description:"...", parameters:{...}, strict:false}
	// while Chat Completions emits them as
	//   {type:"function", function:{name:"...", description:"...", parameters:{...}}}
	// The previous implementation only handled the Chat Completions shape
	// and silently dropped every Responses-API tool — which meant any Codex
	// client routed to an Anthropic downstream received zero tools and
	// could not actually call any function.
	if len(respReq.Tools) > 0 {
		var openaiTools []map[string]interface{}
		if err := json.Unmarshal(respReq.Tools, &openaiTools); err == nil {
			anthroTools := make([]map[string]interface{}, 0, len(openaiTools))
			for _, ot := range openaiTools {
				// Prefer the Chat Completions envelope when present.
				fn, _ := ot["function"].(map[string]interface{})
				var name, desc string
				var params interface{}
				if fn != nil {
					name, _ = fn["name"].(string)
					desc, _ = fn["description"].(string)
					params = fn["parameters"]
				} else {
					// Fall back to the Responses-API flat shape.
					name, _ = ot["name"].(string)
					desc, _ = ot["description"].(string)
					params = ot["parameters"]
				}
				if name == "" {
					// Without a name the tool is unusable — drop it.
					continue
				}
				// Codex emits non-standard tool shapes that have no
				// `parameters` field at all — for example
				// `apply_patch` (type:"custom", carries a freeform
				// `format` description) and the namespace tools
				// `image_gen` / `codex_app` (type:"namespace",
				// contains a sub-list of inner tools). Anthropic
				// rejects `input_schema: null` with HTTP 400 ("tools.
				// 0.input_schema: Input tag does not exist"), so
				// substitute a permissive empty-object schema when
				// params is missing. The Codex client never actually
				// invokes these tools through the proxy for Anthropic
				// backends in practice, so an unrestricted schema is
				// safe.
				if params == nil {
					params = map[string]interface{}{"type": "object"}
				}
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

	reasoningIndex := 0
	for _, c := range anthropicResp.Content {
		switch c.Type {
		case "thinking":
			if c.Thinking == "" && c.Signature == "" {
				continue
			}
			reasoningItem := map[string]any{
				"id":      fmt.Sprintf("%s_reasoning_%d", respID, reasoningIndex),
				"type":    "reasoning",
				"status":  "completed",
				"summary": []any{},
			}
			if c.Thinking != "" {
				reasoningItem["summary"] = []any{
					map[string]any{
						"type": "summary_text",
						"text": c.Thinking,
					},
				}
			}
			if c.Signature != "" {
				reasoningItem["encrypted_content"] = c.Signature
			}
			output = append(output, reasoningItem)
			reasoningIndex++
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
		// Reasoning items were appended to `output` in declaration order; the
		// message item goes at the position right after the last reasoning
		// item (or at index 0 if there is no reasoning).
		insertAt := 0
		for i, item := range output {
			if item["type"] == "reasoning" {
				insertAt = i + 1
			} else {
				break
			}
		}
		output = append(output, nil)
		copy(output[insertAt+1:], output[insertAt:])
		output[insertAt] = msgItem
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
	var out bytes.Buffer
	// Local closure state for this single bulk response. Mirrors r2aStreamState
	// but lives in the closure because this path rewrites the entire stream at
	// once rather than buffering per-chunk state across chunks.
	var (
		id                    string
		model                 string
		createdAt             int64
		textContent           string
		msgItemSent           bool
		contentPartSent       bool
		msgItemID             string
		msgOutputIdx          int
		nextOutputIdx         int
		nextSummaryIdx        int
		openToolCalls         = make(map[string]*r2aStreamToolCall)
		reasoningItemSent     bool
		reasoningItemID       string
		reasoningOutputIdx    int
		reasoningContent      string
		reasoningSignature    string
		summaryPartSent       bool
		// inputTokens/outputTokens are pulled from the upstream Anthropic
		// message_delta event and surfaced on response.completed.usage so the
		// Vercel AI SDK's openai-responses schema (which requires
		// usage.{input_tokens, output_tokens}) validates.
		inputTokens  int
		outputTokens int
		// finalItems maps output_index → the completed output item. The
		// `response.completed` payload must include the full `output` array;
		// the OpenAI Responses SDK uses that payload (not the accumulated
		// streaming events) as the source of truth for the final response.
		finalItems = make(map[int]map[string]any)
	)

	emitReasoningOpen := func() {
		if reasoningItemSent {
			return
		}
		reasoningItemSent = true
		reasoningItemID = id + "_reasoning_0"
		reasoningOutputIdx = nextOutputIdx
		nextOutputIdx++
		writeResponsesSSE(&out, "response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": reasoningOutputIdx,
			"item": map[string]any{
				"id":      reasoningItemID,
				"type":    "reasoning",
				"status":  "in_progress",
				"summary": []any{},
			},
		})
	}

	ensureSummaryPart := func() {
		if summaryPartSent {
			return
		}
		summaryPartSent = true
		writeResponsesSSE(&out, "response.reasoning_summary_part.added", map[string]any{
			"type":          "response.reasoning_summary_part.added",
			"item_id":       reasoningItemID,
			"output_index":  reasoningOutputIdx,
			"summary_index": nextSummaryIdx,
			"part": map[string]any{
				"type": "summary_text",
				"text": "",
			},
		})
	}

	openMessageItem := func() {
		if msgItemSent {
			return
		}
		msgItemSent = true
		msgItemID = id + "_msg_0"
		msgOutputIdx = nextOutputIdx
		nextOutputIdx++
		writeResponsesSSE(&out, "response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": msgOutputIdx,
			"item": map[string]any{
				"id":      msgItemID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		})
	}

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
			model = msg.Message.Model
			if createdAt == 0 {
				createdAt = time.Now().Unix()
			}

			// Vercel AI SDK (Cherry Studio) validates each chunk with a Zod
			// schema. response.created requires response.{id, created_at, model};
			// we include them so the schema parse succeeds.
			writeResponsesSSE(&out, "response.created", map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id":         id,
					"created_at": createdAt,
					"model":      model,
					"status":     "in_progress",
				},
			})
			writeResponsesSSE(&out, "response.in_progress", map[string]any{
				"type": "response.in_progress",
				"response": map[string]any{
					"id":         id,
					"created_at": createdAt,
					"model":      model,
					"status":     "in_progress",
				},
			})

		case "content_block_start":
			var block struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type     string `json:"type"`
					Text     string `json:"text,omitempty"`
					Thinking string `json:"thinking,omitempty"`
					ID       string `json:"id,omitempty"`
					Name     string `json:"name,omitempty"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(data, &block); err != nil {
				return true
			}
			switch block.ContentBlock.Type {
			case "thinking":
				emitReasoningOpen()
				if block.ContentBlock.Thinking != "" {
					reasoningContent += block.ContentBlock.Thinking
					ensureSummaryPart()
					writeResponsesSSE(&out, "response.reasoning_summary_text.delta", map[string]any{
						"type":          "response.reasoning_summary_text.delta",
						"item_id":       reasoningItemID,
						"output_index":  reasoningOutputIdx,
						"summary_index": nextSummaryIdx,
						"delta":         block.ContentBlock.Thinking,
					})
				}
			case "text":
				if block.ContentBlock.Text != "" {
					textContent += block.ContentBlock.Text
					openMessageItem()
					if !contentPartSent {
						contentPartSent = true
						writeResponsesSSE(&out, "response.content_part.added", map[string]any{
							"type":          "response.content_part.added",
							"output_index":  msgOutputIdx,
							"content_index": 0,
							"item_id":       msgItemID,
							"part": map[string]any{
								"type":        "output_text",
								"text":        "",
								"annotations": []any{},
							},
						})
					}
					writeResponsesSSE(&out, "response.output_text.delta", map[string]any{
						"type":         "response.output_text.delta",
						"item_id":      msgItemID,
						"output_index": msgOutputIdx,
						"delta":        block.ContentBlock.Text,
						"logprobs":     []any{},
					})
				}
			case "tool_use":
				openMessageItem()
				oidx := nextOutputIdx
				nextOutputIdx++
				tc := &r2aStreamToolCall{
					CallID:    block.ContentBlock.ID,
					Name:      block.ContentBlock.Name,
					OutputIdx: oidx,
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
					Thinking    string `json:"thinking,omitempty"`
					Signature   string `json:"signature,omitempty"`
					PartialJSON string `json:"partial_json,omitempty"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(data, &delta); err != nil {
				return true
			}
			switch delta.Delta.Type {
			case "thinking_delta":
				if delta.Delta.Thinking != "" {
					reasoningContent += delta.Delta.Thinking
					emitReasoningOpen()
					ensureSummaryPart()
					writeResponsesSSE(&out, "response.reasoning_summary_text.delta", map[string]any{
						"type":          "response.reasoning_summary_text.delta",
						"item_id":       reasoningItemID,
						"output_index":  reasoningOutputIdx,
						"summary_index": nextSummaryIdx,
						"delta":         delta.Delta.Thinking,
					})
				}
			case "signature_delta":
				if delta.Delta.Signature != "" {
					reasoningSignature = delta.Delta.Signature
				}
			case "text_delta":
				if delta.Delta.Text != "" {
					textContent += delta.Delta.Text
					openMessageItem()
					if !contentPartSent {
						contentPartSent = true
						writeResponsesSSE(&out, "response.content_part.added", map[string]any{
							"type":          "response.content_part.added",
							"output_index":  msgOutputIdx,
							"content_index": 0,
							"item_id":       msgItemID,
							"part": map[string]any{
								"type":        "output_text",
								"text":        "",
								"annotations": []any{},
							},
						})
					}
					writeResponsesSSE(&out, "response.output_text.delta", map[string]any{
						"type":         "response.output_text.delta",
						"item_id":      msgItemID,
						"output_index": msgOutputIdx,
						"delta":        delta.Delta.Text,
						"logprobs":     []any{},
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
							"output_index": tc.OutputIdx,
						})
						break
					}
				}
			}

		case "content_block_stop":
			// Close the reasoning item if the just-stopped block was thinking.
			if reasoningItemSent && summaryPartSent {
				writeResponsesSSE(&out, "response.reasoning_summary_text.done", map[string]any{
					"type":          "response.reasoning_summary_text.done",
					"item_id":       reasoningItemID,
					"output_index":  reasoningOutputIdx,
					"summary_index": nextSummaryIdx,
					"text":          reasoningContent,
				})
				writeResponsesSSE(&out, "response.reasoning_summary_part.done", map[string]any{
					"type":          "response.reasoning_summary_part.done",
					"item_id":       reasoningItemID,
					"output_index":  reasoningOutputIdx,
					"summary_index": nextSummaryIdx,
					"part": map[string]any{
						"type": "summary_text",
						"text": reasoningContent,
					},
				})
				reasoningItem := map[string]any{
					"id":      reasoningItemID,
					"type":    "reasoning",
					"status":  "completed",
					"summary": []any{},
				}
				if reasoningSignature != "" {
					reasoningItem["encrypted_content"] = reasoningSignature
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": reasoningOutputIdx,
					"item":         reasoningItem,
				})
				finalItems[reasoningOutputIdx] = reasoningItem
				summaryPartSent = false
				reasoningContent = ""
				reasoningSignature = ""
				reasoningItemSent = false
				nextSummaryIdx++
			}
			for _, tc := range openToolCalls {
				writeResponsesSSE(&out, "response.function_call_arguments.done", map[string]any{
					"type":         "response.function_call_arguments.done",
					"item_id":      tc.CallID,
					"call_id":      tc.CallID,
					"name":         tc.Name,
					"arguments":    tc.Arguments,
					"output_index": tc.OutputIdx,
				})
				fnItem := map[string]any{
					"type":      "function_call",
					"id":        tc.CallID,
					"call_id":   tc.CallID,
					"status":    "completed",
					"name":      tc.Name,
					"arguments": tc.Arguments,
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": tc.OutputIdx,
					"item":         fnItem,
				})
				finalItems[tc.OutputIdx] = fnItem
			}
			openToolCalls = make(map[string]*r2aStreamToolCall)

		case "message_delta":
			var md struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(data, &md); err == nil {
				if md.Usage.InputTokens > 0 {
					inputTokens = md.Usage.InputTokens
				}
				if md.Usage.OutputTokens > 0 {
					outputTokens = md.Usage.OutputTokens
				}
			}

		case "message_stop":
			if textContent != "" {
				if contentPartSent {
					writeResponsesSSE(&out, "response.content_part.done", map[string]any{
						"type":          "response.content_part.done",
						"output_index":  msgOutputIdx,
						"content_index": 0,
						"item_id":       msgItemID,
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
				msgItem := map[string]any{
					"type":    "message",
					"id":      msgItemID,
					"status":  "completed",
					"role":    "assistant",
					"content": msgContent,
				}
				writeResponsesSSE(&out, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": msgOutputIdx,
					"item":         msgItem,
				})
				finalItems[msgOutputIdx] = msgItem
			}
			// Replay the final `output` array (in output_index order) into
			// response.completed so the OpenAI Responses SDK can build a
			// non-empty ParsedResponse from the authoritative `response`
			// payload. Without this, the SDK returns an empty response.
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
					"usage": map[string]any{
						"input_tokens":  inputTokens,
						"output_tokens": outputTokens,
						"total_tokens":  inputTokens + outputTokens,
					},
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
	OutputIdx int
	ItemSent  bool
}

type r2aStreamState struct {
	ResponseID      string
	Model           string
	CreatedAt       int64 // Unix seconds; required by Vercel AI SDK response.created schema
	Created         bool
	TextContent     string
	TextBlockOpen   bool
	MessageItemSent bool
	ContentPartSent bool
	NextOutputIdx   int // monotonically increasing counter for output items

	// Reasoning (Anthropic thinking) tracking. When the upstream emits a
	// `thinking` content block before text/tool_use, we register a reasoning
	// item at NextOutputIdx, advance the counter, and emit
	// response.reasoning_summary_text.delta events for each thinking_delta.
	// Without this, the OpenAI Responses SDK sees a message item at
	// output_index 0 instead of a reasoning item, and surfaces the response
	// as empty.
	ReasoningContent     string
	ReasoningSignature   string
	ReasoningItemSent    bool
	ReasoningItemID      string
	ReasoningOutputIdx   int
	SummaryPartSent      bool
	NextSummaryIdx       int

	ToolOutputIdx int
	ToolCallAcc   map[string]*r2aStreamToolCall

	// FinalItems is keyed by output_index. Each value is the completed
	// output item (with status:"completed" and any aggregated text/signature/
	// arguments). Populated as each output_item.done event is emitted, then
	// replayed into the response.completed payload so the OpenAI Responses
	// SDK can construct a non-empty ParsedResponse — see the bug where an
	// empty `output` in response.completed causes the SDK to return an
	// empty response to the client.
	FinalItems map[int]map[string]any

	// MessageItemID is the stable item id for the assistant message item.
	// Assigned when the message item is first opened.
	MessageItemID string

	// InputTokens / OutputTokens come from the upstream Anthropic
	// message_delta event and are surfaced on response.completed.usage so
	// the Vercel AI SDK's openai-responses schema (which requires
	// usage.{input_tokens, output_tokens}) validates.
	InputTokens  int
	OutputTokens int

	// _msgOutputIdx is the stable output_index for the assistant message
	// item, assigned the first time MessageItemSent is set true. Stored
	// separately from NextOutputIdx so subsequent items (tools, additional
	// reasoning) continue to advance NextOutputIdx without disturbing the
	// message item's index.
	_msgOutputIdx int
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
		if state.CreatedAt == 0 {
			state.CreatedAt = time.Now().Unix()
		}
		state.Created = true
		state.ToolCallAcc = make(map[string]*r2aStreamToolCall)
		state.FinalItems = make(map[int]map[string]any)
		state.NextOutputIdx = 0
		state.NextSummaryIdx = 0
		state.ToolOutputIdx = 0

		// Vercel AI SDK (used by Cherry Studio) validates every chunk with a
		// Zod schema. response.created requires response.{id, created_at, model};
		// response.in_progress is not in the schema at all (drops silently).
		// Without created_at/model the SDK emits an error part and discards the
		// event, but the rest of the stream continues. We include them so the
		// SDK has all it needs to construct the synthetic initial response.
		writeResponsesSSE(&out, "response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":         state.ResponseID,
				"created_at": state.CreatedAt,
				"model":      state.Model,
				"status":     "in_progress",
			},
		})
		writeResponsesSSE(&out, "response.in_progress", map[string]any{
			"type": "response.in_progress",
			"response": map[string]any{
				"id":         state.ResponseID,
				"created_at": state.CreatedAt,
				"model":      state.Model,
				"status":     "in_progress",
			},
		})

	case "content_block_start":
		var block struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				Thinking string `json:"thinking,omitempty"`
				ID       string `json:"id,omitempty"`
				Name     string `json:"name,omitempty"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(chunk.Data, &block); err != nil {
			return chunk, nil
		}
		switch block.ContentBlock.Type {
		case "thinking":
			// Register a reasoning output item at the next available index.
			// Without this, the Responses SDK sees a message item at
			// output_index 0 instead of a reasoning item and surfaces the
			// response as empty.
			if !state.ReasoningItemSent {
				state.ReasoningItemSent = true
				state.ReasoningItemID = state.ResponseID + "_reasoning_0"
				state.ReasoningOutputIdx = state.NextOutputIdx
				state.NextOutputIdx++
				writeResponsesSSE(&out, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": state.ReasoningOutputIdx,
					"item": map[string]any{
						"id":      state.ReasoningItemID,
						"type":    "reasoning",
						"status":  "in_progress",
						"summary": []any{},
					},
				})
			}
			// Some upstreams put the first thinking chunk on the start event.
			if block.ContentBlock.Thinking != "" {
				state.ReasoningContent += block.ContentBlock.Thinking
				if !state.SummaryPartSent {
					state.SummaryPartSent = true
					writeResponsesSSE(&out, "response.reasoning_summary_part.added", map[string]any{
						"type":          "response.reasoning_summary_part.added",
						"item_id":       state.ReasoningItemID,
						"output_index":  state.ReasoningOutputIdx,
						"summary_index": state.NextSummaryIdx,
						"part": map[string]any{
							"type": "summary_text",
							"text": "",
						},
					})
				}
				writeResponsesSSE(&out, "response.reasoning_summary_text.delta", map[string]any{
					"type":          "response.reasoning_summary_text.delta",
					"item_id":       state.ReasoningItemID,
					"output_index":  state.ReasoningOutputIdx,
					"summary_index": state.NextSummaryIdx,
					"delta":         block.ContentBlock.Thinking,
				})
			}
		case "text":
			state.TextBlockOpen = true
			if block.ContentBlock.Text != "" {
				state.TextContent += block.ContentBlock.Text
				if !state.MessageItemSent {
					state.MessageItemSent = true
					state.MessageItemID = state.ResponseID + "_msg_0"
					state._msgOutputIdx = state.NextOutputIdx
					state.NextOutputIdx++
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": state._msgOutputIdx,
						"item": map[string]any{
							"id":      state.MessageItemID,
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
						"output_index":  state.MessageOutputIdx(),
						"content_index": 0,
						"item_id":       state.MessageItemID,
						"part": map[string]any{
							"type":        "output_text",
							"text":        "",
							"annotations": []any{},
						},
					})
				}
				writeResponsesSSE(&out, "response.output_text.delta", map[string]any{
					"type":         "response.output_text.delta",
					"item_id":      state.MessageItemID,
					"output_index": state.MessageOutputIdx(),
					"delta":        block.ContentBlock.Text,
					"logprobs":     []any{},
				})
			}
		case "tool_use":
			oidx := state.NextOutputIdx
			state.NextOutputIdx++
			tc := &r2aStreamToolCall{
				CallID:    block.ContentBlock.ID,
				Name:      block.ContentBlock.Name,
				OutputIdx: oidx,
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
				Thinking    string `json:"thinking,omitempty"`
				Signature   string `json:"signature,omitempty"`
				PartialJSON string `json:"partial_json,omitempty"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(chunk.Data, &delta); err != nil {
			return chunk, nil
		}
		switch delta.Delta.Type {
		case "thinking_delta":
			if delta.Delta.Thinking != "" {
				state.ReasoningContent += delta.Delta.Thinking
				// Ensure the reasoning item + summary part are registered.
				if !state.ReasoningItemSent {
					state.ReasoningItemSent = true
					state.ReasoningItemID = state.ResponseID + "_reasoning_0"
					state.ReasoningOutputIdx = state.NextOutputIdx
					state.NextOutputIdx++
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": state.ReasoningOutputIdx,
						"item": map[string]any{
							"id":      state.ReasoningItemID,
							"type":    "reasoning",
							"status":  "in_progress",
							"summary": []any{},
						},
					})
				}
				if !state.SummaryPartSent {
					state.SummaryPartSent = true
					writeResponsesSSE(&out, "response.reasoning_summary_part.added", map[string]any{
						"type":          "response.reasoning_summary_part.added",
						"item_id":       state.ReasoningItemID,
						"output_index":  state.ReasoningOutputIdx,
						"summary_index": state.NextSummaryIdx,
						"part": map[string]any{
							"type": "summary_text",
							"text": "",
						},
					})
				}
				writeResponsesSSE(&out, "response.reasoning_summary_text.delta", map[string]any{
					"type":          "response.reasoning_summary_text.delta",
					"item_id":       state.ReasoningItemID,
					"output_index":  state.ReasoningOutputIdx,
					"summary_index": state.NextSummaryIdx,
					"delta":         delta.Delta.Thinking,
				})
			}
		case "signature_delta":
			if delta.Delta.Signature != "" {
				state.ReasoningSignature = delta.Delta.Signature
			}
		case "text_delta":
			if delta.Delta.Text != "" {
				state.TextContent += delta.Delta.Text
				if !state.MessageItemSent {
					state.MessageItemSent = true
					state.MessageItemID = state.ResponseID + "_msg_0"
					state._msgOutputIdx = state.NextOutputIdx
					state.NextOutputIdx++
					writeResponsesSSE(&out, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": state._msgOutputIdx,
						"item": map[string]any{
							"id":      state.MessageItemID,
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
						"output_index":  state.MessageOutputIdx(),
						"content_index": 0,
						"item_id":       state.MessageItemID,
						"part": map[string]any{
							"type":        "output_text",
							"text":        "",
							"annotations": []any{},
						},
					})
				}
				writeResponsesSSE(&out, "response.output_text.delta", map[string]any{
					"type":         "response.output_text.delta",
					"item_id":      state.MessageItemID,
					"output_index": state.MessageOutputIdx(),
					"delta":        delta.Delta.Text,
					"logprobs":     []any{},
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
						"output_index": tc.OutputIdx,
					})
					break
				}
			}
		}

	case "content_block_stop":
		// Close reasoning item if it was the block that just stopped.
		if state.ReasoningItemSent && state.SummaryPartSent {
			writeResponsesSSE(&out, "response.reasoning_summary_text.done", map[string]any{
				"type":          "response.reasoning_summary_text.done",
				"item_id":       state.ReasoningItemID,
				"output_index":  state.ReasoningOutputIdx,
				"summary_index": state.NextSummaryIdx,
				"text":          state.ReasoningContent,
			})
			writeResponsesSSE(&out, "response.reasoning_summary_part.done", map[string]any{
				"type":          "response.reasoning_summary_part.done",
				"item_id":       state.ReasoningItemID,
				"output_index":  state.ReasoningOutputIdx,
				"summary_index": state.NextSummaryIdx,
				"part": map[string]any{
					"type": "summary_text",
					"text": state.ReasoningContent,
				},
			})
			reasoningItem := map[string]any{
				"id":      state.ReasoningItemID,
				"type":    "reasoning",
				"status":  "completed",
				"summary": []any{},
			}
			if state.ReasoningSignature != "" {
				reasoningItem["encrypted_content"] = state.ReasoningSignature
			}
			writeResponsesSSE(&out, "response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": state.ReasoningOutputIdx,
				"item":         reasoningItem,
			})
			state.FinalItems[state.ReasoningOutputIdx] = reasoningItem
			state.SummaryPartSent = false
			state.ReasoningContent = ""
			state.ReasoningSignature = ""
			// Allow subsequent thinking blocks to register a new item.
			state.ReasoningItemSent = false
			state.NextSummaryIdx++
		}
		for _, tc := range state.ToolCallAcc {
			writeResponsesSSE(&out, "response.function_call_arguments.done", map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      tc.CallID,
				"call_id":      tc.CallID,
				"name":         tc.Name,
				"arguments":    tc.Arguments,
				"output_index": tc.OutputIdx,
			})
			fnItem := map[string]any{
				"type":      "function_call",
				"id":        tc.CallID,
				"call_id":   tc.CallID,
				"status":    "completed",
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
			writeResponsesSSE(&out, "response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": tc.OutputIdx,
				"item":         fnItem,
			})
			state.FinalItems[tc.OutputIdx] = fnItem
		}
		state.ToolCallAcc = make(map[string]*r2aStreamToolCall)
		state.TextBlockOpen = false

	case "message_delta":
		var md struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(chunk.Data, &md); err == nil {
			if md.Usage.InputTokens > 0 {
				state.InputTokens = md.Usage.InputTokens
			}
			if md.Usage.OutputTokens > 0 {
				state.OutputTokens = md.Usage.OutputTokens
			}
		}

	case "message_stop":
		if state.TextContent != "" {
			if state.ContentPartSent {
				writeResponsesSSE(&out, "response.content_part.done", map[string]any{
					"type":          "response.content_part.done",
					"output_index":  state.MessageOutputIdx(),
					"content_index": 0,
					"item_id":       state.MessageItemID,
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
			msgItem := map[string]any{
				"type":    "message",
				"id":      state.MessageItemID,
				"status":  "completed",
				"role":    "assistant",
				"content": msgContent,
			}
			writeResponsesSSE(&out, "response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": state.MessageOutputIdx(),
				"item":         msgItem,
			})
			state.FinalItems[state.MessageOutputIdx()] = msgItem
		}
		// Build the final `output` array in output_index order. The OpenAI
		// Responses SDK takes the `response` payload inside `response.completed`
		// as the source of truth for the final ParsedResponse — without an
		// `output` array here, the SDK returns an empty response to the client
		// even though the earlier streaming events were all correct.
		output := make([]map[string]any, 0, len(state.FinalItems))
		for i := 0; i < state.NextOutputIdx; i++ {
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
				"usage": map[string]any{
					"input_tokens":  state.InputTokens,
					"output_tokens": state.OutputTokens,
					"total_tokens":  state.InputTokens + state.OutputTokens,
				},
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

// MessageOutputIdx returns the output_index where the assistant message item
// was registered. The message item is always opened before the first text
// delta lands, so callers hitting the message_stop path before any text
// arrived will get NextOutputIdx as a placeholder.
func (s *r2aStreamState) MessageOutputIdx() int {
	if !s.MessageItemSent {
		return s.NextOutputIdx
	}
	return s._msgOutputIdx
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*Responses2Anthropic)(nil)
var _ engine.ResponseTransformer = (*Responses2Anthropic)(nil)
var _ engine.StreamResponseTransformer = (*Responses2Anthropic)(nil)
