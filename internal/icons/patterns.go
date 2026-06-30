package icons

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern maps a compiled regex to a CDN slug. Lookup is first-match-wins,
// so more specific patterns MUST appear before general ones.
type Pattern struct {
	Regex *regexp.Regexp
	Slug  string
}

// versionSuffix matches a trailing version number (with optional dot-segments)
// like "3", "3.5", "1.7.2", or "20250514" — used to strip "qwen3.5" → "qwen"
// and "claude-sonnet-4-20250514" (when no -/: delimiter precedes it) cleanly.
var versionSuffix = regexp.MustCompile(`[0-9]+(?:[\.-][0-9]+)*$`)

// modelPatterns is the ordered list used to resolve a model ID into a CDN slug.
// Adapted from cherry-studio's combined model+provider pattern tables
// (packages/ui/src/components/icons/registry.ts), collapsed into one list
// because we only render model-side icons.
//
// Slugs target @lobehub/icons-static-svg on jsDelivr.
var modelPatterns = []Pattern{
	// GPT / o-series / embeddings / audio
	{regexp.MustCompile(`(?i)gpt-|o1-|o3-|o4-|chatgpt|dall-e|whisper|tts-|text-embedding-`), "openai"},
	// Claude / Anthropic
	{regexp.MustCompile(`(?i)claude|anthropic`), "claude-color"},
	// Google — Gemini family + Veo + Imagen
	{regexp.MustCompile(`(?i)gemini|veo|imagen`), "gemini-color"},
	// DeepSeek (lobehub ships its own deepseek icon)
	{regexp.MustCompile(`(?i)deepseek`), "deepseek-color"},
	// Meta — Llama has no dedicated slug, fall back to the meta provider icon
	{regexp.MustCompile(`(?i)llama`), "meta-color"},
	// Mistral family (incl. mixtral, pixtral, codestral, ministral, magistral)
	{regexp.MustCompile(`(?i)mistral|pixtral|codestral|ministral|mixtral|magistral`), "mistral-color"},
	// Cohere
	{regexp.MustCompile(`(?i)command-r|command-a|cohere|embed-|rerank-`), "cohere-color"},
	// Grok / xAI
	{regexp.MustCompile(`(?i)grok|xai`), "grok"},
	// Perplexity
	{regexp.MustCompile(`(?i)pplx|sonar|perplexity`), "perplexity-color"},
	// NVIDIA Nemotron
	{regexp.MustCompile(`(?i)nemotron|nvidia`), "nvidia-color"},
	// Microsoft (Phi / Orca / WizardLM)
	{regexp.MustCompile(`(?i)phi-|orca|wizardlm`), "microsoft-color"},
	// Qwen / QwQ / QVQ (Alibaba)
	{regexp.MustCompile(`(?i)qwen|qwq|qvq`), "qwen-color"},
	// GLM / ChatGLM / Cogview / Cogvideo (Zhipu)
	{regexp.MustCompile(`(?i)glm|chatglm|cogview|cogvideo|zhipu`), "zhipu-color"},
	// Doubao / Volcengine / Bytedance
	{regexp.MustCompile(`(?i)doubao|seedream|seedance|skylark`), "doubao-color"},
	// Kimi / Moonshot
	{regexp.MustCompile(`(?i)kimi|moonshot`), "kimi"},
	// IBM Granite
	{regexp.MustCompile(`(?i)ibm|granite`), "ibm"},
	// Stability
	{regexp.MustCompile(`(?i)stable-|sd3|sdxl`), "stability"},
}

// Resolve returns the CDN slug for the given model ID, or "" if no pattern
// matches. Lookup is case-insensitive and first-match-wins.
func Resolve(modelID string) string {
	if modelID == "" {
		return ""
	}
	for _, p := range modelPatterns {
		if p.Regex.MatchString(modelID) {
			return p.Slug
		}
	}
	return ""
}

