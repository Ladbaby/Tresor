package engine

import (
	"encoding/json"
	"strings"
)

// canonicalMessage builds a plain canonical Chat message with string content.
func canonicalMessage(role, text string) ChatMessage {
	content, _ := json.Marshal(text)
	return ChatMessage{"role": json.RawMessage(quoteJSON(role)), "content": content}
}

// systemMessage is the canonical leading system turn.
func systemMessage(text string) ChatMessage { return canonicalMessage("system", text) }

// canonicalContentMessage builds a message whose content is plain text, or a
// block list when images are present. Images must be carried as blocks so they
// reach the downstream model instead of being dropped.
func canonicalContentMessage(role string, texts []string, images []json.RawMessage) ChatMessage {
	message := ChatMessage{"role": json.RawMessage(quoteJSON(role))}
	text := strings.Join(nonEmpty(texts), "\n")
	if len(images) == 0 {
		content, _ := json.Marshal(text)
		message["content"] = content
		return message
	}
	blocks := []any{}
	if text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	for _, source := range images {
		if block, ok := canonicalImageBlock(source); ok {
			blocks = append(blocks, block)
		}
	}
	content, _ := json.Marshal(blocks)
	message["content"] = content
	return message
}

// canonicalImageBlock turns a provider image descriptor into the canonical
// OpenAI image_url block, which is what the downstream translation understands.
func canonicalImageBlock(source json.RawMessage) (map[string]any, bool) {
	var media struct {
		Type   string `json:"type"`
		Media  string `json:"media_type"`
		Data   string `json:"data"`
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(source, &media) != nil {
		return nil, false
	}
	switch media.Type {
	case "base64":
		if media.Data == "" || media.Media == "" {
			return nil, false
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + media.Media + ";base64," + media.Data},
		}, true
	case "url":
		if media.URL == "" {
			return nil, false
		}
		imageURL := map[string]any{"url": media.URL}
		if media.Detail != "" {
			imageURL["detail"] = media.Detail
		}
		return map[string]any{"type": "image_url", "image_url": imageURL}, true
	}
	return nil, false
}

// canonicalToolResult builds the canonical tool message that answers one call.
func canonicalToolResult(id, text string, isError bool) ChatMessage {
	if isError {
		// Mark the failure in-band so the model can see it, matching the
		// canonical format's lack of a dedicated error flag.
		text = "error: " + text
	}
	content, _ := json.Marshal(text)
	idJSON, _ := json.Marshal(id)
	return ChatMessage{
		"role":         json.RawMessage(`"tool"`),
		"tool_call_id": idJSON,
		"content":      content,
	}
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// quoteJSON renders a string as a JSON string literal for a raw message field.
func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// remapToolCallID rewrites one canonical tool call's id in place, matching the
// call by its name and arguments so a repeated call is rewritten once rather
// than all calls to the same function. A format that cannot echo an id (Gemini)
// needs this, because the id the loop records must be one the next request can
// reproduce; leaving the downstream's id would make that request unresolvable.
func remapToolCallID(message ChatMessage, name string, arguments json.RawMessage, from, to string) {
	if len(message["tool_calls"]) == 0 || from == to {
		return
	}
	var calls []map[string]json.RawMessage
	if json.Unmarshal(message["tool_calls"], &calls) != nil {
		return
	}
	for _, call := range calls {
		var id string
		var function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		_ = json.Unmarshal(call["id"], &id)
		_ = json.Unmarshal(call["function"], &function)
		if id != from || function.Name != name || function.Arguments != string(arguments) {
			continue
		}
		call["id"], _ = json.Marshal(to)
		message["tool_calls"], _ = json.Marshal(calls)
		return
	}
}
