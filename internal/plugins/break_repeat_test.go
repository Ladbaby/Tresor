package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestBreakRepeat_Anthropic_ToolResultFollowedByReminder(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	toolUse := func(id string) interface{} {
		return map[string]interface{}{
			"type":  "tool_use",
			"id":    id,
			"name":  "grep",
			"input": map[string]interface{}{"pattern": "runner"},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "claude-x",
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "content": []interface{}{toolUse("toolu_1")}},
			map[string]interface{}{"role": "user", "content": []interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_1", "content": "No files found"}}},
			map[string]interface{}{"role": "assistant", "content": []interface{}{toolUse("toolu_2")}},
			map[string]interface{}{"role": "user", "content": []interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_2", "content": "No files found"}}},
			map[string]interface{}{"role": "assistant", "content": []interface{}{toolUse("toolu_3")}},
			map[string]interface{}{"role": "user", "content": []interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_3", "content": "No files found"}}},
		},
	})

	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(newBody, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := payload["messages"].([]interface{})
	if len(msgs) != 6 {
		t.Fatalf("expected reminder to be merged with tool result, got %d messages", len(msgs))
	}
	last := msgs[len(msgs)-1].(map[string]interface{})
	if last["role"] != "user" {
		t.Fatalf("expected last role user, got %v", last["role"])
	}
	content := last["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("expected tool result plus reminder, got %v", content)
	}
	reminder := content[1].(map[string]interface{})
	if reminder["type"] != "text" || reminder["text"] != repeatReminder {
		t.Fatalf("unexpected reminder block: %v", reminder)
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

func TestBreakRepeat_OpenAI_DistinctToolCallsNoTrigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	toolCall := func(id, pattern string) map[string]interface{} {
		return map[string]interface{}{
			"id":   id,
			"type": "function",
			"function": map[string]interface{}{
				"name":      "grep",
				"arguments": fmt.Sprintf(`{"pattern":%q,"path":"."}`, pattern),
			},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "small-model",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "find the bugs"},
			map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{toolCall("c1", "foo")}},
			map[string]interface{}{"role": "user", "content": "tool results"},
			map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{toolCall("c2", "bar")}},
			map[string]interface{}{"role": "user", "content": "tool results"},
			map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{toolCall("c3", "baz")}},
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !bytes.Equal(newBody, body) {
		t.Fatalf("distinct tool calls must not be treated as a repeat")
	}
}

func TestBreakRepeat_OpenAI_IgnoredCallIdsTrigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	toolCall := func(id, pattern string) map[string]interface{} {
		return map[string]interface{}{
			"id":   id,
			"type": "function",
			"function": map[string]interface{}{
				"name":      "grep",
				"arguments": fmt.Sprintf(`{"pattern":%q,"path":"."}`, pattern),
			},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "small-model",
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{toolCall("c1", "foo")}},
			map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{toolCall("c2", "foo")}},
			map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{toolCall("c3", "foo")}},
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if bytes.Equal(newBody, body) {
		t.Fatalf("three identical tool calls (only ids differ) should trigger the reminder")
	}
}

func TestBreakRepeat_Anthropic_DistinctToolCallsNoTrigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	toolUse := func(name, input interface{}) interface{} {
		return map[string]interface{}{
			"type":  "tool_use",
			"id":    fmt.Sprintf("toolu_%v", input),
			"name":  name,
			"input": input,
		}
	}
	assistant := func(input interface{}) interface{} {
		return map[string]interface{}{
			"role":    "assistant",
			"content": []interface{}{toolUse("Grep", map[string]interface{}{"pattern": input})},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "claude-x",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "search"}},
			},
			assistant("foo"),
			assistant("bar"),
			assistant("baz"),
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !bytes.Equal(newBody, body) {
		t.Fatalf("distinct Anthropic tool_use blocks must not be treated as a repeat")
	}
}

func TestBreakRepeat_OpenAIResponses_DistinctToolCallsNoTrigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	fnCall := func(name, args string) interface{} {
		return map[string]interface{}{
			"type":      "function_call",
			"call_id":   "call_x",
			"name":      name,
			"arguments": args,
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "gpt-4o",
		"input": []interface{}{
			map[string]interface{}{"role": "user", "content": "search"},
			fnCall("Grep", `{"pattern":"foo"}`),
			fnCall("Grep", `{"pattern":"bar"}`),
			fnCall("Grep", `{"pattern":"baz"}`),
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !bytes.Equal(newBody, body) {
		t.Fatalf("distinct Responses function_calls must not be treated as a repeat")
	}
}

func TestBreakRepeat_Anthropic_DistinctThinkingBlocksNoTrigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	thinkingAssistant := func(thinking string) interface{} {
		return map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{"type": "thinking", "thinking": thinking},
			},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "claude-x",
		"messages": []interface{}{
			thinkingAssistant("hmm, let me think about foo"),
			thinkingAssistant("hmm, let me think about bar"),
			thinkingAssistant("hmm, let me think about baz"),
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !bytes.Equal(newBody, body) {
		t.Fatalf("distinct thinking blocks must not be treated as a repeat")
	}
}

func TestBreakRepeat_OpenAIResponses_DistinctReasoningNoTrigger(t *testing.T) {
	p, _ := NewBreakRepeatPlugin(nil)
	reasoning := func(text string) interface{} {
		return map[string]interface{}{
			"type": "reasoning",
			"role": "assistant",
			"summary": []interface{}{
				map[string]interface{}{"type": "summary_text", "text": text},
			},
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "gpt-4o",
		"input": []interface{}{
			reasoning("thinking about foo"),
			reasoning("thinking about bar"),
			reasoning("thinking about baz"),
		},
	})
	_, newBody, err := p.TransformRequest(newBreakRepeatRequest(t, body), body, &engine.PipelineContext{})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if !bytes.Equal(newBody, body) {
		t.Fatalf("distinct reasoning items must not be treated as a repeat")
	}
}

func TestBreakRepeat_DetectRequestFormat(t *testing.T) {
	cases := map[string]string{
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`: "gemini",
		`{"input":[{"role":"user","content":"hi"}]}`:             "openai_responses",
		`{"input":"hi"}`: "openai_responses",
		`{"messages":[{"role":"user","content":"hi"}]}`:                          "openai",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`: "anthropic",
		`{"foo":"bar"}`: "",
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
