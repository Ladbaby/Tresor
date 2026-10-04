package plugins

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"tresor/internal/engine"
)

// ForceStreaming requests provider streaming while preserving a non-streaming
// client's response contract. All aggregation state belongs to one request.
type ForceStreaming struct{}

func (*ForceStreaming) PluginName() string { return "force_streaming" }
func (*ForceStreaming) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
	if ctx.ClientStreaming {
		return req, body, nil
	}
	path := req.URL.Path
	// Do not modify count_tokens, model listing or other utility requests.
	if !(strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")) {
		return req, body, nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, nil, fmt.Errorf("force_streaming request: %w", err)
	}
	if obj == nil {
		return nil, nil, fmt.Errorf("force_streaming expects a JSON object")
	}
	switch ctx.DownstreamFormat {
	case "openai", "anthropic", "openai_responses":
		obj["stream"] = true
		if ctx.DownstreamFormat == "openai" {
			options, _ := obj["stream_options"].(map[string]interface{})
			if options == nil {
				options = make(map[string]interface{})
			}
			options["include_usage"] = true
			obj["stream_options"] = options
		}
	case "gemini":
		// Gemini selects transport through its URL, not a stream JSON property.
		delete(obj, "stream")
	default:
		return nil, nil, fmt.Errorf("force_streaming: unsupported downstream format %q", ctx.DownstreamFormat)
	}
	newBody, err := json.Marshal(obj)
	if err != nil {
		return nil, nil, err
	}
	out, err := engine.CopyRequest(req, newBody)
	if err != nil {
		return nil, nil, err
	}
	out.Header.Set("Accept", "text/event-stream")
	if ctx.DownstreamFormat == "gemini" {
		path = out.URL.Path
		if ctx.TargetDownstream != nil && ctx.TargetDownstream.FormatPaths["gemini"] != "" {
			path = ctx.TargetDownstream.FormatPaths["gemini"]
		}
		if !strings.HasSuffix(path, ":generateContent") && !strings.HasSuffix(path, ":streamGenerateContent") {
			return nil, nil, fmt.Errorf("force_streaming: Gemini path must end in :generateContent or :streamGenerateContent")
		}
		path = strings.TrimSuffix(path, ":generateContent")
		if !strings.HasSuffix(path, ":streamGenerateContent") {
			path += ":streamGenerateContent"
		}
		if ctx.TargetDownstream != nil && ctx.TargetDownstream.FormatPaths["gemini"] != "" {
			ctx.RequestPathOverride = path
		} else {
			out.URL.Path = path
		}
		query := out.URL.Query()
		query.Set("alt", "sse")
		out.URL.RawQuery = query.Encode()
	}
	ctx.ResponseBodyCollector = collectForcedStream
	return out, newBody, nil
}
