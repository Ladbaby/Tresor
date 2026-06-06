package engine

import (
	"encoding/json"
	"testing"
)

func TestRewriteModelInBody(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		outputModel string
		wantModel   string
	}{
		{
			name:        "Replace model",
			body:        `{"model": "gpt-4o", "messages": [{"role": "user", "content": "hi"}]}`,
			outputModel: "claude-sonnet",
			wantModel:   "claude-sonnet",
		},
		{
			name:        "Non-JSON body returned unchanged",
			body:        `not json`,
			outputModel: "new-model",
			wantModel:   "", // can't parse, body returned as-is
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := rewriteModelInBody([]byte(tt.body), tt.outputModel)

			if tt.wantModel == "" && string(result) == tt.body {
				return // Non-JSON case: body returned unchanged
			}

			var parsed map[string]interface{}
			if err := json.Unmarshal(result, &parsed); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			gotModel, ok := parsed["model"].(string)
			if !ok || gotModel != tt.wantModel {
				t.Fatalf("expected model %q, got %q", tt.wantModel, gotModel)
			}
		})
	}
}
