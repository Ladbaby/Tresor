package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (externalStream) PreservesFormat() bool { return true }

type externalStream struct {
	p        *ExternalPlugin
	upstream bool
}

// The old interface remains an internal container only. Mixed execution uses
// event lists below rather than squeezing many logical events into one chunk.
func (t externalStream) TransformStreamChunk(chunk SSEChunk, ctx *PipelineContext) (SSEChunk, error) {
	return chunk, fmt.Errorf("external stream must use event-list execution")
}
func eventInput(event SSEChunk) map[string]any {
	var id, retry any
	if event.ID != nil {
		id = *event.ID
	}
	if event.Retry != nil {
		retry = *event.Retry
	}
	return map[string]any{"type": event.EventType, "data": string(event.Data), "id": id, "retry": retry}
}
func eventResult(result map[string]any, fallback []SSEChunk) ([]SSEChunk, error) {
	return eventResultLimit(result, fallback, externalValueLimit)
}
func eventResultLimit(result map[string]any, fallback []SSEChunk, limit int) ([]SSEChunk, error) {
	if result == nil {
		if err := boundEventList(fallback, limit); err != nil {
			return nil, err
		}
		return fallback, nil
	}
	if err := validKeys(result, "events"); err != nil {
		return nil, err
	}
	raw, ok := result["events"].([]any)
	if !ok || len(raw) > externalEventLimit {
		return nil, fmt.Errorf("invalid event array")
	}
	events := make([]SSEChunk, 0, len(raw))
	total := 0
	for _, value := range raw {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid event")
		}
		if err := validKeys(m, "type", "data", "id", "retry"); err != nil {
			return nil, err
		}
		typ, ok := m["type"].(string)
		if !ok || strings.ContainsAny(typ, "\r\n") {
			return nil, fmt.Errorf("invalid event type")
		}
		data, ok := m["data"].(string)
		if !ok {
			return nil, fmt.Errorf("invalid event data")
		}
		event := SSEChunk{EventType: typ, Data: []byte(data)}
		if value := m["id"]; value != nil {
			str, ok := value.(string)
			if !ok || strings.ContainsAny(str, "\x00\r\n") {
				return nil, fmt.Errorf("invalid event id")
			}
			event.ID = &str
		}
		if value := m["retry"]; value != nil {
			number, ok := value.(float64)
			if !ok || number < 0 || number > 2147483647 || number != float64(int(number)) {
				return nil, fmt.Errorf("invalid event retry")
			}
			n := int(number)
			event.Retry = &n
		}
		total += serializedEventSize(event)
		if total > limit {
			return nil, contractError("resource_limit", "event_output")
		}
		events = append(events, event)
	}
	return events, nil
}
func boundEventList(events []SSEChunk, limit int) error {
	if len(events) > externalEventLimit {
		return contractError("resource_limit", "event_amplification")
	}
	total := 0
	for _, event := range events {
		total += serializedEventSize(event)
		if total > limit {
			return contractError("resource_limit", "event_output")
		}
	}
	return nil
}
func effectiveEventLimit(ctx *PipelineContext) int {
	if ctx.execution().Large {
		return externalCollectionLimit
	}
	return externalValueLimit
}

