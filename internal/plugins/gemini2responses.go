package plugins

import (
	"bytes"
	"fmt"
	"net/http"

	"tresor/internal/engine"
)

// Gemini2Responses converts Google Gemini generateContent requests to OpenAI
// Responses API requests and converts the Responses response (JSON or SSE)
// back to Gemini format.
//
// Auth: Responses API uses Bearer Authorization. The model moves from the
// URL path to the body's `model` field; the URL is rewritten to /v1/responses.
//
// Implementation: this plugin composes Gemini2OpenAI (Gemini -> OpenAI Chat
// Completions, /v1beta/models/* -> /v1/chat/completions) and OpenAI2Responses
// (Chat Completions -> Responses API, /v1/chat/completions -> /v1/responses)
// rather than reimplementing both mappings. The reverse composition is used
// on the response side.
//
// Streaming state: OpenAI2Responses stores its per-stream state under
// ctx.Variables["o2r_stream"]. Reusing that key keeps streaming state
// consistent whether the transformer is invoked directly or via this
// composition.
type Gemini2Responses struct{}

// PluginName returns the stable type name for deduplication.
func (t *Gemini2Responses) PluginName() string { return "Gemini2Responses" }

// TransformRequest converts a Gemini generateContent request into an OpenAI
// Responses API request by chaining Gemini2OpenAI and OpenAI2Responses.
func (t *Gemini2Responses) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	step1 := &Gemini2OpenAI{}
	req1, body1, err := step1.TransformRequest(req, body, ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini2responses (gemini->openai step): %w", err)
	}

	step2 := &OpenAI2Responses{}
	req2, body2, err := step2.TransformRequest(req1, body1, ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini2responses (openai->responses step): %w", err)
	}

	return req2, body2, nil
}

// TransformResponse converts an OpenAI Responses API JSON response into a
// Gemini generateContentResponse by chaining OpenAI2Responses and
// Gemini2OpenAI on the response side. The engine only invokes this for
// non-streaming responses — streaming responses go through
// TransformStreamChunk instead (see engine.go's isEventStream branch).
func (t *Gemini2Responses) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	// Responses API -> Chat Completions JSON
	respToChat := &OpenAI2Responses{}
	chatBody, err := respToChat.TransformResponse(resp, body, ctx)
	if err != nil {
		return nil, fmt.Errorf("gemini2responses (responses->openai response): %w", err)
	}

	// Build a synthetic response carrying the rewritten body so the inner
	// Gemini2OpenAI transformer sees Chat Completions content-type.
	chatResp := &http.Response{
		StatusCode: resp.StatusCode,
		Header:     http.Header{},
		Body:       http.NoBody,
	}
	chatResp.Header.Set("Content-Type", "application/json")
	if resp != nil {
		for k, v := range resp.Header {
			if k == "Content-Type" || k == "Content-Length" || k == "Content-Encoding" {
				continue
			}
			chatResp.Header[k] = v
		}
	}

	// Chat Completions -> Gemini
	chatToGemini := &Gemini2OpenAI{}
	geminiBody, err := chatToGemini.TransformResponse(chatResp, chatBody, ctx)
	if err != nil {
		return nil, fmt.Errorf("gemini2responses (openai->gemini response): %w", err)
	}
	return geminiBody, nil
}

// TransformStreamChunk converts a single OpenAI Responses SSE chunk into the
// equivalent Gemini SSE chunks. The Responses->Chat transformer may emit
// multiple Chat Completions events from a single Responses event, so each
// emitted Chat event is then fed through Gemini2OpenAI's stream transformer.
func (t *Gemini2Responses) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	// Drop [DONE] — Gemini does not use it.
	if bytes.Equal(bytes.TrimSpace(chunk.Data), []byte("[DONE]")) {
		return engine.SSEChunk{}, nil
	}

	// Responses -> Chat Completions chunk (may emit zero or more SSE events).
	respToChat := &OpenAI2Responses{}
	chatChunk, err := respToChat.TransformStreamChunk(chunk, ctx)
	if err != nil {
		return engine.SSEChunk{}, err
	}
	if len(chatChunk.Data) == 0 {
		return engine.SSEChunk{}, nil
	}

	// chatChunk.Data may contain multiple Chat Completions SSE events chained
	// as "data: ...\n\n". Run each one through Gemini2OpenAI's stream
	// transformer individually.
	chatToGemini := &Gemini2OpenAI{}
	var out bytes.Buffer
	parseOpenAISSE(chatChunk.Data, func(data []byte) bool {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return true
		}
		geminiChunk, err := chatToGemini.TransformStreamChunk(engine.SSEChunk{Data: data}, ctx)
		if err != nil {
			return true
		}
		if len(geminiChunk.Data) == 0 {
			return true
		}
		// Gemini2OpenAI emits raw JSON (no SSE framing) on its Data field.
		// Wrap it as "data: <json>\n\n". Use string() — passing []byte to
		// writeSSEData would base64-encode the bytes via json.Marshal.
		out.WriteString("data: ")
		out.Write(geminiChunk.Data)
		out.WriteString("\n\n")
		return true
	})
	if out.Len() == 0 {
		return engine.SSEChunk{}, nil
	}
	return engine.SSEChunk{Data: out.Bytes()}, nil
}

// Interface compliance.
var (
	_ engine.RequestTransformer        = (*Gemini2Responses)(nil)
	_ engine.ResponseTransformer       = (*Gemini2Responses)(nil)
	_ engine.StreamResponseTransformer = (*Gemini2Responses)(nil)
)
