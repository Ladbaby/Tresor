package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"tresor/internal/inspect"
	"tresor/internal/middleware"
	"tresor/internal/proxy"
	"tresor/internal/store"

	"slices"
)

// proxyAuth holds the set of allowed API keys for incoming proxy requests.
type proxyAuth struct {
	enabled bool
	keys    map[string]struct{}
}

// PluginRegistry is the interface for looking up plugin factories.
// The concrete implementation is in internal/plugins.
type PluginRegistry interface {
	CreatePlugin(pluginID string, config map[string]interface{}) (interface{}, error)
	ListPlugins() []PluginInfo
}

// PluginInfo describes a registered plugin.
type PluginInfo struct {
	ID           string        `json:"id"`
	Description  string        `json:"description"`
	ConfigSchema interface{}   `json:"config_schema,omitempty"`
}

// Engine is the core proxy handler. It matches incoming requests against
// rules, builds pipelines, and forwards transformed requests to downstreams.
type Engine struct {
	store     *store.Store
	registry  PluginRegistry
	client    *http.Client
	proxyAuth *proxyAuth
	logger    *RequestLogger

	// payloadStore receives the pre-transformer bytes from HandleProxy for
	// the inspector. Set once at startup; presence implies capture is on,
	// absence implies capture is off (so the engine never allocates a
	// scratch buffer for the disabled path).
	payloadStore *inspect.Store

	// retryOnEmpty enables automatic retry when a downstream returns an empty
	// response. When enabled, the gateway re-sends the request with exponential
	// backoff (up to retryMaxCount retries) if the downstream LLM produces no
	// content (e.g., empty choices, no text blocks).
	retryOnEmpty bool
}

// New creates a new Engine.
func New(s *store.Store) *Engine {
	return &Engine{
		store:  s,
		client: &http.Client{},
		logger: NewRequestLogger(),
	}
}

// SetLogger sets the request logger on the engine.
func (e *Engine) SetLogger(l *RequestLogger) {
	e.logger = l
}

// GetLogger returns the request logger.
func (e *Engine) GetLogger() *RequestLogger {
	return e.logger
}

// SetRegistry sets the plugin registry on the engine (called during initialization).
func (e *Engine) SetRegistry(r PluginRegistry) {
	e.registry = r
}

// SetProxyMode configures the outbound HTTP client's proxy behavior and transport settings.
// It replaces the default http.Client with one that uses a custom Transport
// respecting the given proxy mode (auto, env, windows, none).
//
// DisableCompression is set to true so Go does not silently decompress upstream
// responses. Some downstreams return Brotli-encoded streams (Content-Encoding: br)
// which Go's stdlib cannot decode, and when the body is streamed to the client
// (SSE) the raw compressed bytes leak through. Instead, we ask the downstream
// for plain text via Accept-Encoding: identity, set in forwardRequest.
func (e *Engine) SetProxyMode(mode proxy.Mode) {
	transport := &http.Transport{
		Proxy:               proxy.ProxyFunc(mode),
		IdleConnTimeout:     30 * time.Second,       // Close idle connections after 30s of inactivity
		MaxIdleConns:        25,                      // Total idle connection pool
		MaxIdleConnsPerHost: 5,                       // Per-downstream idle pool
		DisableCompression:  true,
	}
	e.client = &http.Client{
		Transport: transport,
	}
}

// SetProxyAuthKeys configures API key authentication for incoming proxy requests.
// If keys is empty or nil, authentication is disabled and all requests are allowed.
func (e *Engine) SetProxyAuthKeys(keys []string) {
	if len(keys) == 0 {
		e.proxyAuth = nil
		return
	}
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}
	e.proxyAuth = &proxyAuth{enabled: true, keys: keySet}
}

// SetPayloadStore wires the in-memory payload store that the inspector
// reads from. The engine calls store.Add on every recorded request keyed
// by the log entry id. Pass nil to disable payload capture.
func (e *Engine) SetPayloadStore(s *inspect.Store) {
	e.payloadStore = s
}

// SetRetryOnEmpty enables or disables automatic retry when a downstream
// returns an empty response.
func (e *Engine) SetRetryOnEmpty(enabled bool) {
	e.retryOnEmpty = enabled
}

// isResponseEmpty checks if a raw upstream response body contains no useful
// content for the given downstream API format.
func (e *Engine) isResponseEmpty(body []byte, downstreamFormat string) bool {
	switch downstreamFormat {
	case "openai":
		return IsOpenAIChatEmpty(body)
	case "anthropic":
		return IsAnthropicEmpty(body)
	case "openai_responses":
		return IsOpenAIResponsesEmpty(body)
	case "gemini":
		return IsGeminiEmpty(body)
	default:
		return false // unknown format, assume not empty
	}
}

// shouldRetry determines whether the gateway should retry a failed request.
// Returns true only if the response meets ALL retry conditions:
// - HTTP status is 200 (OK)
// - The request path is a generation endpoint (retry_on_empty is
//   generation-only; utility endpoints like /v1/messages/count_tokens
//   serve structured data that lacks a `content` field and must not be
//   classified as empty — see count-tokens-trigger-empty-response.txt)
// - The response body is empty for the downstream's API format
//
// Non-200 responses are never retried — LLM client apps handle their own
// retries for HTTP errors (4xx client errors, 5xx server errors), and
// retrying those is out of Tresor's scope.
//
// This method is designed to be extensible for future retry conditions
// (e.g., retry on 429 rate limits, retry on specific error messages).
func (e *Engine) shouldRetry(resp *http.Response, body []byte, downstreamFormat string, path string) bool {
	if resp.StatusCode != http.StatusOK {
		return false // non-200 responses are not retried
	}
	if !isGenerationEndpoint(path) {
		return false // utility endpoints don't produce conversational content
	}
	return e.isResponseEmpty(body, downstreamFormat)
}

// recordAndCapture is the single funnel for the engine's per-request log
// write. It calls logger.Record (which assigns the entry id) and, when
// the inspector store is attached, snapshots the pre-transformer body
// bytes into the payload store keyed by the same id.
//
// Pre-transformer capture is the inspector's whole point: what the UI
// shows is the wire bytes the client sent and the wire bytes the
// downstream returned, with no plugin transformation visible.
//
// requestFormat and downstreamFormat are the API formats of the
// incoming request body and the downstream response body, respectively.
// The inspector UI uses them to pick the correct normaliser for each
// side — important when an auto-translator rewrote the body between
// formats and the client's URL path is no longer a reliable hint for
// the response shape.
func (e *Engine) recordAndCapture(entry *RequestLogEntry, reqBody, respBody []byte, reqCT, respCT string, truncResp bool, requestFormat, downstreamFormat string) {
	e.logger.Record(entry)
	if e.payloadStore == nil {
		return
	}
	e.payloadStore.Add(inspect.Entry{
		ID:                  entry.ID,
		Timestamp:           entry.Timestamp,
		Path:                entry.Path,
		Method:              entry.Method,
		Model:               entry.Model,
		ResolvedModel:       entry.ResolvedModel,
		DownstreamID:        entry.DownstreamID,
		DownstreamName:      entry.DownstreamName,
		Status:              entry.Status,
		ClientIP:            entry.ClientIP,
		RequestBody:         reqBody,
		ResponseBody:        respBody,
		RequestContentType:  reqCT,
		ResponseContentType: respCT,
		TruncatedResponse:   truncResp,
		RequestFormat:       requestFormat,
		DownstreamFormat:    downstreamFormat,
	})
}

