package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
)

var errManagedProjection = &ContractError{Category: "managed_projection", Code: "projection_failed"}
var errManagedProjectionMode = &ContractError{Category: "managed_projection", Code: "replay_mode_mismatch"}

type managedProjectionEnvelope struct {
	Version   int             `json:"managed_projection_version"`
	JSON      json.RawMessage `json:"json"`
	Wire      []byte          `json:"wire"`
	Streaming bool            `json:"streaming"`
	Headers   http.Header     `json:"headers"`
}

// applyManagedProjection transforms only the public Chat projection. The request
// supplied by the caller must already be sanitized of host-owned state.
func applyManagedProjection(pipeline *Pipeline, ctx *PipelineContext, req *http.Request, response []byte, streaming bool) (visible ChatMessage, replay []byte, err error) {
	defer func() {
		if err != nil {
			visible = nil
			replay = nil
			err = errManagedProjection
		}
	}()
	if pipeline == nil || ctx == nil || req == nil || len(response) > externalCollectionLimit {
		return nil, nil, errManagedProjection
	}
	root, err := ValidateOneChoiceResponse(response)
	if err != nil {
		return nil, nil, err
	}
	original, err := managedResponseMessage(root)
	if err != nil {
		return nil, nil, err
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req}
	resp.Header.Set("Content-Type", "application/json")
	for _, step := range pipeline.ExternalSteps {
		step.request = req
	}
	ctx.execution().CurrentFormat = "openai"
	ctx.execution().Delivery = "buffered"
	if streaming {
		ctx.execution().Delivery = "streaming"
	}
	body := append([]byte(nil), response...)
	var streamedWire []byte
	body, err = ExecuteResponsePipeline(resp, body, ctx, pipeline.ResponseSteps)
	if err != nil {
		return nil, nil, err
	}
	if len(body) > externalCollectionLimit {
		return nil, nil, errManagedProjection
	}
	if err = pipeline.responseHeaders(resp, ctx); err != nil {
		return nil, nil, err
	}
	if streaming {
		resp.Header.Set("Content-Type", "text/event-stream")
		ctx.execution().Stage = "stream_pipeline"
		var out bytes.Buffer
		started, e := pipeline.streamLifecycle("streamStart", "", resp, ctx)
		if e != nil {
			return nil, nil, e
		}
		for _, ev := range started {
			if e = writeManagedEvent(&out, ev); e != nil {
				return nil, nil, e
			}
		}
		wireResponse := httptest.NewRecorder()
		if e := writeManagedProjection(wireResponse, body, true); e != nil {
			return nil, nil, e
		}
		if e = scanEvents(bytes.NewReader(wireResponse.Body.Bytes()), func(ev SSEChunk) error {
			events, x := executeEventList([]SSEChunk{ev}, ctx, pipeline.StreamResponseSteps)
			if x != nil {
				return x
			}
			for _, v := range events {
				if x = writeManagedEvent(&out, v); x != nil {
					return x
				}
			}
			return nil
		}); e != nil {
			return nil, nil, e
		}
		ended, e := pipeline.streamLifecycle("streamEnd", "eof", resp, ctx)
		if e != nil {
			return nil, nil, e
		}
		for _, ev := range ended {
			if e = writeManagedEvent(&out, ev); e != nil {
				return nil, nil, e
			}
		}
		if out.Len() > externalCollectionLimit {
			return nil, nil, errManagedProjection
		}
		streamedWire = append([]byte(nil), out.Bytes()...)
		body, err = collectManagedSSE(bytes.NewReader(streamedWire), nil, nil)
		if err != nil {
			return nil, nil, err
		}
	}
	final, err := ValidateOneChoiceResponse(body)
	if err != nil {
		return nil, nil, err
	}
	visible, err = managedResponseMessage(final)
	if err != nil {
		return nil, nil, err
	}
	if _, err = ParseToolCalls(visible, nil); err != nil {
		return nil, nil, err
	}
	var beforeChoices, afterChoices []map[string]json.RawMessage
	_ = json.Unmarshal(root["choices"], &beforeChoices)
	_ = json.Unmarshal(final["choices"], &afterChoices)
	if !semanticEqual(beforeChoices[0]["finish_reason"], afterChoices[0]["finish_reason"]) {
		return nil, nil, errManagedProjection
	}
	if err = validateManagedProjection(original, visible); err != nil {
		return nil, nil, err
	}
	wire := append([]byte(nil), body...)
	if streaming {
		wire = streamedWire
	}
	env := managedProjectionEnvelope{Version: 1, JSON: json.RawMessage(body), Wire: wire, Streaming: streaming, Headers: resp.Header.Clone()}
	replay, err = json.Marshal(env)
	if err != nil {
		return nil, nil, errManagedProjection
	}
	return visible, replay, nil
}

