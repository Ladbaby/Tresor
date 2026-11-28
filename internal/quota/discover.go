package quota

// Discovery exists because nobody should have to write a path expression to see
// their balance.
//
// A vendor's reply is ordinary JSON; the figure that matters is a number with a
// name like "total_balance". Walking the document for numeric leaves and ranking
// them by their key names finds that figure without the user learning a query
// language, so the only thing they type is an endpoint — which they already know,
// because it is the one their vendor documents.
//
// This is a heuristic, and deliberately a transparent one: it returns candidates
// in ranked order with the value each would show, so the UI can offer a choice
// instead of silently guessing. Ranking only ever reorders; it never hides a
// numeric field, so an unexpected reply still offers something to pick.

import (
	"encoding/json"
	"math"
	"sort"
	"strings"

	"tresor/internal/config"
)

// Candidate is one numeric field a reply could be reporting as a balance.
type Candidate struct {
	// Path is the declaration that reads this field. Discovery produces it so a
	// person never has to write one.
	Path string `json:"path"`
	// Value is the number as the reply states it, before any scale.
	Value float64 `json:"value"`
	// Currency is a currency code found beside the field, when there is one.
	Currency string `json:"currency,omitempty"`
	// Label is what a picker shows: "total_balance · ¥110.25". It names the
	// field, so a choice can be made without seeing the path at all.
	Label string `json:"label"`

	// score orders the list. It is not part of the API.
	score int
}

// keyScores rank a leaf by its own name. The positive entries are the words
// vendors use for "money you still have"; the negative ones are the near-misses
// that sit in the same documents and would otherwise win on depth alone —
// a token counter called "total_tokens" is not a balance.
var keyScores = []struct {
	key   string
	score int
}{
	// A sum named outright: this is the figure a wallet shows. Kept clearly ahead
	// of a bare "*_balance", because siblings like granted_balance and
	// topped_up_balance are the parts of it, not alternatives to it.
	{"total_balance", 130}, {"available_balance", 130}, {"remain_balance", 130},
	{"remaining_balance", 130}, {"balance", 100},
	{"credits", 95}, {"credit", 95}, {"quota", 90},
	{"available", 85}, {"remaining", 85}, {"remain", 80},
	{"granted", 40}, {"topped_up", 35}, {"total", 30},
	// Near-misses: present in the same documents, never the balance.
	{"used", -60}, {"usage", -60}, {"spent", -60}, {"consumed", -60},
	{"request", -50}, {"token", -50}, {"count", -40}, {"limit", -35},
	{"percent", -70}, {"rate", -50}, {"id", -90}, {"time", -80},
	{"date", -80}, {"expire", -90},
	// "_at" marks a timestamp. Without it an epoch field passes for a balance.
	{"_at", -95},
}

// maxDiscoverDepth bounds the walk. Balance documents are shallow; a deeper
// document is a page of unrelated data rather than an allowance.
const maxDiscoverDepth = 8

// maxCandidates bounds what a picker has to show.
const maxCandidates = 6

// notABalance is the score below which a numeric field is certainly not money.
const notABalance = -50

// Discover ranks the numeric fields of a reply as balance candidates, best first.
// It returns nothing when the document holds no number, which the caller treats as
// "this endpoint reports no balance" — the same as any other failure to read.
func Discover(body []byte) []Candidate {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	var out []Candidate
	collectCandidates(root, nil, 0, &out)
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		// A tie is broken by depth, then by name, so the order is stable across
		// replies and a picker does not reshuffle under the pointer.
		if len(out[i].Path) != len(out[j].Path) {
			return len(out[i].Path) < len(out[j].Path)
		}
		return out[i].Path < out[j].Path
	})
	// A field that scores deeply negative is plainly not money — a timestamp, an
	// id, a token count. Offering it as a choice is noise, so it is dropped. The
	// best candidate is always kept, so an unrecognised reply still has something
	// to pick rather than coming back empty.
	best := out[0]
	kept := out[:0]
	for _, c := range out {
		if c.score >= notABalance || c.Path == best.Path {
			kept = append(kept, c)
		}
	}
	out = kept
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	// Labels are built after ranking: the top candidate's currency sign is worth
	// showing, a runner-up's is not.
	for i := range out {
		out[i].Label = candidateLabel(out[i])
	}
	return out
}

