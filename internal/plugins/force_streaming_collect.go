package plugins

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"tresor/internal/engine"
)

const forcedStreamLimit = 64 << 20

type forcedEvent struct {
	event string
	data  []byte
}
type forcedUpstreamError struct {
	status  int
	message string
}

func (e forcedUpstreamError) Error() string       { return e.message }
func (e forcedUpstreamError) UpstreamStatus() int { return e.status }

// collectForcedStream consumes an SSE response and reconstructs its upstream JSON representation.
func collectForcedStream(resp *http.Response, ctx *engine.PipelineContext) ([]byte, error) {
	if resp == nil || resp.Body == nil || ctx == nil {
		return nil, fmt.Errorf("forced stream: missing response or context")
	}
	var events []forcedEvent
	br := bufio.NewReader(io.LimitReader(resp.Body, forcedStreamLimit+1))
	var dataLines []string
	var event string
	var total int
	flush := func() error {
		if len(dataLines) == 0 && event == "" {
			return nil
		}
		payload := []byte(strings.Join(dataLines, "\n"))
		if len(payload) == 0 {
			dataLines = nil
			event = ""
			return nil
		}
		if se, ok := engine.ParseStreamError(event, payload); ok {
			return forcedUpstreamError{status: se.Status, message: se.Message}
		}
		if total > forcedStreamLimit {
			return fmt.Errorf("forced stream exceeds %d bytes", forcedStreamLimit)
		}
		events = append(events, forcedEvent{event: event, data: payload})
		dataLines = nil
		event = ""
		return nil
	}
	for {
		line, err := br.ReadString('\n')
		total += len(line)
		if total > forcedStreamLimit {
			return nil, fmt.Errorf("forced stream exceeds %d bytes", forcedStreamLimit)
		}
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if e := flush(); e != nil {
					return nil, e
				}
			} else if strings.HasPrefix(line, ":") { /* comment */
			} else {
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "data":
					dataLines = append(dataLines, value)
				case "event":
					event = value
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil, fmt.Errorf("forced stream read: %w", err)
			}
			if len(line) > 0 {
				return nil, fmt.Errorf("forced stream truncated before event boundary")
			}
			break
		}
	}
	if len(dataLines) > 0 || event != "" {
		return nil, fmt.Errorf("forced stream truncated before event boundary")
	}
	if total > forcedStreamLimit {
		return nil, fmt.Errorf("forced stream exceeds limit")
	}
	switch ctx.DownstreamFormat {
	case "openai":
		return forcedCollectOpenAI(events)
	case "anthropic":
		return forcedCollectAnthropic(events)
	case "openai_responses":
		return forcedCollectResponses(events)
	case "gemini":
		return forcedCollectGemini(events)
	default:
		return nil, fmt.Errorf("forced stream: unsupported downstream format %q", ctx.DownstreamFormat)
	}
}

func forcedJSON(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("malformed SSE JSON: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("SSE JSON must be an object")
	}
	return m, nil
}
func forcedMarshal(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return b, nil
}
func forcedObj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}
func forcedArr(v any) []any   { a, _ := v.([]any); return a }
func forcedText(v any) string { s, _ := v.(string); return s }