func managedResponseMessage(root map[string]json.RawMessage) (ChatMessage, error) {
	var choices []json.RawMessage
	if json.Unmarshal(root["choices"], &choices) != nil || len(choices) != 1 {
		return nil, errManagedProjection
	}
	var c map[string]json.RawMessage
	_ = json.Unmarshal(choices[0], &c)
	var m ChatMessage
	if json.Unmarshal(c["message"], &m) != nil {
		return nil, errManagedProjection
	}
	return m, nil
}
func validateManagedProjection(before, after ChatMessage) error {
	if string(after["role"]) != string(before["role"]) {
		return errManagedProjection
	}
	var a, b []json.RawMessage
	_ = json.Unmarshal(before["tool_calls"], &a)
	_ = json.Unmarshal(after["tool_calls"], &b)
	if len(a) != len(b) {
		return errManagedProjection
	}
	for i := range a {
		var x, y struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(a[i], &x) != nil || json.Unmarshal(b[i], &y) != nil || x.ID != y.ID || x.Type != y.Type || x.Function.Name != y.Function.Name {
			return errManagedProjection
		}
		if !semanticEqual([]byte(x.Function.Arguments), []byte(y.Function.Arguments)) {
			return errManagedProjection
		}
	}
	return nil
}
func managedSSE(data []byte) []byte { return []byte(fmt.Sprintf("data: %s\n\ndata: [DONE]\n\n", data)) }
func writeManagedEvent(w io.Writer, e SSEChunk) error {
	if buffer, ok := w.(*bytes.Buffer); ok && buffer.Len()+len(e.Data)+len(e.EventType)+32 > externalCollectionLimit {
		return errManagedProjection
	}
	if len(e.Data) == 0 {
		return nil
	}
	if e.EventType != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", e.EventType); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", e.Data); err != nil {
		return err
	}
	return nil
}

// storedCanonical returns the canonical Chat JSON held by a stored projection,
// unwrapping the versioned envelope when present. It lets a live stream render
// the durable turn instead of replaying bytes recorded for a different format.
func storedCanonical(stored []byte) []byte {
	var probe map[string]json.RawMessage
	if json.Unmarshal(stored, &probe) != nil {
		return stored
	}
	if len(probe["managed_projection_version"]) == 0 {
		return stored
	}
	var env managedProjectionEnvelope
	if json.Unmarshal(stored, &env) != nil || env.Version != 1 || len(env.JSON) == 0 {
		return stored
	}
	return env.JSON
}

