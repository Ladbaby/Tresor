package engine

import (
	"encoding/json"
	"net/http"
	"reflect"
)

// validateManagedRequestTransform verifies that a managed tool hook changed only
// the current user's content and permitted sampling parameters.
func validateManagedRequestTransform(before, after []byte, beforeReq, afterReq *http.Request, ctx *PipelineContext) error {
	if beforeReq == nil || afterReq == nil || beforeReq.Method != afterReq.Method || beforeReq.URL == nil || afterReq.URL == nil || beforeReq.URL.Path != afterReq.URL.Path || beforeReq.URL.RawQuery != afterReq.URL.RawQuery || beforeReq.URL.Host != afterReq.URL.Host {
		return contractError("managed_transform", "route")
	}
	// The managed loop builds every generation request in canonical Chat form,
	// so a request transform must not move it to another route. Every client
	// format is served through that canonical route, so this is not a check on
	// the client's own path.
	if beforeReq.Method != http.MethodPost || (beforeReq.URL.Path != "/v1/chat/completions" && beforeReq.URL.Path != "/chat/completions") {
		return contractError("managed_transform", "route")
	}
	if ctx != nil && (ctx.ExternalCollector != nil || ctx.RequestPathOverride != "" || ctx.execution().Requested != "auto" || (ctx.execution().CurrentFormat != "" && ctx.execution().CurrentFormat != "openai")) {
		return contractError("managed_transform", "transport_override")
	}
	var a, b map[string]any
	if json.Unmarshal(before, &a) != nil || json.Unmarshal(after, &b) != nil {
		return contractError("managed_transform", "invalid_json")
	}
	if a["model"] != b["model"] || !reflect.DeepEqual(a["stream"], b["stream"]) || !reflect.DeepEqual(a["stream_options"], b["stream_options"]) {
		return contractError("managed_transform", "route")
	}
	allowed := map[string]bool{"temperature": true, "top_p": true, "presence_penalty": true, "frequency_penalty": true, "seed": true, "max_tokens": true, "max_completion_tokens": true, "stop": true, "logit_bias": true, "user": true, "metadata": true, "service_tier": true}
	for k, v := range a {
		if k == "messages" {
			continue
		}
		if k == "tools" || k == "tool_choice" || k == "parallel_tool_calls" || k == "n" || k == "stream" || k == "stream_options" || !allowed[k] {
			if !reflect.DeepEqual(v, b[k]) {
				return contractError("managed_transform", "protected_field")
			}
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok && !allowed[k] {
			return contractError("managed_transform", "protected_field")
		}
	}
	am, aok := a["messages"].([]any)
	bm, bok := b["messages"].([]any)
	if !aok || !bok || len(am) != len(bm) {
		return contractError("managed_transform", "messages")
	}
	latest := -1
	for i, m := range am {
		if obj, ok := m.(map[string]any); ok && obj["role"] == "user" {
			latest = i
		}
	}
	for i := range am {
		x, xok := am[i].(map[string]any)
		y, yok := bm[i].(map[string]any)
		if !xok || !yok {
			if !reflect.DeepEqual(am[i], bm[i]) {
				return contractError("managed_transform", "messages")
			}
			continue
		}
		if i == latest {
			for k, v := range x {
				if k != "content" && !reflect.DeepEqual(v, y[k]) {
					return contractError("managed_transform", "messages")
				}
			}
			for k := range y {
				if _, ok := x[k]; !ok {
					return contractError("managed_transform", "messages")
				}
			}
		} else if !reflect.DeepEqual(x, y) {
			return contractError("managed_transform", "messages")
		}
	}
	return nil
}

// managedTransformPipeline excludes the owning plugin from all ordinary phases.
func managedTransformPipeline(p *Pipeline, owner *ExternalPlugin) (*Pipeline, error) {
	if p == nil {
		return &Pipeline{}, nil
	}
	if owner != nil && (owner.runtime.HasHook("request") || owner.runtime.HasHook("response") || owner.runtime.HasHook("streamStart") || owner.runtime.HasHook("streamEvent") || owner.runtime.HasHook("streamEnd")) {
		return nil, contractError("managed_transform", "owner_hooks")
	}
	if owner != nil && (owner.runtime.HasHook("upstreamStart") || owner.runtime.HasHook("upstreamEvent") || owner.runtime.HasHook("upstreamEnd")) {
		return nil, contractError("managed_transform", "upstream_hooks")
	}
	out := &Pipeline{}
	for _, s := range p.ExternalSteps {
		if s != owner {
			for _, hook := range []string{"upstreamStart", "upstreamEvent", "upstreamEnd", "collectStart", "collectEvent", "collectEnd", "prepare", "prepared"} {
				if s.runtime.HasHook(hook) {
					return nil, contractError("managed_transform", "unsupported_hook")
				}
			}
			out.ExternalSteps = append(out.ExternalSteps, s)
		}
	}
	for _, s := range p.RequestSteps {
		if x, ok := s.(externalRequest); ok && x.p == owner {
			continue
		}
		if !managedStepCarriesOver(s) {
			continue
		}
		out.RequestSteps = append(out.RequestSteps, s)
	}
	for _, s := range p.ResponseSteps {
		if x, ok := s.(externalResponse); ok && x.p == owner {
			continue
		}
		if !managedStepCarriesOver(s) {
			continue
		}
		out.ResponseSteps = append(out.ResponseSteps, s)
	}
	for _, s := range p.StreamResponseSteps {
		if x, ok := s.(externalStream); ok && x.p == owner {
			continue
		}
		if !managedStepCarriesOver(s) {
			continue
		}
		out.StreamResponseSteps = append(out.StreamResponseSteps, s)
	}
	if len(p.UpstreamSteps) != 0 {
		return nil, contractError("managed_transform", "upstream_hooks")
	}
	return out, nil
}

// managedStepCarriesOver reports whether a pipeline step can run inside the
// managed loop, which speaks the canonical format on both sides of the
// downstream leg. External steps preserve the format by contract; native format
// converters do not, because they translate between client and downstream
// formats — a job the loop performs itself when it builds each generation
// request. Those converters are dropped rather than rejected: auto-translation
// inserts them for any client whose format differs from the downstream, and that
// is now the normal case for every non-Chat format. The loop never relies on
// them, and keeping them would translate a body the loop just produced.
func managedStepCarriesOver(step any) bool {
	switch step.(type) {
	case externalRequest, externalResponse, externalStream:
		pipelineStep, ok := step.(FormatPreserver)
		return ok && pipelineStep.PreservesFormat()
	}
	return false
}