// firstSegmentFallback extracts the substring of modelID up to (but not
// including) the first "-" or ":" delimiter. If no delimiter is present,
// the whole string (trimmed of leading/trailing whitespace) is used.
//
// Then any trailing ASCII digits are stripped from that first segment so
// versioned vendor names ("gemma4", "qwen3.5", "claude3", "llama3") collapse
// to the bare vendor ("gemma", "qwen", "claude", "llama") before being
// used as a CDN slug. The result is lowercased.
//
// Examples:
//
//	"MiniMax-M2.5"        -> "minimax"
//	"minimax-m2.5"        -> "minimax"
//	"my-custom-model-xyz" -> "my"
//	"gemma4:e4b"          -> "gemma"
//	"qwen3.5:9b"          -> "qwen"   (trailing digits stripped, then non-alphanumeric chars)
//	"qwen-max"            -> "qwen"
//	"foo"                 -> "foo"
//	"   -   "             -> ""
//	""                    -> ""
//
// We only return a non-empty slug when at least one ASCII letter (not
// just digits) is present in the first segment, so a bare "4" or
// whitespace doesn't accidentally hit a CDN root path.
func firstSegmentFallback(modelID string) string {
	s := strings.TrimSpace(modelID)
	if s == "" {
		return ""
	}
	if i := firstSegmentCut(s); i >= 0 {
		s = s[:i]
	}
	// Strip any trailing version number (incl. dot-separated like "3.5").
	// Examples: "qwen3" → "qwen", "qwen3.5" → "qwen", "gemma4:e4b" → "gemma4"
	// (the ":e4b" was cut above, leaving "gemma4", which then becomes "gemma").
	s = versionSuffix.ReplaceAllString(s, "")
	// Strip any remaining trailing non-alphanumeric chars (e.g. a leftover
	// "-" from the delimiter cut) so we don't produce malformed slugs.
	for len(s) > 0 {
		c := s[len(s)-1]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			break
		}
		s = s[:len(s)-1]
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	hasLetter := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return ""
	}
	return s
}

// firstSegmentCut returns the byte index of the first "-" or ":" in s, or
// -1 if neither is present.
func firstSegmentCut(s string) int {
	i := strings.IndexAny(s, "-:")
	if i < 0 {
		return -1
	}
	return i
}

// CandidateSlugs returns the ordered list of slug candidates to try for a
// model ID. The first entry is what the pattern table resolves (when there
// is a hard-coded match). The remaining entries are fallback slugs derived
// from the model name's first segment, with the "-color" variant tried
// FIRST so vendors that ship a colored icon get it; vendors that only ship
// a flat icon degrade to the non-color slug via the existing 404-miss
// fall-through in Icon().
//
// Dedupe rules:
//   - Empty slugs and case-insensitive duplicates are removed.
//   - When the hard-coded primary is the "-color" twin of the first-segment
//     fallback (e.g. primary="claude-color", fb="claude"), the fallback is
//     skipped entirely — the user already committed to the color version.
//   - When the hard-coded primary is the exact same slug as the fallback
//     (e.g. primary="kimi", fb="kimi"), the fallback is also skipped.
func CandidateSlugs(modelID string) []string {
	if modelID == "" {
		return nil
	}

	primary := Resolve(modelID)
	fb := firstSegmentFallback(modelID)

	seen := map[string]bool{}
	out := make([]string, 0, 3)
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	if primary != "" {
		add(primary)
	}

	// Whether to even consider the fallback
	considerFallback := fb != "" && !strings.EqualFold(fb, primary)
	if considerFallback {
		// Suppress fallback when the hard-coded primary is just the
		// color twin of the first segment — the user already chose color.
		basePrimary := strings.TrimSuffix(primary, "-color")
		if primary != "" && strings.EqualFold(fb, basePrimary) {
			considerFallback = false
		}
	}
	if considerFallback {
		// Color variant first per the user's request, then the flat slug.
		add(fb + "-color")
		add(fb)
	}

	return out
}

// cdnTemplate is the URL pattern for fetching an icon by slug.
// @lobehub/icons-static-svg is published on npm and mirrored on jsDelivr;
// coverage of LLM vendors is far better than simple-icons (which 404s for
// deepseek, qwen, doubao, kimi, zhipu, moonshot, etc.).
const cdnTemplate = "https://cdn.jsdelivr.net/npm/@lobehub/icons-static-svg@latest/icons/%s.svg"

// CDNURL builds the full URL for a slug. The slug is lowercased defensively,
// though all slugs in modelPatterns are already lowercase.
func CDNURL(slug string) string {
	return fmt.Sprintf(cdnTemplate, strings.ToLower(slug))
}
