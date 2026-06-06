package store

import (
	"testing"

	"tresor/internal/config"
)

func TestUpsertDownstreams_InsertNew(t *testing.T) {
	s := newTestStore(t)

	downstreams := []config.DownstreamCfg{
		{ID: "ds-new", Name: "New Provider", BaseURL: "https://new.com/v1", APIKey: "sk-new", OutputModelIDs: []string{"gpt-4o"}},
	}
	if err := s.upsertDownstreams(downstreams); err != nil {
		t.Fatalf("upsert downstreams: %v", err)
	}

	ds, err := s.GetDownstream("ds-new")
	if err != nil {
		t.Fatalf("get downstream: %v", err)
	}
	if ds.Name != "New Provider" {
		t.Fatalf("expected name 'New Provider', got %q", ds.Name)
	}
	if len(ds.OutputModelIDs) != 1 || ds.OutputModelIDs[0] != "gpt-4o" {
		t.Fatalf("expected output_model_ids [gpt-4o], got %v", ds.OutputModelIDs)
	}
}

func TestUpsertDownstreams_UpdateExisting(t *testing.T) {
	s := newTestStore(t)

	// Create initial downstream
	ds := &Downstream{ID: "ds-upd", Name: "Old Name", BaseURL: "https://old.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Upsert with updated fields
	downstreams := []config.DownstreamCfg{
		{ID: "ds-upd", Name: "New Name", BaseURL: "https://new.com/v1", APIKey: "sk-updated", OutputModelIDs: []string{"model-a"}},
	}
	if err := s.upsertDownstreams(downstreams); err != nil {
		t.Fatalf("upsert downstreams: %v", err)
	}

	updated, err := s.GetDownstream("ds-upd")
	if err != nil {
		t.Fatalf("get downstream: %v", err)
	}
	if updated.Name != "New Name" {
		t.Fatalf("expected name 'New Name', got %q", updated.Name)
	}
	if updated.APIKey != "sk-updated" {
		t.Fatalf("expected api_key 'sk-updated', got %q", updated.APIKey)
	}
	if len(updated.OutputModelIDs) != 1 || updated.OutputModelIDs[0] != "model-a" {
		t.Fatalf("expected output_model_ids [model-a], got %v", updated.OutputModelIDs)
	}
}

func TestUpsertDownstreams_Empty_NoOp(t *testing.T) {
	s := newTestStore(t)

	err := s.upsertDownstreams(nil)
	if err != nil {
		t.Fatalf("expected nil for empty downstreams, got %v", err)
	}
	err = s.upsertDownstreams([]config.DownstreamCfg{})
	if err != nil {
		t.Fatalf("expected nil for empty slice, got %v", err)
	}
}

func TestUpsertRules_InsertNew(t *testing.T) {
	s := newTestStore(t)

	rules := []config.RuleCfg{
		{ID: "rule-new", Name: "New Rule", PatternPath: "/v1/chat/completions", ActiveDownstream: "ds-test", PipelineConfig: []config.PipelineStep{{PluginID: "custom_header"}}, IsEnabled: true},
	}
	if err := s.upsertRules(rules); err != nil {
		t.Fatalf("upsert rules: %v", err)
	}

	rule, err := s.GetRule("rule-new")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if rule.Name != "New Rule" {
		t.Fatalf("expected name 'New Rule', got %q", rule.Name)
	}
	// pipeline_config should be stored as JSON string
	if rule.PipelineConfig == "" {
		t.Fatal("expected pipeline_config to be set")
	}
}

func TestUpsertRules_UpdateExisting(t *testing.T) {
	s := newTestStore(t)

	// Create initial rule
	rule := &Rule{ID: "rule-upd", Name: "Old Name", PatternPath: "/old/path", PipelineConfig: "[]", IsEnabled: true}
	if err := s.CreateRule(rule); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Upsert with updated fields
	rules := []config.RuleCfg{
		{ID: "rule-upd", Name: "New Name", PatternPath: "/new/path", ActiveDownstream: "ds-test", PipelineConfig: []config.PipelineStep{{PluginID: "openai2anthropic"}}, IsEnabled: false},
	}
	if err := s.upsertRules(rules); err != nil {
		t.Fatalf("upsert rules: %v", err)
	}

	updated, err := s.GetRule("rule-upd")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if updated.Name != "New Name" {
		t.Fatalf("expected name 'New Name', got %q", updated.Name)
	}
	if updated.PatternPath != "/new/path" {
		t.Fatalf("expected pattern_path '/new/path', got %q", updated.PatternPath)
	}
	if updated.IsEnabled {
		t.Fatal("expected is_enabled to be false")
	}
}

func TestUpsertRules_Empty_NoOp(t *testing.T) {
	s := newTestStore(t)

	err := s.upsertRules(nil)
	if err != nil {
		t.Fatalf("expected nil for empty rules, got %v", err)
	}
}

func TestUpsertAliases_InsertAndActivate(t *testing.T) {
	s := newTestStore(t)

	// Create a downstream first (aliases require valid downstreams)
	ds := &Downstream{ID: "ds-alias", Name: "Alias DS", BaseURL: "https://alias.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	groups := []config.AliasGroupCfg{
		{
			InputModelID: "gpt-4o",
			Options: []config.AliasOptionCfg{
				{ID: "alias-opt-1", DownstreamID: "ds-alias", OutputModelID: "claude-sonnet"},
				{ID: "alias-opt-2", DownstreamID: "ds-alias", OutputModelID: "claude-haiku"},
			},
		},
	}
	if err := s.upsertAliases(groups); err != nil {
		t.Fatalf("upsert aliases: %v", err)
	}

	a1, err := s.GetAlias("alias-opt-1")
	if err != nil {
		t.Fatalf("get alias 1: %v", err)
	}
	if !a1.IsActive {
		t.Fatal("expected first option to be active")
	}

	a2, err := s.GetAlias("alias-opt-2")
	if err != nil {
		t.Fatalf("get alias 2: %v", err)
	}
	if a2.IsActive {
		t.Fatal("expected second option to be inactive")
	}
}

func TestUpsertAliases_StaleCleanup(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds-alias", Name: "Alias DS", BaseURL: "https://alias.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create aliases directly in DB (one will be stale after upsert)
	a1 := &Alias{ID: "alias-keep", InputModelID: "gpt-4o", DownstreamID: "ds-alias", OutputModelID: "model-a"}
	a2 := &Alias{ID: "alias-stale", InputModelID: "gpt-4o", DownstreamID: "ds-alias", OutputModelID: "model-b"}
	if err := s.CreateAlias(a1); err != nil {
		t.Fatalf("create alias 1: %v", err)
	}
	if err := s.CreateAlias(a2); err != nil {
		t.Fatalf("create alias 2: %v", err)
	}

	// Upsert with only alias-keep (alias-stale should be cleaned up)
	groups := []config.AliasGroupCfg{
		{
			InputModelID: "gpt-4o",
			Options: []config.AliasOptionCfg{
				{ID: "alias-keep", DownstreamID: "ds-alias", OutputModelID: "model-a-updated"},
			},
		},
	}
	if err := s.upsertAliases(groups); err != nil {
		t.Fatalf("upsert aliases: %v", err)
	}

	// alias-stale should be gone
	_, err := s.GetAlias("alias-stale")
	if err == nil {
		t.Fatal("expected alias-stale to be deleted")
	}

	// alias-keep should exist with updated output_model_id
	a1, err = s.GetAlias("alias-keep")
	if err != nil {
		t.Fatalf("get alias-keep: %v", err)
	}
	if a1.OutputModelID != "model-a-updated" {
		t.Fatalf("expected output_model_id 'model-a-updated', got %q", a1.OutputModelID)
	}
}

func TestLoadConfigData_SeedsDefaultsWhenEmpty(t *testing.T) {
	s := newTestStore(t)

	cfg := &config.AppConfig{
		Downstreams: []config.DownstreamCfg{},
		Rules:       []config.RuleCfg{},
		Aliases:     []config.AliasGroupCfg{},
	}
	if err := s.LoadConfigData(cfg); err != nil {
		t.Fatalf("load config data: %v", err)
	}

	ds, err := s.ListDownstreams()
	if err != nil {
		t.Fatalf("list downstreams: %v", err)
	}
	if len(ds) != 3 {
		t.Fatalf("expected 3 seeded downstreams, got %d", len(ds))
	}

	// Rules are optional — no default rules seeded
	rules, err := s.ListRules()
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected 0 seeded rules, got %d", len(rules))
	}

	// Verify output_model_ids were seeded for each downstream
	for _, d := range ds {
		if len(d.OutputModelIDs) == 0 {
			t.Errorf("downstream %q has no output_model_ids", d.ID)
		}
	}
}

