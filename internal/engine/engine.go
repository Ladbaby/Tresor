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

// SetProxyMode configures the outbound HTTP client's proxy behavior.
// It replaces the default http.Client with one that uses a custom Transport
// respecting the given proxy mode (auto, env, windows, none).
func (e *Engine) SetProxyMode(mode proxy.Mode) {
	transport := &http.Transport{
		Proxy: proxy.ProxyFunc(mode),
	}
	e.client = &http.Client{Transport: transport}
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

// validateProxyAuth checks the Authorization header against the configured proxy API keys.
// If auth is enabled and the key is invalid, it writes a 401 response and returns false.
// On success (or when auth is disabled), it returns true and strips the Authorization
// header from the request so the downstream's own API key can be injected cleanly.
func (e *Engine) validateProxyAuth(r *http.Request, w http.ResponseWriter) bool {
	if e.proxyAuth == nil || !e.proxyAuth.enabled {
		return true
	}

	token := r.Header.Get("Authorization")
	if strings.HasPrefix(token, "Bearer ") {
		token = strings.TrimPrefix(token, "Bearer ")
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

	// Strip the client's Authorization header so it doesn't leak to downstream.
	// The downstream will receive its own API key from config.
	r.Header.Del("Authorization")

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
	return false
}

// HandleProxy is the main proxy handler for LLM requests.
func (e *Engine) HandleProxy(w http.ResponseWriter, r *http.Request) {
	// Reject non-LLM paths immediately (browser noise like /favicon.ico).
	// These don't need logging — they're not gateway requests.
	if strings.HasPrefix(r.URL.Path, "/api/") || isLLMPath(r.URL.Path) == false {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	start := time.Now()

	// Wrap the response writer to capture status code
	cw := newStatusCaptureWriter(w)

	// Collect metadata for logging
	entry := RequestLogEntry{
		Timestamp: start,
		Method:    r.Method,
		Path:      r.URL.Path,
	}

	// Validate incoming proxy API key (if configured)
	if !e.validateProxyAuth(r, cw) {
		entry.Status = http.StatusUnauthorized
		entry.Error = "unauthorized"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	// Handle model list requests — aggregate models from all downstreams
	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		e.handleModels(cw)
		entry.Status = cw.status
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	// Read the full body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(cw, "failed to read body", http.StatusInternalServerError)
		entry.Status = http.StatusInternalServerError
		entry.Error = "failed to read body"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}
	r.Body.Close()

	// Extract model name from body (if present)
	model := extractModel(body)
	entry.Model = model

	// Model must be specified for forwarding
	if model == "" {
		http.Error(cw, "request body missing model", http.StatusBadRequest)
		entry.Status = http.StatusBadRequest
		entry.Error = "missing model"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	// --- Model Resolution (downstream selection) ---
	// This is the forwarding gate: a valid downstream must be resolved.
	// Rules are optional policy layers for transforms only.

	var ds *store.Downstream
	currentBody := body

	// Step 1: Try active alias first
	alias, err := e.store.FindActiveAlias(model)
	if err != nil {
		log.Printf("Error looking up alias for model %s: %v", model, err)
		http.Error(cw, "internal error", http.StatusInternalServerError)
		entry.Status = http.StatusInternalServerError
		entry.Error = "alias lookup error"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	if alias != nil {
		entry.AliasGroup = alias.InputModelID
		// Alias resolves the downstream and rewrites the model name
		ds, err = e.store.GetDownstream(alias.DownstreamID)
		if err != nil {
			log.Printf("Error getting downstream %s for alias %s: %v", alias.DownstreamID, alias.ID, err)
			http.Error(cw, fmt.Sprintf("alias %q references missing downstream %q", alias.ID, alias.DownstreamID), http.StatusBadGateway)
			entry.Status = http.StatusBadGateway
			entry.Error = "alias downstream missing"
			entry.Duration = DurationMs(time.Since(start))
			e.logger.Record(entry)
			return
		}
		currentBody = rewriteModelInBody(body, alias.OutputModelID)
		entry.ResolvedModel = alias.OutputModelID
	} else {
		// Step 2: No alias — try direct downstream by output_model_ids
		ds, err = e.store.FindDownstreamByOutputModel(model)
		if err != nil {
			log.Printf("Error looking up downstream for model %s: %v", model, err)
			http.Error(cw, "internal error", http.StatusInternalServerError)
			entry.Status = http.StatusInternalServerError
			entry.Error = "downstream lookup error"
			entry.Duration = DurationMs(time.Since(start))
			e.logger.Record(entry)
			return
		}

		if ds == nil {
			http.Error(cw, fmt.Sprintf("unknown model %q", model), http.StatusNotFound)
			entry.Status = http.StatusNotFound
			entry.Error = "unknown model"
			entry.Duration = DurationMs(time.Since(start))
			e.logger.Record(entry)
			return
		}
		// No alias: keep original model in the body
		entry.ResolvedModel = model
	}

	// Step 3: Format-aware rule matching
	// Detect input format from request path.
	inputFormat := detectInputFormat(r.URL.Path)
	// Find all matching rules (path+model+format filters). Rules only contribute
	// pipeline transformers; they never override the target downstream.
	rules, err := e.store.FindMatchingRules(r.URL.Path, model, inputFormat, ds.ID, ds.ApiFormats)
	if err != nil {
		log.Printf("Error finding rules: %v", err)
		http.Error(cw, "internal error", http.StatusInternalServerError)
		entry.Status = http.StatusInternalServerError
		entry.Error = "rule lookup error"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	// Collect rule IDs for logging
	if len(rules) > 0 {
		entry.RuleIDs = make([]string, len(rules))
		for i, r := range rules {
			entry.RuleIDs[i] = r.ID
		}
	}

	// Populate downstream info
	entry.DownstreamID = ds.ID
	entry.DownstreamName = ds.Name

	// Build pipeline context
	ctx := &PipelineContext{
		TargetDownstream: &Downstream{
			ID:         ds.ID,
			Name:       ds.Name,
			BaseURL:    ds.BaseURL,
			APIKey:     ds.APIKey,
			ApiFormats: ds.ApiFormats,
		},
		Variables: make(map[string]interface{}),
	}

	// Build pipeline by concatenating all matching rules' pipelines (in priority order).
	var pipeline Pipeline
	for _, rule := range rules {
		p, err := ParsePipelineConfig(rule.PipelineConfig, e.registry)
		if err != nil {
			log.Printf("Error building pipeline for rule %s: %v", rule.ID, err)
			http.Error(cw, "pipeline error", http.StatusInternalServerError)
			entry.Status = http.StatusInternalServerError
			entry.Error = "pipeline error"
			entry.Duration = DurationMs(time.Since(start))
			e.logger.Record(entry)
			return
		}
		pipeline.RequestSteps = append(pipeline.RequestSteps, p.RequestSteps...)
		pipeline.ResponseSteps = append(pipeline.ResponseSteps, p.ResponseSteps...)
		pipeline.StreamResponseSteps = append(pipeline.StreamResponseSteps, p.StreamResponseSteps...)
	}

	// Auto-translation: compare input format with downstream formats
	if inputFormat != "" && len(ds.ApiFormats) > 0 && !slices.Contains(ds.ApiFormats, inputFormat) {
		pluginID := "openai2anthropic"
		typeName := "OpenAI2Anthropic"
		if inputFormat == "anthropic" {
			pluginID = "anthropic2openai"
			typeName = "Anthropic2OpenAI"
		}
		transformer, err := e.registry.CreatePlugin(pluginID, nil)
		if err != nil {
			log.Printf("Error creating auto-translation plugin %s: %v", pluginID, err)
		} else {
			// Prepend request transformer (convert first), append response/stream transformers (convert last)
			if reqT, ok := transformer.(RequestTransformer); ok && !pluginInList(pipeline.RequestSteps, typeName) {
				pipeline.RequestSteps = append([]RequestTransformer{reqT}, pipeline.RequestSteps...)
			}
			if respT, ok := transformer.(ResponseTransformer); ok && !pluginInListResp(pipeline.ResponseSteps, typeName) {
				pipeline.ResponseSteps = append(pipeline.ResponseSteps, respT)
			}
			if streamT, ok := transformer.(StreamResponseTransformer); ok && !pluginInListStream(pipeline.StreamResponseSteps, typeName) {
				pipeline.StreamResponseSteps = append(pipeline.StreamResponseSteps, streamT)
			}
			log.Printf("Auto-translating %s → downstream %s (formats: %v)", inputFormat, ds.ID, ds.ApiFormats)
		}
	}

	// Execute request transformers (use currentBody which may have been rewritten by alias)
	currentReq, currentBody, err := ExecuteRequestPipeline(r, currentBody, ctx, pipeline.RequestSteps)
	if err != nil {
		log.Printf("Request pipeline error: %v", err)
		http.Error(cw, fmt.Sprintf("request pipeline error: %v", err), http.StatusBadGateway)
		entry.Status = http.StatusBadGateway
		entry.Error = "request pipeline error"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	// Forward the request to the downstream
	resp, cancel, err := e.forwardRequest(currentReq, currentBody, ctx)
	if err != nil {
		cancel()
		log.Printf("Forward error: %v", err)
		http.Error(cw, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		entry.Status = http.StatusBadGateway
		entry.Error = "upstream error"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	// Check if this is a streaming (SSE) response
	isStream := isEventStream(resp.Header.Get("Content-Type"))

	if isStream {
		// Record latency immediately (time-to-first-response, not full stream duration).
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
		http.Error(cw, errMsg, http.StatusBadGateway)
		entry.Status = http.StatusBadGateway
		entry.Error = "failed to read response"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	// Execute response transformers
	transformedBody, err := ExecuteResponsePipeline(resp, respBody, ctx, pipeline.ResponseSteps)
	if err != nil {
		log.Printf("Response pipeline error: %v", err)
		http.Error(cw, fmt.Sprintf("response pipeline error: %v", err), http.StatusBadGateway)
		entry.Status = http.StatusBadGateway
		entry.Error = "response pipeline error"
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	// Copy response headers, but strip framing headers that are now stale
	// after response transformation. The transformed body is always different
	// from the upstream body, so Content-Length and Transfer-Encoding from the
	// upstream response are invalid.
	for k, v := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
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

	// Passthrough mode: no transformers — write each line immediately with flush
	if !hasTransformers {
		for scanner.Scan() {
			select {
			case <-clientCtx.Done():
				return
			default:
			}
			line := strings.TrimRight(scanner.Text(), "\r")
			w.Write([]byte(line + "\n"))
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

	flushEvent := func() {
		if len(dataLines) == 0 {
			return
		}

		select {
		case <-clientCtx.Done():
			return
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
			return
		}
		// Safety guard: skip empty data to avoid sending data: \n\n that could
		// confuse downstream event parsers (e.g. Anthropic SDK).
		if len(chunk.Data) == 0 {
			return
		}

		// If the transformer output contains SSE event boundaries (\n\n), it is
		// already formatted SSE — write it directly without wrapping in data:.
		// This handles format transformers (e.g. anthropic2openai) that produce
		// multiple SSE events from a single upstream data line.
		if strings.Contains(string(chunk.Data), "\n\n") {
			w.Write(chunk.Data)
			flusher.Flush()
			return
		}

		out := &bytes.Buffer{}
		if chunk.EventType != "" {
			fmt.Fprintf(out, "event: %s\n", chunk.EventType)
		}
		out.WriteString("data: ")
		out.Write(chunk.Data)
		out.WriteString("\n\n")

		w.Write(out.Bytes())
		flusher.Flush()
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
			flushEvent()
			eventLine = ""
			dataLines = nil
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			eventLine = strings.TrimPrefix(line, "event: ")
			// Don't write the event line here — flushEvent will emit it
			// as part of the complete SSE event after data lines are collected.
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
			continue
		}

		// Unknown line type — pass through as-is
		w.Write([]byte(line + "\n"))
		flusher.Flush()
	}

	// Flush any remaining event (handles responses that don't end with \n\n)
	flushEvent()

	// If the downstream closed the stream without a [DONE] marker, send a
	// synthetic one through the pipeline so stream transformers can emit their
	// termination sequence (e.g. message_stop for Anthropic format). Without
	// this, the client would hang waiting for the stream to end, eventually
	// timeout, and retry — producing duplicate requests.
	if !doneSent {
		select {
		case <-clientCtx.Done():
		default:
			syntheticChunk := SSEChunk{Data: []byte("[DONE]")}
			transformed, err := ExecuteStreamResponsePipeline(syntheticChunk, ctx, pipeline.StreamResponseSteps)
			if err == nil && len(transformed.Data) > 0 {
				w.Write(transformed.Data)
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
	// already set auth headers (e.g., x-api-key for Anthropic)
	if ctx.TargetDownstream.APIKey != "" {
		hasAuthHeader := forwardedReq.Header.Get("Authorization") != "" ||
			forwardedReq.Header.Get("x-api-key") != ""
		if !hasAuthHeader {
			if slices.Contains(ctx.TargetDownstream.ApiFormats, "anthropic") {
				forwardedReq.Header.Set("x-api-key", ctx.TargetDownstream.APIKey)
				forwardedReq.Header.Set("anthropic-version", "2023-06-01")
			} else {
				forwardedReq.Header.Set("Authorization", "Bearer "+ctx.TargetDownstream.APIKey)
			}
		}
	}
	forwardedReq.Header.Set("Host", parsedURL.Host)

	resp, err := e.client.Do(forwardedReq)
	if err != nil {
		forwardCancel()
		return nil, func() {}, err
	}
	return resp, forwardCancel, nil
}

// extractModel parses the request body JSON to find the "model" field.
func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Model
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
// Returns "openai" for /v1/chat/completions, "anthropic" for /v1/messages, or "" for unknown paths.
func detectInputFormat(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "openai"
	case "/v1/messages":
		return "anthropic"
	default:
		return ""
	}
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

// pluginInList checks if a transformer with the given type name is already in the request pipeline.
func pluginInList(transformers []RequestTransformer, typeName string) bool {
	for _, t := range transformers {
		if transformerTypeName(t) == typeName {
			return true
		}
	}
	return false
}

// pluginInListResp checks if a transformer with the given type name is already in the response pipeline.
func pluginInListResp(transformers []ResponseTransformer, typeName string) bool {
	for _, t := range transformers {
		if transformerTypeName(t) == typeName {
			return true
		}
	}
	return false
}

// pluginInListStream checks if a transformer with the given type name is already in the stream pipeline.
func pluginInListStream(transformers []StreamResponseTransformer, typeName string) bool {
	for _, t := range transformers {
		if transformerTypeName(t) == typeName {
			return true
		}
	}
	return false
}

func transformerTypeName(t interface{}) string {
	typ := reflect.TypeOf(t)
	if typ == nil {
		return ""
	}
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ.Name()
}

// handleModels responds to GET /v1/models with an aggregated model list from
// all downstreams and aliases, formatted as an OpenAI-style model list response.
// Proxy auth is validated by HandleProxy before reaching this function.
func (e *Engine) handleModels(w http.ResponseWriter) {
	models, err := e.store.ListAllModels()
	if err != nil {
		log.Printf("Error listing models: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	openAIModels := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		openAIModels = append(openAIModels, map[string]interface{}{
			"id":       m,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "tresor",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   openAIModels,
	})
}
