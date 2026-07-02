package plugins

import (
	"strings"
)

// --- Gemini request types (generateContent body) ---

// geminiPart is one element of a Gemini contents[].parts array.
// Note: the request side uses functionCall for assistant tool calls and
// functionResponse (an inline map) for tool results; only functionCall is
// represented as a typed field here since responses are the main consumer.
type geminiPart struct {
	Text         string              `json:"text,omitempty"`
	Thought      bool                `json:"thought,omitempty"`
	InlineData   *geminiInlineData   `json:"inlineData,omitempty"`
	FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
}

// geminiInlineData is the inlineData object embedded in a geminiPart.
type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// geminiFunctionCall is the functionCall object embedded in a geminiPart.
type geminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

// geminiContent is one element of a Gemini "contents" array.
type geminiContent struct {
	Role  string       `json:"role"` // "user" or "model"
	Parts []geminiPart `json:"parts"`
}

// geminiUsageMetadata mirrors Gemini's usageMetadata object on the response.
type geminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

// geminiCandidate is one element of a Gemini response's candidates array.
type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
	Index        int           `json:"index"`
}

// geminiGenerateContentResponse mirrors a single Gemini generateContentResponse
// payload (used for both non-streaming responses and individual SSE chunks).
type geminiGenerateContentResponse struct {
	Candidates    []geminiCandidate     `json:"candidates,omitempty"`
	UsageMetadata *geminiUsageMetadata  `json:"usageMetadata,omitempty"`
	ModelVersion  string                `json:"modelVersion,omitempty"`
}

// parseGeminiSSE walks an SSE byte stream and calls fn for each data: payload.
// Gemini SSE has no event: lines — each data: line is a JSON object.
func parseGeminiSSE(data []byte, fn func(data []byte) bool) {
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := []byte(strings.TrimPrefix(line, "data: "))
		if !fn(payload) {
			return
		}
	}
}