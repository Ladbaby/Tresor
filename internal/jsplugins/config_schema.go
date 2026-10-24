package jsplugins

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"
)

const maxSchemaDepth, maxSchemaNodes, maxConfigDepth, maxConfigNodes = 32, 4096, 32, 16384

var schemaKeywords = map[string]bool{"type": true, "properties": true, "required": true, "additionalProperties": true, "items": true, "enum": true, "default": true, "minLength": true, "maxLength": true, "minimum": true, "maximum": true, "minItems": true, "maxItems": true, "title": true, "description": true, "secret": true, "format": true}

// ValidateConfigSchema validates the supported bounded schema subset.
func ValidateConfigSchema(s map[string]any) error {
	if len(s) == 0 {
		return nil
	}
	n := 0
	if e := schemaNode(s, "$", 0, &n); e != nil {
		return e
	}
	if s["type"] != "object" {
		return fmt.Errorf("config schema at $ must have type object")
	}
	return nil
}
func schemaNode(s map[string]any, p string, d int, n *int) error {
	*n = *n + 1
	if d > maxSchemaDepth || *n > maxSchemaNodes {
		return fmt.Errorf("schema limits exceeded at %s", p)
	}
	for k := range s {
		if !schemaKeywords[k] {
			return fmt.Errorf("unsupported schema keyword %s at %s", k, p)
		}
	}
	t, ok := s["type"].(string)
	if !ok {
		return fmt.Errorf("schema type required at %s", p)
	}
	switch t {
	case "object", "string", "boolean", "integer", "number", "array":
	default:
		return fmt.Errorf("unsupported schema type at %s", p)
	}
	for keyword, types := range map[string]string{"properties": "object", "required": "object", "additionalProperties": "object", "items": "array", "minItems": "array", "maxItems": "array", "minLength": "string", "maxLength": "string", "format": "string", "minimum": "number integer", "maximum": "number integer"} {
		if _, exists := s[keyword]; exists && !strings.Contains(types, t) {
			return fmt.Errorf("schema keyword incompatible with type at %s", p)
		}
	}
	for _, k := range []string{"title", "description"} {
		if x, ok := s[k]; ok {
			if _, ok := x.(string); !ok {
				return fmt.Errorf("invalid %s at %s", k, p)
			}
		}
	}
	if x, ok := s["secret"]; ok {
		if _, ok := x.(bool); !ok {
			return fmt.Errorf("invalid secret at %s", p)
		}
	}
	if x, exists := s["format"]; exists {
		f, ok := x.(string)
		if !ok || (f != "password" && f != "uri") {
			return fmt.Errorf("invalid format at %s", p)
		}
	}
	if x, ok := s["enum"]; ok {
		a, ok := x.([]any)
		if !ok || len(a) == 0 {
			return fmt.Errorf("invalid enum at %s", p)
		}
		for _, v := range a {
			if !isType(v, t) {
				return fmt.Errorf("enum type mismatch at %s", p)
			}
		}
	}
	if x, ok := s["default"]; ok {
		if !isType(x, t) {
			return fmt.Errorf("default type mismatch at %s", p)
		}
	}
	if t == "object" {
		props, ok := s["properties"].(map[string]any)
		if !ok && s["properties"] != nil {
			return fmt.Errorf("invalid properties at %s", p)
		}
		if x, ok := s["required"]; ok {
			a, ok := x.([]any)
			seen := map[string]bool{}
			if !ok {
				return fmt.Errorf("invalid required at %s", p)
			}
			for _, v := range a {
				k, ok := v.(string)
				if !ok || seen[k] {
					return fmt.Errorf("invalid required at %s", p)
				}
				if _, exists := props[k]; !exists {
					return fmt.Errorf("required field has no property schema at %s", p)
				}
				seen[k] = true
			}
		}
		if x, ok := s["additionalProperties"]; ok {
			if _, ok := x.(bool); !ok {
				return fmt.Errorf("additionalProperties must be boolean at %s", p)
			}
		}
		for k, x := range props {
			sub, ok := x.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid property schema at %s.%s", p, k)
			}
			if e := schemaNode(sub, p+"."+k, d+1, n); e != nil {
				return e
			}
		}
	} else if t == "array" {
		x, ok := s["items"].(map[string]any)
		if !ok {
			return fmt.Errorf("array items schema required at %s", p)
		}
		if e := schemaNode(x, p+"[]", d+1, n); e != nil {
			return e
		}
	}
	for _, k := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		if x, ok := s[k]; ok {
			v, ok := integer(x)
			if !ok || v < 0 {
				return fmt.Errorf("invalid %s at %s", k, p)
			}
		}
	}
	for _, k := range []string{"minimum", "maximum"} {
		if x, ok := s[k]; ok {
			if _, ok := numeric(x); !ok {
				return fmt.Errorf("invalid %s at %s", k, p)
			}
		}
	}
	for _, q := range [][2]string{{"minLength", "maxLength"}, {"minItems", "maxItems"}, {"minimum", "maximum"}} {
		a, ao := numeric(s[q[0]])
		b, bo := numeric(s[q[1]])
		if ao && bo && a > b {
			return fmt.Errorf("inconsistent bounds at %s", p)
		}
	}
	if x, ok := s["default"]; ok {
		if _, err := valueCheck(s, x, p, 0, new(int)); err != nil {
			return fmt.Errorf("invalid default at %s", p)
		}
	}
	return nil
}
func isType(v any, t string) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		x, ok := numeric(v)
		return ok && math.Trunc(x) == x
	case "number":
		_, ok := numeric(v)
		return ok
	}
	return false
}
func numeric(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	}
	return 0, false
}
func integer(v any) (int, bool) {
	x, ok := numeric(v)
	if !ok || math.Trunc(x) != x || x > float64(math.MaxInt) || x < float64(math.MinInt) {
		return 0, false
	}
	return int(x), true
}