func forcedCollectOpenAI(events []forcedEvent) ([]byte, error) {
	root := map[string]any{"object": "chat.completion", "choices": []any{}}
	choices := map[int]map[string]any{}
	toolMaps := map[int]map[int]map[string]any{}
	finished := map[int]bool{}
	done := false
	for _, ev := range events {
		if bytes.Equal(bytes.TrimSpace(ev.data), []byte("[DONE]")) {
			done = true
			break
		}
		m, e := forcedJSON(ev.data)
		if e != nil {
			return nil, e
		}
		for _, k := range []string{"id", "created", "model", "service_tier", "system_fingerprint"} {
			if v, ok := m[k]; ok {
				root[k] = v
			}
		}
		if u, ok := m["usage"]; ok {
			root["usage"] = u
		}
		for _, raw := range forcedArr(m["choices"]) {
			c := forcedObj(raw)
			ix, _ := c["index"].(float64)
			i := int(ix)
			dst := choices[i]
			if dst == nil {
				dst = map[string]any{"index": i, "message": map[string]any{"role": "assistant"}}
				choices[i] = dst
			}
			msg := forcedObj(dst["message"])
			d := forcedObj(c["delta"])
			for _, k := range []string{"role", "content", "reasoning_content", "reasoning", "refusal", "function_call"} {
				if v, ok := d[k]; ok {
					if k == "content" || k == "reasoning_content" || k == "reasoning" || k == "refusal" {
						msg[k] = forcedText(msg[k]) + forcedText(v)
					} else if k == "function_call" {
						fn := forcedObj(msg[k])
						for fk, fv := range forcedObj(v) {
							fn[fk] = forcedText(fn[fk]) + forcedText(fv)
						}
						msg[k] = fn
					} else {
						msg[k] = v
					}
				}
			}
			for _, tr := range forcedArr(d["tool_calls"]) {
				t := forcedObj(tr)
				ti, _ := t["index"].(float64)
				j := int(ti)
				if toolMaps[i] == nil {
					toolMaps[i] = map[int]map[string]any{}
				}
				tm := toolMaps[i][j]
				if tm == nil {
					tm = map[string]any{"type": "function", "function": map[string]any{}}
					toolMaps[i][j] = tm
				}
				for _, k := range []string{"id", "type"} {
					if v, ok := t[k]; ok {
						tm[k] = v
					}
				}
				fn := forcedObj(tm["function"])
				fd := forcedObj(t["function"])
				if v, ok := fd["name"]; ok {
					fn["name"] = forcedText(fn["name"]) + forcedText(v)
				}
				if v, ok := fd["arguments"]; ok {
					fn["arguments"] = forcedText(fn["arguments"]) + forcedText(v)
				}
				tm["function"] = fn
			}
			if logs, ok := c["logprobs"].(map[string]any); ok {
				merged := forcedObj(dst["logprobs"])
				for k, v := range logs {
					merged[k] = append(forcedArr(merged[k]), forcedArr(v)...)
				}
				dst["logprobs"] = merged
			}
			if v, ok := c["finish_reason"]; ok && v != nil {
				dst["finish_reason"] = v
				finished[i] = true
			}
			dst["message"] = msg
		}
	}
	if !done && len(choices) == 0 {
		return nil, fmt.Errorf("OpenAI stream missing terminal marker")
	}
	if len(choices) > 0 && !done {
		all := true
		for i := range choices {
			if !finished[i] {
				all = false
			}
		}
		if !all {
			return nil, fmt.Errorf("OpenAI stream incomplete")
		}
	}
	ids := make([]int, 0, len(choices))
	for i := range choices {
		ids = append(ids, i)
	}
	sort.Ints(ids)
	out := make([]any, 0, len(ids))
	for _, i := range ids {
		c := choices[i]
		if tm := toolMaps[i]; len(tm) > 0 {
			keys := make([]int, 0, len(tm))
			for j := range tm {
				keys = append(keys, j)
			}
			sort.Ints(keys)
			a := []any{}
			for _, j := range keys {
				a = append(a, tm[j])
			}
			forcedObj(c["message"])["tool_calls"] = a
		}
		out = append(out, c)
	}
	root["choices"] = out
	return forcedMarshal(root)
}

func forcedCollectAnthropic(events []forcedEvent) ([]byte, error) {
	var msg map[string]any
	blocks := map[int]map[string]any{}
	stopped := map[int]bool{}
	terminal := false
	for _, ev := range events {
		m, e := forcedJSON(ev.data)
		if e != nil {
			return nil, e
		}
		switch func() string {
			if ev.event != "" {
				return ev.event
			}
			return forcedText(m["type"])
		}() {
		case "error":
			return nil, fmt.Errorf("Anthropic stream error: %s", string(ev.data))
		case "message_start":
			msg = forcedObj(m["message"])
		case "content_block_start":
			i := forcedInt(m["index"])
			blocks[i] = forcedObj(m["content_block"])
		case "content_block_delta":
			i := forcedInt(m["index"])
			b := blocks[i]
			if b == nil {
				return nil, fmt.Errorf("Anthropic delta for unknown block")
			}
			d := forcedObj(m["delta"])
			switch forcedText(d["type"]) {
			case "text_delta":
				b["text"] = forcedText(b["text"]) + forcedText(d["text"])
			case "thinking_delta":
				b["thinking"] = forcedText(b["thinking"]) + forcedText(d["thinking"])
			case "signature_delta":
				b["signature"] = forcedText(b["signature"]) + forcedText(d["signature"])
			case "citations_delta":
				b["citations"] = append(forcedArr(b["citations"]), d["citation"])
			case "input_json_delta":
				b["partial_json"] = forcedText(b["partial_json"]) + forcedText(d["partial_json"])
			}
		case "content_block_stop":
			stopped[forcedInt(m["index"])] = true
		case "message_delta":
			if msg == nil {
				return nil, fmt.Errorf("Anthropic message_delta before start")
			}
			for _, k := range []string{"stop_reason", "stop_sequence"} {
				if v, ok := forcedObj(m["delta"])[k]; ok {
					msg[k] = v
				}
			}
			if u, ok := m["usage"]; ok {
				msg["usage"] = forcedMerge(forcedObj(msg["usage"]), forcedObj(u))
			}
		case "message_stop":
			terminal = true
		}
	}
	if msg == nil || !terminal {
		return nil, fmt.Errorf("incomplete Anthropic stream")
	}
	for i := range blocks {
		if !stopped[i] {
			return nil, fmt.Errorf("Anthropic content block %d incomplete", i)
		}
	}
	keys := make([]int, 0, len(blocks))
	for i := range blocks {
		keys = append(keys, i)
	}
	sort.Ints(keys)
	a := []any{}
	for _, i := range keys {
		b := blocks[i]
		if x, ok := b["partial_json"]; ok {
			raw := forcedText(x)
			delete(b, "partial_json")
			var v any
			if json.Unmarshal([]byte(raw), &v) != nil {
				return nil, fmt.Errorf("invalid Anthropic tool JSON")
			}
			b["input"] = v
		}
		a = append(a, b)
	}
	msg["content"] = a
	return forcedMarshal(msg)
}
func forcedInt(v any) int { f, _ := v.(float64); return int(f) }
func forcedMerge(a, b map[string]any) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}

