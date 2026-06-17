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

// Anthropic2OpenAI converts Anthropic Messages requests to OpenAI Chat Completion format.
type Anthropic2OpenAI struct{}

type anthropicRequest2 struct {
    Model         string           `json:"model"`
    MaxTokens     int              `json:"max_tokens"`
    Messages      json.RawMessage  `json:"messages"`
    System        *flexibleContent `json:"system,omitempty"`
    Temperature   float64          `json:"temperature,omitempty"`
    TopP          float64          `json:"top_p,omitempty"`
    TopK          int              `json:"top_k,omitempty"`
    Stream        bool             `json:"stream,omitempty"`
    Thinking      json.RawMessage  `json:"thinking,omitempty"`
    Metadata      json.RawMessage  `json:"metadata,omitempty"`
    Tools         json.RawMessage  `json:"tools,omitempty"`
    ToolChoice    json.RawMessage  `json:"tool_choice,omitempty"`
    StopSequences []string         `json:"stop_sequences,omitempty"`
}

// anthropicContentBlock2 is a rich content block struct used for request conversion.
type anthropicContentBlock2 struct {
    Type      string          `json:"type"`
    Text      string          `json:"text,omitempty"`
    ID        string          `json:"id,omitempty"`
    Name      string          `json:"name,omitempty"`
    Input     json.RawMessage `json:"input,omitempty"`
    ToolUseID string          `json:"tool_use_id,omitempty"`
    Content   json.RawMessage `json:"content,omitempty"`
    ImageSrc  json.RawMessage `json:"source,omitempty"`
}

// PluginName returns the stable type name for deduplication.
func (t *Anthropic2OpenAI) PluginName() string { return "Anthropic2OpenAI" }

