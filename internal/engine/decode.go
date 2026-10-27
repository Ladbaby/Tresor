package engine

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// decodeMeasurement observes raw complete upstream events, never plugin output.
// Times retain Go's monotonic clock; the rate is an event-window estimate.
type decodeMeasurement struct {
	format                     string
	first, last                time.Time
	events                     int
	streamed, terminal, failed bool
	unavailable                string
	usage                      *UsageBlock
	reasoningTokens            int64
}

func (ctx *PipelineContext) observeDecodeEvent(event SSEChunk, at time.Time) {
	if ctx != nil && ctx.decode != nil {
		ctx.decode.observe(event, at)
	}
}
func outputString(v any) bool     { s, ok := v.(string); return ok && len(s) > 0 }
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func (d *decodeMeasurement) observe(event SSEChunk, at time.Time) {
	if d == nil || d.failed {
		return
	}
	d.streamed = true
	if string(event.Data) == "[DONE]" {
		if d.format == "openai" {
			d.terminal = true
		}
		return
	}
	if len(event.Data) == 0 {
		return
	}
	var doc map[string]any
	if json.Unmarshal(event.Data, &doc) != nil {
		d.unavailable = "malformed_event"
		return
	}
	if detected := DetectStreamFormat(event); detected != "" {
		d.format = detected
	}
	if usage, ok := UsageFromDecodedUsage(doc); ok && usage != nil {
		if d.usage == nil {
			d.usage = &UsageBlock{}
		}
		d.usage.Merge(usage)
		raw, _ := locateUsageBlock(doc)
		for _, key := range []string{"output_tokens", "completion_tokens", "candidatesTokenCount"} {
			if v, exists := raw[key]; exists {
				n, ok := v.(float64)
				if !ok || n < 0 || math.Trunc(n) != n || n >= float64(math.MaxInt64) {
					d.unavailable = "invalid_output_usage"
				}
			}
		}
		// OpenAI output totals include hidden reasoning. Their separately
		// reported count is excluded from the visible text/tool delta window.
		for _, key := range []string{"output_tokens_details", "completion_tokens_details"} {
			if value, exists := object(raw[key])["reasoning_tokens"]; exists {
				n, valid := value.(float64)
				if !valid || n < 0 || math.Trunc(n) != n || n >= float64(math.MaxInt64) {
					d.unavailable = "invalid_output_usage"
				} else {
					d.reasoningTokens = int64(n)
				}
			}
		}
		// Gemini candidate counts already exclude thoughts.
		if asInt64(raw["thoughtsTokenCount"]) > 0 {
			d.unavailable = "unobserved_output_tokens"
		}
	}
	if _, bad := ParseStreamError(event.EventType, event.Data); bad {
		d.failed = true
		return
	}
	positive := false
	switch d.format {
	case "openai":
		choices := array(doc["choices"])
		if len(choices) > 1 {
			d.unavailable = "unsupported_output"
		}
		for _, v := range choices {
			c := object(v)
			delta := object(c["delta"])
			for _, k := range []string{"content", "refusal"} {
				if outputString(delta[k]) {
					positive = true
				}
			}
			if v := delta["content"]; v != nil {
				if _, ok := v.(string); !ok {
					d.unavailable = "unsupported_output"
				}
			}
			for _, t := range array(delta["tool_calls"]) {
				if outputString(object(object(t)["function"])["arguments"]) {
					positive = true
				}
			}
			if outputString(object(delta["function_call"])["arguments"]) {
				positive = true
			}
			if delta["audio"] != nil {
				d.unavailable = "unsupported_output"
			}
		}
	case "openai_responses":
		typ, _ := doc["type"].(string)
		if typ == "" {
			typ = event.EventType
		}
		switch typ {
		case "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			positive = outputString(doc["delta"])
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			// Reasoning and summaries are not visible answer/tool output.
		case "response.completed":
			d.terminal = true
		case "response.failed", "response.incomplete":
			d.failed = true
		default:
			if strings.Contains(typ, "audio") || strings.Contains(typ, "image") {
				d.unavailable = "unsupported_output"
			}
		}
	case "anthropic":
		typ, _ := doc["type"].(string)
		if typ == "" {
			typ = event.EventType
		}
		switch typ {
		case "content_block_start":
			block := object(doc["content_block"])
			kind, _ := block["type"].(string)
			if kind == "redacted_thinking" {
				d.unavailable = "unobserved_output_tokens"
			}
			if outputString(block["text"]) || outputString(block["thinking"]) {
				positive = true
			}
		case "content_block_delta":
			delta := object(doc["delta"])
			kind, _ := delta["type"].(string)
			switch kind {
			case "text_delta":
				positive = outputString(delta["text"])
			case "thinking_delta":
				positive = outputString(delta["thinking"])
			case "input_json_delta":
				positive = outputString(delta["partial_json"])
			case "signature_delta":
			default:
				d.unavailable = "unsupported_output"
			}
		case "message_stop":
			d.terminal = true
		}
	case "gemini":
		candidates := array(doc["candidates"])
		if len(candidates) > 1 {
			d.unavailable = "unsupported_output"
		}
		for _, v := range candidates {
			c := object(v)
			for _, p := range array(object(c["content"])["parts"]) {
				part := object(p)
				if part["thought"] == true {
					d.unavailable = "unobserved_output_tokens"
				}
				if outputString(part["text"]) {
					positive = true
				}
				if part["functionCall"] != nil || part["inlineData"] != nil || part["fileData"] != nil {
					d.unavailable = "unsupported_output"
				}
			}
			if c["finishReason"] == "STOP" || c["finishReason"] == "MAX_TOKENS" {
				d.terminal = true
			}
		}
	default:
		d.unavailable = "unsupported_format"
	}
	if positive {
		if d.events == 0 {
			d.first = at
		}
		d.last = at
		d.events++
	}
}
func (d *decodeMeasurement) finalize(entry *RequestLogEntry) {
	entry.DecodeTokens, entry.DecodeDurationMS, entry.DecodeTokensPerSecond = nil, nil, nil
	reason := ""
	switch {
	case d == nil:
		reason = "capture_disabled"
	case d.failed || entry.Error != "" || entry.Status < 200 || entry.Status >= 300:
		reason = "request_failed"
	case !d.streamed:
		reason = "not_streamed"
	case d.unavailable != "":
		reason = d.unavailable
	case d.usage == nil || d.usage.OutputTokens == nil || *d.usage.OutputTokens <= 0:
		reason = "missing_output_usage"
	case d.reasoningTokens > *d.usage.OutputTokens:
		reason = "invalid_output_usage"
	case d.reasoningTokens == *d.usage.OutputTokens:
		reason = "missing_output_usage"
	case !d.terminal:
		reason = "missing_terminal_marker"
	case d.events < 2 || !d.last.After(d.first):
		reason = "insufficient_timing_events"
	}
	entry.DecodeUnavailableReason = reason
	if reason != "" {
		return
	}
	tokens := *d.usage.OutputTokens - d.reasoningTokens
	entry.decodeDurationNS = int64(d.last.Sub(d.first))
	duration := float64(entry.decodeDurationNS) / float64(time.Millisecond)
	speed := float64(tokens) * 1000 / duration
	entry.DecodeTokens, entry.DecodeDurationMS, entry.DecodeTokensPerSecond = &tokens, &duration, &speed
}
