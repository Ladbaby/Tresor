package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"tresor/internal/inspect"
)

// streamOutcome reports how a streamed response ended.
type streamOutcome struct {
	// empty is true when no real content was produced and retry-on-empty is
	// enabled, signalling HandleProxy to retry the upstream.
	empty bool
	// streamErr reports fatal upstream events or plugin execution failures.
	streamErr     *StreamError
	pluginFailure bool
	// committed reports whether the HTTP status reached the client. When false,
	// the caller can still replace it with an error status.
	committed bool
}

// handleStreamingResponse delivers upstream SSE through distinct passthrough
// and transformed paths, then closes the response and cancels its connection.
// Header commitment is delayed until vetted events are released. Retry mode
// preserves progressive reasoning/content delivery but holds terminal events
// until the empty-response decision; it does not buffer all output.
// Raw capture precedes transformations. Content detection starts with the
// downstream format (or client format) and can recognize the observed format.
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
	pluginFailure := false
	writePluginFailure := func(err error) {
		if !w.IsFlushed() {
			return
		}
		message := err.Error()
		format := ctx.ClientFormat
		var payload []byte
		switch format {
		case "anthropic":
			payload, _ = json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": message}})
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
		case "responses", "openai_responses":
			payload, _ = json.Marshal(map[string]any{"type": "error", "code": "server_error", "message": message})
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
		case "gemini":
			payload, _ = json.Marshal(map[string]any{"error": map[string]any{"code": 500, "status": "INTERNAL", "message": message}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		default:
			payload, _ = json.Marshal(map[string]any{"error": map[string]string{"type": "server_error", "code": "plugin_failure", "message": message}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		}
		SafeFlush(w)
	}

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
		if streamErr != nil && ctx.execution().Failure == nil {
			ctx.execution().Failure = collectionUpstreamError{status: streamErr.Status, message: streamErr.Message}
		}
		if clientCtx.Err() != nil && ctx.execution().Failure == nil {
			ctx.execution().Failure = clientCtx.Err()
		}
		if ctx.decode != nil && (streamErr != nil || clientCtx.Err() != nil) {
			ctx.decode.failed = true
		}
		empty := bufferForRetry && !contentProduced
		if streamErr == nil && !empty && clientCtx.Err() == nil {
			if err := pipeline.finishAttempt(ctx, "success"); err != nil {
				streamErr = &StreamError{Status: http.StatusBadGateway, Message: fmt.Sprintf("plugin finish error: %v", err)}
			}
		}
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
			empty:         empty,
			streamErr:     streamErr,
			pluginFailure: pluginFailure,
			committed:     w.IsFlushed(),
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

	if len(pipeline.ExternalSteps) > 0 {
		for key, values := range resp.Header {
			if !strings.EqualFold(key, "Content-Length") && !strings.EqualFold(key, "Transfer-Encoding") && !strings.EqualFold(key, "Content-Encoding") {
				w.Header()[key] = append([]string{}, values...)
			}
		}
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

	// Startup metadata and keep-alives stay held until visible output has
	// been vetted. Reasoning/tool activity and unknown events remain progressive;
	// unlike retry content detection, they can commit the HTTP status.
	var pendingBuf bytes.Buffer
	holding := true
	const maxPendingBytes = 1024 * 1024

	tryWrite := func(p []byte) bool {
		if clientGone {
			return false
		}
		if _, err := w.Write(p); err != nil {
			if ctx.decode != nil {
				ctx.decode.failed = true
			}
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
			// Startup metadata cannot force a successful status. Bound its storage
			// and fail while the HTTP status is still replaceable.
			streamErr = &StreamError{Status: http.StatusBadGateway, Message: "upstream startup metadata exceeds 1 MiB limit"}
			discardPending()
			return false
		}
		if !tryWrite([]byte(line + "\n")) {
			return false
		}
		return tryFlush()
	}
	// writeAndFlush is the standard "send some bytes and flush" pair. Any held
	// bytes are released first so ordering is preserved.
	writeAndFlush := func(p []byte) bool {
		if holding && startupSSEBytes(p) {
			if pendingBuf.Len()+len(p) > maxPendingBytes {
				streamErr = &StreamError{Status: http.StatusBadGateway, Message: "upstream startup metadata exceeds 1 MiB limit"}
				discardPending()
				return false
			}
			pendingBuf.Write(p)
			return true
		}
		if holding && !releasePending() {
			return false
		}
		return tryWrite(p) && tryFlush()
	}
	releaseHeld = func() { releasePending() }

	hasExternalStream := len(pipeline.UpstreamSteps) > 0
	for _, step := range pipeline.StreamResponseSteps {
		if _, ok := step.(externalStream); ok {
			hasExternalStream = true
			break
		}
	}
	if hasExternalStream {
		reason := "error"
		defer func() {
			if reason != "eof" {
				if clientCtx.Err() != nil {
					reason = "cancelled"
				}
				_, _ = eventLifecycle(pipeline.UpstreamSteps, "upstreamEnd", reason, resp, ctx)
				_, _ = pipeline.streamLifecycle("streamEnd", reason, resp, ctx)
			}
		}()
		fail := func(err error) streamOutcome {
			pluginFailure = true
			if ctx.execution().Failure == nil {
				ctx.execution().Failure = err
			}
			streamErr = &StreamError{Status: http.StatusBadGateway, Message: err.Error()}
			discardPending()
			writePluginFailure(err)
			record()
			return finish()
		}
		upInitial, err := eventLifecycle(pipeline.UpstreamSteps, "upstreamStart", "", resp, ctx)
		if err != nil {
			return fail(err)
		}
		initial, err := pipeline.streamLifecycle("streamStart", "", resp, ctx)
		if err == nil {
			var out []SSEChunk
			out, err = executeEventList(upInitial, ctx, pipeline.StreamResponseSteps)
			for _, event := range upInitial {
				if upstream, ok := ParseStreamError(event.EventType, event.Data); ok {
					return fail(collectionUpstreamError{status: upstream.Status, message: upstream.Message})
				}
			}
			initial = append(initial, out...)
		}
		if err != nil {
			return fail(err)
		}
		if err := boundEventList(initial, effectiveEventLimit(ctx)); err != nil {
			return fail(err)
		}
		if len(initial) > 0 && !writeAndFlush(serializeEvents(initial)) {
			return finish()
		}
		var terminal []byte
		err = scanEvents(resp.Body, func(event SSEChunk) error {
			if err := clientCtx.Err(); err != nil {
				return err
			}
			for _, line := range strings.Split(string(serializeEvents([]SSEChunk{event})), "\n") {
				teeLine(line)
			}
			if upstream, ok := ParseStreamError(event.EventType, event.Data); ok {
				streamErr = upstream
				return fmt.Errorf("upstream stream error: %s", upstream.Message)
			}
			// Observe each complete raw downstream event before any plugin transforms.
			ctx.observeDecodeEvent(event, time.Now())
			scrapeUsage(event.Data)
			if bufferForRetry {
				if format := DetectStreamFormat(event); format != "" {
					streamFormat = format
				}
			}
			hasContent := bufferForRetry && IsStreamContentLine("data: "+string(event.Data), streamFormat)
			isTerminal := bufferForRetry && isTerminalEvent(string(event.Data), streamFormat)
			ctx.execution().CurrentFormat = ctx.DownstreamFormat
			ctx.execution().Statistics.RawBytes += int64(len(event.Data))
			ctx.execution().Statistics.RawEvents++
			out, err := executeEventList([]SSEChunk{event}, ctx, pipeline.UpstreamSteps)
			if err == nil {
				out, err = executeEventList(out, ctx, pipeline.StreamResponseSteps)
			}
			if err != nil {
				return err
			}
			for _, event := range out {
				ctx.execution().Statistics.OutputBytes += int64(len(event.Data))
				ctx.execution().Statistics.OutputEvents++
			}
			if hasContent {
				contentProduced = true
				if len(terminal) > 0 {
					if !writeAndFlush(terminal) {
						return fmt.Errorf("client write failed")
					}
					terminal = nil
				}
			}
			if isTerminal {
				terminal = append(terminal, serializeEvents(out)...)
				return nil
			}
			if len(out) > 0 && !writeAndFlush(serializeEvents(out)) {
				return fmt.Errorf("client write failed")
			}
			return nil
		})
		if err != nil {
			if streamErr != nil {
				discardPending()
				record()
				return finish()
			}
			return fail(err)
		}
		ctx.execution().CurrentFormat = ctx.DownstreamFormat
		upTail, err := eventLifecycle(pipeline.UpstreamSteps, "upstreamEnd", "eof", resp, ctx)
		if err != nil {
			return fail(err)
		}
		trailing, err := executeEventList(upTail, ctx, pipeline.StreamResponseSteps)
		if err != nil {
			return fail(err)
		}
		ctx.execution().CurrentFormat = ctx.DownstreamFormat
		tail, err := pipeline.streamLifecycle("streamEnd", "eof", resp, ctx)
		trailing = append(trailing, tail...)
		if err != nil {
			return fail(err)
		}
		if err := boundEventList(trailing, effectiveEventLimit(ctx)); err != nil {
			return fail(err)
		}
		reason = "eof"
		if !bufferForRetry || contentProduced {
			if len(terminal) > 0 && !writeAndFlush(terminal) {
				return finish()
			}
			if len(trailing) > 0 && !writeAndFlush(serializeEvents(trailing)) {
				return finish()
			}
		}
		record()
		return finish()
	}

	// Passthrough mode: no transformers — accumulate each SSE event and write
	// it through on its terminating blank line. Terminal events are held (as
	// the full event, event: + data:) until end-of-stream so they are emitted
	// exactly once and can be suppressed when the stream turns out to be empty
	// and is retried. Line-by-line passthrough (with a data-line-only terminal
	// buffer) used to re-emit the terminal's data line after the real
	// message_stop had already been flushed inline, appending an orphan
	// `data:` line; that orphan corrupted a chained downstream that also held
	// its terminal, so the client saw no clean message_stop.
	if !hasTransformers {
		// Event being accumulated: raw lines (CR preserved, for verbatim
		// re-emission), the event name, and the data payload lines.
		var evRaw []string
		var evName string
		var evData []string
		// sseEventName is the event name used for in-band error detection.
		var sseEventName string
		// terminalBuf holds a complete terminal event ([DONE] / message_stop /
		// response.completed) until end-of-stream, so we can suppress it when
		// the stream turned out to be empty (or reasoning-only) and the
		// upstream is being retried. While held, the rest of the stream
		// (reasoning + content) is forwarded to the client immediately.
		var terminalBuf bytes.Buffer
		var terminalEventActive bool
		flushTerminal := func() bool {
			if !terminalEventActive {
				return true
			}
			ok := writeAndFlush(terminalBuf.Bytes())
			terminalBuf.Reset()
			terminalEventActive = false
			return ok
		}
		discardTerminal := func() {
			terminalBuf.Reset()
			terminalEventActive = false
		}

		// writeEvent emits the accumulated event verbatim: each raw line, then
		// its terminating blank line. Returns false when the client is gone.
		writeEvent := func() bool {
			for _, l := range evRaw {
				if !writeLine(l) {
					return false
				}
			}
			return writeLine("")
		}
		resetEvent := func() {
			evRaw = nil
			evName = ""
			evData = nil
			sseEventName = ""
		}
		// closeEvent processes the currently-accumulated event once it is
		// complete (terminated by a blank line, or at end-of-stream). Returns
		// false when the reader should stop and finish (client gone or a fatal
		// in-band error was hit); true when scanning may continue.
		closeEvent := func() bool {
			if evName == "" && len(evData) == 0 {
				return true
			}
			payload := strings.Join(evData, "\n")
			ctx.observeDecodeEvent(SSEChunk{EventType: sseEventName, Data: []byte(payload)}, time.Now())
			// Check first for a fatal in-band error event — the downstream said
			// 200 but is reporting a failure.
			if se, isErr := ParseStreamError(sseEventName, []byte(payload)); isErr {
				streamErr = se
				if w.IsFlushed() {
					// Bytes already went out, so the 200 cannot be recalled.
					// Pass the error event through so the client's SDK can
					// surface it, then stop reading.
					if !writeEvent() {
						return false
					}
				} else {
					// Nothing has reached the client yet: suppress the error
					// event and the held keep-alives so HandleProxy can answer
					// with the provider's real status instead.
					discardPending()
				}
				resetEvent()
				return false
			}
			// Look for a usage block on the raw upstream event. Passthrough keeps
			// the raw payload, so this is the pre-"transform" upstream data —
			// exactly what the transform path scrapes. Without this, a stream
			// served without any stream transformer (input format already matches
			// the downstream) never accumulates usage and the Logs tab shows
			// cache hit rate as N/A even though the raw inspect view has it.
			scrapeUsage([]byte(payload))
			if bufferForRetry {
				if payload != "" && IsStreamContentLine("data: "+payload, streamFormat) {
					contentProduced = true
					// Real content detected — if we were holding a terminal
					// event, flush it now (it was held because the prior
					// reasoning-only events didn't count).
					if !flushTerminal() {
						return false
					}
				} else if isTerminalEvent(payload, streamFormat) {
					// Terminal event ([DONE] / message_stop / etc.): hold the
					// FULL event until end-of-stream so we can suppress it when
					// the upstream needs to be retried. If a terminal is already
					// held (e.g. Anthropic's message_delta followed by
					// message_stop), flush it first so both are emitted exactly
					// once.
					if terminalEventActive && !flushTerminal() {
						return false
					}
					terminalEventActive = true
					terminalBuf.Reset()
					for _, l := range evRaw {
						terminalBuf.WriteString(l)
						terminalBuf.WriteByte('\n')
					}
					terminalBuf.WriteByte('\n')
					resetEvent()
					return true
				}
			}
			// Release before writing visible output so large content events do not
			// consume the startup-only buffer budget.
			if holding && !streamStartupEvent(sseEventName, []byte(payload)) && !releasePending() {
				return false
			}
			if !writeEvent() {
				return false
			}
			resetEvent()
			return true
		}

		for scanner.Scan() {
			select {
			case <-clientCtx.Done():
				record()
				return finish()
			default:
			}
			line := scanner.Text()
			trimmed := strings.TrimRight(line, "\r")
			teeLine(trimmed)

			if trimmed == "" {
				// End of an SSE event (or a bare keep-alive / blank gap).
				if evName != "" || len(evData) > 0 {
					if !closeEvent() {
						record()
						return finish()
					}
				} else {
					// Bare blank (no event data, e.g. a keep-alive gap): pass
					// it through but do not release the hold — that is what lets
					// a provider's trailing `event: error` still become a real
					// HTTP error.
					if !writeLine("") {
						record()
						return finish()
					}
				}
				continue
			}

			if strings.HasPrefix(trimmed, "event: ") {
				sseEventName = strings.TrimPrefix(trimmed, "event: ")
				evName = sseEventName
				evRaw = append(evRaw, line)

				// Detect the downstream's actual stream format from the event
				// name as soon as we see it. The seed is only a hint — a
				// multi-format downstream (e.g. llama-swap with
				// [openai, anthropic]) may speak a different format than
				// ApiFormats[0] suggested, in which case the seed would
				// mislabel the stream and break empty-response detection.
				// OpenAI streams have no event names and are detected via the
				// "choices" payload marker on the first data: line.
				if bufferForRetry {
					if f := DetectStreamFormat(SSEChunk{EventType: sseEventName}); f != "" {
						streamFormat = f
					}
				}
				continue
			}

			if strings.HasPrefix(trimmed, "data: ") {
				data := strings.TrimPrefix(trimmed, "data: ")
				evData = append(evData, data)
				evRaw = append(evRaw, line)

				// On the first data: line of a stream, the payload may carry
				// the format marker that the event name doesn't (e.g. OpenAI's
				// "choices"). Override the seed when this is the case.
				if bufferForRetry {
					if f := DetectStreamFormat(SSEChunk{
						EventType: sseEventName,
						Data:      []byte(data),
					}); f != "" {
						streamFormat = f
					}
				}
				continue
			}

			// Unknown line type (including ":" keep-alive comments) — write
			// through immediately, held while the status is still open. It is
			// not part of any data event, so it does not end the hold.
			if !writeLine(line) {
				record()
				return finish()
			}
		}

		// Close any trailing event that was not terminated by a blank line
		// before the stream ended, so its content (or held terminal) is not
		// lost.
		if evName != "" || len(evData) > 0 {
			if !closeEvent() {
				record()
				return finish()
			}
		}
		// End of stream. If a terminal event is still held and no real content
		// was produced, discard it and let the engine retry the upstream.
		// Otherwise flush it to the client.
		if bufferForRetry && terminalEventActive && !contentProduced {
			discardTerminal()
			if err := scanner.Err(); err != nil {
				if ctx.decode != nil {
					ctx.decode.failed = true
				}
				log.Printf("Stream ended: %v", err)
			}
			record()
			// finish() returns true when bufferForRetry && !contentProduced,
			// which signals HandleProxy to retry the upstream.
			return finish()
		}
		flushTerminal()
		if err := scanner.Err(); err != nil {
			if ctx.decode != nil {
				ctx.decode.failed = true
			}
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
	// holdTerminal retains the bytes intended for the client. In transform
	// mode these must be the transformed bytes: holding the raw upstream
	// terminal would leak a different protocol completion marker to an
	// Anthropic client, which then misses the required message_delta stop reason.
	holdTerminal := func(clientBytes []byte) {
		// A completion can span multiple events. In Anthropic streams,
		// message_delta carries stop_reason and message_stop follows it;
		// replacing the buffer here would silently lose the stop reason.
		heldTerminalBytes = append(heldTerminalBytes, clientBytes...)
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
		ctx.observeDecodeEvent(chunk, time.Now())

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
		// Classify the raw downstream event before looking at transformed bytes:
		// translators can legitimately produce no output for an input lifecycle
		// event, but retry-on-empty must still retain its own terminal state.
		eventHasRealContent := bufferForRetry && IsStreamContentLine("data: "+rawData, streamFormat)
		isTerminal := bufferForRetry && isTerminalEvent(rawData, streamFormat)

		// Safety guard: skip empty data to avoid sending an empty SSE event that
		// could confuse downstream event parsers (e.g. Anthropic SDK). The
		// terminal classification above ensures an intentionally dropped terminal does not bypass
		// retry bookkeeping.
		if len(chunk.Data) == 0 {
			if eventHasRealContent {
				contentProduced = true
				flushHeldTerminal()
			}
			return true
		}

		// If the transformer output contains SSE event boundaries (\n\n), it is
		// already formatted SSE — write it directly without wrapping in data:.
		if strings.Contains(string(chunk.Data), "\n\n") {
			if isTerminal {
				holdTerminal(chunk.Data)
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
			holdTerminal(out.Bytes())
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
			if ctx.decode != nil {
				ctx.decode.failed = true
			}
			log.Printf("Stream ended: %v", err)
		}
		record()
		return finish()
	}
	flushHeldTerminal()

	if err := scanner.Err(); err != nil {
		if ctx.decode != nil {
			ctx.decode.failed = true
		}
		log.Printf("Stream ended: %v", err)
	}
	record()
	return finish()
}
