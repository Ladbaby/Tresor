package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tresor/internal/inspect"
)

type responseReadFailure struct{ closed bool }

func (*responseReadFailure) Read(p []byte) (int, error) {
	copy(p, "partial")
	return len("partial"), errors.New("mock read failure")
}
func (b *responseReadFailure) Close() error { b.closed = true; return nil }
func TestBufferedResponseReadFailureClosesAndCancels(t *testing.T) {
	eng := &Engine{}
	body := &responseReadFailure{}
	resp := &http.Response{StatusCode: 429, Header: http.Header{}, Body: body}
	cancelled := false
	_, ge := eng.readBufferedResponse(resp, &PipelineContext{}, &Pipeline{}, func() { cancelled = true }, context.Background(), false, "")
	if ge == nil || ge.status != 502 || !strings.Contains(ge.httpMsg, "(429): partial") || !body.closed || !cancelled {
		t.Fatalf("error=%+v closed=%v cancelled=%v", ge, body.closed, cancelled)
	}
}

func TestBufferedResponseHeaderFiltering(t *testing.T) {
	resp := &http.Response{StatusCode: 201, Header: http.Header{"Content-Length": []string{"999"}, "Transfer-Encoding": []string{"chunked"}, "Content-Encoding": []string{"gzip"}, "X-Keep": []string{"yes"}}, Body: io.NopCloser(strings.NewReader(""))}
	w := httptest.NewRecorder()
	prepareBufferedHeaders(w, resp, []byte("hello"))
	writeBufferedResponse(w, resp, []byte("hello"))
	if w.Code != 201 || w.Header().Get("Content-Length") != "5" || w.Header().Get("Content-Encoding") != "" || w.Header().Get("Transfer-Encoding") != "" || w.Header().Get("X-Keep") != "yes" || w.Body.String() != "hello" {
		t.Fatalf("response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
}

func TestStreamCaptureTruncationKeepsNewline(t *testing.T) {
	for _, size := range []int{inspect.MaxBodyBytes - 1, inspect.MaxBodyBytes, inspect.MaxBodyBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := newTestStore(t)
			wire := ":prefix\n:" + strings.Repeat("x", size-len(":prefix\n:")-1) + "\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte(wire))
			}))
			defer server.Close()
			addDownstream(t, s, "stream-ds", "stream-ds", server.URL, "test-key", "openai")
			addOutputModelIDs(t, s, "stream-ds", "stream-model")
			eng := New(s)
			defer eng.Stop()
			capture := inspect.New(10)
			eng.SetPayloadStore(capture)
			w := httptest.NewRecorder()
			eng.HandleProxy(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"stream-model","stream":true}`)))
			entries := eng.GetLogger().RecentEntries(1)
			if len(entries) != 1 {
				t.Fatal("missing log")
			}
			entry, ok := capture.Get(entries[0].ID)
			if !ok {
				t.Fatal("missing capture")
			}
			want := []byte(wire)
			if size > inspect.MaxBodyBytes {
				want = append([]byte(wire[:inspect.MaxBodyBytes-1]), '\n')
			}
			if !bytes.Equal(entry.ResponseBody, want) || entry.TruncatedResponse != (size > inspect.MaxBodyBytes) {
				t.Fatalf("capture len=%d truncated=%v want len=%d", len(entry.ResponseBody), entry.TruncatedResponse, len(want))
			}
		})
	}
}

func TestBufferedResponseCaptureBoundaries(t *testing.T) {
	for _, size := range []int{inspect.MaxBodyBytes - 1, inspect.MaxBodyBytes, inspect.MaxBodyBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := newTestStore(t)
			wire := strings.Repeat("x", size)
			server := newTestDownstream(t, 200, wire, nil)
			defer server.Close()
			addDownstream(t, s, "buffer-ds", "buffer-ds", server.URL, "test-key")
			addOutputModelIDs(t, s, "buffer-ds", "buffer-model")
			eng := New(s)
			defer eng.Stop()
			eng.SetRegistry(&mockRegistryImpl{})
			capture := inspect.New(10)
			eng.SetPayloadStore(capture)
			w := httptest.NewRecorder()
			eng.HandleProxy(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"buffer-model"}`)))
			entries := eng.GetLogger().RecentEntries(1)
			if len(entries) != 1 {
				t.Fatal("missing log")
			}
			entry, ok := capture.Get(entries[0].ID)
			if !ok {
				t.Fatal("missing capture")
			}
			wantLen := size
			if wantLen > inspect.MaxBodyBytes {
				wantLen = inspect.MaxBodyBytes
			}
			if string(entry.ResponseBody) != wire[:wantLen] || entry.TruncatedResponse != (size > inspect.MaxBodyBytes) || w.Body.String() != wire {
				t.Fatalf("capture len=%d truncated=%v output len=%d", len(entry.ResponseBody), entry.TruncatedResponse, w.Body.Len())
			}
		})
	}
}

func TestRewriteModelInBody(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		outputModel string
		wantModel   string
	}{
		{
			name:        "Replace model",
			body:        `{"model": "gpt-4o", "messages": [{"role": "user", "content": "hi"}]}`,
			outputModel: "claude-sonnet",
			wantModel:   "claude-sonnet",
		},
		{
			name:        "Non-JSON body returned unchanged",
			body:        `not json`,
			outputModel: "new-model",
			wantModel:   "", // can't parse, body returned as-is
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := rewriteModelInBody([]byte(tt.body), tt.outputModel)

			if tt.wantModel == "" && string(result) == tt.body {
				return // Non-JSON case: body returned unchanged
			}

			var parsed map[string]interface{}
			if err := json.Unmarshal(result, &parsed); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			gotModel, ok := parsed["model"].(string)
			if !ok || gotModel != tt.wantModel {
				t.Fatalf("expected model %q, got %q", tt.wantModel, gotModel)
			}
		})
	}
}