// collectCandidates walks the document, recording numeric leaves.
func collectCandidates(node any, path []string, depth int, out *[]Candidate) {
	if depth > maxDiscoverDepth {
		return
	}
	switch n := node.(type) {
	case map[string]any:
		for key, v := range n {
			if !keyExpressible(key) {
				// A key outside the path grammar cannot be described, so it is not
				// offered as a choice the UI could then save.
				continue
			}
			collectCandidates(v, append(path, key), depth+1, out)
		}
	case []any:
		for i, v := range n {
			collectCandidates(v, append(path, strconvIndex(i)), depth+1, out)
		}
	default:
		value, ok := Number(n)
		if !ok || len(path) == 0 {
			return
		}
		leaf := path[len(path)-1]
		score := scoreKey(leaf) - depthPenalty(len(path)) - valuePenalty(value)
		*out = append(*out, Candidate{
			Path:  "$" + strings.Join(path, "."),
			Value: value,
			score: score,
		})
	}
	// Currency is filled in a second pass, because it is a sibling of the field
	// rather than part of the path that reached it.
}

// scoreKey sums the ranking for a leaf name. A name that matches several entries
// ("total_balance") takes the best one, so a strong word is not diluted by a
// weaker word inside it.
func scoreKey(leaf string) int {
	key := strings.ToLower(leaf)
	best, matched := 0, false
	for _, entry := range keyScores {
		if strings.Contains(key, entry.key) {
			if !matched || entry.score > best {
				best, matched = entry.score, true
			}
		}
	}
	return best
}

// depthPenalty prefers a figure that sits near the top of the document: a balance
// is something a vendor states plainly, not something buried under metadata.
func depthPenalty(depth int) int { return depth * 6 }

// valuePenalty pushes away numbers that are almost certainly not money. A value
// past 1e9 is an epoch timestamp in seconds far more often than it is a balance;
// a negative value is possible (an account in debt) but rarer than a sentinel.
func valuePenalty(v float64) int {
	penalty := 0
	if v >= 1e9 {
		penalty += 45
	}
	if v < 0 {
		penalty += 10
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		penalty += 100
	}
	return penalty
}

// keyExpressible reports whether a key can appear in a quota path, so discovery
// never proposes a choice the grammar would then reject.
func keyExpressible(key string) bool {
	if key == "" {
		return false
	}
	// Reuse the grammar's own parser so discovery can never propose a path the
	// declaration would reject.
	if _, err := config.ParseQuotaPath("$" + key); err != nil {
		return false
	}
	return true
}

// strconvIndex renders a list index without importing strconv for one call.
func strconvIndex(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// candidateLabel renders a choice for a picker: the field's name, then what it
// would show. The path stays out of sight.
func candidateLabel(c Candidate) string {
	name := c.Path
	// Strip the marker and show the last named segment; an index alone is not a
	// label anybody can act on.
	body := strings.TrimPrefix(name, "$")
	segs := strings.Split(body, ".")
	label := body
	for i := len(segs) - 1; i >= 0; i-- {
		if !isAllDigits(segs[i]) {
			label = segs[i]
			break
		}
	}
	sign := ""
	if c.Currency != "" {
		sign = currencySigns[c.Currency]
	}
	return label + " · " + groupedNumber(c.Value, sign)
}

// isAllDigits reports whether a path segment is a list index.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// withCurrency fills each candidate's currency from a sibling key, so a picker can
// show "¥110.25" rather than a bare number.
func withCurrency(body []byte, candidates []Candidate) []Candidate {
	if len(candidates) == 0 {
		return candidates
	}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return candidates
	}
	for i := range candidates {
		if code := siblingCurrency(root, candidates[i].Path); code != "" {
			candidates[i].Currency = code
			candidates[i].Label = candidateLabel(candidates[i])
		}
	}
	return candidates
}

// siblingCurrency looks for a currency code beside the field, then at the root.
// Vendors put it either inside the same balance record or once at the top.
func siblingCurrency(root any, path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "$"), ".")
	if len(segs) > 0 {
		parent := segs[:len(segs)-1]
		for _, key := range []string{"currency", "currency_code", "unit"} {
			if v, st := Eval(config.ParsedQuotaPath{Segments: append(append([]string{}, parent...), key)}, root); st == Present {
				if s, ok := Text(v); ok {
					return strings.ToUpper(s)
				}
			}
		}
	}
	for _, key := range []string{"currency", "currency_code"} {
		if v, st := Eval(config.ParsedQuotaPath{Segments: []string{key}}, root); st == Present {
			if s, ok := Text(v); ok {
				return strings.ToUpper(s)
			}
		}
	}
	return ""
}
