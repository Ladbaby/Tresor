package engine

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"tresor/internal/jsplugins"

	"golang.org/x/net/http/httpguts"
)

const externalValueLimit = 8 << 20
const externalCollectionLimit = 64 << 20
const externalEventLimit = 256

type ExternalRuntime interface {
	HasHook(string) bool
	Reset() error
	Invoke(string, map[string]any) (map[string]any, error)
	PluginName() string
}
type ExternalPlugin struct {
	runtime                      ExternalRuntime
	request                      *http.Request
	finished, begun              bool
	upstream, stream, collection Session
	ctx                          *PipelineContext
	ruleID                       string
	stepIndex                    int
}

func NewExternalPlugin(runtime ExternalRuntime) *ExternalPlugin {
	return &ExternalPlugin{runtime: runtime}
}
func (p *ExternalPlugin) PluginName() string { return p.runtime.PluginName() }
func payload(body []byte) map[string]any {
	encoding, data := "utf8", string(body)
	if !utf8.Valid(body) {
		encoding, data = "base64", base64.StdEncoding.EncodeToString(body)
	}
	return map[string]any{"encoding": encoding, "data": data}
}
func decodePayload(value any) ([]byte, error) {
	return decodePayloadLimit(value, externalValueLimit, false)
}

// Collection bodies have a decoded-byte ceiling, including base64 bodies.
// Ordinary patches retain their existing encoded-value ceiling.
func decodePayloadLimit(value any, limit int, collection bool) ([]byte, error) {
	m, ok := value.(map[string]any)
	if !ok || len(m) != 2 {
		return nil, fmt.Errorf("invalid body patch")
	}
	text, ok := m["data"].(string)
	encodedLimit := limit
	if collection && m["encoding"] == "base64" {
		encodedLimit = base64.StdEncoding.EncodedLen(limit)
	}
	if !ok || len(text) > encodedLimit {
		return nil, fmt.Errorf("invalid/oversized body data")
	}
	switch m["encoding"] {
	case "utf8":
		if !utf8.ValidString(text) {
			return nil, fmt.Errorf("invalid UTF-8")
		}
		return []byte(text), nil
	case "base64":
		body, err := base64.StdEncoding.Strict().DecodeString(text)
		if err != nil {
			return nil, err
		}
		if len(body) > limit {
			return nil, fmt.Errorf("decoded body exceeds limit")
		}
		return body, nil
	default:
		return nil, fmt.Errorf("invalid body encoding")
	}
}
func protectedHeader(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "cookie", "set-cookie", "host", "content-length", "transfer-encoding", "connection", "content-encoding", "openai-beta", "chatgpt-account-id", "originator":
		return true
	}
	return strings.HasPrefix(strings.ToLower(key), "sec-")
}
func headerSnapshot(headers http.Header) map[string]any {
	out := map[string]any{}
	for key, values := range headers {
		key = strings.ToLower(key)
		if !protectedHeader(key) {
			copy := append([]string{}, values...)
			out[key] = copy
		}
	}
	return out
}
func requestSnapshot(req *http.Request, ctx *PipelineContext) map[string]any {
	path := req.URL.Path
	if ctx.RequestPathOverride != "" {
		path = ctx.RequestPathOverride
	} else if ctx.TargetDownstream != nil && ctx.TargetDownstream.FormatPaths[ctx.DownstreamFormat] != "" {
		path = ctx.TargetDownstream.FormatPaths[ctx.DownstreamFormat]
	}
	return map[string]any{"method": req.Method, "path": path, "query": req.URL.RawQuery, "headers": headerSnapshot(req.Header)}
}
func (p *ExternalPlugin) input(ctx *PipelineContext) map[string]any {
	p.ctx = ctx
	return map[string]any{"context": ctx.invocationContext(), "attempt": map[string]any{"index": ctx.AttemptIndex}}
}
func (p *ExternalPlugin) call(name string, input map[string]any) (map[string]any, error) {
	if !p.runtime.HasHook(name) {
		return nil, nil
	}
	if configured, ok := p.runtime.(interface{ SetLimits(jsplugins.Limits) }); ok && p.ctx != nil {
		configured.SetLimits(jsplugins.EffectiveLimits(p.ctx.execution().Large))
	}
	result, err := p.runtime.Invoke(name, input)
	if err != nil {
		return nil, p.invocationFailure(name, err)
	}
	return result, nil
}
func validKeys(m map[string]any, allowed ...string) error {
	for key := range m {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unsupported effect %q", key)
		}
	}
	return nil
}
func patchHeaders(original http.Header, raw map[string]any) (http.Header, error) {
	out := original.Clone()
	seen := map[string]bool{}
	if value, ok := raw["setHeaders"]; ok {
		sets, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid setHeaders")
		}
		for key, value := range sets {
			lower := strings.ToLower(key)
			if !httpguts.ValidHeaderFieldName(key) || protectedHeader(key) || seen[lower] {
				return nil, fmt.Errorf("unsafe/duplicate header %s", key)
			}
			seen[lower] = true
			var values []string
			switch array := value.(type) {
			case []any:
				for _, entry := range array {
					str, ok := entry.(string)
					if !ok {
						return nil, fmt.Errorf("header values must be strings")
					}
					values = append(values, str)
				}
			case []string:
				values = array
			default:
				return nil, fmt.Errorf("header values must be an array")
			}
			if len(values) == 0 {
				return nil, fmt.Errorf("empty header values are not deletion")
			}
			for _, v := range values {
				if !httpguts.ValidHeaderFieldValue(v) {
					return nil, fmt.Errorf("invalid header value")
				}
			}
			out[http.CanonicalHeaderKey(key)] = append([]string{}, values...)
		}
	}
	if value, ok := raw["deleteHeaders"]; ok {
		array, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("invalid deleteHeaders")
		}
		for _, entry := range array {
			key, ok := entry.(string)
			if !ok || !httpguts.ValidHeaderFieldName(key) || protectedHeader(key) || seen[strings.ToLower(key)] {
				return nil, fmt.Errorf("unsafe/conflicting header deletion")
			}
			seen[strings.ToLower(key)] = true
			out.Del(key)
		}
	}
	return out, nil
}
func (p *ExternalPlugin) transformRequest(req *http.Request, body []byte, ctx *PipelineContext) (outReq *http.Request, out []byte, failure error) {
	defer func() {
		if failure != nil {
			if fault, ok := failure.(*jsplugins.InvocationError); ok && fault.Hook == "request" {
				return // call already recorded this invocation failure.
			}
			failure = p.invocationFailure("request", failure)
		}
	}()
	p.request = req
	input := p.input(ctx)
	input["request"] = requestSnapshot(req, ctx)
	input["body"] = payload(body)
	result, err := p.call("request", input)
	if err != nil {
		return nil, nil, err
	}
	if result == nil {
		return req, body, nil
	}
	if err := validKeys(result, "request", "body", "transport"); err != nil {
		return nil, nil, err
	}
	outBody := body
	if value, ok := result["body"]; ok {
		outBody, err = decodePayload(value)
		if err != nil {
			return nil, nil, err
		}
	}
	clone, err := CopyRequest(req, outBody)
	if err != nil {
		return nil, nil, err
	}
	override := ctx.RequestPathOverride
	if value, ok := result["request"]; ok {
		patch, ok := value.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("invalid request patch")
		}
		if err := validKeys(patch, "method", "path", "query", "setHeaders", "deleteHeaders"); err != nil {
			return nil, nil, err
		}
		clone.Header, err = patchHeaders(req.Header, patch)
		if err != nil {
			return nil, nil, err
		}
		if value, ok := patch["method"]; ok {
			method, ok := value.(string)
			if !ok || !httpguts.ValidHeaderFieldName(method) {
				return nil, nil, fmt.Errorf("invalid method")
			}
			clone.Method = method
		}
		if value, ok := patch["path"]; ok {
			path, ok := value.(string)
			if !ok || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\r\n\\") {
				return nil, nil, fmt.Errorf("invalid path")
			}
			if _, err := url.ParseRequestURI(path); err != nil {
				return nil, nil, err
			}
			override = path
			clone.URL.Path = path
			clone.URL.RawPath = ""
		}
		if value, ok := patch["query"]; ok {
			query, ok := value.(string)
			if !ok || strings.ContainsAny(query, "#\r\n") {
				return nil, nil, fmt.Errorf("invalid query")
			}
			if _, err := url.ParseQuery(query); err != nil {
				return nil, nil, err
			}
			clone.URL.RawQuery = query
		}
	}
	owner := ctx.ExternalCollector
	if value, ok := result["transport"]; ok {
		transport, ok := value.(map[string]any)
		if !ok || validKeys(transport, "delivery", "upstream") != nil || len(transport) == 0 {
			return nil, nil, fmt.Errorf("invalid transport effect")
		}
		if value, exists := transport["upstream"]; exists && value != "sse" {
			return nil, nil, fmt.Errorf("invalid upstream transport")
		}
		if value, exists := transport["delivery"]; exists {
			if value != "collect" {
				return nil, nil, fmt.Errorf("invalid delivery")
			}
			if ctx.ClientStreaming || owner != nil {
				return nil, nil, fmt.Errorf("conflicting collection delivery")
			}
			for _, hook := range []string{"collectStart", "collectEvent", "collectEnd"} {
				if !p.runtime.HasHook(hook) {
					return nil, nil, fmt.Errorf("collection requires %s", hook)
				}
			}
			owner = p
		}
	}
	if transport, ok := result["transport"].(map[string]any); ok && transport["upstream"] == "sse" {
		ctx.execution().Requested = "sse"
	}
	ctx.RequestPathOverride = override
	ctx.ExternalCollector = owner
	p.request = clone
	return clone, outBody, nil
}
func (p *ExternalPlugin) metadata(ctx *PipelineContext, resp *http.Response) map[string]any {
	input := p.input(ctx)
	input["request"] = requestSnapshot(p.request, ctx)
	input["response"] = map[string]any{"status": resp.StatusCode, "headers": headerSnapshot(resp.Header)}
	return input
}
func applyResponse(result map[string]any, resp *http.Response, body []byte, allowBody bool) ([]byte, error) {
	return applyResponseLimit(result, resp, body, allowBody, externalValueLimit, false)
}
func applyResponseLimit(result map[string]any, resp *http.Response, body []byte, allowBody bool, limit int, collection bool) ([]byte, error) {
	if result == nil {
		return body, nil
	}
	allowed := []string{"response"}
	if allowBody {
		allowed = append(allowed, "body")
	}
	if err := validKeys(result, allowed...); err != nil {
		return nil, err
	}
	headers := resp.Header
	var err error
	out := body
	if value, ok := result["body"]; ok {
		out, err = decodePayloadLimit(value, limit, collection)
		if err != nil {
			return nil, err
		}
	}
	if value, ok := result["response"]; ok {
		patch, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid response patch")
		}
		if err := validKeys(patch, "setHeaders", "deleteHeaders"); err != nil {
			return nil, err
		}
		headers, err = patchHeaders(resp.Header, patch)
		if err != nil {
			return nil, err
		}
	}
	resp.Header = headers
	return out, nil
}

