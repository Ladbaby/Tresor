package plugins

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"tresor/internal/engine"
)

func newBreakRepeatRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "http://example.com/", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

func TestBreakRepeat_OpenAI_Trigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	body, _ := json.Marshal(map[string]interface{}{
		"model": "small-model",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "do something"},
			map[string]interface{}{"role": "assistant", "content": "I will do it now."},
			map[string]interface{}{"role": "assistant", "content": "I will do it now."},
			map[string]interface{}{"role": "assistant", "content": "I will do it now."},
		},
	})
	ctx := &engine.PipelineContext{}
	newReq, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, ctx)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if newReq == nil || len(newBody) == len(body) {
		t.Fatalf("expected body to grow, got %d bytes", len(newBody))
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(newBody, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := resp["messages"].([]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})
	if last["role"] != "user" {
		t.Fatalf("expected last role user, got %v", last["role"])
	}
	if last["content"] != repeatReminder {
		t.Fatalf("expected reminder content, got %v", last["content"])
	}
}

func TestBreakRepeat_OpenAI_NoRepeat(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	body, _ := json.Marshal(map[string]interface{}{
		"model": "small-model",
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "content": "one"},
			map[string]interface{}{"role": "assistant", "content": "two"},
			map[string]interface{}{"role": "assistant", "content": "three"},
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !bytes.Equal(newBody, body) {
		t.Fatalf("expected body unchanged:\n%s", newBody)
	}
}

func TestBreakRepeat_OpenAI_FewerThanThree(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	body, _ := json.Marshal(map[string]interface{}{
		"model": "small-model",
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "content": "same"},
			map[string]interface{}{"role": "assistant", "content": "same"},
		},
	})
	_, newBody, _ := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if !bytes.Equal(newBody, body) {
		t.Fatalf("expected body unchanged")
	}
}

func TestBreakRepeat_OpenAI_IgnoresUserTurnsBetween(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	body, _ := json.Marshal(map[string]interface{}{
		"model": "small-model",
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "content": "same"},
			map[string]interface{}{"role": "user", "content": "keep going"},
			map[string]interface{}{"role": "assistant", "content": "same"},
			map[string]interface{}{"role": "user", "content": "keep going"},
			map[string]interface{}{"role": "assistant", "content": "same"},
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(newBody) == len(body) {
		t.Fatalf("expected reminder appended despite user turns between")
	}
	var resp map[string]interface{}
	json.Unmarshal(newBody, &resp)
	msgs := resp["messages"].([]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})
	if last["role"] != "user" || last["content"] != repeatReminder {
		t.Fatalf("unexpected last message: %v", last)
	}
}

func TestBreakRepeat_OpenAIResponses_Trigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	assistant := func() interface{} {
		return map[string]interface{}{
			"role":    "assistant",
			"content": []interface{}{map[string]interface{}{"type": "output_text", "text": "stuck"}},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "gpt-4o",
		"input": []interface{}{
			map[string]interface{}{"role": "user", "content": "go"},
			assistant(),
			assistant(),
			assistant(),
		},
		"stream": false,
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var resp map[string]interface{}
	json.Unmarshal(newBody, &resp)
	items := resp["input"].([]interface{})
	last := items[len(items)-1].(map[string]interface{})
	if last["role"] != "user" || last["content"] != repeatReminder {
		t.Fatalf("unexpected last input item: %v", last)
	}
}

func TestBreakRepeat_OpenAIResponses_StringInput(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	body, _ := json.Marshal(map[string]interface{}{
		"model": "gpt-4o",
		"input": "hello",
	})
	_, newBody, _ := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if !bytes.Equal(newBody, body) {
		t.Fatalf("string input has no assistant turns; expected unchanged")
	}
}

func TestBreakRepeat_Anthropic_Trigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	assistant := func() interface{} {
		return map[string]interface{}{
			"role":    "assistant",
			"content": []interface{}{map[string]interface{}{"type": "text", "text": "again"}},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "claude-x",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "start"}},
			},
			assistant(),
			assistant(),
			assistant(),
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var resp map[string]interface{}
	json.Unmarshal(newBody, &resp)
	msgs := resp["messages"].([]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})
	if last["role"] != "user" {
		t.Fatalf("expected last role user, got %v", last["role"])
	}
	content := last["content"].([]interface{})
	block := content[0].(map[string]interface{})
	if block["type"] != "text" || block["text"] != repeatReminder {
		t.Fatalf("unexpected reminder block: %v", block)
	}
}

func TestBreakRepeat_Gemini_Trigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	model := func() interface{} {
		return map[string]interface{}{
			"role":  "model",
			"parts": []interface{}{map[string]interface{}{"text": "loop"}},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{"role": "user", "parts": []interface{}{map[string]interface{}{"text": "begin"}}},
			model(),
			model(),
			model(),
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var resp map[string]interface{}
	json.Unmarshal(newBody, &resp)
	contents := resp["contents"].([]interface{})
	last := contents[len(contents)-1].(map[string]interface{})
	if last["role"] != "user" {
		t.Fatalf("expected last role user, got %v", last["role"])
	}
	parts := last["parts"].([]interface{})
	if parts[0].(map[string]interface{})["text"] != repeatReminder {
		t.Fatalf("unexpected reminder part")
	}
}

func TestBreakRepeat_DetectRequestFormat(t *testing.T) {
	cases := map[string]string{
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`: "gemini",
		`{"input":[{"role":"user","content":"hi"}]}`:             "openai_responses",
		`{"input":"hi"}`:                                         "openai_responses",
		`{"messages":[{"role":"user","content":"hi"}]}`:          "openai",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`: "anthropic",
		`{"foo":"bar"}`:                                          "",
	}
	for body, want := range cases {
		if got := detectRequestFormat([]byte(body)); got != want {
			t.Errorf("detectRequestFormat(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestBreakRepeat_Passthrough(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	ctx := &engine.PipelineContext{}

	body := []byte(`{"hello":"world"}`)
	out, err := p.TransformResponse(nil, body, ctx)
	if err != nil || !bytes.Equal(out, body) {
		t.Fatalf("TransformResponse should pass through")
	}

	chunk := engine.SSEChunk{Data: []byte(`{"a":1}`)}
	got, err := p.TransformStreamChunk(chunk, ctx)
	if err != nil || !bytes.Equal(got.Data, chunk.Data) {
		t.Fatalf("TransformStreamChunk should pass through")
	}
}

func TestBreakRepeat_UnknownFormatUnchanged(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	body := []byte(`{"unrelated":true}`)
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !bytes.Equal(newBody, body) {
		t.Fatalf("unknown format should be unchanged")
	}
}