// Exact size of serializeEvents' output without allocating the wire copy.
func serializedEventSize(event SSEChunk) int {
	n := len(event.Data) + 6*bytes.Count(event.Data, []byte{'\n'}) + 8
	if event.EventType != "" {
		n += len(event.EventType) + 8
	}
	if event.ID != nil {
		n += len(*event.ID) + 5
	}
	if event.Retry != nil {
		n += len(strconv.Itoa(*event.Retry)) + 8
	}
	return n
}
func serializeEvents(events []SSEChunk) []byte {
	var out bytes.Buffer
	for _, event := range events {
		if event.EventType != "" {
			fmt.Fprintf(&out, "event: %s\n", event.EventType)
		}
		if event.ID != nil {
			fmt.Fprintf(&out, "id: %s\n", *event.ID)
		}
		if event.Retry != nil {
			fmt.Fprintf(&out, "retry: %d\n", *event.Retry)
		}
		for _, line := range strings.Split(string(event.Data), "\n") {
			fmt.Fprintf(&out, "data: %s\n", line)
		}
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// scanEvents parses complete logical SSE events, bounded including multiline
// data. Comments remain transport-only and do not fabricate plugin events.
func scanEvents(reader io.Reader, emit func(SSEChunk) error) error {
	return scanEventsMode(reader, emit, externalValueLimit, false)
}

// Collection requires complete event boundaries; ordinary streaming retains its
// historical EOF flush. The stream-wide bound is enforced by its reader.
func scanEventsMode(reader io.Reader, emit func(SSEChunk) error, limit int, strict bool) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), limit+1)
	pending := false
	var event SSEChunk
	var data []string
	total := 0
	hasData := false
	flush := func() error {
		if hasData {
			event.Data = []byte(strings.Join(data, "\n"))
			if err := emit(event); err != nil {
				return err
			}
		}
		event = SSEChunk{}
		data = nil
		total = 0
		hasData = false
		pending = false
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > limit {
			return contractError("resource_limit", "event_input")
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, has := strings.Cut(line, ":")
		if !has {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			pending = true
			event.EventType = value
		case "data":
			pending = true
			data = append(data, value)
			hasData = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				copy := value
				event.ID = &copy
			}
		case "retry":
			if n, err := strconv.Atoi(value); err == nil && n >= 0 {
				event.Retry = &n
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if strict && pending {
		return contractError("truncated_event", "incomplete_boundary")
	}
	return flush()
}
func nativeEvents(event SSEChunk) []SSEChunk {
	if len(event.Data) == 0 {
		return nil
	}
	if strings.Contains(string(event.Data), "\n\n") && (strings.HasPrefix(string(event.Data), "event:") || strings.HasPrefix(string(event.Data), "data:")) {
		var events []SSEChunk
		if scanEvents(bytes.NewReader(event.Data), func(event SSEChunk) error { events = append(events, event); return nil }) == nil {
			return events
		}
	}
	return []SSEChunk{event}
}
func executeEventList(events []SSEChunk, ctx *PipelineContext, steps []StreamResponseTransformer) ([]SSEChunk, error) {
	var retainedBytes int64
	for _, step := range steps {
		retainedBytes = 0
		var next []SSEChunk
		for _, event := range events {
			var emitted []SSEChunk
			if external, ok := step.(externalStream); ok {
				name := "streamEvent"
				ctx.execution().Phase = "events"
				ctx.execution().Stage = "response_pipeline"
				if external.upstream {
					name = "upstreamEvent"
					ctx.execution().Stage = "upstream_events"
				}
				input := external.p.input(ctx)
				input["event"] = eventInput(event)
				result, err := external.p.call(name, input)
				if err != nil {
					return nil, err
				}
				emitted, err = eventResultLimit(result, []SSEChunk{event}, effectiveEventLimit(ctx))
				if err != nil {
					return nil, external.p.invocationFailure(name, err)
				}
			} else {
				out, err := step.TransformStreamChunk(event, ctx)
				if err != nil {
					// Keep legacy Go log-and-skip; JS failures above abort.
					log.Printf("stream transform error: %v", err)
					continue
				}
				emitted = nativeEvents(out)
			}
			for _, out := range emitted {
				if se, ok := ParseStreamError(out.EventType, out.Data); ok {
					return nil, collectionUpstreamError{status: se.Status, message: se.Message}
				}
			}
			for _, out := range emitted {
				retainedBytes += int64(serializedEventSize(out))
				if retainedBytes > int64(effectiveEventLimit(ctx)) {
					return nil, fmt.Errorf("composed event bytes exceed limit")
				}
			}
			next = append(next, emitted...)
			if len(next) > externalEventLimit {
				return nil, fmt.Errorf("composed event amplification exceeds limit")
			}
		}
		advanceFormat(ctx, step, false)
		events = next
	}
	if err := boundEventList(events, effectiveEventLimit(ctx)); err != nil {
		return nil, err
	}
	return events, nil
}
func (p *Pipeline) streamLifecycle(name, reason string, resp *http.Response, ctx *PipelineContext) ([]SSEChunk, error) {
	return eventLifecycle(p.StreamResponseSteps, name, reason, resp, ctx)
}
func eventLifecycle(steps []StreamResponseTransformer, name, reason string, resp *http.Response, ctx *PipelineContext) ([]SSEChunk, error) {
	var all []SSEChunk
	var allBytes int
	var first error
	baseFormat := ctx.execution().CurrentFormat
	for index, step := range steps {
		ctx.execution().CurrentFormat = baseFormat
		for _, previous := range steps[:index] {
			advanceFormat(ctx, previous, false)
		}
		external, ok := step.(externalStream)
		if !ok {
			continue
		}
		session := &external.p.stream
		ctx.execution().Phase, ctx.execution().Stage = "events", "response_pipeline"
		if external.upstream {
			session = &external.p.upstream
			ctx.execution().Stage = "upstream_events"
		}
		isStart := name == "streamStart" || name == "upstreamStart"
		if isStart {
			if !session.Start() {
				continue
			}
		} else {
			if !session.End() {
				continue
			}
		}
		input := external.p.input(ctx)
		if isStart {
			input = external.p.metadata(ctx, resp)
		} else {
			input["end"] = ctx.termination(reason)
		}
		result, err := external.p.call(name, input)
		if err != nil {
			if reason != "" && reason != "eof" {
				if first == nil {
					first = err
				}
				continue
			}
			return nil, err
		}
		events, err := eventResultLimit(result, nil, effectiveEventLimit(ctx))
		if err != nil {
			err = external.p.invocationFailure(name, err)
			if reason != "" && reason != "eof" {
				if first == nil {
					first = err
				}
				continue
			}
			return nil, err
		}
		if reason != "" && reason != "eof" && len(events) > 0 {
			cleanupErr := external.p.invocationFailure(name, fmt.Errorf("error/cancel cleanup cannot emit events"))
			if first == nil {
				first = cleanupErr
			}
			continue
		}
		for _, event := range events {
			if se, ok := ParseStreamError(event.EventType, event.Data); ok {
				return nil, collectionUpstreamError{status: se.Status, message: se.Message}
			}
		}
		events, err = executeEventList(events, ctx, steps[index+1:])
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			allBytes += serializedEventSize(event)
		}
		if allBytes > effectiveEventLimit(ctx) {
			return nil, contractError("resource_limit", "lifecycle_output")
		}
		all = append(all, events...)
		if len(all) > externalEventLimit {
			return nil, fmt.Errorf("lifecycle event amplification exceeds limit")
		}
	}
	return all, first
}

type collectionLimitReader struct {
	io.ReadCloser
	remaining  int
	statistics *Statistics
}

func (r *collectionLimitReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, contractError("resource_limit", "collection_input")
		}
		return 0, err
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= n
	if r.statistics != nil {
		r.statistics.RawBytes += int64(n)
	}
	return n, err
}

