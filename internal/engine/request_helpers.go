package engine

import (
	"encoding/json"
	"mime"
	"strings"
)

// isLLMPath returns true if the path looks like an LLM API endpoint.
func isLLMPath(path string) bool {
	if strings.HasPrefix(path, "/v1/") {
		return true
	}
	switch path {
	case "/chat/completions", "/completions", "/models", "/embeddings":
		return true
	case "/messages", "/count_tokens":
		return true
	}
	// Gemini endpoints: /v1beta/models and /v1beta/models/{model}:{action}
	if strings.HasPrefix(path, "/v1beta/models") {
		return true
	}
	return false
}

// isGenerationEndpoint reports whether the path is a generation endpoint whose
// response carries conversational content (text, tool_use, function_call, etc.).
// These are the only endpoints where retry_on_empty makes sense: empty
// generation responses are the symptom of an upstream coil that produced no
// content (e.g. thinking-only).
//
// Utility endpoints like /v1/messages/count_tokens, /v1/models,
// /v1/embeddings, and /v1beta/models/* return structured data with no
// `content` field. Treating them as empty would trigger spurious retries
// and eventually a 502 (see count-tokens-trigger-empty-response.txt for a
// real capture: `{"input_tokens":7802}`).
func isGenerationEndpoint(path string) bool {
	// Normalize: clients may hit /v1/... or bare paths such as /messages.
	// Strip a leading /v1 prefix so both forms map to the same canonical
	// suffix.
	normalized := path
	if strings.HasPrefix(normalized, "/v1/") {
		normalized = strings.TrimPrefix(normalized, "/v1")
	}
	switch normalized {
	case "/chat/completions",
		"/completions",
		"/messages",
		"/responses":
		return true
	}
	return false
}

// isStreamRequest reports whether the request body is a JSON object carrying
// "stream": true. It is a best-effort parse: anything that is not a parseable
// object with a boolean stream field is treated as non-streaming.
func isStreamRequest(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Stream
}

// extractModel parses the request body JSON to find the "model" field.
// For Gemini paths (/v1beta/models/{model}:action), the model is embedded
// in the URL path instead of the body, so we extract it from pathFallback
// when a body parse returns nothing.
func extractModel(body []byte, pathFallback string) string {
	if len(body) > 0 {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err == nil && payload.Model != "" {
			return payload.Model
		}
	}
	// Fallback: parse from path. Gemini-style: /v1beta/models/{model}:{action}
	if m := geminiModelFromPath(pathFallback); m != "" {
		return m
	}
	return ""
}

// geminiModelFromPath extracts the model segment from a Gemini path.
// Examples:
//
//	/v1beta/models                              → ""
//	/v1beta/models/gemini-2.5-pro               → "gemini-2.5-pro"
//	/v1beta/models/gemini-2.5-pro:generateContent          → "gemini-2.5-pro"
//	/v1beta/models/qwen3.5:9b-mtp:instruct:streamGenerateContent → "qwen3.5:9b-mtp:instruct"
//
// Returns "" for non-Gemini paths.
//
// Model names may legitimately contain colons (e.g. self-hosted models like
// "qwen3.5:9b-mtp:instruct"), so we only strip a trailing ":{action}" when
// {action} is a known Gemini verb. Anything else is part of the model id.
func geminiModelFromPath(path string) string {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return ""
	}
	// Strip a trailing slash if any.
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return ""
	}
	// Strip ONLY a trailing known Gemini action. Otherwise the colon is
	// part of the model id (e.g. "qwen3.5:9b-mtp:instruct").
	knownActions := []string{
		":streamGenerateContent",
		":generateContent",
		":countTokens",
		":embedContent",
		":batchGenerateContent",
	}
	for _, suffix := range knownActions {
		if strings.HasSuffix(rest, suffix) {
			rest = strings.TrimSuffix(rest, suffix)
			break
		}
	}
	return rest
}

// rewriteModelInBody replaces the "model" field in a JSON request body with
// the given output model name. Returns the original body if parsing fails.
func rewriteModelInBody(body []byte, outputModel string) []byte {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body // not JSON, pass through unchanged
	}
	if _, ok := payload["model"]; ok {
		payload["model"] = outputModel
	}
	newBody, err := json.Marshal(payload)
	if err != nil {
		return body // marshal failed, return original
	}
	return newBody
}

// detectInputFormat determines the API format of an incoming request based on its URL path.
// Returns "openai" for /v1/chat/completions, "anthropic" for /v1/messages, "openai_responses"
// for /v1/responses, or "gemini" for /v1beta/models/* action paths.
func detectInputFormat(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "openai"
	case "/v1/messages":
		return "anthropic"
	case "/v1/responses":
		return "openai_responses"
	}
	// Gemini action paths: /v1beta/models/{model}:generateContent,
	// /v1beta/models/{model}:streamGenerateContent, /v1beta/models/{model}:countTokens.
	// /v1beta/models (without an action suffix) is a listing endpoint and has no body.
	if strings.HasPrefix(path, "/v1beta/models/") {
		return "gemini"
	}
	return ""
}

func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = contentType
	}
	return strings.EqualFold(mediaType, "text/event-stream")
}

// truncateString truncates a string to n characters, adding "..." if truncated.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