// validateProxyAuth checks the proxy API key sent by the client, supporting
// Authorization: Bearer <key>, x-api-key: <key>, and x-goog-api-key: <key> headers
// (the latter two are used by Anthropic- and Gemini-format clients respectively),
// and the ?key=<key> query parameter on Gemini paths (/v1beta/*).
// If auth is enabled and the key is invalid, it writes a 401 response and returns false.
// On success (or when auth is disabled), it returns true and strips the auth header
// from the request so the downstream's own API key can be injected cleanly.
func (e *Engine) validateProxyAuth(r *http.Request, w http.ResponseWriter) bool {
	if e.proxyAuth == nil || !e.proxyAuth.enabled {
		return true
	}

	// Try Authorization: Bearer <key>, then x-api-key: <key>, then x-goog-api-key: <key>
	token := ""
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token = strings.TrimPrefix(authHeader, "Bearer ")
	}
	if token == "" {
		if xak := r.Header.Get("x-api-key"); xak != "" {
			token = xak
		}
	}
	if token == "" {
		if xgak := r.Header.Get("x-goog-api-key"); xgak != "" {
			token = xgak
		}
	}
	// Gemini paths also accept the key via the ?key=<token> query parameter.
	// Cherry Studio (and Google's own SDKs) use this form. Only honor it on
	// Gemini paths so other formats can't smuggle credentials through the URL.
	queryHadKey := false
	if token == "" && strings.HasPrefix(r.URL.Path, "/v1beta/") {
		if qk := r.URL.Query().Get("key"); qk != "" {
			token = qk
			queryHadKey = true
		}
	}

	if _, ok := e.proxyAuth.keys[token]; !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "unauthorized",
			"message": "invalid or missing proxy API key",
		})
		return false
	}

	// Strip the client's auth header so it doesn't leak to downstream.
	// The downstream will receive its own API key from config.
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	r.Header.Del("x-goog-api-key")
	// Strip ?key= from the URL so the proxy key isn't forwarded to the downstream.
	if queryHadKey {
		q := r.URL.Query()
		q.Del("key")
		r.URL.RawQuery = q.Encode()
	}

	return true
}

// Registry returns the current plugin registry.
func (e *Engine) Registry() PluginRegistry {
	return e.registry
}

// Store returns the store for admin API access.
func (e *Engine) Store() *store.Store {
	return e.store
}

// ServeProxy starts an HTTP server that serves both the admin API and the LLM gateway.
// It uses a mux that routes /api/* to the admin handler and everything else to the proxy.
// webHandler serves the embedded web UI at the root.
// isWebPath is a function that checks if a path belongs to the web UI.
func ServeProxy(eng *Engine, adminHandler http.Handler, webHandler http.Handler, isWebPath func(string) bool, listener net.Listener) error {
	mux := http.NewServeMux()

	// Admin API routes (wrapped with security headers)
	mux.Handle("/api/", middleware.SecurityHeaders(adminHandler))

	// Everything else: web UI for known paths, proxy for API-like paths
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			webHandler.ServeHTTP(w, r)
			return
		}
		// Check if it's a web UI asset
		if isWebPath != nil && isWebPath(r.URL.Path) {
			webHandler.ServeHTTP(w, r)
			return
		}
		// Otherwise proxy the request
		eng.HandleProxy(w, r)
	}))

	return http.Serve(listener, middleware.SecurityHeaders(mux))
}

// ServeAdminOnly starts an HTTP server that serves both the admin API and web UI (for UDS).
func ServeAdminOnly(adminHandler http.Handler, listener net.Listener) error {
	return http.Serve(listener, middleware.SecurityHeaders(adminHandler))
}

// statusCaptureWriter wraps http.ResponseWriter to capture the status code.
type statusCaptureWriter struct {
	http.ResponseWriter
	status int
}

func newStatusCaptureWriter(w http.ResponseWriter) *statusCaptureWriter {
	return &statusCaptureWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *statusCaptureWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying ResponseWriter if it supports Flusher.
func (w *statusCaptureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

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

// corsHeaders writes CORS headers into the response. These are needed so that
// browser-based LLM clients (e.g. Claude plugin webviews) can make cross-origin
// requests to the gateway. We list all headers used by the Anthropic SDK/Stainless
// library so CORS preflights don't reject the actual request.
func corsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, anthropic-dangerous-direct-browser-access, x-stainless-arch, x-stainless-helper-method, x-stainless-lang, x-stainless-os, x-stainless-package-version, x-stainless-retry-count, x-stainless-runtime, x-stainless-runtime-version, x-stainless-timeout")
}

// logAndReturnError handles the repeated pattern of logging an error,
// writing an HTTP error response, updating the log entry, and recording it.
func (e *Engine) logAndReturnError(w *statusCaptureWriter, entry *RequestLogEntry, start time.Time, ge *gatewayError) {
	if ge.cause != nil {
		log.Printf("%s: %v", ge.logMsg, ge.cause)
	} else {
		log.Println(ge.logMsg)
	}
	http.Error(w, ge.httpMsg, ge.status)
	entry.Status = ge.status
	entry.Error = ge.errLabel
	entry.Duration = DurationMs(time.Since(start))
	e.logger.Record(entry)
}

// resolveModel reads the request body, extracts the model name, and resolves
// the target downstream via alias lookup or direct output_model_id matching.
// Always returns a modelResult (may be partial on error) for entry population.
func (e *Engine) resolveModel(r *http.Request) (*modelResult, *gatewayError) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return &modelResult{body: body}, &gatewayError{http.StatusInternalServerError, "failed to read body", "failed to read body", "failed to read body", err}
	}

	model := extractModel(body, r.URL.Path)
	if model == "" {
		return &modelResult{body: body}, &gatewayError{http.StatusBadRequest, "request body missing model", "request body missing model", "missing model", nil}
	}

	// Step 1: Try active alias
	alias, err := e.store.FindActiveAlias(model)
	if err != nil {
		return &modelResult{model: model, body: body}, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error looking up alias for model %s", model), "internal error", "alias lookup error", err}
	}

	if alias != nil {
		ds, err := e.store.GetDownstream(alias.DownstreamID)
		if err != nil {
			return &modelResult{model: model, body: body}, &gatewayError{http.StatusBadGateway, fmt.Sprintf("error getting downstream %s for alias %s", alias.DownstreamID, alias.ID),
				fmt.Sprintf("alias %q references missing downstream %q", alias.ID, alias.DownstreamID), "alias downstream missing", err}
		}
		e.logger.Debug("alias match: model %q → alias %q → downstream %q (%s)", model, alias.ID, ds.ID, alias.OutputModelID)
		return &modelResult{ds: ds, alias: alias, model: model, resolvedModel: alias.OutputModelID, body: rewriteModelInBody(body, alias.OutputModelID)}, nil
	}

	// Step 2: No alias — try direct downstream by output_model_ids
	ds, err := e.store.FindDownstreamByOutputModel(model)
	if err != nil {
		return &modelResult{model: model, body: body}, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error looking up downstream for model %s", model), "internal error", "downstream lookup error", err}
	}
	if ds == nil {
		msg := fmt.Sprintf("unknown model %q", model)
		e.logger.Debug("model %q did not match any alias or downstream output_model_ids", model)
		return &modelResult{model: model, body: body}, &gatewayError{http.StatusNotFound, msg, msg, "unknown model", nil}
	}

	e.logger.Debug("direct resolution: model %q → downstream %q (%s)", model, ds.ID, ds.Name)
	return &modelResult{ds: ds, model: model, resolvedModel: model, body: body}, nil
}

