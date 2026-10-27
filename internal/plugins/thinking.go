package plugins

import (
	"encoding/json"
	"strings"
)

// This file centralizes the thinking/reasoning level translation shared by the
// format converters. Every converter maps its native thinking representation to
// a canonical level (off / minimal / low / medium / high) and back, so the
// tables and "off vs absent" semantics stay consistent across formats.
//
// The three-way state is deliberate and is the root of a prior bug:
//   - absent  → omit the target field entirely (let the downstream model decide)
//   - off     → emit the format's explicit OFF representation
//   - on      → emit the level (or a budget derived from it)

// Canonical thinking/reasoning levels shared by every format converter.
const (
	thinkingLevelOff     = "off"
	thinkingLevelMinimal = "minimal"
	thinkingLevelLow     = "low"
	thinkingLevelMedium  = "medium"
	thinkingLevelHigh    = "high"
)

// effortToBudget maps canonical ON levels to thinking token budgets (aligned
// with pi's DEFAULT_THINKING_BUDGETS). The "off" level has no budget — it is
// represented explicitly as a disabled thinking config instead.
var effortToBudget = map[string]int{
	thinkingLevelMinimal: 1024,
	thinkingLevelLow:     2048,
	thinkingLevelMedium:  8192,
	thinkingLevelHigh:    16384,
}

// budgetToEffort buckets a thinking token budget into a canonical level.
//
//	<=1024 -> minimal, <=2048 -> low, <=8192 -> medium, else high
func budgetToEffort(budget int) string {
	switch {
	case budget <= 1024:
		return thinkingLevelMinimal
	case budget <= 2048:
		return thinkingLevelLow
	case budget <= 8192:
		return thinkingLevelMedium
	default:
		return thinkingLevelHigh
	}
}

// normalizeThinkingLevel maps a wire effort string to a canonical level:
// "" -> "" (absent/omit), "none"/"off" -> off, known levels -> themselves
// (lowercased), any other non-empty value -> medium. Used where a real budget
// must be derived from an arbitrary effort string.
func normalizeThinkingLevel(effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	switch e {
	case "":
		return ""
	case "none", "off":
		return thinkingLevelOff
	case thinkingLevelMinimal, thinkingLevelLow, thinkingLevelMedium, thinkingLevelHigh:
		return e
	default:
		return thinkingLevelMedium
	}
}

// reasoningEffortWireValue maps a canonical (or pass-through) level to the
// OpenAI Chat Completions / Responses wire value: "off" -> "none"; every other
// value is forwarded unchanged so unknown efforts are preserved.
func reasoningEffortWireValue(level string) string {
	if level == thinkingLevelOff {
		return "none"
	}
	return level
}

// anthropicThinkingLevel extracts the canonical thinking level from an
// Anthropic `thinking` value. Accepts json.RawMessage, []byte, string or a
// decoded map — the converters parse `thinking` into different shapes.
//
//	absent / null / malformed / unknown type -> ""  (omit)
//	type "disabled"                          -> off
//	type "enabled"  + budget N>0             -> budgetToEffort(N); N<=0 -> medium
//	type "adaptive": embedded effort (normalized) if present, else budget>0,
//	                 else medium
func anthropicThinkingLevel(v interface{}) string {
	obj := asStringMap(v)
	if obj == nil {
		return ""
	}
	typ, _ := obj["type"].(string)
	switch typ {
	case "disabled":
		return thinkingLevelOff
	case "enabled":
		return anthropicEnabledLevel(obj)
	case "adaptive":
		// Adaptive (managed) thinking carries an effort hint when present;
		// fall back to the budget, then a medium default.
		if eff, ok := obj["effort"].(string); ok && eff != "" {
			return normalizeThinkingLevel(eff)
		}
		if oc, ok := obj["output_config"].(map[string]interface{}); ok {
			if eff, ok := oc["effort"].(string); ok && eff != "" {
				return normalizeThinkingLevel(eff)
			}
		}
		return anthropicEnabledLevel(obj)
	}
	return ""
}