func TestLoadConfigData_MergesWithExisting(t *testing.T) {
	s := newTestStore(t)

	// Create a downstream that won't be in the config
	ds := &Downstream{ID: "ds-existing", Name: "Existing", BaseURL: "https://existing.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Load config with a different downstream
	cfg := &config.AppConfig{
		Downstreams: []config.DownstreamCfg{
			{ID: "ds-config", Name: "From Config", BaseURL: "https://config.com/v1"},
		},
		Rules:   []config.RuleCfg{},
		Aliases: []config.AliasGroupCfg{},
	}
	if err := s.LoadConfigData(cfg); err != nil {
		t.Fatalf("load config data: %v", err)
	}

	// Both downstreams should exist
	dsList, err := s.ListDownstreams()
	if err != nil {
		t.Fatalf("list downstreams: %v", err)
	}
	if len(dsList) != 2 {
		t.Fatalf("expected 2 downstreams (existing + config), got %d", len(dsList))
	}

	// Verify the existing one is preserved
	existing, err := s.GetDownstream("ds-existing")
	if err != nil {
		t.Fatal("existing downstream should still exist after upsert")
	}
	if existing.Name != "Existing" {
		t.Fatalf("expected name 'Existing', got %q", existing.Name)
	}
}

func TestDeleteDownstream_CascadeNullifiesRules(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds-cascade", Name: "Cascade DS", BaseURL: "https://cascade.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	rule := &Rule{ID: "rule-cascade", Name: "Cascade Rule", PatternPath: "/v1/chat/completions", ActiveDownstream: "ds-cascade", PipelineConfig: "[]", IsEnabled: true}
	if err := s.CreateRule(rule); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	if err := s.DeleteDownstream("ds-cascade"); err != nil {
		t.Fatalf("delete downstream: %v", err)
	}

	// Rule should still exist but with nullified active_downstream
	updated, err := s.GetRule("rule-cascade")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if updated.ActiveDownstream != "" {
		t.Fatalf("expected active_downstream to be empty after cascade delete, got %q", updated.ActiveDownstream)
	}
}

func TestDeleteDownstream_CascadeDeletesAliases(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds-alias-cascade", Name: "Alias Cascade DS", BaseURL: "https://cascade.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	alias := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds-alias-cascade", OutputModelID: "claude-sonnet"}
	if err := s.CreateAlias(alias); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	if err := s.DeleteDownstream("ds-alias-cascade"); err != nil {
		t.Fatalf("delete downstream: %v", err)
	}

	// Alias should be deleted
	aliases, err := s.ListAliases()
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("expected 0 aliases after cascade delete, got %d", len(aliases))
	}
}