// buildPipeline constructs the transformation pipeline from matching rules and
// adds auto-translation transformers when input format differs from downstream format.
// Returns the pipeline and the list of matching rules (for logging).
func (e *Engine) buildPipeline(path, model string, inputFormat string, ds *store.Downstream, alias *store.Alias) (Pipeline, []store.Rule, *gatewayError) {
	// Build the candidate model list for pattern_model matching.
	// A rule's pattern_model matches if it equals ANY of these strings
	// (exact match). This makes rules useful whether the user keys them
	// on the raw incoming model, a downstream output_model_id, the alias
	// input_model_id, the regex pattern of a regex alias, or one of the
	// announced names of a regex alias.
	candidateModels := []string{model}
	if ds != nil {
		candidateModels = append(candidateModels, ds.OutputModelIDs...)
	}
	if alias != nil {
		candidateModels = append(candidateModels, alias.OutputModelID)
		if alias.IsRegex {
			candidateModels = append(candidateModels, alias.InputModelID) // the regex pattern itself
			candidateModels = append(candidateModels, alias.AnnouncedNames...)
		}
	}
	// De-duplicate and drop empty strings.
	seen := make(map[string]struct{}, len(candidateModels))
	uniq := candidateModels[:0]
	for _, m := range candidateModels {
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		uniq = append(uniq, m)
	}
	candidateModels = uniq

	// Find all matching rules (path+model+format filters)
	rules, err := e.store.FindMatchingRulesWithCandidates(path, candidateModels, inputFormat, ds.ID, ds.ApiFormats)
	if err != nil {
		return Pipeline{}, nil, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error finding rules: %v", err), "internal error", "rule lookup error", err}
	}

	// Concatenate all matching rules' pipelines (in priority order)
	var pipeline Pipeline
	for _, rule := range rules {
		p, err := ParsePipelineConfig(rule.PipelineConfig, e.registry)
		if err != nil {
			return Pipeline{}, nil, &gatewayError{http.StatusInternalServerError, fmt.Sprintf("error building pipeline for rule %s: %v", rule.ID, err), "pipeline error", "pipeline error", err}
		}
		pipeline.RequestSteps = append(pipeline.RequestSteps, p.RequestSteps...)
		pipeline.ResponseSteps = append(pipeline.ResponseSteps, p.ResponseSteps...)
		pipeline.StreamResponseSteps = append(pipeline.StreamResponseSteps, p.StreamResponseSteps...)
	}

	if len(rules) > 0 {
		e.logger.Debug("matched %d rule(s) for %s %s: %v", len(rules), path, model, func() []string { ids := make([]string, len(rules)); for i, r := range rules { ids[i] = r.ID }; return ids }())
	} else {
		e.logger.Debug("no rules matched for %s %s", path, model)
	}

	// Auto-translation: compare input format with downstream formats
	if inputFormat != "" && len(ds.ApiFormats) > 0 && !slices.Contains(ds.ApiFormats, inputFormat) {
		var pluginID string
		switch inputFormat {
		case "openai":
			switch {
			case slices.Contains(ds.ApiFormats, "openai_responses"):
				pluginID = "openai2responses"
			case slices.Contains(ds.ApiFormats, "gemini"):
				pluginID = "openai2gemini"
			default:
				pluginID = "openai2anthropic"
			}
		case "anthropic":
			switch {
			case slices.Contains(ds.ApiFormats, "openai_responses"):
				pluginID = "anthropic2responses"
			case slices.Contains(ds.ApiFormats, "gemini"):
				pluginID = "anthropic2gemini"
			default:
				pluginID = "anthropic2openai"
			}
		case "openai_responses":
			if slices.Contains(ds.ApiFormats, "openai") {
				pluginID = "responses2openai"
			} else if slices.Contains(ds.ApiFormats, "anthropic") {
				pluginID = "responses2anthropic"
			}
			// Note: responses2gemini is not implemented. To route OpenAI
			// Responses traffic to a Gemini downstream, configure a rule
			// with an explicit pipeline_config that converts through OpenAI
			// first (responses2openai + openai2gemini chained).
		case "gemini":
			switch {
			case slices.Contains(ds.ApiFormats, "openai"):
				pluginID = "gemini2openai"
			case slices.Contains(ds.ApiFormats, "anthropic"):
				pluginID = "gemini2anthropic"
			case slices.Contains(ds.ApiFormats, "openai_responses"):
				pluginID = "gemini2responses"
			}
		}
		if pluginID != "" {
			transformer, err := e.registry.CreatePlugin(pluginID, nil)
			if err != nil {
				log.Printf("Error creating auto-translation plugin %s: %v", pluginID, err)
			} else {
				name := transformerTypeName(transformer)
				if reqT, ok := transformer.(RequestTransformer); ok && !pluginInList[RequestTransformer](pipeline.RequestSteps, name) {
					pipeline.RequestSteps = append([]RequestTransformer{reqT}, pipeline.RequestSteps...)
				}
				if respT, ok := transformer.(ResponseTransformer); ok && !pluginInList[ResponseTransformer](pipeline.ResponseSteps, name) {
					pipeline.ResponseSteps = append(pipeline.ResponseSteps, respT)
				}
				if streamT, ok := transformer.(StreamResponseTransformer); ok && !pluginInList[StreamResponseTransformer](pipeline.StreamResponseSteps, name) {
					pipeline.StreamResponseSteps = append(pipeline.StreamResponseSteps, streamT)
				}
				e.logger.Debug("auto-translating %s → downstream %s (formats: %v)", inputFormat, ds.ID, ds.ApiFormats)
			}
		}
	}

	return pipeline, rules, nil
}

