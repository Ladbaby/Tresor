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
	e.logger.Record(*entry)
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
func (e *Engine) buildPipeline(path, model string, inputFormat string, ds *store.Downstream) (Pipeline, []store.Rule, *gatewayError) {
	// Find all matching rules (path+model+format filters)
	rules, err := e.store.FindMatchingRules(path, model, inputFormat, ds.ID, ds.ApiFormats)
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

	if !e.validateProxyAuth(r, cw) {
		entry.Status = http.StatusUnauthorized
		entry.Error = "unauthorized"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		e.handleModels(cw)
		entry.Status = cw.status
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
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
		e.logger.Record(entry)
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

	// Build transformation pipeline (rule matching + auto-translation)
	inputFormat := detectInputFormat(r.URL.Path)
	pipeline, rules, ge := e.buildPipeline(r.URL.Path, result.model, inputFormat, result.ds)
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
	e.logger.Debug("forwarding %s %s → downstream %q (%s) with model %q", r.Method, r.URL.Path, result.ds.ID, result.ds.Name, result.resolvedModel)
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

	// Execute request transformers
	currentReq, currentBody, err := ExecuteRequestPipeline(r, result.body, ctx, pipeline.RequestSteps)
	if err != nil {
		e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, "request pipeline error", fmt.Sprintf("request pipeline error: %v", err), "request pipeline error", err})
		return
	}

	// Forward request to downstream
	resp, cancel, err := e.forwardRequest(currentReq, currentBody, ctx)
	if err != nil {
		cancel()
		e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, "forward error", fmt.Sprintf("upstream error: %v", err), "upstream error", err})
		return
	}

	// Handle streaming response
	if isEventStream(resp.Header.Get("Content-Type")) {
		entry.Status = resp.StatusCode
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		e.handleStreamingResponse(cw, resp, ctx, &pipeline, cancel, r.Context())
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

	transformedBody, err := ExecuteResponsePipeline(resp, respBody, ctx, pipeline.ResponseSteps)
	if err != nil {
		e.logAndReturnError(cw, &entry, start, &gatewayError{http.StatusBadGateway, "response pipeline error", fmt.Sprintf("response pipeline error: %v", err), "response pipeline error", err})
		return
	}

	// Copy response headers, stripping stale framing headers and any
// Content-Encoding marker left from upstream (we ask for identity, but be
// defensive in case the downstream ignores Accept-Encoding).
	for k, v := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		cw.Header()[k] = v
	}
	cw.Header().Set("Content-Length", strconv.Itoa(len(transformedBody)))
	entry.Status = resp.StatusCode
	entry.Duration = DurationMs(time.Since(start))
	e.logger.Record(entry)
	cw.WriteHeader(resp.StatusCode)
	cw.Write(transformedBody)
}

// handleStreamingResponse pipes an SSE response from the downstream to the client.
// If stream transformers exist, each SSE event is transformed before sending.
// Without stream transformers, the response is passed through line-by-line (no buffering).
// The cancel function is called after the stream completes to clean up the downstream context.
func (e *Engine) handleStreamingResponse(w http.ResponseWriter, resp *http.Response, ctx *PipelineContext, pipeline *Pipeline, cancel context.CancelFunc, clientCtx context.Context) {
	defer resp.Body.Close()
	defer cancel()

	// Copy SSE-relevant headers to the client response
	for _, header := range []string{"Content-Type", "Cache-Control", "Connection"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}
	// Prevent reverse proxy buffering
	w.Header().Set("X-Accel-Buffering", "no")

	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("Streaming failed: ResponseWriter does not support Flusher")
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 1024*1024)

	hasTransformers := len(pipeline.StreamResponseSteps) > 0

	// clientGone is set when a write to the response fails, indicating the client
	// has disconnected. Once set, the function stops reading from the downstream.
	var clientGone bool
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
		flusher.Flush()
		return true
	}

	// Passthrough mode: no transformers — write each line immediately with flush
	if !hasTransformers {
		for scanner.Scan() {
			select {
			case <-clientCtx.Done():
				return
			default:
			}
			line := strings.TrimRight(scanner.Text(), "\r")
			if !tryWrite([]byte(line + "\n")) {
				return
			}
			flusher.Flush()
		}
		if err := scanner.Err(); err != nil {
			log.Printf("Stream ended: %v", err)
		}
		return
	}

	// Transform mode: accumulate SSE events, transform, then write
	var eventLine string
	var dataLines []string
	var doneSent bool // tracks whether downstream sent [DONE] marker

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

		// Track whether the downstream sent [DONE] so we know if synthetic
		// termination is needed when the stream ends.
		if eventLine == "" && strings.TrimSpace(rawData) == "[DONE]" {
			doneSent = true
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

		// If the transformer output contains SSE event boundaries (\n\n), it is
		// already formatted SSE — write it directly without wrapping in data:.
		// This handles format transformers (e.g. anthropic2openai) that produce
		// multiple SSE events from a single upstream data line.
		if strings.Contains(string(chunk.Data), "\n\n") {
			return tryWrite(chunk.Data) && tryFlush()
		}

		out := &bytes.Buffer{}
		if chunk.EventType != "" {
			fmt.Fprintf(out, "event: %s\n", chunk.EventType)
		}
		out.WriteString("data: ")
		out.Write(chunk.Data)
		out.WriteString("\n\n")

		return tryWrite(out.Bytes()) && tryFlush()
	}

	for scanner.Scan() {
		select {
		case <-clientCtx.Done():
			return
		default:
		}

		line := strings.TrimRight(scanner.Text(), "\r")

		if line == "" {
			// Empty line terminates an SSE event — flush it
			if !flushEvent() {
				return
			}
			eventLine = ""
			dataLines = nil
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

		// Unknown line type — pass through as-is
		if !tryWrite([]byte(line + "\n")) {
			return
		}
		flusher.Flush()
	}

	// Flush any remaining event (handles responses that don't end with \n\n)
	flushEvent()

	// If the downstream closed the stream without a [DONE] marker and the client
	// is still connected, send a synthetic one through the pipeline so stream
	// transformers can emit their termination sequence (e.g. message_stop for
	// Anthropic format). Without this, the client would hang waiting for the
	// stream to end, eventually timeout, and retry — producing duplicate requests.
	if !doneSent && !clientGone {
		select {
		case <-clientCtx.Done():
			return
		default:
			syntheticChunk := SSEChunk{Data: []byte("[DONE]")}
			transformed, err := ExecuteStreamResponsePipeline(syntheticChunk, ctx, pipeline.StreamResponseSteps)
			if err == nil && len(transformed.Data) > 0 {
				tryWrite(transformed.Data)
				flusher.Flush()
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Stream ended: %v", err)
	}
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
			// Skip regex patterns — they aren't concrete model IDs.
			continue
		}
		ds, err := e.store.GetDownstream(a.DownstreamID)
		if err != nil || ds == nil {
			continue
		}
		name := "models/" + a.InputModelID
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, geminiModelRecord{
			Name:                     name,
			DisplayName:              a.InputModelID,
			Description:              "via " + dsName[ds.ID] + " (alias for " + a.OutputModelID + ")",
			SupportedGenerationMethods: geminiMethods,
		})
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
