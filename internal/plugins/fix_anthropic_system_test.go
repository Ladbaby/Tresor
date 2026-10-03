package plugins

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"tresor/internal/engine"
)

func TestFixAnthropicSystem(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"cowork", `{"system":[{"type":"text","text":"SDK instructions","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"Powerpoint?"}]},{"role":"system","content":"# Environment"}],"tools":[{"name":"test"}],"big":9007199254740993}`, "SDK instructions\n\n# Environment"},
		{"string", `{"system":"initial","messages":[{"role":"system","content":"first"},{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}]}`, "initial\n\nfirst\n\nsecond"},
		{"absent", `{"messages":[{"role":"user","content":"hi"},{"role":"system","content":"environment"}]}`, "environment"},
		{"null", `{"system":null,"messages":[{"role":"system","content":"environment"}]}`, "environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &FixAnthropicSystem{}
			req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(tc.body))
			newReq, out, err := p.TransformRequest(req, []byte(tc.body), &engine.PipelineContext{})
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				System   []struct{ Text string }
				Messages []struct{ Role string }
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			text := ""
			for _, block := range got.System {
				text += block.Text
			}
			if text != tc.want {
				t.Fatalf("system = %q, want %q", text, tc.want)
			}
			for _, msg := range got.Messages {
				if msg.Role == "system" {
					t.Fatal("system role remains")
				}
			}
			if newReq == req || newReq.ContentLength != int64(len(out)) {
				t.Fatal("request not updated")
			}
			body, _ := io.ReadAll(newReq.Body)
			replay, _ := newReq.GetBody()
			defer replay.Close()
			replayBody, _ := io.ReadAll(replay)
			if !bytes.Equal(body, out) || !bytes.Equal(replayBody, out) {
				t.Fatal("body mismatch")
			}
			_, twice, _ := p.TransformRequest(newReq, out, nil)
			if !bytes.Equal(out, twice) {
				t.Fatal("not idempotent")
			}
			if tc.name == "cowork" {
				for _, value := range []string{`9007199254740993`, `"ttl":"1h"`, `"tools":[{"name":"test"}]`, `"role":"user"`} {
					if !bytes.Contains(out, []byte(value)) {
						t.Fatalf("lost %s: %s", value, out)
					}
				}
			}
		})
	}
}

func TestFixAnthropicSystemNoOp(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"messages":[{"role":"system","content":"hi"}]}`},
		{"/v1/messages", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/messages", `not json`},
		{"/v1/messages", `{"messages":[{"role":"system","content":null}]}`},
		{"/v1/messages", `{"messages":[{"role":"system","content":[{"type":"image"}]}]}`},
		{"/v1/messages", `{"system":42,"messages":[{"role":"system","content":"hi"}]}`},
		{"/v1/messages", `{"messages":[{"role":"system","content":"valid"},{"role":"system","content":42}]}`},
	} {
		req := httptest.NewRequest("POST", tc.path, nil)
		gotReq, out, err := (&FixAnthropicSystem{}).TransformRequest(req, []byte(tc.body), nil)
		if err != nil || gotReq != req || string(out) != tc.body {
			t.Fatalf("changed unsupported request: %s", tc.body)
		}
	}
}