// anthropicEnabledLevel resolves the level for an enabled/adaptive thinking
// object from its budget_tokens, defaulting to medium when no usable budget is
// present (replacing an older hardcoded 10000-token fallback).
func anthropicEnabledLevel(obj map[string]interface{}) string {
	if budget := jsonInt(obj["budget_tokens"]); budget > 0 {
		return budgetToEffort(budget)
	}
	return thinkingLevelMedium
}

// anthropicThinkingForLevel builds the Anthropic `thinking` object for a
// canonical level. Returns nil for the absent level so callers omit the field.
//
//	""   -> nil (omit)
//	off  -> {"type":"disabled"}
//	ON   -> {"type":"enabled","budget_tokens": effortToBudget[level]}
func anthropicThinkingForLevel(level string) map[string]interface{} {
	switch level {
	case "":
		return nil
	case thinkingLevelOff:
		return map[string]interface{}{"type": "disabled"}
	}
	return map[string]interface{}{
		"type":          "enabled",
		"budget_tokens": budgetForLevel(level),
	}
}

// geminiThinkingConfigForLevel builds the Gemini generationConfig.thinkingConfig
// object for a canonical level. Returns nil for the absent level so callers omit
// the field.
//
//	""   -> nil (omit)
//	off  -> {"thinkingBudget":0}
//	ON   -> {"thinkingBudget": effortToBudget[level], "includeThoughts":true}
func geminiThinkingConfigForLevel(level string) map[string]interface{} {
	switch level {
	case "":
		return nil
	case thinkingLevelOff:
		return map[string]interface{}{"thinkingBudget": 0}
	}
	return map[string]interface{}{
		"thinkingBudget":  budgetForLevel(level),
		"includeThoughts": true,
	}
}

// levelForGeminiThinking maps a parsed Gemini thinkingConfig to a canonical
// level. A nil budget means the field was absent.
//
//	budget absent        -> ""     (omit)
//	0, !includeThoughts  -> off    (explicit OFF)
//	0, includeThoughts   -> low    (ON; thoughts requested without a budget)
//	negative (dynamic)   -> medium
//	N > 0                -> budgetToEffort(N)
func levelForGeminiThinking(budget *int, includeThoughts bool) string {
	if budget == nil {
		return ""
	}
	b := *budget
	if b == 0 {
		if includeThoughts {
			return thinkingLevelLow
		}
		return thinkingLevelOff
	}
	if b < 0 {
		return thinkingLevelMedium
	}
	return budgetToEffort(b)
}

// responsesReasoningForLevel builds the OpenAI Responses API `reasoning` object
// for a canonical level. Returns nil for the absent level so callers omit the
// field.
//
//	""   -> nil (omit)
//	off  -> {"effort":"none"}
//	ON   -> {"effort": level}
func responsesReasoningForLevel(level string) map[string]interface{} {
	switch level {
	case "":
		return nil
	case thinkingLevelOff:
		return map[string]interface{}{"effort": "none"}
	}
	return map[string]interface{}{"effort": level}
}

// budgetForLevel returns the token budget for an ON level, defaulting to the
// medium budget for an unrecognized level so the intent to think is preserved.
func budgetForLevel(level string) int {
	if budget, ok := effortToBudget[level]; ok {
		return budget
	}
	return effortToBudget[thinkingLevelMedium]
}

// asStringMap coerces a parsed JSON value (map, json.RawMessage, []byte or
// string) into a map[string]interface{}. Returns nil for absent or non-object
// values.
func asStringMap(v interface{}) map[string]interface{} {
	switch val := v.(type) {
	case nil:
		return nil
	case map[string]interface{}:
		return val
	case json.RawMessage:
		return decodeJSONMap(val)
	case []byte:
		return decodeJSONMap(val)
	case string:
		return decodeJSONMap([]byte(val))
	}
	return nil
}

// decodeJSONMap decodes a JSON object into a map, returning nil on empty or
// malformed input.
func decodeJSONMap(b []byte) map[string]interface{} {
	if len(b) == 0 {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// jsonInt coerces a JSON number (float64 from map decoding, or a native int
// type) to an int. Returns 0 when absent or non-numeric.
func jsonInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}
