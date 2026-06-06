package plugins

import (
	"fmt"
	"net/http"

	"tresor/internal/engine"
)

// CustomHeaderPlugin injects custom HTTP headers into the forwarded request.
type CustomHeaderPlugin struct {
	headers map[string]string
}

// NewCustomHeaderPlugin creates a CustomHeaderPlugin from configuration.
// Config format: {"headers": {"X-Custom": "Value", ...}}
func NewCustomHeaderPlugin(config map[string]interface{}) (*CustomHeaderPlugin, error) {
	p := &CustomHeaderPlugin{
		headers: make(map[string]string),
	}

	if config == nil {
		return p, nil
	}

	rawHeaders, ok := config["headers"]
	if !ok {
		return p, nil
	}

	headerMap, ok := rawHeaders.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("custom_header: 'headers' must be an object")
	}

	for k, v := range headerMap {
		if str, ok := v.(string); ok {
			p.headers[k] = str
		} else {
			return nil, fmt.Errorf("custom_header: header %q value must be a string", k)
		}
	}

	return p, nil
}

// TransformRequest adds configured headers to the request.
func (p *CustomHeaderPlugin) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	return req, body, nil
}

// TransformResponse is a no-op for this plugin.
func (p *CustomHeaderPlugin) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
	return body, nil
}

// TransformStreamChunk passes the chunk through unchanged.
func (p *CustomHeaderPlugin) TransformStreamChunk(chunk engine.SSEChunk, ctx *engine.PipelineContext) (engine.SSEChunk, error) {
	return chunk, nil
}

// Ensure interface compliance.
var _ engine.RequestTransformer = (*CustomHeaderPlugin)(nil)
var _ engine.ResponseTransformer = (*CustomHeaderPlugin)(nil)
var _ engine.StreamResponseTransformer = (*CustomHeaderPlugin)(nil)
