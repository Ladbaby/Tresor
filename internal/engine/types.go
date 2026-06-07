package engine

import (
	"net/http"
)

// Downstream is a resolved target endpoint.
type Downstream struct {
ID        string
Name      string
BaseURL   string
APIKey    string
ApiFormat string
}

// PipelineContext carries state through the transformation pipeline.
type PipelineContext struct {
	TargetDownstream *Downstream
	Variables        map[string]interface{}
}

// RequestTransformer modifies an outgoing request before it is forwarded.
type RequestTransformer interface {
	TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error)
}

// ResponseTransformer modifies a full response body before it is returned to the client.
type ResponseTransformer interface {
	TransformResponse(resp *http.Response, body []byte, ctx *PipelineContext) ([]byte, error)
}

// SSEChunk represents a single SSE event: an optional event type and the data
// payload (without "data: " prefix). For protocols without named events (e.g.,
// OpenAI), EventType is empty.
type SSEChunk struct {
	EventType string // e.g. "message_start", "content_block_delta" — empty for unnamed events
	Data      []byte // the JSON payload
}

// StreamResponseTransformer transforms a single SSE event chunk. Plugins that do
// not support streaming should return the chunk unchanged.
type StreamResponseTransformer interface {
	TransformStreamChunk(chunk SSEChunk, ctx *PipelineContext) (SSEChunk, error)
}

// PluginFactory creates a plugin instance from a configuration map.
type PluginFactory func(config map[string]interface{}) (interface{}, error)