type collectionUpstreamError struct {
	status  int
	message string
}

func (e collectionUpstreamError) Error() string       { return e.message }
func (e collectionUpstreamError) UpstreamStatus() int { return e.status }

func collectExternal(resp *http.Response, ctx *PipelineContext) ([]byte, error) {
	return collectExternalPipeline(resp, ctx, &Pipeline{})
}
func collectExternalPipeline(resp *http.Response, ctx *PipelineContext, pipeline *Pipeline) (body []byte, err error) {
	owner := ctx.ExternalCollector
	e := ctx.execution()
	e.Large = true
	e.Delivery = "collect"
	e.Phase = "events"
	e.Stage = "collection"
	e.CurrentFormat = ctx.DownstreamFormat
	owner.collection.Start()
	defer func() {
		if err != nil && e.Failure == nil {
			e.Failure = err
			e.FailureStage = e.Stage
		}
		if err != nil {
			reason := "error"
			if owner.request.Context().Err() != nil {
				reason = "cancelled"
			}
			_, _ = eventLifecycle(pipeline.UpstreamSteps, "upstreamEnd", reason, resp, ctx)
			if owner.collection.End() {
				e.Stage = "collection"
				in := owner.input(ctx)
				in["end"] = ctx.termination(reason)
				cleanupResult, cleanupErr := owner.call("collectEnd", in)
				if cleanupErr == nil && cleanupResult != nil {
					owner.invocationFailure("collectEnd", fmt.Errorf("error/cancel cleanup cannot emit output"))
				}
			}
		}
	}()
	result, err := owner.call("collectStart", owner.metadata(ctx, resp))
	if err != nil {
		return nil, err
	}
	if result != nil {
		return nil, fmt.Errorf("collectStart cannot emit output")
	}
	feed := func(events []SSEChunk) error {
		e.Stage = "collection"
		e.CurrentFormat = ctx.DownstreamFormat
		for _, event := range events {
			if se, ok := ParseStreamError(event.EventType, event.Data); ok {
				return collectionUpstreamError{status: se.Status, message: se.Message}
			}
			e.Statistics.OutputBytes += int64(serializedEventSize(event))
			e.Statistics.OutputEvents++
			if e.Statistics.OutputBytes > externalCollectionLimit {
				return contractError("resource_limit", "collection_output_events")
			}
			in := owner.input(ctx)
			in["event"] = eventInput(event)
			out, callErr := owner.call("collectEvent", in)
			if callErr != nil {
				return callErr
			}
			if out != nil {
				return fmt.Errorf("collectEvent cannot emit output")
			}
		}
		return nil
	}
	initial, err := eventLifecycle(pipeline.UpstreamSteps, "upstreamStart", "", resp, ctx)
	if err != nil {
		return nil, err
	}
	if err = feed(initial); err != nil {
		return nil, err
	}
	resp.Body = &collectionLimitReader{ReadCloser: resp.Body, remaining: externalCollectionLimit, statistics: &e.Statistics}
	err = scanEventsMode(resp.Body, func(event SSEChunk) error {
		ctx.observeDecodeEvent(event, time.Now())
		e.Statistics.RawEvents++
		if IsStreamContentLine("data: "+string(event.Data), ctx.DownstreamFormat) {
			e.RawContent = true
		}
		if se, ok := ParseStreamError(event.EventType, event.Data); ok {
			return collectionUpstreamError{status: se.Status, message: se.Message}
		}
		if err := owner.request.Context().Err(); err != nil {
			return err
		}
		events, err := executeEventList([]SSEChunk{event}, ctx, pipeline.UpstreamSteps)
		if err != nil {
			return err
		}
		return feed(events)
	}, externalCollectionLimit, true)
	if err != nil {
		return nil, err
	}
	tail, err := eventLifecycle(pipeline.UpstreamSteps, "upstreamEnd", "eof", resp, ctx)
	if err != nil {
		return nil, err
	}
	if err = feed(tail); err != nil {
		return nil, err
	}
	owner.collection.End()
	e.Stage = "collection"
	in := owner.input(ctx)
	in["end"] = ctx.termination("eof")
	result, err = owner.call("collectEnd", in)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("collectEnd must return body")
	}
	if _, ok := result["body"]; !ok {
		return nil, fmt.Errorf("collectEnd must return body")
	}
	body, err = applyResponseLimit(result, resp, nil, true, externalCollectionLimit, true)
	if err == nil {
		e.Statistics.CollectedBytes = int64(len(body))
	}
	return body, err
}