// writeStoredManagedProjection replays a versioned exact wire image; legacy Chat
// JSON is delegated to the historical writer. It never executes plugin hooks.
//
// The stored wire is the canonical Chat rendering, so it is replayed verbatim
// only for a Chat client. Any other format renders the stored canonical turn
// through its own codec, which keeps the durable turn authoritative while the
// wire matches what the client asked for.
func writeStoredManagedProjection(w http.ResponseWriter, stored []byte, streaming bool, renderer StreamRenderer, started bool) error {
	var probe map[string]json.RawMessage
	if json.Unmarshal(stored, &probe) != nil {
		return errManagedProjection
	}
	if len(probe["managed_projection_version"]) == 0 {
		if _, isChat := renderer.(chatRenderer); isChat || renderer == nil {
			return writeManagedProjection(w, stored, streaming)
		}
		return renderManagedProjection(w, stored, streaming, renderer, started)
	}
	var env managedProjectionEnvelope
	if json.Unmarshal(stored, &env) != nil || env.Version != 1 || len(env.JSON) == 0 || len(env.Wire) == 0 {
		return errManagedProjection
	}
	if env.Streaming != streaming {
		return errManagedProjectionMode
	}
	var compact bytes.Buffer
	if json.Compact(&compact, env.JSON) != nil {
		return errManagedProjection
	}
	if _, isChat := renderer.(chatRenderer); isChat || renderer == nil {
		w.Header().Set("Content-Type", map[bool]string{true: "text/event-stream", false: "application/json"}[streaming])
		for k, vs := range env.Headers {
			w.Header()[k] = append([]string(nil), vs...)
		}
		_, err := w.Write(env.Wire)
		return err
	}
	return renderManagedProjection(w, env.JSON, streaming, renderer, started)
}

// renderManagedProjection writes a stored canonical Chat response through the
// client's codec, so a replayed turn is delivered in the format it was
// requested in.
func renderManagedProjection(w http.ResponseWriter, canonical []byte, streaming bool, renderer StreamRenderer, started bool) error {
	var probe map[string]json.RawMessage
	if json.Unmarshal(canonical, &probe) != nil {
		return errManagedProjection
	}
	// The stored projection may itself be an envelope from an older write.
	if envJSON, ok := probe["json"]; ok && len(probe["managed_projection_version"]) > 0 {
		return renderManagedProjection(w, envJSON, streaming, renderer, started)
	}
	root, err := ValidateOneChoiceResponse(canonical)
	if err != nil {
		return errManagedProjection
	}
	message, err := managedResponseMessage(root)
	if err != nil {
		return errManagedProjection
	}
	var text string
	if raw, ok := message["content"]; ok {
		_ = json.Unmarshal(raw, &text)
	}
	usage := root["usage"]
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		// A stream already opened by the streamer keeps its single message_start;
		// only a replay that opens its own response writes one.
		if start := renderer.FrameStart(); len(start) > 0 && !started {
			if _, err := w.Write(start); err != nil {
				return err
			}
		}
		if text != "" {
			if _, err := w.Write(renderer.FrameContent(text)); err != nil {
				return err
			}
		}
		// A turn may end by asking the client to run tools. Dropping the calls
		// would leave the client unable to continue the loop.
		if caller, ok := renderer.(streamToolCaller); ok {
			if calls, err := storedToolCalls(message); err == nil && len(calls) > 0 {
				if _, err := w.Write(caller.FrameToolCalls(calls)); err != nil {
					return err
				}
			}
		}
		if _, err := w.Write(renderer.FrameFinish(usage)); err != nil {
			return err
		}
		if _, err := w.Write(renderer.FrameEnd()); err != nil {
			return err
		}
		return nil
	}
	jsonRenderer, ok := renderer.(jsonResponseRenderer)
	if !ok {
		// A renderer that cannot render JSON must fail loudly rather than
		// hand a client another format's bytes.
		return errManagedProjection
	}
	body, err := jsonRenderer.RenderJSON(canonical)
	if err != nil {
		return errManagedProjection
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(body)
	return err
}

// storedToolCalls reads the tool calls a stored canonical turn requested.
func storedToolCalls(message ChatMessage) ([]ToolCall, error) {
	if len(message["tool_calls"]) == 0 {
		return nil, nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(message["tool_calls"], &calls); err != nil {
		return nil, err
	}
	out := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		out = append(out, ToolCall{
			ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments),
		})
	}
	return out, nil
}
