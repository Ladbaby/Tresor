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

	// Create downstream first (needed for match_downstreams validation)
	ds := &Downstream{Name: "Test", BaseURL: "https://test.com", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create
	r := &Rule{
		Name:               "test-rule",
		PatternPath:        "/v1/chat/completions",
		PatternModel:       "gpt-4o",
		MatchDownstreams:   []string{ds.ID},
		PipelineConfig:     `[{"plugin_id":"custom_header"}]`,
		IsEnabled:          true,
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

	// Update: change match_downstreams
	ds2 := &Downstream{Name: "Test2", BaseURL: "https://test2.com", ApiFormats: []string{"anthropic"}}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream2: %v", err)
	}
	got.MatchDownstreams = []string{ds2.ID}
	if err := s.UpdateRule(got); err != nil {
		t.Fatalf("update rule: %v", err)
	}
	got, _ = s.GetRule(r.ID)
	if len(got.MatchDownstreams) != 1 || got.MatchDownstreams[0] != ds2.ID {
		t.Fatalf("expected match_downstreams [%s], got %v", ds2.ID, got.MatchDownstreams)
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
		Name:     "Test Provider",
		BaseURL:  "https://test.api.com/v1",
		APIKey:   "sk-test123",
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

func TestStore_FindMatchingRules(t *testing.T) {
	s := newTestStore(t)

	// Create downstreams
	ds1 := &Downstream{Name: "OpenAI", BaseURL: "https://openai.com/v1", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	ds2 := &Downstream{Name: "Anthropic", BaseURL: "https://anthropic.com", ApiFormats: []string{"anthropic"}}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create rules
	rules := []*Rule{
		{Name: "openai-chat", PatternPath: "/v1/chat/completions", MatchDownstreams: []string{ds1.ID}, PipelineConfig: "[]", IsEnabled: true},
		{Name: "wildcard", PatternPath: "*", MatchDownstreams: []string{ds2.ID}, PipelineConfig: "[]", IsEnabled: true},
		{Name: "disabled", PatternPath: "/v1/models", MatchDownstreams: []string{ds1.ID}, PipelineConfig: "[]", IsEnabled: false},
	}
	for _, r := range rules {
		if err := s.CreateRule(r); err != nil {
			t.Fatalf("create rule %s: %v", r.Name, err)
		}
	}

	// Match exact (openai format, openai downstream)
	matches, err := s.FindMatchingRules("/v1/chat/completions", "", "openai", ds1.ID, []string{"openai"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Name != "openai-chat" {
		t.Fatalf("expected 'openai-chat', got %q", matches[0].Name)
	}

	// Match wildcard (no exact match)
	matches, err = s.FindMatchingRules("/v1/completions", "", "anthropic", ds2.ID, []string{"anthropic"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Name != "wildcard" {
		t.Fatalf("expected 'wildcard', got %q", matches[0].Name)
	}

	// Disabled rule should not match, but wildcard will (use ds2 to match wildcard's downstream filter)
	matches, err = s.FindMatchingRules("/v1/models", "", "anthropic", ds2.ID, []string{"anthropic"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (wildcard), got %d", len(matches))
	}
	if matches[0].Name != "wildcard" {
		t.Fatalf("expected wildcard match, got %q", matches[0].Name)
	}

	// Wildcard matches unknown path
	matches, err = s.FindMatchingRules("/some/unknown/path", "", "anthropic", ds2.ID, []string{"anthropic"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Name != "wildcard" {
		t.Fatalf("expected wildcard, got %q", matches[0].Name)
	}
}

func TestStore_FindMatchingRules_NoMatch(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{Name: "Test", BaseURL: "https://test.com", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	r := &Rule{
		Name:               "unrelated",
		PatternPath:        "/v1/chat/completions",
		PatternModel:       "gpt-4o",
		MatchDownstreams:   []string{ds.ID},
		PipelineConfig:     "[]",
		IsEnabled:          true,
	}
	if err := s.CreateRule(r); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	matches, err := s.FindMatchingRules("/v1/completions", "", "openai", ds.ID, []string{"openai"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestStore_FindMatchingRules_ModelPriority(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{Name: "Test", BaseURL: "https://test.com", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	r1 := &Rule{
		Name:               "path-only",
		PatternPath:        "/v1/chat/completions",
		PatternModel:       "",
		MatchDownstreams:   []string{ds.ID},
		PipelineConfig:     "[]",
		IsEnabled:          true,
	}
	r2 := &Rule{
		Name:               "model-specific",
		PatternPath:        "/v1/chat/completions",
		PatternModel:       "gpt-4o",
		MatchDownstreams:   []string{ds.ID},
		PipelineConfig:     "[]",
		IsEnabled:          true,
	}
	if err := s.CreateRule(r1); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if err := s.CreateRule(r2); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Looking up with a model should match the model-specific rule first
	matches, err := s.FindMatchingRules("/v1/chat/completions", "gpt-4o", "openai", ds.ID, []string{"openai"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches (both rules), got %d", len(matches))
	}
	if matches[0].Name != "model-specific" {
		t.Fatalf("expected 'model-specific' to beat 'path-only', got %q", matches[0].Name)
	}
	if matches[1].Name != "path-only" {
		t.Fatalf("expected 'path-only' second, got %q", matches[1].Name)
	}
}

func TestFindMatchingRules_FormatFilter(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{Name: "Test", BaseURL: "https://test.com", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Rule that only matches anthropic format
	r := &Rule{
		Name:               "anthropic-only",
		PatternPath:        "/v1/messages",
		MatchFormat:        []string{"anthropic"},
		MatchDownstreams:   []string{ds.ID},
		PipelineConfig:     "[]",
		IsEnabled:          true,
	}
	if err := s.CreateRule(r); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// OpenAI format should NOT match
	matches, err := s.FindMatchingRules("/v1/messages", "", "openai", ds.ID, []string{"openai"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (format filter should exclude openai), got %d", len(matches))
	}

	// Anthropoc format SHOULD match
	matches, err = s.FindMatchingRules("/v1/messages", "", "anthropic", ds.ID, []string{"openai"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
}

func TestFindMatchingRules_DownstreamFilter(t *testing.T) {
	s := newTestStore(t)

	ds1 := &Downstream{Name: "OpenAI", BaseURL: "https://openai.com/v1", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	ds2 := &Downstream{Name: "Anthropic", BaseURL: "https://anthropic.com", ApiFormats: []string{"anthropic"}}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Rule that only matches ds1
	r := &Rule{
		Name:               "openai-only",
		PatternPath:        "/v1/chat/completions",
		MatchDownstreams:   []string{ds1.ID},
		PipelineConfig:     "[]",
		IsEnabled:          true,
	}
	if err := s.CreateRule(r); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// ds1 should match
	matches, err := s.FindMatchingRules("/v1/chat/completions", "", "openai", ds1.ID, []string{"openai"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	// ds2 should NOT match
	matches, err = s.FindMatchingRules("/v1/chat/completions", "", "openai", ds2.ID, []string{"anthropic"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (downstream filter should exclude ds2), got %d", len(matches))
	}
}

func TestFindMatchingRules_DownstreamFormatFilter(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{Name: "Test", BaseURL: "https://test.com", ApiFormats: []string{"openai"}}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Rule that only matches anthropic downstream format
	r := &Rule{
		Name:                 "anthropic-format-only",
		PatternPath:         "/v1/chat/completions",
		MatchDownstreamFmt:  []string{"anthropic"},
		PipelineConfig:      "[]",
		IsEnabled:           true,
	}
	if err := s.CreateRule(r); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// OpenAI format downstream should NOT match
	matches, err := s.FindMatchingRules("/v1/chat/completions", "", "openai", ds.ID, []string{"openai"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (downstream format filter should exclude openai), got %d", len(matches))
	}
}

func TestFindMatchingRules_Combo(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{Name: "Test", BaseURL: "https://test.com", ApiFormats: []string{"openai", "anthropic"}}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Rule with all three filters: match_format=[openai], match_downstream_format=[openai], match_downstreams=[ds.ID]
	r := &Rule{
		Name:                 "combo-rule",
		PatternPath:         "/v1/chat/completions",
		MatchFormat:         []string{"openai"},
		MatchDownstreamFmt:  []string{"openai"},
		MatchDownstreams:    []string{ds.ID},
		PipelineConfig:      "[]",
		IsEnabled:           true,
	}
	if err := s.CreateRule(r); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// All conditions met: openai input format, openai downstream format, correct downstream
	matches, err := s.FindMatchingRules("/v1/chat/completions", "", "openai", ds.ID, []string{"openai", "anthropic"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	// Wrong input format (anthropic instead of openai)
	matches, err = s.FindMatchingRules("/v1/chat/completions", "", "anthropic", ds.ID, []string{"openai", "anthropic"})
	if err != nil {
		t.Fatalf("find matching rules: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (input format doesn't match), got %d", len(matches))
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

func TestSessions_SaveLoadDelete(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveSessionToken("tok-a"); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := s.SaveSessionToken("tok-b"); err != nil {
		t.Fatalf("save b: %v", err)
	}
	// Saving the same token again must be a no-op (INSERT OR IGNORE).
	if err := s.SaveSessionToken("tok-a"); err != nil {
		t.Fatalf("save a again: %v", err)
	}

	tokens, err := s.LoadAllSessionTokens()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d: %v", len(tokens), tokens)
	}
	got := map[string]bool{}
	for _, tok := range tokens {
		got[tok] = true
	}
	if !got["tok-a"] || !got["tok-b"] {
		t.Fatalf("missing tokens, got %v", tokens)
	}

	if err := s.DeleteSessionToken("tok-a"); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	tokens, err = s.LoadAllSessionTokens()
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if len(tokens) != 1 || tokens[0] != "tok-b" {
		t.Fatalf("expected only tok-b, got %v", tokens)
	}
}

func TestSessions_DeleteEmptyRemovesAll(t *testing.T) {
	s := newTestStore(t)

	for _, tok := range []string{"a", "b", "c"} {
		if err := s.SaveSessionToken(tok); err != nil {
			t.Fatalf("save %s: %v", tok, err)
		}
	}

	if err := s.DeleteSessionToken(""); err != nil {
		t.Fatalf("delete all: %v", err)
	}

	tokens, err := s.LoadAllSessionTokens()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("expected 0 tokens, got %v", tokens)
	}
}

func TestSessions_PersistAcrossInstances(t *testing.T) {
	// Open store A, write tokens, close it.
	f, err := os.CreateTemp("", "tresor-sessions-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	storeA, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	if err := storeA.SaveSessionToken("survivor-1"); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := storeA.SaveSessionToken("survivor-2"); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if err := storeA.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}

	// Reopen the same DB file — both tokens must still be there.
	storeB, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer storeB.Close()

	tokens, err := storeB.LoadAllSessionTokens()
	if err != nil {
		t.Fatalf("load B: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens to survive restart, got %d: %v", len(tokens), tokens)
	}
}