// HandleProxy is the main proxy handler for LLM requests.
func (e *Engine) HandleProxy(w http.ResponseWriter, r *http.Request) {
	corsHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/") || isLLMPath(r.URL.Path) == false {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	start := time.Now()
	cw := newStatusCaptureWriter(w)
	entry := RequestLogEntry{Timestamp: start, Method: r.Method, Path: r.URL.Path}

	// ClientIP: trust forwarded headers when connected via localhost (reverse proxy).
	entry.ClientIP = middleware.ExtractClientIP(r)

	if !e.validateProxyAuth(r, cw) {
		entry.Status = http.StatusUnauthorized
		entry.Error = "unauthorized"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(&entry)
		return
	}

	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		e.handleModels(cw)
		entry.Status = cw.status
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(&entry)
		return
	}

	// Gemini-format model listing: GET /v1beta/models (no model suffix).
	// Cherry Studio and Google's SDKs call this to discover available models.
	// Returns a Gemini-style {models: [...], nextPageToken} response aggregated
	// from downstreams that support the gemini API format.
	if r.URL.Path == "/v1beta/models" {
		e.handleGeminiModels(r, cw)
		entry.Status = cw.status
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(&entry)
		return
	}

	// Resolve model → downstream (reads body, extracts model, tries alias then direct lookup)
	result, ge := e.resolveModel(r)
	if ge != nil {
		entry.Model = result.model
		e.logAndReturnError(cw, &entry, start, ge)
		return
	}
	entry.Model = result.model
	entry.ResolvedModel = result.resolvedModel
	if result.alias != nil {
		entry.AliasGroup = result.alias.InputModelID
	}

	// Pre-transformer request snapshot for the inspector.
	var rawReq []byte
	if e.payloadStore != nil && len(result.body) > 0 {
		rawReq = append(make([]byte, 0, len(result.body)), result.body...)
	}

	// Build transformation pipeline (rule matching + auto-translation)
	inputFormat := detectInputFormat(r.URL.Path)
	pipeline, rules, ge := e.buildPipeline(r.URL.Path, result.model, inputFormat, result.ds, result.alias)
	if ge != nil {
		e.logAndReturnError(cw, &entry, start, ge)
		return
	}

	// Collect rule IDs for logging
	if len(rules) > 0 {
		entry.RuleIDs = make([]string, len(rules))
		for i, rule := range rules {
			entry.RuleIDs[i] = rule.ID
		}
	}

	// Populate downstream info and build pipeline context
	entry.DownstreamID = result.ds.ID
	entry.DownstreamName = result.ds.Name
	ctx := &PipelineContext{
		TargetDownstream: &Downstream{
			ID:         result.ds.ID,
			Name:       result.ds.Name,
			BaseURL:    result.ds.BaseURL,
			APIKey:     result.ds.APIKey,
			ApiFormats: result.ds.ApiFormats,
		},
		Variables: make(map[string]interface{}),
	}

	// Determine max retries (3 when retryOnEmpty enabled, 0 otherwise)
	maxRetries := 0
	if e.retryOnEmpty {
		maxRetries = retryMaxCount
	}
	// downstreamFormat is the API format the downstream will actually return.
	// A multi-format downstream (e.g. llama-swap with [openai, anthropic])
	// echoes the incoming request's format when it supports it (no
	// auto-translation), so ApiFormats[0] mislabels the response and breaks
	// empty-response detection (Anthropic SSE inspected with the OpenAI
	// parser matches "content":[] as content, never retrying). When the
	// request format is unknown or not supported, auto-translation rewrites
	// the request INTO the downstream's format — fall back to ApiFormats[0].
	downstreamFormat := ""
	if inputFormat != "" && len(result.ds.ApiFormats) > 0 && slices.Contains(result.ds.ApiFormats, inputFormat) {
		downstreamFormat = inputFormat
	} else if len(result.ds.ApiFormats) > 0 {
		downstreamFormat = result.ds.ApiFormats[0]
	}

	var cancelFn context.CancelFunc
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Client disconnected — stop retrying
			select {
			case <-r.Context().Done():
				entry.Status = http.StatusBadGateway
				entry.Error = "client disconnected during retry"
				entry.Duration = DurationMs(time.Since(start))
				e.recordAndCapture(&entry, rawReq, nil, r.Header.Get("Content-Type"), "", false, inputFormat, downstreamFormat)
				return
			default:
			}
			// Wait with exponential backoff
			delay := CalculateBackoff(attempt)
			e.logger.Debug("retry attempt %d/%d: empty response, waiting %v", attempt, maxRetries, delay)
			time.Sleep(delay)
			// Reset pipeline context variables (stream transformers maintain state)
			ctx.Variables = make(map[string]interface{})
		}

		// Execute request transformers
		currentReq, currentBody, err := ExecuteRequestPipeline(r, result.body, ctx, pipeline.RequestSteps)
		if err != nil {
			if cancelFn != nil {
				cancelFn()
			}
			e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, "request pipeline error", fmt.Sprintf("request pipeline error: %v", err), "request pipeline error", err})
			return
		}

		e.logger.Debug("forwarding %s %s → downstream %q (%s) with model %q (attempt %d)", r.Method, r.URL.Path, result.ds.ID, result.ds.Name, result.resolvedModel, attempt+1)

		// Forward request to downstream
		resp, cancel, err := e.forwardRequest(currentReq, currentBody, ctx)
		if err != nil {
			cancel()
			e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, "forward error", fmt.Sprintf("upstream error: %v", err), "upstream error", err})
			return
		}
		cancelFn = cancel

		// Handle streaming response
		if isEventStream(resp.Header.Get("Content-Type")) {
			// Streaming responses always go through a headerDelayWriter so the
			// HTTP status stays uncommitted until the first bytes are released.
			// This is what lets the gateway replace a downstream's 200 with a
			// real error status when the stream turns out to carry an in-band
			// error event, and what lets retry-on-empty send a 502 after
			// exhausting its attempts.
			//
			// Reasoning tokens are preserved (streamed to the client),
			// but reasoning-only streams are detected as empty and
			// retried transparently by holding the terminal [DONE]
			// marker until end-of-stream.
			hw := newHeaderDelayWriter(cw)
			outcome := e.handleStreamingResponse(hw, resp, ctx, &pipeline, cancel, r.Context(), rawReq, r.Header.Get("Content-Type"), resp.Header.Get("Content-Type"), &entry, e.retryOnEmpty, inputFormat, downstreamFormat)

			// An explicit error event is a definitive answer, never an empty
			// response — do not retry it.
			if outcome.streamErr != nil {
				if outcome.committed {
					// Bytes already reached the client, so the 200 status cannot
					// be recalled; the error event was passed through verbatim.
					// Record it as a failure so it does not log as a success.
					entry.Status = cw.status
					entry.Error = "upstream stream error"
					entry.Duration = DurationMs(time.Since(start))
					e.logger.Record(&entry)
					log.Printf("upstream stream error after content: %s", outcome.streamErr.Message)
					return
				}
				e.logAndReturnError(cw, &entry, start, &gatewayError{outcome.streamErr.Status, "upstream stream error", outcome.streamErr.Message, "upstream stream error", nil})
				return
			}

			if outcome.empty && resp.StatusCode == http.StatusOK {
				// Empty stream with HTTP 200 — retry if attempts remain
				if attempt >= maxRetries {
					entry.Status = cw.status
					entry.Duration = DurationMs(time.Since(start))
					e.recordAndCapture(&entry, rawReq, nil, r.Header.Get("Content-Type"), resp.Header.Get("Content-Type"), false, inputFormat, downstreamFormat)
					e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, "empty response after retries exhausted", "empty response after retries exhausted", "empty response", nil})
					return
				}
				// Close upstream response before retrying
				resp.Body.Close()
				continue
			}
			// Response had content or non-200 status — done
			return
		}

		// Non-streaming response: buffer and transform
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if err != nil {
			errMsg := fmt.Sprintf("failed to read upstream response (%d)", resp.StatusCode)
			if len(respBody) > 0 {
				errMsg += ": " + truncateString(string(respBody), 500)
			}
			e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, errMsg, errMsg, "failed to read response", err})
			return
		}

		// Pre-transformer response snapshot for the inspector.
		var rawResp []byte
		var respTrunc bool
		if e.payloadStore != nil && len(respBody) > 0 {
			if len(respBody) > inspect.MaxBodyBytes {
				rawResp = append(make([]byte, 0, inspect.MaxBodyBytes), respBody[:inspect.MaxBodyBytes]...)
				respTrunc = true
			} else {
				rawResp = append(make([]byte, 0, len(respBody)), respBody...)
			}
		}

		transformedBody, err := ExecuteResponsePipeline(resp, respBody, ctx, pipeline.ResponseSteps)
		if err != nil {
			e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, "response pipeline error", fmt.Sprintf("response pipeline error: %v", err), "response pipeline error", err})
			return
		}

		// Check for empty response (using raw upstream body, not transformed)
		// Only retry if HTTP status is 200 AND response is empty AND the path
		// is a generation endpoint. Utility endpoints like /count_tokens can
		// legitimately return 200 with bodies that look "empty" to the
		// parser (e.g. {"input_tokens":7802}); retrying them produces a
		// spurious 502.
		if e.retryOnEmpty && e.shouldRetry(resp, respBody, downstreamFormat, r.URL.Path) {
			if attempt >= maxRetries {
				entry.Status = resp.StatusCode
				entry.Duration = DurationMs(time.Since(start))
				e.recordAndCapture(&entry, rawReq, rawResp, r.Header.Get("Content-Type"), resp.Header.Get("Content-Type"), respTrunc, inputFormat, downstreamFormat)
				e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, "empty response after retries exhausted", "empty response after retries exhausted", "empty response", nil})
				return
			}
			continue
		}

		// Response has content — write to client
		for k, v := range resp.Header {
			if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Content-Encoding") {
				continue
			}
			cw.Header()[k] = v
		}
		cw.Header().Set("Content-Length", strconv.Itoa(len(transformedBody)))
		entry.Status = resp.StatusCode
		entry.Duration = DurationMs(time.Since(start))

		// Only accumulate usage statistics when the inspector is enabled.
		if e.payloadStore != nil {
			if usage, ok := UsageFromResponseBody(respBody); ok && usage != nil {
				entry.Usage = usage
			}
		}

		e.recordAndCapture(&entry, rawReq, rawResp, r.Header.Get("Content-Type"), resp.Header.Get("Content-Type"), respTrunc, inputFormat, downstreamFormat)
		cw.WriteHeader(resp.StatusCode)
		cw.Write(transformedBody)
		return
	}
}

// handleStreamingResponse pipes an SSE response from the downstream to
// the client. If stream transformers exist, each SSE event is transformed
// before sending; without them, the response is passed through
// line-by-line. cancel is called after the stream completes.
//
// When bufferForRetry is true, the writer is a bufferedWriter that delays
// all output until content is confirmed. On first content event, the buffer
// is flushed and the writer switches to pass-through mode. After scanning,
// returns true if the stream was empty (no content produced), false otherwise.
//
// When the engine's payload store is attached, rawReq/reqCT/respCT and
// the raw downstream SSE bytes are recorded on completion so the
// inspector can show the wire bytes — pre-transformer.
//
// requestFormat and downstreamFormat are the API formats of the client's
// incoming request and the downstream's response, respectively. They
// are persisted on the inspect entry so the inspector UI can pick the
// correct normaliser for each side independently. The streaming seed
// for content detection is derived as downstreamFormat (falling back to
// requestFormat when the downstream declared no api_formats), and is
// then confirmed or overridden by on-the-fly detection from the first
// SSE chunk we actually read.
// streamOutcome reports how a streamed response ended.
type streamOutcome struct {
	// empty is true when no real content was produced and retry-on-empty is
	// enabled, signalling HandleProxy to retry the upstream.
	empty bool
	// streamErr is non-nil when the downstream delivered a fatal error event
	// in-band on an otherwise-200 stream.
	streamErr *StreamError
	// committed reports whether the HTTP status reached the client. When false,
	// the caller can still replace it with an error status.
	committed bool
}

