package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"tresor/internal/store"
)

// reconstructManaged restores complete inference segments at their visible anchors.
// FullJSON is an ordered message array; VisibleJSON is the emitted assistant.
func reconstructManaged(messages []ChatMessage, segments []store.ManagedSegment, epoch int64) ([]ChatMessage, error) {
	out := []ChatMessage{}
	expected := map[string]bool{}
	for _, seg := range segments {
		if seg.Epoch == epoch {
			for _, id := range seg.PendingClientIDs {
				if expected[id] {
					return nil, fmt.Errorf("continuation_mismatch: reused client ID")
				}
				expected[id] = true
			}
		}
	}
	seenResults := map[string]bool{}
	for _, message := range messages {
		var role, id string
		_ = json.Unmarshal(message["role"], &role)
		if role != "tool" {
			continue
		}
		_ = json.Unmarshal(message["tool_call_id"], &id)
		if !expected[id] || seenResults[id] {
			return nil, fmt.Errorf("continuation_mismatch: foreign or duplicated result")
		}
		seenResults[id] = true
	}
	position := 0
	for _, seg := range segments {
		if seg.Epoch != epoch {
			continue
		}
		var full []ChatMessage
		if json.Unmarshal(seg.FullJSON, &full) != nil {
			return nil, fmt.Errorf("continuation_mismatch: corrupt history")
		}
		if string(seg.VisibleJSON) == "null" {
			return nil, fmt.Errorf("operation_uncertain: unfinished generation")
		}
		match := -1
		for i := position; i < len(messages); i++ {
			raw, _ := json.Marshal(messages[i])
			if managedAnchorEqual(raw, seg.VisibleJSON) {
				if match != -1 {
					return nil, fmt.Errorf("continuation_mismatch: ambiguous anchor")
				}
				match = i
			}
		}
		if match < 0 {
			return nil, fmt.Errorf("continuation_mismatch: missing anchor")
		}
		out = append(out, messages[position:match]...)
		out = append(out, full...)
		position = match + 1
		results := map[string]ChatMessage{}
		for _, id := range seg.PendingClientIDs {
			if position >= len(messages) {
				return nil, fmt.Errorf("continuation_mismatch: missing tool result")
			}
			var got, role string
			_ = json.Unmarshal(messages[position]["tool_call_id"], &got)
			_ = json.Unmarshal(messages[position]["role"], &role)
			if role != "tool" || got == "" || results[got] != nil {
				return nil, fmt.Errorf("continuation_mismatch: invalid tool result")
			}
			results[got] = messages[position]
			position++
			_ = id
		}
		if len(seg.PendingClientIDs) > 0 {
			if len(full) == 0 {
				return nil, fmt.Errorf("continuation_mismatch: empty transcript")
			}
			var calls []struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(full[len(full)-1]["tool_calls"], &calls) != nil {
				return nil, fmt.Errorf("continuation_mismatch: missing calls")
			}
			var hostResults map[string]ChatMessage
			if json.Unmarshal([]byte(seg.Provenance), &hostResults) != nil {
				return nil, fmt.Errorf("continuation_mismatch: corrupt results")
			}
			for _, id := range seg.PendingClientIDs {
				if results[id] == nil {
					return nil, fmt.Errorf("continuation_mismatch: required result absent")
				}
			}
			for _, call := range calls {
				if result := results[call.ID]; result != nil {
					out = append(out, result)
					delete(results, call.ID)
				} else if host := hostResults[call.ID]; host != nil {
					out = append(out, host)
				} else {
					return nil, fmt.Errorf("continuation_mismatch: result absent")
				}
			}
			if len(results) != 0 {
				return nil, fmt.Errorf("continuation_mismatch: foreign result")
			}
			for _, id := range seg.PendingClientIDs {
				found := false
				for _, call := range calls {
					if call.ID == id {
						found = true
					}
				}
				if !found {
					return nil, fmt.Errorf("continuation_mismatch: invalid pending ID")
				}
			}
		}
	}
	out = append(out, messages[position:]...)
	return out, nil
}

func managedAnchorEqual(actual, expected []byte) bool {
	var got, want ChatMessage
	if json.Unmarshal(actual, &got) != nil || json.Unmarshal(expected, &want) != nil {
		return false
	}
	if version, ok := want["managed_visible_version"]; ok {
		var message ChatMessage
		if string(version) != "1" || json.Unmarshal(want["message"], &message) != nil {
			return false
		}
		want = message
	} else {
		// Legacy anchors included provider-private fields rather than a final projection.
		delete(got, "reasoning")
		delete(got, "reasoning_content")
		delete(want, "reasoning")
		delete(want, "reasoning_content")
	}
	// Tool-only turns use null, omitted, or empty content across OpenAI clients.
	// Never normalize nonempty assistant text or tool identity.
	for _, message := range []ChatMessage{got, want} {
		if raw, ok := message["content"]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || bytes.Equal(bytes.TrimSpace(raw), []byte(`""`)) {
			delete(message, "content")
		}
		if raw, ok := message["tool_calls"]; ok {
			var calls []map[string]json.RawMessage
			if json.Unmarshal(raw, &calls) != nil {
				return false
			}
			for _, call := range calls {
				var fn map[string]json.RawMessage
				if json.Unmarshal(call["function"], &fn) != nil {
					return false
				}
				var args string
				if json.Unmarshal(fn["arguments"], &args) != nil {
					return false
				}
				canonical, err := canonicalJSON([]byte(args))
				if err != nil {
					return false
				}
				fn["arguments"], _ = json.Marshal(string(canonical))
				call["function"], _ = json.Marshal(fn)
			}
			message["tool_calls"], _ = json.Marshal(calls)
		}
	}
	return semanticEqualMessage(got, want)
}

func semanticEqualMessage(a, b ChatMessage) bool {
	ax, e1 := json.Marshal(a)
	by, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && semanticEqual(ax, by)
}

func semanticEqual(a, b []byte) bool {
	ax, e1 := canonicalJSON(a)
	by, e2 := canonicalJSON(b)
	return e1 == nil && e2 == nil && bytes.Equal(ax, by)
}
