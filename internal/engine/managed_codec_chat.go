package engine

import (
	"encoding/json"
	"fmt"
	"time"
)

// openAIChatCodec serves the native OpenAI Chat format, which is also the
// canonical form the managed loop works in. Decoding is therefore the existing
// validation; nothing is translated.
type openAIChatCodec struct{}

func (c *openAIChatCodec) Name() string   { return "openai-chat.v1" }
func (c *openAIChatCodec) Format() string { return "openai" }

func (c *openAIChatCodec) Decode(raw []byte, path string) (DecodedRequest, error) {
	request, err := ParseManagedChatRequest(raw)
	if err != nil {
		return DecodedRequest{}, err
	}
	return DecodedRequest{
		Messages: request.Messages,
		Model:    request.Model,
		Stream:   clientWantsStream(raw, path),
		Raw:      request.Raw,
	}, nil
}

// AcceptsOpaque keeps the historical stance for the native format: only plain
// textual reasoning is permitted. Structured provider state is rejected, as it
// always has been, because this format has no verified echo contract here.
func (c *openAIChatCodec) AcceptsOpaque(key string, value json.RawMessage) bool {
	return false
}

// Renderer returns the wire renderer for this format.
func (c *openAIChatCodec) Renderer() StreamRenderer { return chatRenderer{} }

// ClientCallID keeps the downstream's call id: a chat client echoes it verbatim
// on the turn that answers the call.
func (c *openAIChatCodec) ClientCallID(call ToolCall, occurrence int) string { return call.ID }

// decodeCanonical validates a canonical transcript that already arrived in Chat
// shape. Formats that translate on decode still need the canonical invariants
// checked, so they call this after converting.
func decodeCanonical(messages []ChatMessage, model string, stream bool, raw map[string]json.RawMessage) (DecodedRequest, error) {
	if len(messages) == 0 {
		return DecodedRequest{}, fmt.Errorf("%w: messages required", ErrManagedChat)
	}
	return DecodedRequest{Messages: messages, Model: model, Stream: stream, Raw: raw}, nil
}

// chatRenderer reproduces the Chat wire exactly as the streamer emitted it
// before the codec indirection, so the native format stays byte-compatible.
type chatRenderer struct{}

func (chatRenderer) FrameStart() []byte { return nil }

func (chatRenderer) FrameProgress(text string) []byte {
	chunk, err := json.Marshal(map[string]any{
		"id": "chatcmpl-managed", "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"reasoning_content": text}, "finish_reason": nil,
		}},
	})
	if err != nil {
		return nil
	}
	return sseData(chunk)
}

func (chatRenderer) FrameContent(text string) []byte {
	chunk, err := json.Marshal(map[string]any{
		"id": "chatcmpl-managed", "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": text}, "finish_reason": nil,
		}},
	})
	if err != nil {
		return nil
	}
	return sseData(chunk)
}

func (chatRenderer) FrameFinish(usage json.RawMessage) []byte {
	stop := map[string]any{
		"id": "chatcmpl-managed", "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}
	if len(usage) > 0 {
		stop["usage"] = usage
	}
	chunk, err := json.Marshal(stop)
	if err != nil {
		return nil
	}
	return sseData(chunk)
}

func (chatRenderer) FrameEnd() []byte { return []byte("data: [DONE]\n\n") }

// FrameToolCalls renders the calls a stored turn requested, each as the delta a
// chat stream carries so a client sees the calls it must answer.
func (chatRenderer) FrameToolCalls(calls []ToolCall) []byte {
	out := []byte{}
	for index, call := range calls {
		chunk, err := json.Marshal(map[string]any{
			"id": "chatcmpl-managed", "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": index, "id": call.ID, "type": "function",
					"function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)},
				}},
			}}},
		})
		if err != nil {
			return out
		}
		out = append(out, sseData(chunk)...)
	}
	return out
}

func (chatRenderer) FrameError(code string) []byte {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": code}})
	return sseData(payload)
}

// sseData frames one payload as an SSE data event.
func sseData(payload []byte) []byte { return []byte("data: " + string(payload) + "\n\n") }
