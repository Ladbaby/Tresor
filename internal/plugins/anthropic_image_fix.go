package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"tresor/internal/engine"
)

// FixAnthropicImages extracts image parts nested inside tool_result content
// and promotes them to top-level message content parts.
//
// Some Anthropic-compatible backends (e.g. llama.cpp) cannot handle images
// nested inside tool_result.content[]. This plugin rewrites the request so
// that images become sibling parts of the user message, which all backends
// can process correctly.
//
// Reference: https://github.com/ggml-org/llama.cpp/pull/22536
type FixAnthropicImages struct{}

// TransformRequest rewrites tool_result image content into top-level user
// image parts before forwarding the request.
func (t *FixAnthropicImages) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("fix_anthropic_images: failed to parse request: %w", err)
	}

	rewritten, changed := rewriteToolResultImages(payload)
	if !changed {
		return req, body, nil
	}

	newBody, err := json.Marshal(rewritten)
	if err != nil {
		return nil, nil, fmt.Errorf("fix_anthropic_images: failed to marshal rewritten request: %w", err)
	}

	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(newBody))
	newReq.ContentLength = int64(len(newBody))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}

	return newReq, newBody, nil
}

// TransformResponse is a no-op for this plugin.
func (t *FixAnthropicImages) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	return body, nil
}

// TransformStreamChunk passes the chunk through unchanged.
func (t *FixAnthropicImages) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	return chunk, nil
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*FixAnthropicImages)(nil)
var _ engine.ResponseTransformer = (*FixAnthropicImages)(nil)
var _ engine.StreamResponseTransformer = (*FixAnthropicImages)(nil)

// rewriteToolResultImages iterates over the messages array in an Anthropic
// request payload and extracts image parts from tool_result.content[] into
// top-level message content. Returns the (possibly modified) payload and a
// boolean indicating whether any changes were made.
func rewriteToolResultImages(payload map[string]interface{}) (map[string]interface{}, bool) {
	if payload == nil {
		return payload, false
	}

	messages, ok := payload["messages"].([]interface{})
	if !ok {
		return payload, false
	}

	rewrittenMessages := make([]interface{}, 0, len(messages))
	rewroteAny := false

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			rewrittenMessages = append(rewrittenMessages, msg)
			continue
		}

		content, ok := msgMap["content"].([]interface{})
		if !ok {
			rewrittenMessages = append(rewrittenMessages, msgMap)
			continue
		}

		newContent := make([]interface{}, 0, len(content))
		extractedImages := make([]interface{}, 0)

		for _, part := range content {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				newContent = append(newContent, part)
				continue
			}

			images := extractImagePartsFromToolResult(partMap)
			if len(images) > 0 {
				extractedImages = append(extractedImages, images...)
				rewroteAny = true
				// Drop the tool_result part — images are promoted.
				continue
			}

			newContent = append(newContent, partMap)
		}

		if len(extractedImages) > 0 {
			// If removing tool_results left the message empty, add a placeholder.
			if len(newContent) == 0 {
				newContent = append(newContent, map[string]interface{}{
					"type": "text",
					"text": "[Proxy rewrite] Converted tool_result image(s) into top-level user image part(s).",
				})
			}
			newContent = append(newContent, extractedImages...)
		}

		rewrittenMsg := make(map[string]interface{})
		for k, v := range msgMap {
			rewrittenMsg[k] = v
		}
		rewrittenMsg["content"] = newContent
		rewrittenMessages = append(rewrittenMessages, rewrittenMsg)
	}

	if !rewroteAny {
		return payload, false
	}

	rewritten := make(map[string]interface{})
	for k, v := range payload {
		rewritten[k] = v
	}
	rewritten["messages"] = rewrittenMessages
	return rewritten, true
}

// extractImagePartsFromToolResult extracts image parts from a tool_result
// content block. It returns a slice of Anthropic-style image parts ready to
// be inserted as top-level message content.
func extractImagePartsFromToolResult(part map[string]interface{}) []interface{} {
	if part["type"] != "tool_result" {
		return nil
	}

	content, ok := part["content"].([]interface{})
	if !ok || len(content) == 0 {
		return nil
	}

	var imageParts []interface{}
	for _, item := range content {
		itemMap, ok := item.(map[string]interface{})
		if !ok || itemMap["type"] != "image" {
			continue
		}

		source, ok := itemMap["source"].(map[string]interface{})
		if !ok || source["type"] != "base64" {
			continue
		}

		data, _ := source["data"].(string)
		if data == "" {
			continue
		}

		imageParts = append(imageParts, map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": source["media_type"],
				"data":       data,
			},
		})
	}

	return imageParts
}
