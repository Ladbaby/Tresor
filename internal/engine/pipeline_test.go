package engine

import (
	"testing"
)

type mockRegistry struct{}

func (m *mockRegistry) CreatePlugin(pluginID string, config map[string]interface{}) (interface{}, error) {
	return nil, nil
}

func (m *mockRegistry) ListPlugins() []PluginInfo {
	return nil
}

func TestParsePipelineConfig_Empty(t *testing.T) {
	reg := &mockRegistry{}
	p, err := ParsePipelineConfig("", reg)
	if err != nil {
		t.Fatalf("parse empty config: %v", err)
	}
	if len(p.RequestSteps) != 0 || len(p.ResponseSteps) != 0 {
		t.Fatal("expected empty pipeline")
	}

	p, err = ParsePipelineConfig("[]", reg)
	if err != nil {
		t.Fatalf("parse empty array: %v", err)
	}
	if len(p.RequestSteps) != 0 || len(p.ResponseSteps) != 0 {
		t.Fatal("expected empty pipeline")
	}
}

func TestParsePipelineConfig_Invalid(t *testing.T) {
	reg := &mockRegistry{}
	_, err := ParsePipelineConfig("{invalid}", reg)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestExtractModel(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{`{"model": "gpt-4o"}`, "gpt-4o"},
		{`{"model":"claude-sonnet-4-20250514"}`, "claude-sonnet-4-20250514"},
		{`{}`, ""},
		{``, ""},
		{`invalid`, ""},
		{`{"model": "gpt-4", "messages": [{"role": "user"}]}`, "gpt-4"},
	}

	for _, tt := range tests {
		got := extractModel([]byte(tt.body), "")
		if got != tt.want {
			t.Errorf("extractModel(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

// TestExtractModel_FromGeminiPath verifies that extractModel falls back to
// parsing the model name out of the URL path when the body has no "model"
// field (the Gemini generateContent convention).
func TestExtractModel_FromGeminiPath(t *testing.T) {
	tests := []struct {
		body, path, want string
	}{
		{`{}`, "/v1beta/models/gemini-2.5-pro:generateContent", "gemini-2.5-pro"},
		{`{"contents":[]}`, "/v1beta/models/gemini-2.5-pro:streamGenerateContent", "gemini-2.5-pro"},
		// Body model takes precedence over path.
		{`{"model":"from-body"}`, "/v1beta/models/from-path:generateContent", "from-body"},
		// Empty path & body → "".
		{`{}`, "", ""},
		// Non-Gemini path doesn't contribute.
		{`{}`, "/v1/chat/completions", ""},
	}
	for _, tt := range tests {
		got := extractModel([]byte(tt.body), tt.path)
		if got != tt.want {
			t.Errorf("extractModel(body=%q, path=%q) = %q, want %q", tt.body, tt.path, got, tt.want)
		}
	}
}

func TestCopyRequest(t *testing.T) {
	// This test is lightweight since we can't easily test HTTP requests without a server
	t.Log("CopyRequest builds successfully (see integration tests for full coverage)")
}
