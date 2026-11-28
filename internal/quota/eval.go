// Package quota reads a provider's remaining balance and rate-limit windows and
// keeps the last reading of each.
//
// A downstream declares how to ask in its quota block (config.QuotaCfg): one
// authenticated GET and a small closed path expression into the JSON reply. This
// package is the only place such a declaration becomes a number, so a vendor
// needs no Go code of its own.
//
// Two rules run through everything here:
//
//   - A provider that declares nothing is never asked, and never appears in a
//     reading. Unsupported looks exactly like absent, all the way to the UI.
//   - A failed read never surfaces as an error to the browser. The last good
//     reading stands for a grace period, and then the provider drops out of the
//     response entirely. There is no "unknown" state and no warning badge.
package quota

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"tresor/internal/config"
)

// EvalState says how a path's walk ended. The distinction between a value that
// is not there and a value that is explicitly null matters: an absent figure is
// one the vendor does not report and must be left out, while a null can carry a
// meaning (a null limit is an uncapped allowance).
type EvalState int

const (
	// Absent means the walk failed: a missing key, an index past the end of a
	// list, or a container of the wrong type. It is never an error.
	Absent EvalState = iota
	// Null means the walk succeeded and the leaf is JSON null.
	Null
	// Present means the walk found a value.
	Present
)

// Eval walks a parsed path through a decoded JSON reply.
//
// A pure-digit segment indexes a list when it meets one and is a key otherwise,
// so the same path shape works for os and os_release style replies.
func Eval(path config.ParsedQuotaPath, root any) (any, EvalState) {
	cur := root
	for _, seg := range path.Segments {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, Absent
			}
			cur = v
		case []any:
			idx, ok := listIndex(seg, len(node))
			if !ok {
				return nil, Absent
			}
			cur = node[idx]
		default:
			// A scalar in the middle of a path: there is nothing further to walk.
			return nil, Absent
		}
	}
	if cur == nil {
		return nil, Null
	}
	return cur, Present
}

// listIndex reads a list index from a path segment, reporting whether it names a
// position the list actually has.
func listIndex(seg string, length int) (int, bool) {
	if seg == "" || len(seg) > 9 {
		return 0, false
	}
	for _, r := range seg {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(seg)
	if err != nil || n < 0 || n >= length {
		return 0, false
	}
	return n, true
}

// Number reads a value as a number, accepting a numeric string because vendors
// routinely quote amounts ("total_balance": "110.00").
func Number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, true
		}
		return 0, false
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// Text reads a value as a string, for a balance a vendor reports as prose.
func Text(v any) (string, bool) {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s), true
	}
	return "", false
}

// Time reads a reset instant. Vendors report Unix seconds, Unix milliseconds, or
// an RFC 3339 string, and the three are told apart by magnitude: no plausible
// timestamp in seconds reaches 1e12.
func Time(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return time.Time{}, false
		}
		if parsed, err := time.Parse(time.RFC3339, s); err == nil {
			return parsed, true
		}
		// A numeric string still reads as an epoch.
		if f, ok := Number(s); ok {
			return epochTime(f)
		}
		return time.Time{}, false
	case float64, int, int64, json.Number:
		if f, ok := Number(t); ok {
			return epochTime(f)
		}
	}
	return time.Time{}, false
}

// epochTime converts a Unix timestamp in seconds or milliseconds.
func epochTime(n float64) (time.Time, bool) {
	if n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return time.Time{}, false
	}
	if n >= 1e12 {
		return time.UnixMilli(int64(n)), true
	}
	return time.Unix(int64(n), 0), true
}

// Sum is the small amount of arithmetic the declaration language cannot express:
// several windows share one allowance, and the figure that matters is the
// tightest of them. A nil slice means no window could be read.
func maxUsed(values []float64) float64 {
	best, found := 0.0, false
	for _, v := range values {
		if !found || v > best {
			best, found = v, true
		}
	}
	return best
}
