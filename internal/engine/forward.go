package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SSRF validation is not applied here — downstreams are admin-configured via auth-protected API.
// Returns the response and a cancel function; caller must call cancel after consuming resp.Body.
func (e *Engine) forwardRequest(original *http.Request, body []byte, ctx *PipelineContext) (*http.Response, context.CancelFunc, error) {
	// Pick the base URL: prefer a per-format override (e.g. DeepSeek serves
	// Anthropic at /anthropic but OpenAI at the root URL), fall back to
	// BaseURL when no override is configured for the downstream format.
	baseURL := ""
	if ctx.DownstreamFormat != "" && ctx.TargetDownstream.FormatURLs != nil {
		baseURL = ctx.TargetDownstream.FormatURLs[ctx.DownstreamFormat]
	}
	if baseURL == "" {
		baseURL = ctx.TargetDownstream.BaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	// Determine the path to send. Two modes:
	//  1. Per-format path override (FormatPaths): when the downstream declares
	//     a request path for its active format (e.g. openai_responses ->
	//     "/responses"), use it directly and ignore the client's own path. The
	//     final path is base_url + format_path, which is what the provider
	//     expects regardless of what the client called.
	//  2. Default: append the client's path, stripping a duplicated API
	//     version prefix (e.g. "https://host/v1" + "/v1/chat/completions").
	// An empty DownstreamFormat or missing map key simply yields "" and falls
	// through to the default, so the lookup is safe without an explicit guard.
	requestPath := ctx.RequestPathOverride
	if requestPath == "" && ctx.TargetDownstream != nil && ctx.TargetDownstream.FormatPaths != nil {
		requestPath = ctx.TargetDownstream.FormatPaths[ctx.DownstreamFormat]
	}
	if requestPath == "" {
		requestPath = original.URL.Path
		parsedBase, parseErr := url.Parse(baseURL)
		if parseErr == nil && parsedBase.Path != "" {
			basePrefix := strings.TrimSuffix(parsedBase.Path, "/")
			if strings.HasPrefix(requestPath, basePrefix) {
				requestPath = strings.TrimPrefix(requestPath, basePrefix)
			}
		}
	}

	targetURL := baseURL + requestPath
	if original.URL.RawQuery != "" {
		targetURL += "?" + original.URL.RawQuery
	}

	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return nil, func() {}, fmt.Errorf("parse target URL: %w", err)
	}

	outBody := body
	var codex codexRequest
	if ctx.CodexBackend {
		codex = prepareCodexRequest(original, body)
		outBody = codex.body
	}

	// Build forwarded request. Use a detached context so the downstream connection
	// isn't killed if the client disconnects (common with long-running SSE streams).
	forwardCtx, forwardCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	forwardedReq, err := http.NewRequestWithContext(forwardCtx, original.Method, targetURL, bytes.NewReader(outBody))
	if err != nil {
		forwardCancel()
		return nil, func() {}, fmt.Errorf("create forwarded request: %w", err)
	}

	// Copy headers, overriding Host and Authorization
	for k, v := range original.Header {
		// Skip hop-by-hop headers
		if strings.EqualFold(k, "Host") {
			forwardedReq.Host = parsedURL.Host
			continue
		}
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Proxy-Connection") {
			continue
		}
		forwardedReq.Header[k] = v
	}

	// Set the downstream API key only if a pipeline transformer hasn't
	// already set auth headers (e.g., x-api-key for Anthropic, x-goog-api-key for Gemini)
	if ctx.TargetDownstream.APIKey != "" {
		hasAuthHeader := forwardedReq.Header.Get("Authorization") != "" ||
			forwardedReq.Header.Get("x-api-key") != "" ||
			forwardedReq.Header.Get("x-goog-api-key") != ""
		if !hasAuthHeader {
			// Pick the auth header style from the actual downstream format
			// being used for THIS request, not the full list of formats the
			// downstream declares. A downstream with api_formats: [openai,
			// anthropic] forwards an OpenAI request as OpenAI (no auto-
			// translation needed), so it expects Authorization: Bearer —
			// not the Anthropic-style x-api-key. The previous check
			// (slices.Contains(ApiFormats, "anthropic")) misfired here and
			// caused 401s whenever the list contained "anthropic".
			switch ctx.DownstreamFormat {
			case "anthropic":
				forwardedReq.Header.Set("x-api-key", ctx.TargetDownstream.APIKey)
				forwardedReq.Header.Set("anthropic-version", "2023-06-01")
			case "gemini":
				// Google Gemini accepts the key either as the `x-goog-api-key` header
				// or as a `?key=...` query param. Use the header to avoid leaking the
				// key into proxy/access logs.
				forwardedReq.Header.Set("x-goog-api-key", ctx.TargetDownstream.APIKey)
			default:
				// "openai", "openai_responses", or empty/unknown — assume the
				// OpenAI-compatible Authorization: Bearer scheme.
				forwardedReq.Header.Set("Authorization", "Bearer "+ctx.TargetDownstream.APIKey)
			}
		}
	}
	// Apply provider-specific extra headers (e.g. ChatGPT "originator").
	// These do not affect auth selection above; they are additive metadata
	// the OAuth provider requires on LLM requests.
	for k, v := range ctx.OAuthExtraHeaders {
		forwardedReq.Header.Set(k, v)
	}
	forwardedReq.Header.Set("Host", parsedURL.Host)
	// Ask the downstream for an uncompressed response. We disable compression
	// in the transport (DisableCompression: true) but Go's Transport only stops
	// auto-setting Accept-Encoding — it does not actively request identity. By
	// setting it here we ensure upstream returns plain text we can stream or
	// transform without a decoder in the loop. If a downstream only serves
	// compressed, our SSE handler will surface garbled bytes; the per-request
	// fix can be added when that becomes a real problem.
	forwardedReq.Header.Set("Accept-Encoding", "identity")

	if ctx.CodexBackend {
		codex.applyHeaders(forwardedReq)
	}

	resp, err := e.client.Do(forwardedReq)
	if err != nil {
		forwardCancel()
		return nil, func() {}, err
	}
	return resp, forwardCancel, nil
}
