package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"tresor/internal/proxy"
	"tresor/internal/store"
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
}

// New creates a new Engine.
func New(s *store.Store) *Engine {
	return &Engine{
		store:  s,
		client: &http.Client{},
	}
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

// ServeProxy starts an HTTP server that serves both the admin API and the LLM proxy.
// It uses a mux that routes /api/* to the admin handler and everything else to the proxy.
// webHandler serves the embedded web UI at the root.
// isWebPath is a function that checks if a path belongs to the web UI.
func ServeProxy(eng *Engine, adminHandler http.Handler, webHandler http.Handler, isWebPath func(string) bool, listener net.Listener) error {
	mux := http.NewServeMux()

	// Admin API routes
	mux.Handle("/api/", adminHandler)

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

	return http.Serve(listener, mux)
}

// ServeAdminOnly starts an HTTP server that serves both the admin API and web UI (for UDS).
func ServeAdminOnly(adminHandler http.Handler, listener net.Listener) error {
	return http.Serve(listener, adminHandler)
}

// HandleProxy is the main proxy handler for LLM requests.
func (e *Engine) HandleProxy(w http.ResponseWriter, r *http.Request) {
	// Only forward certain LLM-type requests; serve admin API via /api/*
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Validate incoming proxy API key (if configured)
	if !e.validateProxyAuth(r, w) {
		return
	}

	// Handle model list requests — aggregate models from all downstreams
	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		e.handleModels(w, r)
		return
	}

	// Read the full body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusInternalServerError)
		return
	}
	r.Body.Close()

	// Extract model name from body (if present)
	model := extractModel(body)

	// Model must be specified for forwarding
	if model == "" {
		http.Error(w, "request body missing model", http.StatusBadRequest)
		return
	}

	// --- Model Resolution (downstream selection) ---
	// This is the forwarding gate: a valid downstream must be resolved.
	// Rules are optional policy layers for transforms only.

	var ds *store.Downstream
	currentBody := body
	hasAlias := false

	// Step 1: Try active alias first
	alias, err := e.store.FindActiveAlias(model)
	if err != nil {
		log.Printf("Error looking up alias for model %s: %v", model, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if alias != nil {
		hasAlias = true
		// Alias resolves the downstream and rewrites the model name
		ds, err = e.store.GetDownstream(alias.DownstreamID)
		if err != nil {
			log.Printf("Error getting downstream %s for alias %s: %v", alias.DownstreamID, alias.ID, err)
			http.Error(w, fmt.Sprintf("alias %q references missing downstream %q", alias.ID, alias.DownstreamID), http.StatusBadGateway)
			return
		}
		currentBody = rewriteModelInBody(body, alias.OutputModelID)
	} else {
		// Step 2: No alias — try direct downstream by output_model_ids
		ds, err = e.store.FindDownstreamByOutputModel(model)
		if err != nil {
			log.Printf("Error looking up downstream for model %s: %v", model, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if ds == nil {
			http.Error(w, fmt.Sprintf("unknown model %q", model), http.StatusNotFound)
			return
		}
		// No alias: keep original model in the body
	}

	// Step 3: Optional rule lookup (for pipeline transforms only)
	rule, err := e.store.FindMatchingRule(r.URL.Path, model)
	if err != nil {
		log.Printf("Error finding rule: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// If a rule matched and there's no alias, let the rule override the downstream.
	// Alias takes precedence when present (explicit model-to-downstream contract).
	if rule != nil && !hasAlias {
		if rule.ActiveDownstream != "" {
			ruleDs, err := e.store.GetDownstream(rule.ActiveDownstream)
			if err != nil {
				log.Printf("Error getting downstream %s for rule %s: %v", rule.ActiveDownstream, rule.ID, err)
				http.Error(w, fmt.Sprintf("rule %q references missing downstream %q", rule.ID, rule.ActiveDownstream), http.StatusBadGateway)
				return
			}
			ds = ruleDs
		}
	}

	// Build pipeline context
	ctx := &PipelineContext{
		TargetDownstream: &Downstream{
			ID:      ds.ID,
			Name:    ds.Name,
			BaseURL: ds.BaseURL,
			APIKey:  ds.APIKey,
		},
		Variables: make(map[string]interface{}),
	}

	// Build and execute pipeline (rule's config or empty)
	pipelineConfig := "[]"
	if rule != nil {
		pipelineConfig = rule.PipelineConfig
	}

	pipeline, err := ParsePipelineConfig(pipelineConfig, e.registry)
	if err != nil {
		log.Printf("Error building pipeline: %v", err)
		http.Error(w, "pipeline error", http.StatusInternalServerError)
		return
	}

	// Execute request transformers (use currentBody which may have been rewritten by alias)
	currentReq, currentBody, err := ExecuteRequestPipeline(r, currentBody, ctx, pipeline.RequestSteps)
	if err != nil {
		log.Printf("Request pipeline error: %v", err)
		http.Error(w, fmt.Sprintf("request pipeline error: %v", err), http.StatusBadGateway)
		return
	}

	// Forward the request to the downstream
	resp, err := e.forwardRequest(currentReq, currentBody, ctx)
	if err != nil {
		log.Printf("Forward error: %v", err)
		http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		return
	}

	// Check if this is a streaming (SSE) response
	contentType := resp.Header.Get("Content-Type")
	isStream := strings.EqualFold(contentType, "text/event-stream")

	if isStream {
		// Streaming path: pipe SSE events to the client in real-time.
		// If stream transformers exist, each chunk is transformed on the fly.
		// Otherwise, events are passed through unchanged but still flushed immediately.
		e.handleStreamingResponse(w, resp, ctx, pipeline)
		return
	}

	// Non-streaming response: buffer and transform
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		resp.Body.Close()
		http.Error(w, "failed to read upstream response", http.StatusInternalServerError)
		return
	}

	// Execute response transformers
	transformedBody, err := ExecuteResponsePipeline(resp, respBody, ctx, pipeline.ResponseSteps)
	if err != nil {
		resp.Body.Close()
		log.Printf("Response pipeline error: %v", err)
		http.Error(w, fmt.Sprintf("response pipeline error: %v", err), http.StatusBadGateway)
		return
	}

	// Copy response headers
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(transformedBody)
}

// handleStreamingResponse pipes an SSE response from the downstream to the client.
// If stream transformers exist, each SSE event is transformed before sending.
// Without stream transformers, the response is passed through line-by-line (no buffering).
func (e *Engine) handleStreamingResponse(w http.ResponseWriter, resp *http.Response, ctx *PipelineContext, pipeline *Pipeline) {
	defer resp.Body.Close()

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
			line := strings.TrimRight(scanner.Text(), "\r")
			w.Write([]byte(line + "\n"))
			flusher.Flush()
		}
		if err := scanner.Err(); err != nil {
			log.Printf("Stream scanner error: %v", err)
		}
		return
	}

	// Transform mode: accumulate SSE events, transform, then write
	var eventLine string
	var dataLines []string

	flushEvent := func() {
		if len(dataLines) == 0 {
			return
		}

		// Combine data lines to get the raw SSE payload
		rawData := strings.Join(dataLines, "\n")
		chunk := SSEChunk{EventType: eventLine, Data: []byte(rawData)}

		// Run through stream transformers
		var err error
		chunk, err = ExecuteStreamResponsePipeline(chunk, ctx, pipeline.StreamResponseSteps)
		if err != nil {
			log.Printf("Stream transform error: %v", err)
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
			w.Write([]byte(line + "\n"))
			flusher.Flush()
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

	if err := scanner.Err(); err != nil {
		log.Printf("Stream scanner error: %v", err)
	}
}

// forwardRequest sends the (possibly transformed) request to the target downstream.
func (e *Engine) forwardRequest(original *http.Request, body []byte, ctx *PipelineContext) (*http.Response, error) {
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
		return nil, fmt.Errorf("parse target URL: %w", err)
	}

	// Build forwarded request
	forwardedReq, err := http.NewRequestWithContext(original.Context(), original.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create forwarded request: %w", err)
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
			forwardedReq.Header.Set("Authorization", "Bearer "+ctx.TargetDownstream.APIKey)
		}
	}
	forwardedReq.Header.Set("Host", parsedURL.Host)

	return e.client.Do(forwardedReq)
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

// handleModels responds to GET /v1/models with an aggregated model list from
// all downstreams and aliases, formatted as an OpenAI-style model list response.
func (e *Engine) handleModels(w http.ResponseWriter, r *http.Request) {
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