func TestAddRemoveOutputModelID(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds-models", Name: "Models DS", BaseURL: "https://models.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Add a model
	if err := s.AddOutputModelID("ds-models", "gpt-4o"); err != nil {
		t.Fatalf("add model: %v", err)
	}

	updated, err := s.GetDownstream("ds-models")
	if err != nil {
		t.Fatalf("get downstream: %v", err)
	}
	if len(updated.OutputModelIDs) != 1 || updated.OutputModelIDs[0] != "gpt-4o" {
		t.Fatalf("expected [gpt-4o], got %v", updated.OutputModelIDs)
	}

	// Duplicate add should be ignored (INSERT OR IGNORE)
	if err := s.AddOutputModelID("ds-models", "gpt-4o"); err != nil {
		t.Fatalf("duplicate add should succeed, got %v", err)
	}

	updated, _ = s.GetDownstream("ds-models")
	if len(updated.OutputModelIDs) != 1 {
		t.Fatalf("expected 1 model after duplicate add, got %d", len(updated.OutputModelIDs))
	}

	// Remove the model
	if err := s.RemoveOutputModelID("ds-models", "gpt-4o"); err != nil {
		t.Fatalf("remove model: %v", err)
	}

	updated, _ = s.GetDownstream("ds-models")
	if len(updated.OutputModelIDs) != 0 {
		t.Fatalf("expected 0 models after remove, got %d", len(updated.OutputModelIDs))
	}

	// Remove nonexistent model should error
	err = s.RemoveOutputModelID("ds-models", "nonexistent")
	if err == nil {
		t.Fatal("expected error removing nonexistent model")
	}

	// Add to nonexistent downstream should error
	err = s.AddOutputModelID("nonexistent-ds", "model-x")
	if err == nil {
		t.Fatal("expected error adding model to nonexistent downstream")
	}
}