// ResolveConfig applies defaults and returns a detached validated config.
func ResolveConfig(schema, values map[string]any) (map[string]any, error) {
	if e := ValidateConfigSchema(schema); e != nil {
		return nil, e
	}
	if len(schema) == 0 {
		if len(values) > 0 {
			return nil, fmt.Errorf("unknown config field %s", first(values))
		}
		return map[string]any{}, nil
	}
	if values == nil {
		values = map[string]any{}
	}
	x, e := objectValue(schema, values, "$", 0, new(int))
	if e != nil {
		return nil, e
	}
	return x.(map[string]any), nil
}
func first(m map[string]any) string {
	for k := range m {
		return k
	}
	return "?"
}
func objectValue(s, v map[string]any, p string, d int, n *int) (any, error) {
	if d > maxConfigDepth {
		return nil, fmt.Errorf("config depth exceeded at %s", p)
	}
	props, _ := s["properties"].(map[string]any)
	out := map[string]any{}
	for k := range v {
		if _, ok := props[k]; !ok && s["additionalProperties"] != true {
			return nil, fmt.Errorf("unknown config field %s", join(p, k))
		}
	}
	for k, x := range props {
		sub := x.(map[string]any)
		val, ok := v[k]
		if !ok {
			val, ok = sub["default"]
		}
		if !ok {
			if required(s["required"], k) {
				return nil, fmt.Errorf("missing required config field %s", join(p, k))
			}
			continue
		}
		r, e := valueCheck(sub, val, join(p, k), d+1, n)
		if e != nil {
			return nil, e
		}
		out[k] = r
	}
	if s["additionalProperties"] == true {
		for k, x := range v {
			if _, ok := props[k]; !ok {
				r, e := clone(x, d+1, n)
				if e != nil {
					return nil, e
				}
				out[k] = r
			}
		}
	}
	return out, nil
}
func valueCheck(s map[string]any, v any, p string, d int, n *int) (any, error) {
	*n = *n + 1
	if *n > maxConfigNodes || d > maxConfigDepth {
		return nil, fmt.Errorf("config limits exceeded at %s", p)
	}
	if !isType(v, s["type"].(string)) {
		return nil, fmt.Errorf("invalid config value at %s", p)
	}
	if a, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range a {
			equal := reflect.DeepEqual(x, v)
			if a, aok := numeric(x); aok {
				if b, bok := numeric(v); bok {
					equal = a == b
				}
			}
			if equal {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("value not allowed at %s", p)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		return objectValue(s, x, p, d+1, n)
	case []any:
		if lo, ok := integer(s["minItems"]); ok && len(x) < lo {
			return nil, fmt.Errorf("too few items at %s", p)
		}
		if hi, ok := integer(s["maxItems"]); ok && len(x) > hi {
			return nil, fmt.Errorf("too many items at %s", p)
		}
		r := make([]any, len(x))
		item := s["items"].(map[string]any)
		for i, y := range x {
			z, e := valueCheck(item, y, fmt.Sprintf("%s[%d]", p, i), d+1, n)
			if e != nil {
				return nil, e
			}
			r[i] = z
		}
		return r, nil
	case string:
		if lo, ok := integer(s["minLength"]); ok && utf8.RuneCountInString(x) < lo {
			return nil, fmt.Errorf("string too short at %s", p)
		}
		if hi, ok := integer(s["maxLength"]); ok && utf8.RuneCountInString(x) > hi {
			return nil, fmt.Errorf("string too long at %s", p)
		}
	}
	if x, ok := numeric(v); ok {
		if lo, ok := numeric(s["minimum"]); ok && x < lo {
			return nil, fmt.Errorf("number too small at %s", p)
		}
		if hi, ok := numeric(s["maximum"]); ok && x > hi {
			return nil, fmt.Errorf("number too large at %s", p)
		}
	}
	return clone(v, d, n)
}
func clone(v any, d int, n *int) (any, error) {
	*n = *n + 1
	if d > maxConfigDepth || *n > maxConfigNodes {
		return nil, fmt.Errorf("config limits exceeded")
	}
	switch x := v.(type) {
	case map[string]any:
		r := make(map[string]any, len(x))
		for k, y := range x {
			z, e := clone(y, d+1, n)
			if e != nil {
				return nil, e
			}
			r[k] = z
		}
		return r, nil
	case []any:
		r := make([]any, len(x))
		for i, y := range x {
			z, e := clone(y, d+1, n)
			if e != nil {
				return nil, e
			}
			r[i] = z
		}
		return r, nil
	default:
		if v == nil {
			return nil, nil
		}
		switch v.(type) {
		case string, bool:
			return v, nil
		}
		if _, ok := numeric(v); ok {
			return v, nil
		}
		return nil, fmt.Errorf("config must contain JSON values only")
	}
}
func required(v any, k string) bool {
	a, ok := v.([]any)
	if !ok {
		return false
	}
	for _, x := range a {
		if x == k {
			return true
		}
	}
	return false
}
func join(p, k string) string {
	if p == "$" {
		return p + "." + k
	}
	return strings.Join([]string{p, k}, ".")
}
