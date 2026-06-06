package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// PipelineStepConfig represents one step in a pipeline_config JSON array.
type PipelineStepConfig struct {
	PluginID string                 `json:"plugin_id"`
	Config   map[string]interface{} `json:"config,omitempty"`
}

// Pipeline describes the transformation steps for a matched rule.
type Pipeline struct {
	RequestSteps         []RequestTransformer
	ResponseSteps        []ResponseTransformer
	StreamResponseSteps  []StreamResponseTransformer
}

// ExecuteRequestPipeline runs all request transformers sequentially.
func ExecuteRequestPipeline(req *http.Request, body []byte, ctx *PipelineContext, steps []RequestTransformer) (*http.Request, []byte, error) {
	var err error
	for i, t := range steps {
		req, body, err = t.TransformRequest(req, body, ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("request transformer %d: %w", i, err)
		}
	}
	return req, body, nil
}

// ExecuteResponsePipeline runs all response transformers sequentially.
func ExecuteResponsePipeline(resp *http.Response, body []byte, ctx *PipelineContext, steps []ResponseTransformer) ([]byte, error) {
	var err error
	for i, t := range steps {
		body, err = t.TransformResponse(resp, body, ctx)
		if err != nil {
			return nil, fmt.Errorf("response transformer %d: %w", i, err)
		}
	}
	return body, nil
}

// ExecuteStreamResponsePipeline runs all stream response transformers sequentially
// on a single SSE event chunk.
func ExecuteStreamResponsePipeline(chunk SSEChunk, ctx *PipelineContext, steps []StreamResponseTransformer) (SSEChunk, error) {
	var err error
	for i, t := range steps {
		chunk, err = t.TransformStreamChunk(chunk, ctx)
		if err != nil {
			return chunk, fmt.Errorf("stream response transformer %d: %w", i, err)
		}
	}
	return chunk, nil
}

// ParsePipelineConfig parses the JSON pipeline_config into a Pipeline of
// transformer instances using the given registry.
func ParsePipelineConfig(jsonConfig string, registry PluginRegistry) (*Pipeline, error) {
	if jsonConfig == "" || jsonConfig == "[]" {
		return &Pipeline{}, nil
	}

	var steps []PipelineStepConfig
	if err := json.Unmarshal([]byte(jsonConfig), &steps); err != nil {
		return nil, fmt.Errorf("parse pipeline config: %w", err)
	}

	p := &Pipeline{}
	for _, step := range steps {
		plugin, err := registry.CreatePlugin(step.PluginID, step.Config)
		if err != nil {
			return nil, fmt.Errorf("create plugin %s: %w", step.PluginID, err)
		}

		if rt, ok := plugin.(RequestTransformer); ok {
			p.RequestSteps = append(p.RequestSteps, rt)
		}
		if rsp, ok := plugin.(ResponseTransformer); ok {
			p.ResponseSteps = append(p.ResponseSteps, rsp)
		}
		if srt, ok := plugin.(StreamResponseTransformer); ok {
			p.StreamResponseSteps = append(p.StreamResponseSteps, srt)
		}
	}

	return p, nil
}

// CopyRequest creates a deep-ish copy of an HTTP request for forwarding.
func CopyRequest(original *http.Request, newBody []byte) (*http.Request, error) {
	req := original.Clone(original.Context())
	req.Body = io.NopCloser(bytes.NewReader(newBody))
	req.ContentLength = int64(len(newBody))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	// Reset the URL to ensure it's forwarded properly
	if req.URL != nil {
		req.RequestURI = ""
	}
	return req, nil
}
