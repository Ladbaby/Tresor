package icons

import "testing"

func TestResolve(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		// GPT family (user-merged gpt-5/gpt-3 etc. into a single pattern with openai slug)
		{"gpt-4o", "openai"},
		{"gpt-4o-mini", "openai"},
		{"gpt-4-turbo", "openai"},
		{"gpt-3.5-turbo", "openai"},
		{"o1-preview", "openai"},
		{"o3-mini", "openai"},
		{"text-embedding-3-large", "openai"},
		{"gpt-5", "openai"},
		{"gpt-5.1", "openai"},
		{"gpt-5.1-codex", "openai"},
		{"gpt-5.1-codex-mini", "openai"},
		// Claude → color twin
		{"claude-sonnet-4-20250514", "claude-color"},
		{"claude-haiku-4.5", "claude-color"},
		{"claude-opus-4-7", "claude-color"},
		// Gemini → color twin
		{"gemini-2.5-pro", "gemini-color"},
		{"gemini-2.5-flash-image", "gemini-color"},
		// DeepSeek → color twin
		{"deepseek-chat", "deepseek-color"},
		{"deepseek-reasoner", "deepseek-color"},
		{"DeepSeek-V3", "deepseek-color"},
		// Llama → meta-color
		{"llama-3.1-70b", "meta-color"},
		{"Llama-3.3-70B-Instruct", "meta-color"},
		// Mistral family → color twin
		{"mistral-large", "mistral-color"},
		{"mixtral-8x7b", "mistral-color"},
		// Cohere → color twin
		{"command-r-plus", "cohere-color"},
		{"embed-english-v3.0", "cohere-color"},
		// Grok / xAI — flat icon (no -color variant ships on the CDN for these)
		{"grok-2", "grok"},
		{"grok-beta", "grok"},
		// Perplexity → color twin
		{"sonar-pro", "perplexity-color"},
		// Chinese labs (user-removed hunyuan/baichuan/internlm/yi-/moonshot split from kimi;
		// kimi keeps a flat slug)
		{"qwen-max", "qwen-color"},
		{"qwq-32b", "qwen-color"},
		{"glm-4-plus", "zhipu-color"},
		{"chatglm-6b", "zhipu-color"},
		{"doubao-pro", "doubao-color"},
		{"kimi-k2", "kimi"},
		{"moonshot-v1-128k", "kimi"}, // user merged kimi+moonshot under "kimi"
		// Microsoft
		{"phi-3-medium", "microsoft-color"},
		// NVIDIA
		{"nemotron-70b", "nvidia-color"},
		// IBM / Stability — flat icons
		{"ibm-granite-3b", "ibm"},
		{"sd3", "stability"},
		// Unmatched
		{"my-custom-model-xyz", ""},
		{"", ""},
		{"ollama-llama3", "meta-color"}, // contains "llama" — falls through to meta-color
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := Resolve(tc.model)
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestCDNURL(t *testing.T) {
	got := CDNURL("OpenAI")
	want := "https://cdn.jsdelivr.net/npm/@lobehub/icons-static-svg@latest/icons/openai.svg"
	if got != want {
		t.Errorf("CDNURL(OpenAI) = %q, want %q", got, want)
	}

	// Already-lowercase slug is unchanged
	got = CDNURL("claude-color")
	if got != "https://cdn.jsdelivr.net/npm/@lobehub/icons-static-svg@latest/icons/claude-color.svg" {
		t.Errorf("CDNURL(claude-color) = %q, want lowercased", got)
	}
}

func TestFirstSegmentFallback(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"MiniMax-M2.5", "minimax"},
		{"minimax-m2.5", "minimax"},
		{"BrandNewLab-XYZ-PRO", "brandnewlab"},
		{"single", "single"},
		{"my-custom-model-xyz", "my"},
		{"foo-", "foo"},
		{"-foo", ""},
		{"---", ""},
		{"", ""},
		{"   bar-baz  ", "bar"},
		{"🦀-something", ""}, // no a-z or 0-9 in first segment
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := firstSegmentFallback(tc.model)
			if got != tc.want {
				t.Errorf("firstSegmentFallback(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestCandidateSlugs(t *testing.T) {
	cases := []struct {
		model string
		want  []string
		// why briefly documents the dedupe reasoning
		why string
	}{
		// Hard-coded "claude-color" is the color twin of first segment
		// "claude" → fallback is suppressed (user already chose color).
		{"claude-sonnet-4-20250514", []string{"claude-color"}, "primary=claude-color is color twin of fb=claude"},
		// Hard-coded "openai" + first-segment fallback "gpt". Color variant
		// "gpt-color" doesn't exist on the CDN for some families, so try
		// "gpt-color" first then "gpt" as the graceful-degradation path.
		{"gpt-4o", []string{"openai", "gpt-color", "gpt"}, "primary=openai, fb=gpt"},
		// "qwen-color" is the color twin of first segment "qwen" → fallback suppressed.
		{"qwen-max", []string{"qwen-color"}, "primary=qwen-color is color twin of fb=qwen"},
		// Hard-coded "kimi" (flat slug, no -color twin). First segment is
		// also "kimi", which equals primary, so fallback suppressed.
		{"kimi-k2", []string{"kimi"}, "primary=kimi equals fb=kimi"},
		// Hard-coded "kimi" merged for "moonshot-v1-128k"; fb = "moonshot".
		// basePrimary="kimi" (no -color suffix to strip), so "moonshot-color"
		// then "moonshot" is appended.
		{"moonshot-v1-128k", []string{"kimi", "moonshot-color", "moonshot"}, "primary=kimi, fb=moonshot"},
		// Pure fallback path: no hard-coded primary, so "<seg>-color" first.
		{"MiniMax-M2.5", []string{"minimax-color", "minimax"}, "no primary → just fallback (color first)"},
		{"BrandNewLab-XYZ-PRO", []string{"brandnewlab-color", "brandnewlab"}, "no primary → just fallback"},
		// Empty input → no candidates.
		{"", nil, "empty input"},
		// No "-" delimiter, primary empty → single segment as fallback (with color variant).
		{"singleVendor", []string{"singlevendor-color", "singlevendor"}, "no primary, single segment"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := CandidateSlugs(tc.model)
			if len(got) != len(tc.want) {
				t.Fatalf("CandidateSlugs(%q) = %v (%s), want %v", tc.model, got, tc.why, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("CandidateSlugs(%q)[%d] = %q, want %q  (%s)", tc.model, i, got[i], tc.want[i], tc.why)
				}
			}
		})
	}
}