func (e *Engine) handleStreamingResponse(w *headerDelayWriter, resp *http.Response, ctx *PipelineContext, pipeline *Pipeline, cancel context.CancelFunc, clientCtx context.Context, rawReq []byte, reqCT, respCT string, entry *RequestLogEntry, bufferForRetry bool, requestFormat, downstreamFormat string) streamOutcome {
	defer resp.Body.Close()
	defer cancel()

	var contentProduced bool

	// streamFormat is the format used for IsStreamContentLine. It starts
	// as the downstream's declared format (falling back to the request
	// format) and is then confirmed or overridden by on-the-fly
	// detection from the first SSE chunk we actually read. On-the-fly
	// detection is required because auto-translation may convert the
	// request format to the downstream's format, so the seed is only
	// a hint — the actual response format is what matters for content
	// detection (otherwise we'd retry every non-empty response as empty).
	seedFormat := downstreamFormat
	if seedFormat == "" {
		seedFormat = requestFormat
	}
	var streamFormat string = seedFormat

	var respBuf bytes.Buffer
	var truncated bool
	captureOn := e.payloadStore != nil

	// scrapeUsage inspects one complete SSE event payload and, if it
	// carries a recognisable usage block, sparsely merges it into the
	// entry's Usage accumulator.
	scrapeUsage := func(data []byte) {
		if !captureOn {
			return
		}
		usage, ok := UsageFromResponseBody(data)
		if !ok || usage == nil {
			return
		}
		if entry.Usage == nil {
			entry.Usage = &UsageBlock{}
		}
		entry.Usage.Merge(usage)
	}

	// teeLine writes the CR-trimmed line + '\n' to the inspector buffer
	// (subject to inspect.MaxBodyBytes).
	teeLine := func(line string) {
		if !captureOn {
			return
		}
		if respBuf.Len() >= inspect.MaxBodyBytes {
			truncated = true
			return
		}
		if respBuf.Len()+len(line)+1 > inspect.MaxBodyBytes {
			room := inspect.MaxBodyBytes - respBuf.Len() - 1
			respBuf.Write([]byte(line[:room]))
			respBuf.WriteByte('\n')
			truncated = true
			return
		}
		respBuf.WriteString(line)
		respBuf.WriteByte('\n')
	}

	record := func() {
		var respBody []byte
		if captureOn {
			respBody = respBuf.Bytes()
		}
		entry.Status = resp.StatusCode
		entry.Duration = DurationMs(time.Since(entry.Timestamp))
		e.recordAndCapture(entry, rawReq, respBody, reqCT, respCT, truncated, requestFormat, downstreamFormat)
	}

	// streamErr is set when a fatal in-band error event is seen. Scanning stops
	// at that point — nothing after the error is meaningful.
	var streamErr *StreamError

	// releaseHeld and commitStatus are assigned below, once the write helpers
	// exist. finish() is declared first because the early-exit paths that run
	// before those helpers are set up also need it.
	releaseHeld := func() {}

	// finish handles the end-of-stream return.
	//
	// It commits the deferred status header only when the caller is done with
	// the response. When an error was detected or the stream was empty, the
	// header is left open so HandleProxy can send the provider's error status
	// (or a 502 after retries are exhausted) instead of the downstream's 200.
	finish := func() streamOutcome {
		empty := bufferForRetry && !contentProduced
		if streamErr == nil && !empty {
			// Release any bytes still being held (e.g. a stream of nothing but
			// keep-alives) and commit the downstream's status. For a stream
			// that already wrote bytes the status was committed on the first
			// Write; this covers streams that wrote nothing, so a non-200
			// downstream status is not silently replaced by an implicit 200.
			releaseHeld()
			w.Flush()
		}
		return streamOutcome{
			empty:     empty,
			streamErr: streamErr,
			committed: w.IsFlushed(),
		}
	}

	// Always copy SSE-relevant headers to the underlying writer BEFORE capturing
	// the status. Go's net/http commits headers at WriteHeader time and ignores
	// subsequent Header() mutations, so headers must be set first.
	// headerDelayWriter.Header() is a passthrough to the underlying writer, so
	// these Set calls take effect immediately on the real response writer.
	for _, header := range []string{"Content-Type", "Cache-Control", "Connection"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}
	// X-Accel-Buffering: no — only meaningful when streaming straight through.
	// When buffering for retry, the response is being held in memory anyway, so
	// preventing reverse-proxy buffering is unnecessary and could mask the
	// fact that the gateway itself is buffering.
	if !bufferForRetry {
		w.Header().Set("X-Accel-Buffering", "no")
	}

	// Record the downstream's status without committing it. headerDelayWriter
	// stores it and commits on the first Write (or on Flush), which keeps the
	// status replaceable until bytes are actually released to the client.
	w.WriteHeader(resp.StatusCode)

	flusher := http.Flusher(w)

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 1024*1024)

	hasTransformers := len(pipeline.StreamResponseSteps) > 0

	// clientGone is set when a write to the response fails, indicating the client
	// has disconnected. Once set, the function stops reading from the downstream.
	var clientGone bool

	// Until the first substantive SSE event has been vetted, outgoing lines are
	// held in pendingBuf rather than written, because writing anything commits
	// the HTTP status. Some providers emit a long run of ":" keep-alive comments
	// and then a fatal `event: error`; holding those bytes keeps the status
	// replaceable so the error can surface as a real HTTP error instead of a
	// 200. The hold is released as soon as one complete non-error event has been
	// seen, so streaming stays progressive.
	var pendingBuf bytes.Buffer
	holding := true
	const maxPendingBytes = 8 * 1024

	tryWrite := func(p []byte) bool {
		if clientGone {
			return false
		}
		if _, err := w.Write(p); err != nil {
			clientGone = true
			return false
		}
		return true
	}
	tryFlush := func() bool {
		if clientGone {
			return false
		}
		if !SafeFlush(flusher) {
			clientGone = true
			return false
		}
		return true
	}
	// releasePending writes any held bytes and stops holding for good.
	releasePending := func() bool {
		holding = false
		if pendingBuf.Len() == 0 {
			return true
		}
		p := pendingBuf.Bytes()
		pendingBuf.Reset()
		if !tryWrite(p) {
			return false
		}
		return tryFlush()
	}
	// discardPending drops held bytes without writing them. Used when the held
	// event turned out to be a fatal error the client should not receive.
	discardPending := func() {
		pendingBuf.Reset()
	}
	// writeLine sends one raw SSE line, buffering it while the status is still
	// being held open.
	writeLine := func(line string) bool {
		if holding {
			if pendingBuf.Len()+len(line)+1 <= maxPendingBytes {
				pendingBuf.WriteString(line)
				pendingBuf.WriteByte('\n')
				return true
			}
			// Hold buffer full — release so a client behind an intermediary
			// keeps receiving bytes rather than being starved.
			if !releasePending() {
				return false
			}
		}
		if !tryWrite([]byte(line + "\n")) {
			return false
		}
		return tryFlush()
	}
	// writeAndFlush is the standard "send some bytes and flush" pair. Any held
	// bytes are released first so ordering is preserved.
	writeAndFlush := func(p []byte) bool {
		if holding && !releasePending() {
			return false
		}
		return tryWrite(p) && tryFlush()
	}
	releaseHeld = func() { releasePending() }

	// Passthrough mode: no transformers — write each line immediately with flush
	if !hasTransformers {
		// SSE state trackers for usage extraction and error detection.
		var sseEvent string
		var sseEventName string
		// hadData tracks whether the event currently being accumulated carried
		// any event:/data: lines, distinguishing a real event from a bare
		// keep-alive comment followed by its blank line.
		var hadData bool
		flushSSEEvent := func() {
			if sseEvent != "" {
				scrapeUsage([]byte(sseEvent))
			}
			sseEvent = ""
			sseEventName = ""
		}
		// terminalBuf holds terminal events ([DONE] / message_stop /
		// response.completed) until end-of-stream, so we can suppress
		// them when the stream turned out to be empty (case a) or
		// reasoning-only (case b) and the upstream is being retried.
		// While held, the rest of the stream (reasoning + content) is
		// forwarded to the client immediately and progressively.
		var terminalBuf bytes.Buffer
		var terminalEventActive bool
		var terminalDataLines []string
		flushTerminal := func() {
			if !terminalEventActive {
				return
			}
			writeAndFlush(terminalBuf.Bytes())
			terminalBuf.Reset()
			terminalEventActive = false
			terminalDataLines = nil
		}
		discardTerminal := func() {
			terminalBuf.Reset()
			terminalEventActive = false
			terminalDataLines = nil
		}

		for scanner.Scan() {
			select {
			case <-clientCtx.Done():
				flushSSEEvent()
				record()
				return finish()
			default:
			}
			line := scanner.Text()
			trimmed := strings.TrimRight(line, "\r")
			teeLine(trimmed)

			// Track simple event boundaries for the cache scraper.
			// This block must run BEFORE the on-the-fly format detector
			// below so that sseEventName holds the current event's
			// name when the first data: line of that event is being
			// classified.
			if trimmed == "" {
				// End of an SSE event. Check first for a fatal in-band error
				// event — the downstream said 200 but is reporting a failure.
				if se, isErr := ParseStreamError(sseEventName, []byte(sseEvent)); isErr {
					streamErr = se
					if w.IsFlushed() {
						// Bytes already went out, so the 200 cannot be recalled.
						// Pass the error event through so the client's SDK can
						// surface it, then stop reading.
						writeLine(line)
					} else {
						// Nothing has reached the client yet: suppress the error
						// event and the held keep-alives so HandleProxy can
						// answer with the provider's real status instead.
						discardPending()
					}
					flushSSEEvent()
					record()
					return finish()
				}
				// If buffering for retry is on, check whether this event carries
				// real content (excludes reasoning/thinking). Reasoning events
				// are still streamed to the client (preserved) but do not flip
				// contentProduced.
				if bufferForRetry {
					if sseEvent != "" && IsStreamContentLine("data: "+sseEvent, streamFormat) {
						contentProduced = true
						// Real content detected — if we were holding a
						// terminal event, flush it now (it was held because
						// the prior reasoning-only events didn't count).
						flushTerminal()
					} else if isTerminalEvent(sseEvent, streamFormat) {
						// Terminal event ([DONE] / message_stop / etc.):
						// hold it until end-of-stream, so we can suppress
						// it when the upstream needs to be retried.
						// Reasoning tokens remain visible to the client;
						// only the terminal marker is held.
						terminalEventActive = true
						terminalBuf.WriteString("data: ")
						terminalBuf.WriteString(sseEvent)
						terminalBuf.WriteString("\n\n")
						terminalDataLines = []string{sseEvent}
					}
				}
				flushSSEEvent()
			} else if strings.HasPrefix(trimmed, "event: ") {
				sseEventName = strings.TrimPrefix(trimmed, "event: ")
				hadData = true

				// Detect the downstream's actual stream format from the
				// event name as soon as we see it. The seed is only a
				// hint — a multi-format downstream (e.g. llama-swap
				// with [openai, anthropic]) may speak a different format
				// than ApiFormats[0] suggested, in which case the seed
				// would mislabel the stream and break empty-response
				// detection. OpenAI streams have no event names and are
				// detected via the "choices" payload marker on the first
				// data: line.
				if bufferForRetry {
					if f := DetectStreamFormat(SSEChunk{EventType: sseEventName}); f != "" {
						streamFormat = f
					}
				}
			} else if strings.HasPrefix(trimmed, "data: ") {
				if sseEvent != "" {
					sseEvent += "\n"
				}
				sseEvent += strings.TrimPrefix(trimmed, "data: ")
				hadData = true

				// On the first data: line of a stream, the payload may
				// carry the format marker that the event name doesn't
				// (e.g. OpenAI's "choices"). Override the seed when this
				// is the case.
				if bufferForRetry {
					if f := DetectStreamFormat(SSEChunk{
						EventType: sseEventName,
						Data:      []byte(strings.TrimPrefix(trimmed, "data: ")),
					}); f != "" {
						streamFormat = f
					}
				}
			}

			// Write the line. While we're holding a terminal event,
			// suppress the terminal's bytes (they're already in
			// terminalBuf). Everything else flows through to the
			// client immediately and progressively.
			if terminalEventActive && bufferForRetry && isDataLineInTerminal(terminalDataLines, line) {
				continue
			}
			if !writeLine(line) {
				flushSSEEvent()
				record()
				return finish()
			}
			// A substantive event completed without being an error, so the bytes
			// held to keep the status replaceable can be released now. Keep-alive
			// comments carry no data and do not end the hold — that is what lets
			// a provider's trailing `event: error` still become a real HTTP error.
			if trimmed == "" && hadData && holding && !releasePending() {
				record()
				return finish()
			}
			if trimmed == "" {
				hadData = false
			}
		}
		// End of stream. If a terminal event is still held and no
		// real content was produced, discard it and let the engine
		// retry the upstream. Otherwise flush it to the client.
		if bufferForRetry && terminalEventActive && !contentProduced {
			discardTerminal()
			flushSSEEvent()
			if err := scanner.Err(); err != nil {
				log.Printf("Stream ended: %v", err)
			}
			record()
			// finish() returns true when bufferForRetry && !contentProduced,
			// which signals HandleProxy to retry the upstream.
			return finish()
		}
		flushTerminal()
		flushSSEEvent()
		if err := scanner.Err(); err != nil {
			log.Printf("Stream ended: %v", err)
		}
		record()
		return finish()
	}

	// Transform mode: accumulate SSE events, transform, then write
	var eventLine string
	var dataLines []string
	var doneSent bool // tracks whether downstream sent [DONE] marker

	// heldTerminalBytes accumulates a terminal event ([DONE] /
	// message_stop / response.completed) so we can suppress it at
	// end-of-stream when the upstream needs to be retried (the client
	// should not see [DONE] for an empty response). Reasoning tokens
	// remain visible to the client; only the terminal marker is held.
	var heldTerminalBytes []byte
	holdTerminal := func(rawData string) {
		// Reconstruct the full SSE event the way it arrived from the
		// downstream (preserving event: line if present).
		var b bytes.Buffer
		if eventLine != "" {
			fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", eventLine, rawData)
		} else {
			fmt.Fprintf(&b, "data: %s\n\n", rawData)
		}
		heldTerminalBytes = b.Bytes()
	}
	flushHeldTerminal := func() {
		if len(heldTerminalBytes) == 0 || clientGone {
			heldTerminalBytes = nil
			return
		}
		writeAndFlush(heldTerminalBytes)
		heldTerminalBytes = nil
	}
	discardHeldTerminal := func() {
		heldTerminalBytes = nil
	}

	// errorDetected is set by flushEvent when it sees a fatal in-band error, so
	// the scan loop knows to stop reading.
	var errorDetected bool

	flushEvent := func() bool {
		if len(dataLines) == 0 {
			return true
		}

		if clientGone {
			return false
		}

		select {
		case <-clientCtx.Done():
			return false
		default:
		}

		// Combine data lines to get the raw SSE payload
		rawData := strings.Join(dataLines, "\n")
		chunk := SSEChunk{EventType: eventLine, Data: []byte(rawData)}

		// Check for a fatal in-band error before running transformers, so a
		// format translator cannot mangle the error into something
		// unrecognisable. The downstream said 200 but is reporting a failure.
		if se, isErr := ParseStreamError(eventLine, []byte(rawData)); isErr {
			streamErr = se
			errorDetected = true
			if w.IsFlushed() {
				// Bytes already went out, so the 200 cannot be recalled. Pass
				// the error event through verbatim so the client's SDK can
				// surface it.
				var out bytes.Buffer
				if eventLine != "" {
					fmt.Fprintf(&out, "event: %s\n", eventLine)
				}
				fmt.Fprintf(&out, "data: %s\n\n", rawData)
				writeAndFlush(out.Bytes())
			} else {
				// Nothing has reached the client yet: suppress the error event
				// and any held bytes so HandleProxy can answer with the
				// provider's real status instead.
				discardPending()
			}
			return false
		}

		// Track whether the downstream sent [DONE] so we know if synthetic
		// termination is needed when the stream ends.
		if eventLine == "" && strings.TrimSpace(rawData) == "[DONE]" {
			doneSent = true
		}

		// Look for a usage block on the raw upstream event.
		scrapeUsage([]byte(rawData))

		// Detect the downstream's actual stream format from the raw
		// upstream event (pre-transform). The seed is only a hint — the
		// downstream may speak a different format than ApiFormats[0]
		// suggested, in which case the seed would mislabel the stream
		// and break empty-response detection. IsStreamContentLine
		// classifies raw rawData, so the format must describe the raw
		// upstream event, not the post-transform output.
		if bufferForRetry {
			if f := DetectStreamFormat(SSEChunk{
				EventType: eventLine,
				Data:      []byte(rawData),
			}); f != "" {
				streamFormat = f
			}
		}

		// Run through stream transformers
		var err error
		chunk, err = ExecuteStreamResponsePipeline(chunk, ctx, pipeline.StreamResponseSteps)
		if err != nil {
			log.Printf("Stream transform error: %v", err)
			return true
		}
		// Safety guard: skip empty data to avoid sending data: \n\n that could
		// confuse downstream event parsers (e.g. Anthropic SDK).
		if len(chunk.Data) == 0 {
			return true
		}

		// Detect real content vs reasoning-only.
		eventHasRealContent := bufferForRetry && IsStreamContentLine("data: "+rawData, streamFormat)

		// Detect terminal events (held until end-of-stream).
		isTerminal := bufferForRetry && isTerminalEvent(rawData, streamFormat)

		// If the transformer output contains SSE event boundaries (\n\n), it is
		// already formatted SSE — write it directly without wrapping in data:.
		if strings.Contains(string(chunk.Data), "\n\n") {
			if isTerminal {
				holdTerminal(rawData)
				return true
			}
			if eventHasRealContent {
				contentProduced = true
				// If we held a terminal, flush it now (real content
				// confirms the stream isn't empty).
				flushHeldTerminal()
			}
			return writeAndFlush(chunk.Data)
		}

		var out bytes.Buffer
		if chunk.EventType != "" {
			fmt.Fprintf(&out, "event: %s\n", chunk.EventType)
		}
		out.WriteString("data: ")
		out.Write(chunk.Data)
		out.WriteString("\n\n")

		if isTerminal {
			holdTerminal(rawData)
			return true
		}
		if eventHasRealContent {
			contentProduced = true
			flushHeldTerminal()
		}
		return writeAndFlush(out.Bytes())
	}

	for scanner.Scan() {
		select {
		case <-clientCtx.Done():
			record()
			return finish()
		default:
		}

		rawLine := scanner.Text()
		teeLine(strings.TrimRight(rawLine, "\r"))
		line := strings.TrimRight(rawLine, "\r")

		if line == "" {
			// Empty line terminates an SSE event — flush it
			hadEventData := eventLine != "" || len(dataLines) > 0
			if !flushEvent() {
				if errorDetected {
					// Fatal in-band error: drop any held terminal so the client
					// does not also receive a completion marker.
					discardHeldTerminal()
				}
				record()
				return finish()
			}
			eventLine = ""
			dataLines = nil
			// A substantive event completed without being an error, so the bytes
			// held to keep the status replaceable can be released now. Keep-alive
			// comments carry no data and do not end the hold — that is what lets
			// a provider's trailing `event: error` still become a real HTTP error.
			if hadEventData && holding && !releasePending() {
				record()
				return finish()
			}
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			eventLine = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			// Guard against unbounded accumulation from malformed downstreams
			if cap(dataLines) > 64*1024 {
				log.Printf("SSE data buffer exceeded 64KB limit, truncating event")
				eventLine = ""
				dataLines = nil
			}
			continue
		}

		// Unknown line type (including ":" keep-alive comments) — pass through
		// as-is, held while the status is still open.
		if !writeLine(line) {
			record()
			return finish()
		}
	}

	// Flush any remaining event (handles responses that don't end with \n\n)
	if !flushEvent() && errorDetected {
		discardHeldTerminal()
		record()
		return finish()
	}

	// If the downstream closed the stream without a [DONE] marker and the client
	// is still connected, send a synthetic one through the pipeline so stream
	// transformers can emit their termination sequence.
	// When retry-on-empty is enabled and no real content was produced,
	// skip the synthetic [DONE] — the upstream will be retried and any
	// held terminal will be discarded so the client doesn't see an
	// empty completion.
	if !doneSent && !clientGone && !bufferForRetry {
		select {
		case <-clientCtx.Done():
			record()
			return finish()
		default:
			syntheticChunk := SSEChunk{Data: []byte("[DONE]")}
			transformed, err := ExecuteStreamResponsePipeline(syntheticChunk, ctx, pipeline.StreamResponseSteps)
			if err == nil && len(transformed.Data) > 0 {
				writeAndFlush(transformed.Data)
			}
		}
	}

	// At end-of-stream, decide whether to flush or discard any held
	// terminal event. If no real content was produced, discard it and
	// signal retry to the engine.
	if bufferForRetry && !contentProduced && len(heldTerminalBytes) > 0 {
		discardHeldTerminal()
		if err := scanner.Err(); err != nil {
			log.Printf("Stream ended: %v", err)
		}
		record()
		return finish()
	}
	flushHeldTerminal()

	if err := scanner.Err(); err != nil {
		log.Printf("Stream ended: %v", err)
	}
	record()
	return finish()
}

