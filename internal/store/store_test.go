package store

import (
	"os"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	f, err := os.CreateTemp("", "tresor-test-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	s, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_Migrate(t *testing.T) {
	s := newTestStore(t)

	// Verify tables exist
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM downstreams").Scan(&count)
	if err != nil {
		t.Fatalf("query downstreams: %v", err)
	}
	err = s.db.QueryRow("SELECT COUNT(*) FROM rules").Scan(&count)
	if err != nil {
		t.Fatalf("query rules: %v", err)
	}
}

func TestStore_SeedDefaults(t *testing.T) {
	s := newTestStore(t)

	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}

	ds, err := s.ListDownstreams()
	if err != nil {
		t.Fatalf("list downstreams: %v", err)
	}
	if len(ds) != 3 {
		t.Fatalf("expected 3 default downstreams, got %d", len(ds))
	}

	// Rules are optional — no default rules seeded
	rules, err := s.ListRules()
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected 0 default rules, got %d", len(rules))
	}

	// Aliases should be seeded
	aliases, err := s.ListAliases()
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if len(aliases) != 3 {
		t.Fatalf("expected 3 default aliases, got %d", len(aliases))
	}

	// Verify output_model_ids were seeded for each downstream
	for _, d := range ds {
		resolved, err := s.FindDownstreamByOutputModel(d.OutputModelIDs[0])
		if err != nil {
			t.Fatalf("find downstream by model %s: %v", d.OutputModelIDs[0], err)
		}
		if resolved == nil {
			t.Fatalf("expected downstream for model %q, got nil", d.OutputModelIDs[0])
		}
	}
}

