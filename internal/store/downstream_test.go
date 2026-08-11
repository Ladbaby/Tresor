package store

import (
	"os"
	"path/filepath"
	"testing"

	"tresor/internal/config"

	"gopkg.in/yaml.v3"
)

func TestDownstream_FormatURLs_RoundTrip(t *testing.T) {
	s := newTestStore(t)

	// Create a downstream with per-format URLs (simulates DeepSeek).
	ds := &Downstream{
		Name:       "DeepSeek",
		BaseURL:    "https://api.deepseek.com",
		ApiFormats: []string{"openai", "anthropic"},
		FormatURLs: map[string]string{
			"anthropic": "https://api.deepseek.com/anthropic",
		},
	}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Read back via GetDownstream
	got, err := s.GetDownstream(ds.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FormatURLs == nil {
		t.Fatal("FormatURLs is nil")
	}
	if got.FormatURLs["anthropic"] != "https://api.deepseek.com/anthropic" {
		t.Fatalf("anthropic URL: got %q, want %q", got.FormatURLs["anthropic"], "https://api.deepseek.com/anthropic")
	}
	if _, ok := got.FormatURLs["openai"]; ok {
		t.Fatal("openai key should not be present (no override set)")
	}

	// Update: add the openai override (mirrors the API patch path).
	got.FormatURLs["openai"] = "https://api.openai.com"
	if err := s.UpdateDownstream(got); err != nil {
		t.Fatalf("update: %v", err)
	}

	got2, err := s.GetDownstream(ds.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got2.FormatURLs["openai"] != "https://api.openai.com" {
		t.Fatalf("openai URL after update: got %q", got2.FormatURLs["openai"])
	}
	if got2.FormatURLs["anthropic"] != "https://api.deepseek.com/anthropic" {
		t.Fatalf("anthropic URL after update: got %q", got2.FormatURLs["anthropic"])
	}

	// Replace the whole map (simulates the API clearing a key).
	got2.FormatURLs = map[string]string{"openai": "https://api.openai.com"}
	if err := s.UpdateDownstream(got2); err != nil {
		t.Fatalf("update (clear anthropic): %v", err)
	}

	got3, err := s.GetDownstream(ds.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if _, ok := got3.FormatURLs["anthropic"]; ok {
		t.Fatal("anthropic key should have been removed by the full-map replacement")
	}
	if got3.FormatURLs["openai"] != "https://api.openai.com" {
		t.Fatalf("openai URL after clear: got %q", got3.FormatURLs["openai"])
	}
}

func TestDownstream_FormatURLs_ListIncludes(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{
		Name:       "Multi",
		BaseURL:    "https://x.com",
		ApiFormats: []string{"openai", "anthropic", "gemini"},
		FormatURLs: map[string]string{
			"anthropic": "https://x.com/a",
			"gemini":    "https://x.com/g",
		},
	}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := s.ListDownstreams()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 downstream, got %d", len(list))
	}
	if list[0].FormatURLs["anthropic"] != "https://x.com/a" {
		t.Fatalf("list anthropic URL: got %q", list[0].FormatURLs["anthropic"])
	}
	if list[0].FormatURLs["gemini"] != "https://x.com/g" {
		t.Fatalf("list gemini URL: got %q", list[0].FormatURLs["gemini"])
	}
}

func TestDownstream_FormatURLs_FindByOutputModel(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{
		ID:          "ds-find",
		Name:        "Find",
		BaseURL:     "https://x.com",
		ApiFormats:  []string{"anthropic"},
		FormatURLs:  map[string]string{"anthropic": "https://x.com/a"},
		OutputModelIDs: []string{"claude-x"},
	}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.FindDownstreamByOutputModel("claude-x")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil {
		t.Fatal("expected downstream, got nil")
	}
	if got.FormatURLs["anthropic"] != "https://x.com/a" {
		t.Fatalf("anthropic URL after find: got %q", got.FormatURLs["anthropic"])
	}
}

func TestDownstream_FormatURLs_NilMapRoundTrip(t *testing.T) {
	s := newTestStore(t)

	// Downstream with no per-format URLs.
	ds := &Downstream{Name: "Plain", BaseURL: "https://plain.com", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetDownstream(ds.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// decodeDownstreamJSON guarantees a non-nil empty map so the JSON
	// response always serializes as {} rather than being omitted.
	if got.FormatURLs == nil {
		t.Fatal("FormatURLs should be a non-nil empty map, got nil")
	}
	if len(got.FormatURLs) != 0 {
		t.Fatalf("FormatURLs should be empty, got %v", got.FormatURLs)
	}
}

func TestDownstream_FormatURLs_YAMLRoundTrip(t *testing.T) {
	s := newTestStore(t)

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	ds := &Downstream{
		Name:       "DeepSeek",
		BaseURL:    "https://api.deepseek.com",
		ApiFormats: []string{"openai", "anthropic"},
		FormatURLs: map[string]string{"anthropic": "https://api.deepseek.com/anthropic"},
	}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create: %v", err)
	}

	cfg := &config.AppConfig{ConfigPath: configPath}
	if err := s.WriteConfig(cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !contains(data, "format_urls:") {
		t.Fatalf("expected format_urls in YAML, got:\n%s", data)
	}

	var parsed config.AppConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Downstreams) != 1 {
		t.Fatalf("expected 1 downstream, got %d", len(parsed.Downstreams))
	}
	if parsed.Downstreams[0].FormatURLs == nil {
		t.Fatal("FormatURLs is nil after YAML round-trip")
	}
	if parsed.Downstreams[0].FormatURLs["anthropic"] != "https://api.deepseek.com/anthropic" {
		t.Fatalf("anthropic URL: got %q", parsed.Downstreams[0].FormatURLs["anthropic"])
	}
}

// contains is a tiny helper to avoid pulling in strings just for one test.
func contains(b []byte, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}