func forcedCollectResponses(events []forcedEvent) ([]byte, error) {
	var snapshot map[string]any
	for _, ev := range events {
		m, e := forcedJSON(ev.data)
		if e != nil {
			return nil, e
		}
		typ := ev.event
		if typ == "" {
			typ = forcedText(m["type"])
		}
		if typ == "error" || typ == "response.failed" {
			return nil, fmt.Errorf("Responses stream failed: %s", string(ev.data))
		}
		if typ == "response.completed" || typ == "response.incomplete" {
			snapshot, _ = m["response"].(map[string]any)
			if snapshot == nil {
				return nil, fmt.Errorf("missing terminal response snapshot")
			}
		}
	}
	if snapshot == nil {
		return nil, fmt.Errorf("Responses stream missing authoritative terminal snapshot")
	}
	return forcedMarshal(snapshot)
}

func forcedCollectGemini(events []forcedEvent) ([]byte, error) {
	root := map[string]any{}
	candidates := map[int]map[string]any{}
	finished := map[int]bool{}
	for _, ev := range events {
		m, err := forcedJSON(ev.data)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			if k != "candidates" {
				root[k] = v
			}
		}
		for _, raw := range forcedArr(m["candidates"]) {
			c := forcedObj(raw)
			i := forcedInt(c["index"])
			dst := candidates[i]
			if dst == nil {
				dst = map[string]any{"index": i, "content": map[string]any{"role": "model", "parts": []any{}}}
				candidates[i] = dst
			}
			for k, v := range c {
				if k != "content" {
					dst[k] = v
				}
			}
			if forcedText(c["finishReason"]) != "" {
				finished[i] = true
			}
			content := forcedObj(dst["content"])
			incoming := forcedObj(c["content"])
			if role, ok := incoming["role"]; ok {
				content["role"] = role
			}
			parts := forcedArr(content["parts"])
			for _, rawPart := range forcedArr(incoming["parts"]) {
				p := forcedObj(rawPart)
				// Gemini part positions restart in each event. Only coalesce adjacent
				// plain text with matching thought status; preserve tools and signatures.
				if len(parts) > 0 && p["text"] != nil && len(p) <= 2 && p["thoughtSignature"] == nil {
					prev := forcedObj(parts[len(parts)-1])
					if prev["text"] != nil && len(prev) <= 2 && prev["thought"] == p["thought"] {
						prev["text"] = forcedText(prev["text"]) + forcedText(p["text"])
						continue
					}
				}
				parts = append(parts, p)
			}
			content["parts"] = parts
			dst["content"] = content
		}
	}
	if len(candidates) == 0 {
		if forcedObj(root["promptFeedback"])["blockReason"] != nil {
			root["candidates"] = []any{}
			return forcedMarshal(root)
		}
		return nil, fmt.Errorf("Gemini stream has no candidates")
	}
	keys := []int{}
	for i := range candidates {
		if !finished[i] {
			return nil, fmt.Errorf("Gemini candidate %d missing finishReason", i)
		}
		keys = append(keys, i)
	}
	sort.Ints(keys)
	out := []any{}
	for _, i := range keys {
		out = append(out, candidates[i])
	}
	root["candidates"] = out
	return forcedMarshal(root)
}