func TestStore_CRUD_Rules(t *testing.T) {
	s := newTestStore(t)

	// Create
	r := &Rule{
		Name:             "test-rule",
		PatternPath:      "/v1/chat/completions",
		PatternModel:     "gpt-4o",
		ActiveDownstream: "openai-gpt4o",
		PipelineConfig:   `[{"plugin_id":"custom_header"}]`,
		IsEnabled:        true,
	}
	if err := s.CreateRule(r); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if r.ID == "" {
		t.Fatal("expected rule ID to be set")
	}

	// Read
	got, err := s.GetRule(r.ID)
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if got.Name != r.Name {
		t.Fatalf("expected name %q, got %q", r.Name, got.Name)
	}
	if got.PipelineConfig != r.PipelineConfig {
		t.Fatalf("expected pipeline %q, got %q", r.PipelineConfig, got.PipelineConfig)
	}

	// List
	rules, err := s.ListRules()
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}

	// Update downstream
	if err := s.UpdateRuleDownstream(r.ID, "anthropic-sonnet"); err != nil {
		t.Fatalf("update downstream: %v", err)
	}
	got, _ = s.GetRule(r.ID)
	if got.ActiveDownstream != "anthropic-sonnet" {
		t.Fatalf("expected anthropic-sonnet, got %q", got.ActiveDownstream)
	}

	// Delete
	if err := s.DeleteRule(r.ID); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	_, err = s.GetRule(r.ID)
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestStore_CRUD_Downstreams(t *testing.T) {
	s := newTestStore(t)

	// Create
	d := &Downstream{
		Name:    "Test Provider",
		BaseURL: "https://test.api.com/v1",
		APIKey:  "sk-test123",
	}
	if err := s.CreateDownstream(d); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	if d.ID == "" {
		t.Fatal("expected downstream ID to be set")
	}

	// Read
	got, err := s.GetDownstream(d.ID)
	if err != nil {
		t.Fatalf("get downstream: %v", err)
	}
	if got.BaseURL != d.BaseURL {
		t.Fatalf("expected URL %q, got %q", d.BaseURL, got.BaseURL)
	}

	// Update
	d.Name = "Updated Provider"
	if err := s.UpdateDownstream(d); err != nil {
		t.Fatalf("update downstream: %v", err)
	}
	got, _ = s.GetDownstream(d.ID)
	if got.Name != "Updated Provider" {
		t.Fatalf("expected 'Updated Provider', got %q", got.Name)
	}

	// Delete
	if err := s.DeleteDownstream(d.ID); err != nil {
		t.Fatalf("delete downstream: %v", err)
	}
	_, err = s.GetDownstream(d.ID)
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestStore_FindMatchingRule(t *testing.T) {
	s := newTestStore(t)

	// Create rules
	rules := []*Rule{
		{Name: "openai-chat", PatternPath: "/v1/chat/completions", PatternModel: "", ActiveDownstream: "openai-gpt4o", PipelineConfig: "[]", IsEnabled: true},
		{Name: "wildcard", PatternPath: "*", PatternModel: "", ActiveDownstream: "anthropic-sonnet", PipelineConfig: "[]", IsEnabled: true},
		{Name: "disabled", PatternPath: "/v1/models", PatternModel: "", ActiveDownstream: "openai-gpt4o", PipelineConfig: "[]", IsEnabled: false},
	}
	for _, r := range rules {
		if err := s.CreateRule(r); err != nil {
			t.Fatalf("create rule %s: %v", r.Name, err)
		}
	}

	// Match exact
	rule, err := s.FindMatchingRule("/v1/chat/completions", "")
	if err != nil {
		t.Fatalf("find matching rule: %v", err)
	}
	if rule == nil {
		t.Fatal("expected a match")
	}
	if rule.Name != "openai-chat" {
		t.Fatalf("expected 'openai-chat', got %q", rule.Name)
	}

	// Match wildcard (no exact match)
	rule, err = s.FindMatchingRule("/v1/completions", "")
	if err != nil {
		t.Fatalf("find matching rule: %v", err)
	}
	if rule == nil {
		t.Fatal("expected wildcard match")
	}
	if rule.Name != "wildcard" {
		t.Fatalf("expected 'wildcard', got %q", rule.Name)
	}

	// Disabled rule should not match, but wildcard will
	// (since the specific rule is disabled and has no exact-match counterpart)
	rule, err = s.FindMatchingRule("/v1/models", "")
	if err != nil {
		t.Fatalf("find matching rule: %v", err)
	}
	if rule == nil {
		t.Fatal("expected wildcard match since specific rule is disabled")
	}
	if rule.Name != "wildcard" {
		t.Fatalf("expected wildcard match, got %q", rule.Name)
	}

	// Test that wildcard matches when nothing specific exists
	rule, err = s.FindMatchingRule("/some/unknown/path", "")
	if err != nil {
		t.Fatalf("find matching rule: %v", err)
	}
	if rule == nil {
		t.Fatal("expected wildcard match")
	}
	if rule.Name != "wildcard" {
		t.Fatalf("expected wildcard, got %q", rule.Name)
	}
}

func TestStore_FindMatchingRule_NoMatch(t *testing.T) {
	s := newTestStore(t)

	r := &Rule{
		Name:             "unrelated",
		PatternPath:      "/v1/chat/completions",
		PatternModel:     "gpt-4o",
		ActiveDownstream: "openai-gpt4o",
		PipelineConfig:   "[]",
		IsEnabled:        true,
	}
	if err := s.CreateRule(r); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	rule, err := s.FindMatchingRule("/v1/completions", "")
	if err != nil {
		t.Fatalf("find matching rule: %v", err)
	}
	if rule != nil {
		t.Fatalf("expected nil for non-matching path, got rule %q", rule.Name)
	}
}

func TestStore_FindMatchingRule_ModelPriority(t *testing.T) {
	s := newTestStore(t)

	r1 := &Rule{
		Name:             "path-only",
		PatternPath:      "/v1/chat/completions",
		PatternModel:     "",
		ActiveDownstream: "openai-gpt4o",
		PipelineConfig:   "[]",
		IsEnabled:        true,
	}
	r2 := &Rule{
		Name:             "model-specific",
		PatternPath:      "/v1/chat/completions",
		PatternModel:     "gpt-4o",
		ActiveDownstream: "anthropic-sonnet",
		PipelineConfig:   "[]",
		IsEnabled:        true,
	}
	if err := s.CreateRule(r1); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if err := s.CreateRule(r2); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Looking up with a model should match the model-specific rule
	rule, err := s.FindMatchingRule("/v1/chat/completions", "gpt-4o")
	if err != nil {
		t.Fatalf("find matching rule: %v", err)
	}
	if rule == nil {
		t.Fatal("expected a match")
	}
	if rule.Name != "model-specific" {
		t.Fatalf("expected 'model-specific' to beat 'path-only', got %q", rule.Name)
	}
}

func TestStore_FindDownstreamByOutputModel(t *testing.T) {
	s := newTestStore(t)

	// Create downstream with output_model_ids
	d1 := &Downstream{
		Name:    "Provider A",
		BaseURL: "https://a.example.com/v1",
		APIKey:  "key-a",
	}
	if err := s.CreateDownstream(d1); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	for _, m := range []string{"gpt-4o", "gpt-3.5-turbo"} {
		if err := s.AddOutputModelID(d1.ID, m); err != nil {
			t.Fatalf("add model id %s: %v", m, err)
		}
	}

	d2 := &Downstream{
		Name:    "Provider B",
		BaseURL: "https://b.example.com/v1",
		APIKey:  "key-b",
	}
	if err := s.CreateDownstream(d2); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	if err := s.AddOutputModelID(d2.ID, "claude-sonnet"); err != nil {
		t.Fatalf("add model ids: %v", err)
	}

	// Find by a known model
	resolved, err := s.FindDownstreamByOutputModel("gpt-4o")
	if err != nil {
		t.Fatalf("find downstream: %v", err)
	}
	if resolved == nil {
		t.Fatal("expected downstream for gpt-4o")
	}
	if resolved.ID != d1.ID {
		t.Fatalf("expected downstream %s, got %s", d1.ID, resolved.ID)
	}
	if resolved.APIKey != "key-a" {
		t.Fatalf("expected api_key key-a, got %q", resolved.APIKey)
	}

	// Find by another known model (same downstream)
	resolved, err = s.FindDownstreamByOutputModel("gpt-3.5-turbo")
	if err != nil {
		t.Fatalf("find downstream: %v", err)
	}
	if resolved == nil {
		t.Fatal("expected downstream for gpt-3.5-turbo")
	}
	if resolved.ID != d1.ID {
		t.Fatalf("expected downstream %s, got %s", d1.ID, resolved.ID)
	}

	// Find by model on a different downstream
	resolved, err = s.FindDownstreamByOutputModel("claude-sonnet")
	if err != nil {
		t.Fatalf("find downstream: %v", err)
	}
	if resolved == nil {
		t.Fatal("expected downstream for claude-sonnet")
	}
	if resolved.ID != d2.ID {
		t.Fatalf("expected downstream %s, got %s", d2.ID, resolved.ID)
	}

	// Unknown model returns nil, nil
	resolved, err = s.FindDownstreamByOutputModel("unknown-model-xyz")
	if err != nil {
		t.Fatalf("find downstream: %v", err)
	}
	if resolved != nil {
		t.Fatalf("expected nil for unknown model, got downstream %s", resolved.ID)
	}
}

func TestStore_FindDownstreamByOutputModel_Deterministic(t *testing.T) {
	s := newTestStore(t)

	// Two downstreams share the same model — earliest created_at wins
	d1 := &Downstream{
		Name: "Provider A", BaseURL: "https://a.example.com/v1", APIKey: "key-a",
	}
	if err := s.CreateDownstream(d1); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	if err := s.AddOutputModelID(d1.ID, "shared-model"); err != nil {
		t.Fatalf("add model id: %v", err)
	}

	d2 := &Downstream{
		Name: "Provider B", BaseURL: "https://b.example.com/v1", APIKey: "key-b",
	}
	if err := s.CreateDownstream(d2); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	if err := s.AddOutputModelID(d2.ID, "shared-model"); err != nil {
		t.Fatalf("add model ids: %v", err)
	}

	// d1 was created first, so it should win
	resolved, err := s.FindDownstreamByOutputModel("shared-model")
	if err != nil {
		t.Fatalf("find downstream: %v", err)
	}
	if resolved == nil {
		t.Fatal("expected downstream for shared-model")
	}
	if resolved.ID != d1.ID {
		t.Fatalf("expected first-created downstream %s, got %s", d1.ID, resolved.ID)
	}
}
