package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"tresor/internal/middleware"
	"unicode"
)

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

// HandleProxy is the main proxy handler for LLM requests.
func (e *Engine) HandleProxy(w http.ResponseWriter, r *http.Request) {
	corsHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/") || isLLMPath(r.URL.Path) == false {
		http.Error(w, fmt.Sprintf("%s path not found", r.URL.Path), http.StatusNotFound)
		return
	}

	start := time.Now()
	cw := newStatusCaptureWriter(w)
	entry := &RequestLogEntry{Timestamp: start, Method: r.Method, Path: r.URL.Path}

	// ClientIP: trust forwarded headers when connected via localhost (reverse proxy).
	entry.ClientIP = middleware.ExtractClientIP(r)

	// Authenticate reserved metadata independently of broad proxy authentication.
	if hasIntegrationHeaders(r.Header) {
		if r.Body == nil {
			stripIntegrationHeaders(r.Header)
			integrationError(cw, 401, "execution_unauthorized")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(cw, r.Body, externalValueLimit))
		r.Body.Close()
		if err != nil {
			stripIntegrationHeaders(r.Header)
			integrationError(cw, 413, "invalid_request")
			return
		}
		authenticated, authError := e.authenticateExecution(r, body)
		if authError != nil {
			e.logAndReturnError(cw, entry, start, authError)
			return
		}
		r = authenticated
		activeRequest, cleanup, activeErr := e.registerManagedExecution(r)
		if activeErr != nil {
			integrationError(cw, 401, "execution_unauthorized")
			return
		}
		defer cleanup()
		r = activeRequest
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	namespace := ""
	if e.proxyAuth != nil && e.proxyAuth.enabled {
		namespace = proxyNamespace(r)
	}
	if executionFromRequest(r) == nil && !e.validateProxyAuth(r, cw) {
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
	// All enabled downstream formats are discoverable through auto-translation.
	if r.URL.Path == "/v1beta/models" {
		e.handleGeminiModels(r, cw)
		entry.Status = cw.status
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return
	}

	r = withRouteNamespace(r, namespace)
	// Resolve model → downstream (reads body, extracts model, tries alias then direct lookup)
	result, ge := e.resolveModel(r)
	if ge != nil {
		entry.Model = result.model
		e.logAndReturnError(cw, entry, start, ge)
		return
	}
	r.Header.Del("x-tresor-session-id")
	entry.deferRecord = true
	defer e.publishRequest(entry)
	if result.release == nil {
		result.release = e.routes.reserve(result.ds.ID + "\x00" + result.resolvedModel)
	}
	defer func() {
		result.release()
		if result.sessionDone != nil {
			result.sessionDone()
		}
	}()
	e.integrationMu.RLock()
	registry := e.registry
	if snapshots, ok := registry.(interface{ Snapshot() PluginRegistry }); ok {
		registry = snapshots.Snapshot()
	}
	state := e.integrations
	e.integrationMu.RUnlock()
	r = r.WithContext(context.WithValue(r.Context(), integrationSnapshotKey{}, state))
	for {
		result.retry = false
		e.executeSelection(cw, r, result, entry, start, registry)
		if !result.retry {
			return
		}
		result.release()
		result.routeIndex++
		x := result.candidates[result.routeIndex]
		result.ds, result.alias, result.resolvedModel = x.ds, x.alias, x.alias.OutputModelID
		result.body = rewriteModelInBody(result.originalBody, result.resolvedModel)
		result.release = e.routes.reserve(x.target)
	}
}

func (e *Engine) executeSelection(cw *statusCaptureWriter, r *http.Request, result *modelResult, entry *RequestLogEntry, start time.Time, registry PluginRegistry) {
	entry.Model = result.model
	entry.RuleIDs, entry.Usage = nil, nil
	entry.Error = ""
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
	pipeline, rules, ge := e.buildPipelineWithRegistry(r.URL.Path, result.model, inputFormat, result.ds, result.alias, registry)
	if ge != nil {
		e.logAndReturnError(cw, entry, start, ge)
		return
	}

	if e.prepareIntegration(cw, r, result, &pipeline, entry, start) {
		return
	}

	// Collect rule IDs for logging
	if len(rules) > 0 {
		entry.RuleIDs = make([]string, len(rules))
		for i, rule := range rules {
			entry.RuleIDs[i] = rule.ID
		}
	}

	// Populate downstream info for request logging.
	entry.DownstreamID = result.ds.ID
	entry.DownstreamName = result.ds.Name
	ctx, ge := e.prepareDownstreamContext(r, result, inputFormat)
	if ge != nil {
		for _, p := range pipeline.ExternalSteps {
			if p.prepared {
				p.prepared = false
				if p.ctx != nil {
					p.ctx.execution().Failure = ge.cause
					cleanup := Pipeline{ExternalSteps: []*ExternalPlugin{p}}
					_ = cleanup.finishAttempt(p.ctx, "error")
				}
			}
		}
		entry.Duration = DurationMs(time.Since(start))
		e.logAndReturnError(cw, entry, start, ge)
		return
	}

	// Determine max retries (3 when retryOnEmpty enabled, 0 otherwise).
	// Codex requests are never replayed to avoid duplicate account-side generations.
	maxRetries := 0
	if e.retryOnEmpty && !ctx.CodexBackend {
		maxRetries = retryMaxCount
	}
	downstreamFormat := ctx.DownstreamFormat

	var cancelFn context.CancelFunc
	attemptOutcome := "error"
	defer func() {
		if r.Context().Err() != nil {
			attemptOutcome = "cancelled"
		}
		if r.Context().Err() != nil {
			ctx.execution().Failure = r.Context().Err()
		} else if attemptOutcome == "error" && ctx.execution().Failure == nil {
			ctx.execution().Failure = fmt.Errorf("request attempt failed")
		}
		if err := pipeline.finishAttempt(ctx, attemptOutcome); err != nil {
			if ctx.decode != nil {
				ctx.decode.failed = true
			}
		}
		if ctx.decode != nil && attemptOutcome != "success" {
			ctx.decode.failed = true
		}
		ctx.decode.finalize(entry)
	}()
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Client disconnected — stop retrying
			select {
			case <-r.Context().Done():
				entry.Status = http.StatusBadGateway
				entry.Error = "client disconnected during retry"
				entry.Duration = DurationMs(time.Since(start))
				e.recordAndCapture(entry, rawReq, nil, r.Header.Get("Content-Type"), "", false, inputFormat, downstreamFormat)
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

		entry.Usage = nil
		ctx.decode = nil
		if e.payloadStore != nil {
			ctx.decode = &decodeMeasurement{format: downstreamFormat}
		}
		ctx.RequestPathOverride = ""
		cleanReq, _ := CopyRequest(r, result.body)
		if err := pipeline.beginAttempt(ctx, cleanReq, attempt); err != nil {
			e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "plugin initialization error", "plugin initialization error", "plugin initialization error", err})
			return
		}
		// Execute request transformers with pristine headers on every attempt.
		currentReq, currentBody, err := ExecuteRequestPipeline(cleanReq, result.body, ctx, pipeline.RequestSteps)
		if err != nil {
			if cancelFn != nil {
				cancelFn()
			}
			e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "request pipeline error", fmt.Sprintf("request pipeline error: %v", err), "request pipeline error", err})
			return
		}

		for _, step := range pipeline.ExternalSteps {
			step.request = currentReq
		}
		e.logger.Debug("forwarding %s %s → downstream %q (%s) with model %q (attempt %d)", r.Method, r.URL.Path, result.ds.ID, result.ds.Name, result.resolvedModel, attempt+1)

		// Forward request to downstream
		resp, cancel, err := e.forwardRequest(currentReq, currentBody, ctx)
		if err != nil {
			cancel()
			if isNetworkFailure(err) && e.tryFallback(r, result, 502, "", err) {
				return
			}
			e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "forward error", fmt.Sprintf("upstream error: %v", err), "upstream error", err})
			return
		}
		cancelFn = cancel
		upstreamContentType := resp.Header.Get("Content-Type")
		ctx.observeResponse(resp, isEventStream(upstreamContentType) || (ctx.execution().Requested == "sse" && resp.StatusCode >= 200 && resp.StatusCode < 300))
		if err := pipeline.responseHeaders(resp, ctx); err != nil {
			ctx.recordFailure(err)
			attemptOutcome = "error"
			resp.Body.Close()
			cancel()
			e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "response header pipeline error", fmt.Sprintf("response header pipeline error: %v", err), "pipeline error", err})
			return
		}

		// Handle streaming response. A response is streamed when the downstream
		// marks it as an event stream, OR when the ChatGPT/Codex backend returns
		// to a streaming request: the reference client always reads Codex
		// responses as SSE (it hardcodes stream:true and never trusts the
		// response Content-Type), and Codex can return the SSE body under a
		// Content-Type that is not text/event-stream. Routing that body through
		// the non-streaming path would hand an "event:"-prefixed payload to the
		// transformer's JSON parser and 502 ("invalid character 'e'").
		streamed := isEventStream(upstreamContentType) || (ctx.execution().Requested == "sse" && resp.StatusCode >= 200 && resp.StatusCode < 300)
		ctx.observeResponse(resp, streamed)
		if !streamed && ctx.CodexBackend && isStreamRequest(currentBody) {
			streamed = true
			ctx.observeResponse(resp, true)
			ctx.execution().Classification = "host"
		}
		collectStream := streamed && ctx.ExternalCollector != nil && resp.StatusCode >= 200 && resp.StatusCode < 300
		if streamed && !collectStream {
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
			outcome := e.handleStreamingResponse(hw, resp, ctx, &pipeline, cancel, r.Context(), rawReq, r.Header.Get("Content-Type"), resp.Header.Get("Content-Type"), entry, e.retryOnEmpty && !ctx.CodexBackend, inputFormat, downstreamFormat)
			// An explicit error event is a definitive answer, never an empty
			// response — do not retry it.
			if outcome.streamErr != nil {
				label := "upstream stream error"
				if outcome.pluginFailure {
					label = "plugin stream error"
				}
				if !outcome.pluginFailure {
					message := outcome.streamErr.Message
					// Provider messages are untrusted. Never publish configured keys,
					// control characters, or unbounded text in the request log.
					keys := []string{result.ds.EffectiveAPIKey()}
					if ctx.TargetDownstream != nil {
						keys = append(keys, ctx.TargetDownstream.APIKey)
					}
					if e.proxyAuth != nil {
						for key := range e.proxyAuth.keys {
							keys = append(keys, key)
						}
					}
					for _, key := range keys {
						if key != "" {
							message = strings.ReplaceAll(message, key, "[redacted]")
						}
					}
					message = strings.Map(func(r rune) rune {
						if unicode.IsControl(r) {
							return ' '
						}
						return r
					}, message)
					message = strings.Join(strings.Fields(message), " ")
					if runes := []rune(message); len(runes) > 1024 {
						message = string(runes[:1024]) + "..."
					}
					if message != "" {
						label += ": " + message
					}
				}
				if !outcome.committed && !outcome.pluginFailure && e.tryFallback(r, result, outcome.streamErr.Status, outcome.streamErr.Message, nil) {
					if err := pipeline.finishAttempt(ctx, "error"); err != nil {
						result.retry = false
						e.logAndReturnError(cw, entry, start, &gatewayError{502, "plugin finish error", "plugin finish error", "pipeline error", err})
					}
					return
				}
				if outcome.committed {
					// Bytes already reached the client, so the 200 status cannot
					// be recalled; the error event was passed through verbatim.
					// Record it as a failure so it does not log as a success.
					entry.Status = cw.status
					entry.Error = label
					entry.Duration = DurationMs(time.Since(start))
					e.logger.Record(entry)
					log.Printf("%s after content", label)
					return
				}
				e.logAndReturnError(cw, entry, start, &gatewayError{outcome.streamErr.Status, label, outcome.streamErr.Message, label, nil})
				return
			}

			if outcome.empty && resp.StatusCode == http.StatusOK {
				// Empty stream with HTTP 200 — retry if attempts remain
				if attempt >= maxRetries {
					entry.Status = cw.status
					entry.Duration = DurationMs(time.Since(start))
					e.recordAndCapture(entry, rawReq, nil, r.Header.Get("Content-Type"), resp.Header.Get("Content-Type"), false, inputFormat, downstreamFormat)
					e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "empty response after retries exhausted", "empty response after retries exhausted", "empty response", nil})
					return
				}
				// Close upstream response before retrying
				resp.Body.Close()
				cancel()
				if err := pipeline.finishAttempt(ctx, "retry"); err != nil {
					e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "plugin finish error", "plugin finish error", "pipeline error", err})
					return
				}
				continue
			}
			// Response had content or non-200 status — done
			attemptOutcome = "success"
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				e.confirmRoute(result)
			}
			return
		}

		buffered, ge := e.readBufferedResponse(resp, ctx, &pipeline, cancel, r.Context(), collectStream, upstreamContentType)
		if ge != nil {
			if ge.cause != nil && isNetworkFailure(ge.cause) && e.tryFallback(r, result, http.StatusBadGateway, "", ge.cause) {
				if finishErr := pipeline.finishAttempt(ctx, "retry"); finishErr == nil {
					return
				}
				result.retry = false
			}
			e.logAndReturnError(cw, entry, start, ge)
			return
		}
		respBody := buffered.body
		if e.tryFallback(r, result, resp.StatusCode, string(respBody), nil) {
			if err := pipeline.finishAttempt(ctx, "error"); err != nil {
				result.retry = false
				e.logAndReturnError(cw, entry, start, &gatewayError{502, "plugin finish error", "plugin finish error", "pipeline error", err})
			}
			return
		}
		transformedBody := buffered.transformed
		rawResp := buffered.captured
		respTrunc := buffered.truncated
		inspectRespCT := buffered.captureType

		// Check for empty response (using raw upstream body, not transformed)
		// Only retry if HTTP status is 200 AND response is empty AND the path
		// is a generation endpoint. Utility endpoints like /count_tokens can
		// legitimately return 200 with bodies that look "empty" to the
		// parser (e.g. {"input_tokens":7802}); retrying them produces a
		// spurious 502.
		if !ctx.CodexBackend && e.retryOnEmpty && e.shouldRetry(resp, respBody, downstreamFormat, r.URL.Path) && (!collectStream || !ctx.execution().RawContent) {
			if attempt >= maxRetries {
				entry.Status = resp.StatusCode
				entry.Duration = DurationMs(time.Since(start))
				e.recordAndCapture(entry, rawReq, rawResp, r.Header.Get("Content-Type"), inspectRespCT, respTrunc, inputFormat, downstreamFormat)
				e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "empty response after retries exhausted", "empty response after retries exhausted", "empty response", nil})
				return
			}
			if err := pipeline.finishAttempt(ctx, "retry"); err != nil {
				e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "plugin finish error", "plugin finish error", "pipeline error", err})
				return
			}
			continue
		}

		if err := pipeline.finishAttempt(ctx, "success"); err != nil {
			e.logAndReturnError(cw, entry, start, &gatewayError{http.StatusBadGateway, "plugin finish error", "plugin finish error", "pipeline error", err})
			return
		}
		attemptOutcome = "success"
		// Response has content — write to client
		prepareBufferedHeaders(cw, resp, transformedBody)
		entry.Status = resp.StatusCode
		entry.Duration = DurationMs(time.Since(start))

		// Only accumulate usage statistics when the inspector is enabled.
		if e.payloadStore != nil {
			if collectStream && ctx.decode != nil && ctx.decode.usage != nil {
				entry.Usage = ctx.decode.usage
			} else if usage, ok := UsageFromResponseBody(respBody); ok && usage != nil {
				entry.Usage = usage
			}
		}

		e.recordAndCapture(entry, rawReq, rawResp, r.Header.Get("Content-Type"), inspectRespCT, respTrunc, inputFormat, downstreamFormat)
		writeBufferedResponse(cw, resp, transformedBody)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			e.confirmRoute(result)
		}
		return
	}
}