func TestUpdateRuleEnabled(t *testing.T) {
	s := newTestStore(t)

	rule := &Rule{ID: "rule-enable", Name: "Enable Rule", PatternPath: "/v1/chat/completions", PipelineConfig: "[]", IsEnabled: true}
	if err := s.CreateRule(rule); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Disable it
	if err := s.UpdateRuleEnabled("rule-enable", false); err != nil {
		t.Fatalf("disable rule: %v", err)
	}
	got, _ := s.GetRule("rule-enable")
	if got.IsEnabled {
		t.Fatal("expected rule to be disabled")
	}

	// Re-enable it
	if err := s.UpdateRuleEnabled("rule-enable", true); err != nil {
		t.Fatalf("enable rule: %v", err)
	}
	got, _ = s.GetRule("rule-enable")
	if !got.IsEnabled {
		t.Fatal("expected rule to be enabled")
	}

	// Not found error
	err := s.UpdateRuleEnabled("nonexistent-rule", true)
	if err == nil {
		t.Fatal("expected error for nonexistent rule")
	}
}

func TestUpdateRule(t *testing.T) {
	s := newTestStore(t)

	rule := &Rule{ID: "rule-full-upd", Name: "Old Name", PatternPath: "/old/path", PipelineConfig: "[]", IsEnabled: true}
	if err := s.CreateRule(rule); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Full update
	updated := &Rule{ID: "rule-full-upd", Name: "New Name", PatternPath: "/new/path", PatternModel: "gpt-4o", ActiveDownstream: "ds-test", PipelineConfig: "[\"x\"]", IsEnabled: false}
	if err := s.UpdateRule(updated); err != nil {
		t.Fatalf("update rule: %v", err)
	}

	got, err := s.GetRule("rule-full-upd")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if got.Name != "New Name" {
		t.Fatalf("expected name 'New Name', got %q", got.Name)
	}
	if got.PatternPath != "/new/path" {
		t.Fatalf("expected pattern_path '/new/path', got %q", got.PatternPath)
	}
	if got.IsEnabled {
		t.Fatal("expected is_enabled to be false")
	}

	// Not found error
	err = s.UpdateRule(&Rule{ID: "nonexistent-rule", Name: "X"})
	if err == nil {
		t.Fatal("expected error for nonexistent rule")
	}
}

func TestDeleteGroup(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds-group", Name: "Group DS", BaseURL: "https://group.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create multiple aliases for the same group
	for i, out := range []string{"model-a", "model-b", "model-c"} {
		alias := &Alias{ID: "group-alias-" + string(rune('a'+i)), InputModelID: "gpt-4o", DownstreamID: "ds-group", OutputModelID: out}
		if err := s.CreateAlias(alias); err != nil {
			t.Fatalf("create alias: %v", err)
		}
	}

	n, err := s.DeleteGroup("gpt-4o")
	if err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 deleted aliases, got %d", n)
	}

	// Verify all are gone
	aliases, _ := s.ListAliases()
	matched := 0
	for _, a := range aliases {
		if a.InputModelID == "gpt-4o" {
			matched++
		}
	}
	if matched != 0 {
		t.Fatalf("expected 0 gpt-4o aliases after delete group, got %d", matched)
	}

	// Not found error
	_, err = s.DeleteGroup("nonexistent-model")
	if err == nil {
		t.Fatal("expected error for nonexistent group")
	}
}