func (externalRequest) PreservesFormat() bool  { return true }
func (externalResponse) PreservesFormat() bool { return true }

type externalRequest struct{ p *ExternalPlugin }

func (t externalRequest) TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error) {
	return t.p.transformRequest(req, body, ctx)
}

type externalResponse struct{ p *ExternalPlugin }

func (t externalResponse) TransformResponse(resp *http.Response, body []byte, ctx *PipelineContext) ([]byte, error) {
	input := t.p.metadata(ctx, resp)
	input["body"] = payload(body)
	result, err := t.p.call("response", input)
	if err != nil {
		return nil, err
	}
	limits := jsplugins.EffectiveLimits(ctx.execution().Large)
	out, err := applyResponseLimit(result, resp, body, true, limits.BodyBytes, true)
	return out, t.p.invocationFailure("response", err)
}
func (p *Pipeline) beginAttempt(ctx *PipelineContext, req *http.Request, index int) error {
	ctx.AttemptIndex = index
	ctx.ExternalCollector = nil
	ctx.Execution = nil
	ctx.execution()
	// Invalidate every participant before resetting any VM: a partial reset must
	// never leave later participants eligible for stale-session cleanup.
	for _, step := range p.ExternalSteps {
		step.ctx = ctx
		step.upstream, step.stream, step.collection = Session{}, Session{}, Session{}
		step.finished, step.begun = false, false
		step.request = req
		step.releaseBuffers()
	}
	for _, step := range p.ExternalSteps {
		if err := step.runtime.Reset(); err != nil {
			err = step.invocationFailure("initialize", err)
			step.releaseBuffers() // Reset may have allocated storage before failing.
			return err
		}
		step.begun = true
	}
	return nil
}
func (p *ExternalPlugin) releaseBuffers() {
	if storage, ok := p.runtime.(interface{ ReleaseBuffers() }); ok {
		storage.ReleaseBuffers()
	}
}
func (p *Pipeline) finishAttempt(ctx *PipelineContext, reason string) error {
	var first error
	if reason != "success" {
		endReason := "error"
		if reason == "cancelled" {
			endReason = "cancelled"
		}
		_, first = eventLifecycle(p.UpstreamSteps, "upstreamEnd", endReason, nil, ctx)
		_, cleanupErr := p.streamLifecycle("streamEnd", endReason, nil, ctx)
		if first == nil {
			first = cleanupErr
		}
	}
	for _, step := range p.ExternalSteps {
		if !step.begun || step.finished {
			step.releaseBuffers()
			continue
		}
		step.finished = true
		input := step.input(ctx)
		input["outcome"] = ctx.termination(reason)
		ctx.execution().Phase, ctx.execution().Stage = "finish", "finish"
		input["context"] = ctx.invocationContext()
		result, err := step.call("finish", input)
		step.releaseBuffers()
		if err == nil && result != nil {
			err = step.invocationFailure("finish", fmt.Errorf("finish cannot emit output"))
		}
		if first == nil {
			first = err
		}
	}
	return first
}
func (p *Pipeline) responseHeaders(resp *http.Response, ctx *PipelineContext) error {
	ctx.execution().Phase = "response_headers"
	ctx.execution().Stage = "response_pipeline"
	for _, step := range p.ExternalSteps {
		result, err := step.call("responseHeaders", step.metadata(ctx, resp))
		if err != nil {
			return err
		}
		if _, err := applyResponse(result, resp, nil, false); err != nil {
			return step.invocationFailure("responseHeaders", err)
		}
	}
	return nil
}