// TransformRequest converts an Anthropic Messages request into an OpenAI Chat Completion request.
func (t *Anthropic2OpenAI) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
    var anthropicReq anthropicRequest2
    if err := json.Unmarshal(body, &anthropicReq); err != nil {
        return nil, nil, fmt.Errorf("anthropic2openai: failed to parse request: %w", err)
    }

    // Default max_tokens to 4096 if not specified (Anthropic requires this, but be robust)
    maxTokens := anthropicReq.MaxTokens
    if maxTokens <= 0 {
        maxTokens = 4096
    }

    // Build OpenAI request body as a map for maximum flexibility
    openAIBody := map[string]interface{}{
        "model":       mapModelReverse(anthropicReq.Model),
        "messages":    make([]map[string]interface{}, 0),
        "max_tokens":  maxTokens,
        "temperature": anthropicReq.Temperature,
        "stream":      anthropicReq.Stream,
    }

    // Pass through top_p and top_k if set
    if anthropicReq.TopP > 0 {
        openAIBody["top_p"] = anthropicReq.TopP
    }
    if anthropicReq.TopK > 0 {
        openAIBody["top_k"] = anthropicReq.TopK
    }

    // Convert stop_sequences → stop
    if len(anthropicReq.StopSequences) > 0 {
        openAIBody["stop"] = anthropicReq.StopSequences
    }

    // Convert thinking param: thinking.type == "enabled" → thinking_budget_tokens
    if len(anthropicReq.Thinking) > 0 {
        var thinking struct {
            Type         string `json:"type"`
            BudgetTokens int    `json:"budget_tokens"`
        }
        if err := json.Unmarshal(anthropicReq.Thinking, &thinking); err == nil {
            if thinking.Type == "enabled" {
                budget := thinking.BudgetTokens
                if budget <= 0 {
                    budget = 10000
                }
                openAIBody["thinking_budget_tokens"] = budget
            }
        }
    }

    // Convert metadata.user_id → __metadata_user_id
    if len(anthropicReq.Metadata) > 0 {
        var metadata struct {
            UserID string `json:"user_id"`
        }
        if err := json.Unmarshal(anthropicReq.Metadata, &metadata); err == nil {
            if metadata.UserID != "" {
                openAIBody["__metadata_user_id"] = metadata.UserID
            }
        }
    }

    // Convert tool definitions: Anthropic → OpenAI format
    if len(anthropicReq.Tools) > 0 {
        var anthropicTools []map[string]interface{}
        if err := json.Unmarshal(anthropicReq.Tools, &anthropicTools); err == nil {
            openAITools := make([]map[string]interface{}, 0, len(anthropicTools))
            for _, at := range anthropicTools {
                name, _ := at["name"].(string)
                desc, _ := at["description"].(string)
                inputSchema := at["input_schema"]
                openAITool := map[string]interface{}{
                    "type": "function",
                    "function": map[string]interface{}{
                        "name":        name,
                        "description": desc,
                        "parameters":  inputSchema,
                    },
                }
                openAITools = append(openAITools, openAITool)
            }
            openAIBody["tools"] = openAITools
        }
    }

    // Convert tool_choice: Anthropic → OpenAI format
    if len(anthropicReq.ToolChoice) > 0 {
        var tc struct {
            Type string `json:"type"`
            Name string `json:"name,omitempty"`
        }
        if err := json.Unmarshal(anthropicReq.ToolChoice, &tc); err == nil {
            switch tc.Type {
            case "auto":
                openAIBody["tool_choice"] = "auto"
            case "any":
                openAIBody["tool_choice"] = "required"
            case "tool":
                openAIBody["tool_choice"] = map[string]interface{}{
                    "type": "function",
                    "function": map[string]interface{}{
                        "name": tc.Name,
                    },
                }
            default:
                openAIBody["tool_choice"] = tc.Type
            }
        }
    }

    // Convert system prompt
    var oaiMessages []map[string]interface{}
    if anthropicReq.System != nil && anthropicReq.System.Text != "" {
        sysContent := normalizeAnthropicBillingHeader(anthropicReq.System.Text)
        oaiMessages = append(oaiMessages, map[string]interface{}{
            "role":    "system",
            "content": sysContent,
        })
    }

    // Ensure oaiMessages is never nil so it marshals as [] not null
    if oaiMessages == nil {
        oaiMessages = make([]map[string]interface{}, 0)
    }

    // Convert Anthropic messages to OpenAI format, handling content blocks
    var anthroMessages []struct {
        Role    string          `json:"role"`
        Content json.RawMessage `json:"content"`
    }
    if err := json.Unmarshal(anthropicReq.Messages, &anthroMessages); err == nil {
        for _, msg := range anthroMessages {
            // Try plain string content first
            var contentStr string
            if json.Unmarshal(msg.Content, &contentStr) == nil {
                oaiMessages = append(oaiMessages, map[string]interface{}{
                    "role":    msg.Role,
                    "content": contentStr,
                })
                continue
            }

            // Try array of content blocks
            var blocks []anthropicContentBlock2
            if err := json.Unmarshal(msg.Content, &blocks); err != nil {
                // Unknown format — pass raw content
                oaiMessages = append(oaiMessages, map[string]interface{}{
                    "role":    msg.Role,
                    "content": string(msg.Content),
                })
                continue
            }


			// Process content blocks
			var textParts []string
			var imageParts []map[string]interface{}
			var toolCalls []openAIChatToolCall
			var reasoningContent string
			hasToolUse := false
			hasToolResult := false

			for _, block := range blocks {
				switch block.Type {
				case "text":
					if block.Text != "" {
						textParts = append(textParts, block.Text)
					}
				case "thinking":
					if block.Text != "" {
						reasoningContent += block.Text
					}
				case "image":
					img := convertAnthropicImageToOpenAI(block)
					if img != nil {
						imageParts = append(imageParts, img)
					}
				case "tool_use":
					hasToolUse = true
					tc := openAIChatToolCall{
						ID:   block.ID,
						Type: "function",
					}
					tc.Function.Name = block.Name
					if block.Input != nil {
						tc.Function.Arguments = string(block.Input)
					}
					toolCalls = append(toolCalls, tc)
				case "tool_result":
					hasToolResult = true
					var toolTextParts []string
					var toolImageParts []map[string]interface{}
					if block.Content != nil {
						var s string
						if json.Unmarshal(block.Content, &s) == nil {
							toolTextParts = append(toolTextParts, s)
						} else {
							var innerBlocks []anthropicContentBlock2
							if json.Unmarshal(block.Content, &innerBlocks) == nil {
								for _, ib := range innerBlocks {
									if ib.Type == "text" {
										toolTextParts = append(toolTextParts, ib.Text)
									} else if ib.Type == "image" {
										img := convertAnthropicImageToOpenAI(ib)
										if img != nil {
											toolImageParts = append(toolImageParts, img)
										}
									}
								}
							}
						}
					}

					toolMsg := map[string]interface{}{
						"role":         "tool",
						"tool_call_id": block.ToolUseID,
					}
					if len(toolImageParts) > 0 {
						var contentArray []interface{}
						for _, tp := range toolTextParts {
							if tp != "" {
								contentArray = append(contentArray, map[string]interface{}{
									"type": "text",
									"text": tp,
								})
							}
						}
						for _, img := range toolImageParts {
							contentArray = append(contentArray, img)
						}
						toolMsg["content"] = contentArray
					} else {
						toolMsg["content"] = joinTextParts(toolTextParts)
					}
					oaiMessages = append(oaiMessages, toolMsg)
				}
			}

			if hasToolUse {
				text := joinTextParts(textParts)
				if len(imageParts) > 0 {
					var contentArray []interface{}
					if text != "" {
						contentArray = append(contentArray, map[string]interface{}{
							"type": "text",
							"text": text,
						})
					}
					for _, img := range imageParts {
						contentArray = append(contentArray, img)
					}
					oaiMsg := map[string]interface{}{
						"role":       "assistant",
						"content":    contentArray,
						"tool_calls": toolCalls,
					}
					if reasoningContent != "" {
						oaiMsg["reasoning_content"] = reasoningContent
					}
					oaiMessages = append(oaiMessages, oaiMsg)
				} else {
					oaiMsg := map[string]interface{}{
						"role":       "assistant",
						"content":    text,
						"tool_calls": toolCalls,
					}
					if reasoningContent != "" {
						oaiMsg["reasoning_content"] = reasoningContent
					}
					oaiMessages = append(oaiMessages, oaiMsg)
				}
			} else if hasToolResult {
				// Already handled above — tool_result messages are appended inline
			} else {
				text := joinTextParts(textParts)
				if len(imageParts) > 0 {
					var contentArray []interface{}
					if text != "" {
						contentArray = append(contentArray, map[string]interface{}{
							"type": "text",
							"text": text,
						})
					}
					for _, img := range imageParts {
						contentArray = append(contentArray, img)
					}
					oaiMsg := map[string]interface{}{
						"role":    msg.Role,
						"content": contentArray,
					}
					if reasoningContent != "" {
						oaiMsg["reasoning_content"] = reasoningContent
					}
					oaiMessages = append(oaiMessages, oaiMsg)
				} else {
					oaiMsg := map[string]interface{}{
						"role":    msg.Role,
						"content": text,
					}
					if reasoningContent != "" {
						oaiMsg["reasoning_content"] = reasoningContent
					}
					oaiMessages = append(oaiMessages, oaiMsg)
				}
			}
		}
	}


    openAIBody["messages"] = oaiMessages

    newBody, err := json.Marshal(openAIBody)
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

