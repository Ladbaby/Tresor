package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"tresor/internal/inspect"
)

type shortInspectionWriter struct{ http.ResponseWriter }

func (w shortInspectionWriter) Write(p []byte) (int, error) { return 2, errors.New("disconnected") }

func TestTaskSummaryUsageDoesNotCountAsAnotherGeneration(t *testing.T) {
	e := New(newTestStore(t))
	captures := inspect.New(10)
	e.SetPayloadStore(captures)
	entry := &RequestLogEntry{deferRecord: true}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	w := newStatusCaptureWriter(httptest.NewRecorder())
	finish := e.beginIntegrationCapture(w, req, &modelResult{}, entry)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("data: {\"tresorbot\":{\"usage_scope\":\"task\",\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":80}}}}\n\ndata: [DONE]\n\n"))
	finish(true)
	e.publishRequest(entry)
	if entry.Usage == nil || *entry.Usage.InputTokens != 100 || *entry.Usage.OutputTokens != 12 || entry.UsageScope != "task" {
		t.Fatalf("missing task usage: %+v", entry)
	}
	if len(e.statsBuf) != 0 {
		t.Fatal("task summary double-counted model stats")
	}
	if captures.Len() != 1 {
		t.Fatal("summary inspection absent")
	}
}

func TestSummaryUsageRequiresExplicitTaskMarker(t *testing.T) {
	for _, body := range []string{`{"usage":{"prompt_tokens":100,"completion_tokens":12}}`, `{"tresorbot":{"usage_scope":"generation","usage":{"prompt_tokens":100,"completion_tokens":12}}}`, `{"tresorbot":{"usage_scope":"task","usage":null}}`, `{"tresorbot":{"usage_scope":"task","usage":`} {
		if usage := taskSummaryUsage([]byte(body), "application/json"); usage != nil {
			t.Fatalf("invalid summary accepted: %s", body)
		}
	}
}

func TestIntegrationWriterRetainsOnlyDeliveredBytes(t *testing.T) {
	raw := &boundedResponseCapture{}
	w := &inspectionWriter{ResponseWriter: shortInspectionWriter{httptest.NewRecorder()}, capture: raw}
	n, err := w.Write([]byte("partial"))
	if n != 2 || err == nil || raw.String() != "pa" {
		t.Fatalf("capture differs from actual write: %d %v %q", n, err, raw.String())
	}
}

func TestIntegrationCaptureRecordsErrorAndPartialOutput(t *testing.T) {
	e := New(newTestStore(t))
	payloads := inspect.New(10)
	e.SetPayloadStore(payloads)
	entry := &RequestLogEntry{deferRecord: true, Status: 502, Error: "integration_unavailable"}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	w := newStatusCaptureWriter(httptest.NewRecorder())
	finish := e.beginIntegrationCapture(w, req, &modelResult{originalBody: []byte(`{"model":"bot/personal"}`)}, entry)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(502)
	w.Write([]byte("data: partial\n\n"))
	finish(true)
	e.publishRequest(entry)
	got, ok := payloads.Get(entry.ID)
	if !ok || got.Status != 502 || string(got.ResponseBody) != "data: partial\n\n" || len(got.Flow) != 1 || got.Flow[0].Error != "integration_unavailable" {
		t.Fatalf("missing partial failed capture: %+v", got)
	}
}

func TestIntegrationProjectionCapturesWireNotReplayEnvelope(t *testing.T) {
	wire := []byte("data: [DONE]\n\n")
	stored, _ := json.Marshal(managedProjectionEnvelope{Version: 1, Streaming: true, Wire: wire})
	c := &integrationCapture{}
	c.projection("replay", 0, stored, true)
	if len(c.events) != 1 || c.events[0].Body != string(wire) || c.events[0].ContentType != "text/event-stream" || !c.events[0].Replayed {
		t.Fatalf("incorrect replay capture: %+v", c.events)
	}
}

func TestManagedUsageIncludesEveryGeneration(t *testing.T) {
	entry := &RequestLogEntry{}
	a, b := int64(10), int64(3)
	usage := &UsageBlock{InputTokens: &a, OutputTokens: &b}
	accumulateManagedUsage(entry, usage)
	accumulateManagedUsage(entry, usage)
	if *entry.Usage.InputTokens != 20 || *entry.Usage.OutputTokens != 6 || a != 10 || b != 3 {
		t.Fatalf("usage accumulation changed inputs or lost round: %+v", entry.Usage)
	}
}

func TestIntegrationTraceBounds(t *testing.T) {
	c := &integrationCapture{}
	c.add(inspect.Event{Kind: "generation_response", Body: strings.Repeat("x", inspect.MaxBodyBytes+1)})
	if !c.truncated || len(c.events) != 1 || !c.events[0].Truncated || c.bytes > inspect.MaxBodyBytes {
		t.Fatalf("unbounded trace: bytes=%d events=%d", c.bytes, len(c.events))
	}
	c.add(inspect.Event{Kind: "tool_call", Body: "omitted"})
	if len(c.events) != 1 {
		t.Fatal("full trace kept allocating")
	}
	c = &integrationCapture{}
	for i := 0; i < maxFlowEvents+10; i++ {
		c.add(inspect.Event{Kind: "tool_call"})
	}
	if len(c.events) != maxFlowEvents || !c.truncated {
		t.Fatal("event count not bounded")
	}
	var disabled *integrationCapture
	disabled.add(inspect.Event{Kind: "ignored"})
}

func TestIntegrationCaptureDeferredPublication(t *testing.T) {
	e := New(newTestStore(t))
	payloads := inspect.New(10)
	e.SetPayloadStore(payloads)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	w := newStatusCaptureWriter(httptest.NewRecorder())
	entry := &RequestLogEntry{deferRecord: true}
	finish := e.beginIntegrationCapture(w, req, &modelResult{originalBody: []byte(`{"model":"bot/personal"}`)}, entry)
	entry.integrationCapture.add(inspect.Event{Kind: "tool_call", Body: `{"input":"hello"}`})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("data: [DONE]\n\n"))
	w.Flush()
	finish(true)
	if entry.integrationCapture != nil {
		t.Fatal("trace not transferred into pending capture")
	}
	if entry.capture == nil || entry.capture.integration == nil {
		t.Fatal("pending capture lost trace ownership")
	}
	if payloads.Len() != 0 {
		t.Fatal("published before lifecycle completion")
	}
	e.publishRequest(entry)
	captured, ok := payloads.Get(entry.ID)
	if !ok || len(captured.Flow) != 1 || string(captured.ResponseBody) != "data: [DONE]\n\n" {
		t.Fatalf("lost deferred capture: %+v", captured)
	}
	if entry.integrationCapture != nil {
		t.Fatal("request retained trace after publication")
	}
}

func TestIntegrationCaptureFallthroughAndDisabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		e := New(newTestStore(t))
		if enabled {
			e.SetPayloadStore(inspect.New(10))
		}
		entry := &RequestLogEntry{deferRecord: true}
		w := newStatusCaptureWriter(httptest.NewRecorder())
		original := w.ResponseWriter
		finish := e.beginIntegrationCapture(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), &modelResult{}, entry)
		if !enabled && entry.integrationCapture != nil {
			t.Fatal("capture disabled but allocated trace")
		}
		finish(false)
		if entry.capture != nil || entry.integrationCapture != nil || w.ResponseWriter != original {
			t.Fatal("fallthrough retained integration capture")
		}
	}
}