// forwardRequest sends the (possibly transformed) request to the target downstream.
// SSRF validation is not applied here — downstreams are admin-configured via auth-protected API.
// Returns the response and a cancel function; caller must call cancel after consuming resp.Body.
func (e *Engine) forwardRequest(original *http.Request, body []byte, ctx *PipelineContext) (*http.Response, context.CancelFunc, error) {
	baseURL := strings.TrimRight(ctx.TargetDownstream.BaseURL, "/")

	// Determine the path to append. If the base_url already contains the API
	// version prefix (e.g., "/v1"), strip it from the request path to avoid
	// duplication (e.g., "https://host/v1" + "/v1/chat/completions").
	requestPath := original.URL.Path
	parsedBase, parseErr := url.Parse(baseURL)
	if parseErr == nil && parsedBase.Path != "" {
		basePrefix := strings.TrimSuffix(parsedBase.Path, "/")
		if strings.HasPrefix(requestPath, basePrefix) {
			requestPath = strings.TrimPrefix(requestPath, basePrefix)
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

	// Build forwarded request. Use a detached context so the downstream connection
	// isn't killed if the client disconnects (common with long-running SSE streams).
	forwardCtx, forwardCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	forwardedReq, err := http.NewRequestWithContext(forwardCtx, original.Method, targetURL, bytes.NewReader(body))
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
			switch {
			case slices.Contains(ctx.TargetDownstream.ApiFormats, "anthropic"):
				forwardedReq.Header.Set("x-api-key", ctx.TargetDownstream.APIKey)
				forwardedReq.Header.Set("anthropic-version", "2023-06-01")
			case slices.Contains(ctx.TargetDownstream.ApiFormats, "gemini"):
				// Google Gemini accepts the key either as the `x-goog-api-key` header
				// or as a `?key=...` query param. Use the header to avoid leaking the
				// key into proxy/access logs.
				forwardedReq.Header.Set("x-goog-api-key", ctx.TargetDownstream.APIKey)
			default:
				forwardedReq.Header.Set("Authorization", "Bearer "+ctx.TargetDownstream.APIKey)
			}
		}
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

	resp, err := e.client.Do(forwardedReq)
	if err != nil {
		forwardCancel()
		return nil, func() {}, err
	}
	return resp, forwardCancel, nil
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
//   /v1beta/models                              → ""
//   /v1beta/models/gemini-2.5-pro               → "gemini-2.5-pro"
//   /v1beta/models/gemini-2.5-pro:generateContent          → "gemini-2.5-pro"
//   /v1beta/models/qwen3.5:9b-mtp:instruct:streamGenerateContent → "qwen3.5:9b-mtp:instruct"
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

// pluginInList checks if a transformer with the given type name is already in the pipeline.
func pluginInList[T any](transformers []T, typeName string) bool {
	for _, t := range transformers {
		if transformerTypeName(t) == typeName {
			return true
		}
	}
	return false
}

func transformerTypeName(t interface{}) string {
	if namer, ok := t.(PluginNamer); ok {
		return namer.PluginName()
	}
	typ := reflect.TypeOf(t)
	if typ == nil {
		return ""
	}
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ.Name()
}

// modelRecord is one entry in the OpenAI-compatible /v1/models listing.
type modelRecord struct {
	ID          string         `json:"id"`
	Object      string         `json:"object"`
	Created     int64          `json:"created"`
	OwnedBy     string         `json:"owned_by"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`
}

// handleModels responds to GET /v1/models and GET /models with an aggregated
// model list from all downstreams and aliases, formatted as an OpenAI-style
// model list response. Each entry carries downstream attribution (owned_by)
// and metadata (name, source type).
// Proxy auth is validated by HandleProxy before reaching this function.
func (e *Engine) handleModels(w http.ResponseWriter) {
	created := time.Now().Unix()
	data := make([]modelRecord, 0)

	newRecord := func(id, name, ownedBy string, source string) modelRecord {
		rec := modelRecord{
			ID:      id,
			Object:  "model",
			Created: created,
			OwnedBy: ownedBy,
			Name:    name,
		}
		rec.Meta = map[string]any{"source": source}
		return rec
	}

	// Models from downstream output_model_ids
	downstreams, err := e.store.ListDownstreams()
	if err != nil {
		log.Printf("Error listing downstreams for models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, ds := range downstreams {
		for _, m := range ds.OutputModelIDs {
			data = append(data, newRecord(m, ds.Name, ds.ID, "downstream"))
		}
	}

	// Models from aliases (input and output)
	aliases, err := e.store.ListAliases()
	if err != nil {
		log.Printf("Error listing aliases for models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Build a downstream ID -> name lookup
	dsName := make(map[string]string, len(downstreams))
	for _, ds := range downstreams {
		dsName[ds.ID] = ds.Name
	}

	for _, a := range aliases {
		// Skip regex aliases for input_model_id (they represent patterns, not model IDs)
		if !a.IsRegex {
			data = append(data, newRecord(a.InputModelID, dsName[a.DownstreamID], a.DownstreamID, "alias"))
		}
		// Surface announced names from regex groups so clients see routable model IDs.
		// Announced names always win over the group's output_model_id in dedup because
		// they're emitted earlier and the dedup pass keeps the first occurrence.
		for _, n := range a.AnnouncedNames {
			data = append(data, newRecord(n, dsName[a.DownstreamID], a.DownstreamID, "alias"))
		}
		data = append(data, newRecord(a.OutputModelID, dsName[a.DownstreamID], a.DownstreamID, "alias"))
	}

	// Deduplicate: keep the first occurrence of each model ID
	seen := make(map[string]struct{}, len(data))
	deduped := make([]modelRecord, 0, len(data))
	for _, m := range data {
		if _, ok := seen[m.ID]; !ok {
			seen[m.ID] = struct{}{}
			deduped = append(deduped, m)
		}
	}

	// Sort by ID for deterministic output
	sort.Slice(deduped, func(i, j int) bool { return deduped[i].ID < deduped[j].ID })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   deduped,
	})
}

// geminiModelRecord is one entry in the Gemini-format /v1beta/models listing.
// Google's schema is documented at
// https://ai.google.dev/api/models#method:-models.list — we emit the fields
// that Gemini-format clients (e.g. Cherry Studio) actually consume, namely
// `name` (the model identifier, e.g. "models/gemini-2.5-pro") and `displayName`.
type geminiModelRecord struct {
	Name                     string   `json:"name"`
	DisplayName              string   `json:"displayName,omitempty"`
	Description              string   `json:"description,omitempty"`
	Version                  string   `json:"version,omitempty"`
	InputTokenLimit          int      `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit         int      `json:"outputTokenLimit,omitempty"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
}

// handleGeminiModels responds to GET /v1beta/models with a Gemini-format list
// of available models. We surface every downstream model regardless of the
// downstream's configured `api_formats`: when a Gemini-format request comes
// in for a downstream that speaks OpenAI, Anthropic, or OpenAI Responses, the
// engine auto-inserts the appropriate Gemini->X transformer (see
// buildPipeline). Hiding those models here would be a lie about what's
// reachable.
//
// Alias inputs (the model name the client uses to talk to the gateway) are
// also surfaced so human-friendly alias names appear in the picker.
//
// Query parameters honored:
//   - pageSize:  cap on returned entries (default 1000, max 1000 to match Google's behavior)
//   - pageToken: opaque cursor returned in the previous response; we don't paginate
//                (all results fit in one page unless the catalog grows huge), so we
//                accept and ignore it but never emit one.
//
// Proxy auth is validated by HandleProxy before reaching this function.
func (e *Engine) handleGeminiModels(r *http.Request, w http.ResponseWriter) {
	const maxPageSize = 1000
	const defaultPageSize = 1000
	pageSize := defaultPageSize
	if ps := r.URL.Query().Get("pageSize"); ps != "" {
		if n, err := strconv.Atoi(ps); err == nil && n > 0 {
			if n > maxPageSize {
				n = maxPageSize
			}
			pageSize = n
		}
	}

	downstreams, err := e.store.ListDownstreams()
	if err != nil {
		log.Printf("Error listing downstreams for gemini models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	aliases, err := e.store.ListAliases()
	if err != nil {
		log.Printf("Error listing aliases for gemini models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Build a downstream-id -> name map so we can attribute models to their
	// upstream in the description field.
	dsName := make(map[string]string, len(downstreams))
	for _, ds := range downstreams {
		dsName[ds.ID] = ds.Name
	}

	geminiMethods := []string{"generateContent", "streamGenerateContent", "countTokens"}
	seen := make(map[string]struct{})
	out := make([]geminiModelRecord, 0)

	// Surface every model that any downstream advertises. Google's convention
	// is for `name` to be prefixed with "models/", so we add the prefix if
	// the downstream's output_model_id doesn't already include it. This
	// matches what Google's own /v1beta/models returns and what Cherry
	// Studio's parser (listModels.ts line 192) handles via
	// `m.name.startsWith('models/') ? m.name.slice(7) : m.name`.
	//
	// We include models from every downstream — not just Gemini-format ones
	// — because the engine can auto-translate Gemini->OpenAI/Anthropic/
	// OpenAI Responses. See the function-level comment for the rationale.
	for _, ds := range downstreams {
		for _, m := range ds.OutputModelIDs {
			name := m
			if !strings.HasPrefix(name, "models/") {
				name = "models/" + name
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, geminiModelRecord{
				Name:                     name,
				DisplayName:              m,
				Description:              "via " + ds.Name,
				SupportedGenerationMethods: geminiMethods,
			})
		}
	}

	// Also surface alias inputs so users see their human-friendly alias
	// names in the model picker rather than only the upstream IDs. We include
	// aliases regardless of the downstream's api_formats for the same reason
	// as above (auto-translation makes any of them reachable).
	for _, a := range aliases {
		if a.IsRegex {
			// Skip the regex pattern itself — it's not a concrete model ID.
			// Announced names (if any) are surfaced below.
		} else {
			ds, err := e.store.GetDownstream(a.DownstreamID)
			if err != nil || ds == nil {
				continue
			}
			name := "models/" + a.InputModelID
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				out = append(out, geminiModelRecord{
					Name:                     name,
					DisplayName:              a.InputModelID,
					Description:              "via " + dsName[ds.ID] + " (alias for " + a.OutputModelID + ")",
					SupportedGenerationMethods: geminiMethods,
				})
			}
		}
		// Surface announced names from regex groups. These are concrete IDs
		// the user wants clients to see in the picker.
		ds, err := e.store.GetDownstream(a.DownstreamID)
		if err != nil || ds == nil {
			continue
		}
		for _, n := range a.AnnouncedNames {
			name := "models/" + n
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, geminiModelRecord{
				Name:                     name,
				DisplayName:              n,
				Description:              "via " + dsName[ds.ID] + " (alias for " + a.OutputModelID + ")",
				SupportedGenerationMethods: geminiMethods,
			})
		}
	}

	// Sort by name for deterministic output.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	// Apply pageSize cap.
	if len(out) > pageSize {
		out = out[:pageSize]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": out,
	})
}