// joinTextParts concatenates text parts with a newline.
func joinTextParts(parts []string) string {
    switch len(parts) {
    case 0:
        return ""
    case 1:
        return parts[0]
    default:
        var b bytes.Buffer
        for i, p := range parts {
            if i > 0 {
                b.WriteString("\n")
            }
            b.WriteString(p)
        }
        return b.String()
    }
}

// normalizeAnthropicBillingHeader scrubs Claude Code billing headers from system prompt text.
// Replaces the 5 characters after "cch=" with "fffff" to prevent usage data leakage.
func normalizeAnthropicBillingHeader(systemText string) string {
    const prefix = "x-anthropic-billing-header:"
    if !strings.HasPrefix(systemText, prefix) {
        return systemText
    }
    afterPrefix := systemText[len(prefix):]
    cchIdx := strings.Index(afterPrefix, "cch=")
    if cchIdx < 0 {
        return systemText
    }
    replaceIdx := len(prefix) + cchIdx + 4 // position of first char after "cch="
    if replaceIdx+5 < len(systemText) && systemText[replaceIdx+5] == ';' {
        return systemText[:replaceIdx] + "fffff" + systemText[replaceIdx+5:]
    }
    return systemText
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
    outputTextLen := 0 // accumulated output text length (for usage estimate)

    parseOpenAISSE(body, func(data []byte) bool {
        var chunk openAIChunk
        if err := json.Unmarshal(data, &chunk); err != nil {
            return false
        }

        if !messageStarted {
            id = chunk.ID
            model = mapModel(chunk.Model)
            messageStarted = true
            // Emit message_start to match Anthropic SSE protocol
            msg := struct {
                Type    string `json:"type"`
                Message struct {
                    ID    string `json:"id"`
                    Model string `json:"model"`
                    Usage struct {
                        InputTokens  int `json:"input_tokens"`
                        OutputTokens int `json:"output_tokens"`
                    } `json:"usage"`
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
                // Always emit content_block_start immediately, even with empty text.
                // Deferring (pendingContentBlock) caused an empty data: \n\n SSE event
                // that could confuse Anthropic SDK event parsers.
                msg := struct {
                    Type         string `json:"type"`
                    ContentBlock struct {
                        Type string `json:"type"`
                        Text string `json:"text"`
                    } `json:"content_block"`
                    Index int `json:"index"`
                }{
                    Type:  "content_block_start",
                    Index: choice.Index,
                }
                msg.ContentBlock.Type = "text"
                msg.ContentBlock.Text = choice.Delta.Content
                outputTextLen += len(choice.Delta.Content)
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
                outputTextLen += len(choice.Delta.Content)
                writeAnthropicSSE(&out, "content_block_delta", delta)
            }

            if choice.FinishReason != nil {
                stopReason := *choice.FinishReason
                if stopReason == "stop" {
                    stopReason = "end_turn"
                }
                if stopReason == "tool_calls" {
                    stopReason = "tool_use"
                }
                writeAnthropicSSE(&out, "content_block_stop", struct {
                    Type  string `json:"type"`
                    Index int    `json:"index"`
                }{Type: "content_block_stop", Index: choice.Index})
                outputTokens := outputTextLen / 4
                if outputTokens < 1 {
                    outputTokens = 1
                }
                msgDelta := struct {
                    Type  string `json:"type"`
                    Delta struct {
                        StopReason   string `json:"stop_reason"`
                        StopSequence string `json:"stop_sequence"`
                    } `json:"delta"`
                    Usage struct {
                        OutputTokens int `json:"output_tokens"`
                    } `json:"usage"`
                }{
                    Type: "message_delta",
                }
                msgDelta.Usage.OutputTokens = outputTokens
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
        // Not valid JSON (e.g. downstream error page) — pass through unchanged
        return body, nil
    }

    // Build Anthropic response content blocks (text + tool_use)
    content := make([]anthropicContent, 0)
    for _, choice := range openAIResp.Choices {
        // Add text content if present
        if choice.Message.Content != "" {
            content = append(content, anthropicContent{Type: "text", Text: choice.Message.Content})
        }
        // Add tool_use content blocks for each tool call
        for _, tc := range choice.Message.ToolCalls {
            input := json.RawMessage(tc.Function.Arguments)
            // Parse arguments string as JSON for a prettier output
            var parsed interface{}
            if json.Unmarshal([]byte(tc.Function.Arguments), &parsed) == nil {
                input, _ = json.Marshal(parsed)
            }
            content = append(content, anthropicContent{
                Type:  "tool_use",
                ID:    tc.ID,
                Name:  tc.Function.Name,
                Input: input,
            })
        }
    }

    stopReason := "end_turn"
    if len(openAIResp.Choices) > 0 && openAIResp.Choices[0].FinishReason == "stop" {
        stopReason = "end_turn"
    } else if len(openAIResp.Choices) > 0 && openAIResp.Choices[0].FinishReason == "length" {
        stopReason = "max_tokens"
    }

    anthropicResp := anthropicResponse{
        ID:      openAIResp.ID,
        Model:   mapModel(openAIResp.Model),
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

    // Check for [DONE] marker BEFORE JSON parsing — it is not valid JSON.
    if string(bytes.TrimSpace(chunk.Data)) == "[DONE]" {
        if state.messageDeltaSent {
            // Finish reason was already emitted — just close the stream.
            writeAnthropicSSE(&out, "message_stop", struct{ Type string `json:"type"` }{Type: "message_stop"})
        } else {
            // No finish reason seen (some non-OpenAI servers omit it) — emit message_delta + message_stop.
            outputTokens := state.outputTokens
            if outputTokens < 1 {
                outputTokens = 1
            }
            msgDelta := struct {
                Type  string `json:"type"`
                Delta struct {
                    StopReason   string `json:"stop_reason"`
                    StopSequence string `json:"stop_sequence"`
                } `json:"delta"`
                Usage struct {
                    OutputTokens int `json:"output_tokens"`
                } `json:"usage"`
            }{Type: "message_delta"}
            msgDelta.Usage.OutputTokens = outputTokens
            msgDelta.Delta.StopReason = "end_turn"
            writeAnthropicSSE(&out, "message_delta", msgDelta)
            writeAnthropicSSE(&out, "message_stop", struct{ Type string `json:"type"` }{Type: "message_stop"})
        }
        return engine.SSEChunk{EventType: "", Data: out.Bytes()}, nil
    }

    // Parse the OpenAI chunk
    var oaiChunk openAIChunk
    if err := json.Unmarshal(chunk.Data, &oaiChunk); err != nil {
        // Not valid JSON — pass through unchanged
        return chunk, nil
    }

    // First chunk: emit message_start
    if !state.messageStarted {
        state.ID = oaiChunk.ID
        state.Model = mapModel(oaiChunk.Model)
        state.messageStarted = true
        // Initialize tool call tracking
        if state.toolCallAcc == nil {
            state.toolCallAcc = make(map[int]*toolCallAccum)
        }
        msg := struct {
            Type    string `json:"type"`
            Message struct {
                ID    string `json:"id"`
                Model string `json:"model"`
                Usage struct {
                    InputTokens  int `json:"input_tokens"`
                    OutputTokens int `json:"output_tokens"`
                } `json:"usage"`
            } `json:"message"`
        }{Type: "message_start"}
        msg.Message.ID = state.ID
        msg.Message.Model = state.Model
        writeAnthropicSSE(&out, "message_start", msg)
    }

    // Ensure tool call accumulator map is initialized
    if state.toolCallAcc == nil {
        state.toolCallAcc = make(map[int]*toolCallAccum)
    }

    for _, choice := range oaiChunk.Choices {
        if choice.Delta.Role == "assistant" && !state.inContentBlock {
            // Always emit content_block_start immediately, even with empty text.
            msg := struct {
                Type         string `json:"type"`
                ContentBlock struct {
                    Type string `json:"type"`
                    Text string `json:"text"`
                } `json:"content_block"`
                Index int `json:"index"`
            }{Type: "content_block_start", Index: choice.Index}
            msg.ContentBlock.Type = "text"
            msg.ContentBlock.Text = choice.Delta.Content
            state.outputTokens += len(choice.Delta.Content)
            state.contentBlockIdx++
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
            state.outputTokens += len(choice.Delta.Content)
            writeAnthropicSSE(&out, "content_block_delta", delta)
        }

        // Handle tool calls in streaming delta
        for _, tc := range choice.Delta.ToolCalls {
            existing, exists := state.toolCallAcc[tc.Index]
            if !exists {
                // New tool call — emit content_block_start with type "tool_use"
                state.contentBlockIdx++
                acc := &toolCallAccum{
                    assignedBlockIdx: state.contentBlockIdx,
                    id:               tc.ID,
                    name:             tc.Function.Name,
                }
                state.toolCallAcc[tc.Index] = acc

                msg := struct {
                    Type         string `json:"type"`
                    ContentBlock struct {
                        Type string `json:"type"`
                        ID   string `json:"id"`
                        Name string `json:"name"`
                    } `json:"content_block"`
                    Index int `json:"index"`
                }{
                    Type:  "content_block_start",
                    Index: state.contentBlockIdx,
                }
                msg.ContentBlock.Type = "tool_use"
                msg.ContentBlock.ID = tc.ID
                msg.ContentBlock.Name = tc.Function.Name
                writeAnthropicSSE(&out, "content_block_start", msg)

                // Emit first arguments fragment if present
                if tc.Function.Arguments != "" {
                    acc.argumentsAccum += tc.Function.Arguments
                    delta := struct {
                        Type  string `json:"type"`
                        Index int    `json:"index"`
                        Delta struct {
                            Type        string `json:"type"`
                            PartialJSON string `json:"partial_json"`
                        } `json:"delta"`
                    }{
                        Type:  "content_block_delta",
                        Index: state.contentBlockIdx,
                    }
                    delta.Delta.Type = "input_json_delta"
                    delta.Delta.PartialJSON = tc.Function.Arguments
                    writeAnthropicSSE(&out, "content_block_delta", delta)
                }
            } else {
                // Existing tool call — append arguments delta
                if tc.Function.Arguments != "" {
                    existing.argumentsAccum += tc.Function.Arguments
                    delta := struct {
                        Type  string `json:"type"`
                        Index int    `json:"index"`
                        Delta struct {
                            Type        string `json:"type"`
                            PartialJSON string `json:"partial_json"`
                        } `json:"delta"`
                    }{
                        Type:  "content_block_delta",
                        Index: existing.assignedBlockIdx,
                    }
                    delta.Delta.Type = "input_json_delta"
                    delta.Delta.PartialJSON = tc.Function.Arguments
                    writeAnthropicSSE(&out, "content_block_delta", delta)
                }
            }
        }

        if choice.FinishReason != nil {
            stopReason := *choice.FinishReason
            switch stopReason {
            case "stop":
                stopReason = "end_turn"
            case "tool_calls":
                stopReason = "tool_use"
            case "length":
                stopReason = "max_tokens"
            }

            // Close text content block if open
            if state.inContentBlock {
                writeAnthropicSSE(&out, "content_block_stop", struct {
                    Type  string `json:"type"`
                    Index int    `json:"index"`
                }{Type: "content_block_stop", Index: choice.Index})
                state.inContentBlock = false
            }

            // Close tool call content blocks
            for _, acc := range state.toolCallAcc {
                writeAnthropicSSE(&out, "content_block_stop", struct {
                    Type  string `json:"type"`
                    Index int    `json:"index"`
                }{Type: "content_block_stop", Index: acc.assignedBlockIdx})
            }
            state.toolCallAcc = nil

            outputTokens := state.outputTokens / 4
            if outputTokens < 1 {
                outputTokens = 1
            }
            msgDelta := struct {
                Type  string `json:"type"`
                Delta struct {
                    StopReason   string `json:"stop_reason"`
                    StopSequence string `json:"stop_sequence"`
                } `json:"delta"`
                Usage struct {
                    OutputTokens int `json:"output_tokens"`
                } `json:"usage"`
            }{Type: "message_delta"}
            msgDelta.Usage.OutputTokens = outputTokens
            msgDelta.Delta.StopReason = stopReason
            writeAnthropicSSE(&out, "message_delta", msgDelta)
            state.messageDeltaSent = true
        }
    }

    return engine.SSEChunk{EventType: "", Data: out.Bytes()}, nil
}

// toolCallAccum tracks a tool call being built across streaming chunks.
type toolCallAccum struct {
    assignedBlockIdx int
    id               string
    name             string
    argumentsAccum   string
}

// anthropic2openaiStreamState tracks state across SSE chunks for a single stream.
type anthropic2openaiStreamState struct {
    ID               string
    Model            string
    messageStarted   bool
    inContentBlock   bool
    messageDeltaSent bool
    outputTokens     int // accumulated output text (rough estimate for usage)

    // Tool call tracking across chunks
    contentBlockIdx int                    // sequential content block index across the message
    toolCallAcc     map[int]*toolCallAccum // OpenAI tool_call index → accumulator
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*Anthropic2OpenAI)(nil)
var _ engine.ResponseTransformer = (*Anthropic2OpenAI)(nil)
var _ engine.StreamResponseTransformer = (*Anthropic2OpenAI)(nil)

// convertAnthropicImageToOpenAI converts an Anthropic-style image content block
// to an OpenAI-style image_url part. Returns nil if the image is not valid.
func convertAnthropicImageToOpenAI(block anthropicContentBlock2) map[string]interface{} {
	if block.Type != "image" || len(block.ImageSrc) == 0 {
		return nil
	}

	var src map[string]interface{}
	if err := json.Unmarshal(block.ImageSrc, &src); err != nil {
		return nil
	}

	srcType, _ := src["type"].(string)
	data, _ := src["data"].(string)
	mediaType, _ := src["media_type"].(string)

	switch srcType {
	case "base64":
		if data == "" {
			return nil
		}
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]interface{}{
			"type": "image_url",
			"image_url": map[string]interface{}{
				"url": "data:" + mediaType + ";base64," + data,
			},
		}
	case "url":
		url, _ := src["url"].(string)
		if url == "" {
			return nil
		}
		return map[string]interface{}{
			"type": "image_url",
			"image_url": map[string]interface{}{
				"url": url,
			},
		}
	}

	return nil
}


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
