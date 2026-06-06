package store

import (
	"testing"
)

func TestStore_CRUD_Aliases(t *testing.T) {
	s := newTestStore(t)

	// Create a downstream first for the alias to reference
	ds := &Downstream{
		ID:    "test-ds",
		Name:  "Test DS",
		BaseURL: "https://test.api.com/v1",
	}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create alias
	a := &Alias{
		InputModelID:  "gpt-4o",
		DownstreamID:  "test-ds",
		OutputModelID: "gpt-4o",
		IsActive:      true,
	}
	if err := s.CreateAlias(a); err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if a.ID == "" {
		t.Fatal("expected alias ID to be set")
	}

	// Read
	got, err := s.GetAlias(a.ID)
	if err != nil {
		t.Fatalf("get alias: %v", err)
	}
	if got.InputModelID != a.InputModelID {
		t.Fatalf("expected input_model_id %q, got %q", a.InputModelID, got.InputModelID)
	}
	if !got.IsActive {
		t.Fatal("expected alias to be active")
	}

	// List
	aliases, err := s.ListAliases()
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if len(aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d", len(aliases))
	}

	// Update
	got.OutputModelID = "gpt-4o-2024-05-13"
	if err := s.UpdateAlias(got); err != nil {
		t.Fatalf("update alias: %v", err)
	}
	got2, _ := s.GetAlias(a.ID)
	if got2.OutputModelID != "gpt-4o-2024-05-13" {
		t.Fatalf("expected updated output_model_id, got %q", got2.OutputModelID)
	}

	// Delete
	if err := s.DeleteAlias(a.ID); err != nil {
		t.Fatalf("delete alias: %v", err)
	}
	_, err = s.GetAlias(a.ID)
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestStore_Alias_Activation(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com/v1"}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create two aliases for the same input model group
	a1 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds1", OutputModelID: "gpt-4o", IsActive: true}
	if err := s.CreateAlias(a1); err != nil {
		t.Fatalf("create alias 1: %v", err)
	}

	a2 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds2", OutputModelID: "claude-sonnet"}
	if err := s.CreateAlias(a2); err != nil {
		t.Fatalf("create alias 2: %v", err)
	}

	// Verify a1 is active, a2 is not
	got1, _ := s.GetAlias(a1.ID)
	got2, _ := s.GetAlias(a2.ID)
	if !got1.IsActive {
		t.Fatal("expected a1 to be active")
	}
	if got2.IsActive {
		t.Fatal("expected a2 to NOT be active")
	}

	// Activate a2 — should deactivate a1
	if err := s.ActivateAlias(a2.ID); err != nil {
		t.Fatalf("activate alias 2: %v", err)
	}

	got1, _ = s.GetAlias(a1.ID)
	got2, _ = s.GetAlias(a2.ID)
	if got1.IsActive {
		t.Fatal("expected a1 to be deactivated after activating a2")
	}
	if !got2.IsActive {
		t.Fatal("expected a2 to be active")
	}
}

func TestStore_CreateAlias_AutoDeactivate(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com/v1"}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create first alias as active
	a1 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds1", OutputModelID: "gpt-4o", IsActive: true}
	if err := s.CreateAlias(a1); err != nil {
		t.Fatalf("create alias 1: %v", err)
	}

	// Create second alias for same group as active — should auto-deactivate a1
	a2 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds2", OutputModelID: "claude-sonnet", IsActive: true}
	if err := s.CreateAlias(a2); err != nil {
		t.Fatalf("create alias 2: %v", err)
	}

	got1, _ := s.GetAlias(a1.ID)
	if got1.IsActive {
		t.Fatal("expected a1 to be auto-deactivated when a2 was created as active")
	}
}

func TestStore_FindActiveAlias(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	a := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds1", OutputModelID: "gpt-4o", IsActive: true}
	if err := s.CreateAlias(a); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	// Find active for gpt-4o
	got, err := s.FindActiveAlias("gpt-4o")
	if err != nil {
		t.Fatalf("find active alias: %v", err)
	}
	if got == nil {
		t.Fatal("expected to find active alias for gpt-4o")
	}
	if got.OutputModelID != "gpt-4o" {
		t.Fatalf("expected output_model_id 'gpt-4o', got %q", got.OutputModelID)
	}

	// Find active for non-existent model returns nil
	got, err = s.FindActiveAlias("nonexistent")
	if err != nil {
		t.Fatalf("find active alias: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent model")
	}
}

func TestStore_ListGroups(t *testing.T) {
	s := newTestStore(t)

	ds1 := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com/v1"}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com/v1"}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create aliases for two groups
	a1 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds1", OutputModelID: "gpt-4o", IsActive: true}
	if err := s.CreateAlias(a1); err != nil {
		t.Fatalf("create alias 1: %v", err)
	}
	a2 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds2", OutputModelID: "claude-sonnet"}
	if err := s.CreateAlias(a2); err != nil {
		t.Fatalf("create alias 2: %v", err)
	}
	a3 := &Alias{InputModelID: "claude-3", DownstreamID: "ds1", OutputModelID: "gpt-4o", IsActive: true}
	if err := s.CreateAlias(a3); err != nil {
		t.Fatalf("create alias 3: %v", err)
	}

	groups, err := s.ListGroups()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// Check gpt-4o group
	gptGroup := groups[0]
	if gptGroup.InputModelID != "gpt-4o" {
		t.Fatalf("expected first group 'gpt-4o', got %q", gptGroup.InputModelID)
	}
	if len(gptGroup.Options) != 2 {
		t.Fatalf("expected 2 options in gpt-4o group, got %d", len(gptGroup.Options))
	}
	if gptGroup.ActiveID == nil || *gptGroup.ActiveID != a1.ID {
		t.Fatalf("expected active_id to be a1.ID (%s)", a1.ID)
	}

	// Check claude-3 group
	claudeGroup := groups[1]
	if claudeGroup.InputModelID != "claude-3" {
		t.Fatalf("expected second group 'claude-3', got %q", claudeGroup.InputModelID)
	}
	if len(claudeGroup.Options) != 1 {
		t.Fatalf("expected 1 option in claude-3 group, got %d", len(claudeGroup.Options))
	}
	if claudeGroup.ActiveID == nil || *claudeGroup.ActiveID != a3.ID {
		t.Fatalf("expected active_id to be a3.ID (%s)", a3.ID)
	}
}

func TestStore_Alias_InvalidDownstream(t *testing.T) {
	s := newTestStore(t)

	a := &Alias{
		InputModelID:  "gpt-4o",
		DownstreamID:  "nonexistent-ds",
		OutputModelID: "gpt-4o",
	}
	err := s.CreateAlias(a)
	if err == nil {
		t.Fatal("expected error for invalid downstream")
	}
}

func TestStore_DeleteAlias_PromoteSibling(t *testing.T) {
	s := newTestStore(t)

	ds1 := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com/v1"}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream ds1: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com/v1"}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream ds2: %v", err)
	}

	// Create two aliases for the same input model group (a1 active, a2 inactive)
	a1 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds1", OutputModelID: "gpt-4o", IsActive: true}
	if err := s.CreateAlias(a1); err != nil {
		t.Fatalf("create alias 1: %v", err)
	}

	a2 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds2", OutputModelID: "claude-sonnet"}
	if err := s.CreateAlias(a2); err != nil {
		t.Fatalf("create alias 2: %v", err)
	}

	// Verify initial state: a1 active, a2 inactive
	got1, _ := s.GetAlias(a1.ID)
	got2, _ := s.GetAlias(a2.ID)
	if !got1.IsActive {
		t.Fatal("expected a1 to be active before delete")
	}
	if got2.IsActive {
		t.Fatal("expected a2 to NOT be active before delete")
	}

	// Delete the active alias (a1) — should promote a2
	if err := s.DeleteAlias(a1.ID); err != nil {
		t.Fatalf("delete alias 1: %v", err)
	}

	// a1 is gone
	_, err := s.GetAlias(a1.ID)
	if err == nil {
		t.Fatal("expected error getting deleted alias a1")
	}

	// a2 should now be active
	got2, _ = s.GetAlias(a2.ID)
	if !got2.IsActive {
		t.Fatal("expected a2 to be promoted to active after deleting a1")
	}

	// FindActiveAlias should return a2
	active, err := s.FindActiveAlias("gpt-4o")
	if err != nil {
		t.Fatalf("find active alias: %v", err)
	}
	if active == nil {
		t.Fatal("expected to find active alias (a2) for gpt-4o")
	}
	if active.ID != a2.ID {
		t.Fatalf("expected active alias to be a2 (%s), got %s", a2.ID, active.ID)
	}
}

func TestStore_DeleteAlias_LastInGroup(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com/v1"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// Create a single active alias
	a := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds1", OutputModelID: "gpt-4o", IsActive: true}
	if err := s.CreateAlias(a); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	// Delete it — should succeed with no error (no sibling to promote)
	if err := s.DeleteAlias(a.ID); err != nil {
		t.Fatalf("delete alias: %v", err)
	}

	// FindActiveAlias should return nil (no aliases left in group)
	active, err := s.FindActiveAlias("gpt-4o")
	if err != nil {
		t.Fatalf("find active alias: %v", err)
	}
	if active != nil {
		t.Fatal("expected nil active alias when last alias was deleted")
	}
}

func TestStore_DeleteAlias_InactiveNoPromote(t *testing.T) {
	s := newTestStore(t)

	ds1 := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com/v1"}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream ds1: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com/v1"}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream ds2: %v", err)
	}

	// Create two aliases: a1 active, a2 inactive
	a1 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds1", OutputModelID: "gpt-4o", IsActive: true}
	if err := s.CreateAlias(a1); err != nil {
		t.Fatalf("create alias 1: %v", err)
	}

	a2 := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds2", OutputModelID: "claude-sonnet"}
	if err := s.CreateAlias(a2); err != nil {
		t.Fatalf("create alias 2: %v", err)
	}

	// Delete the inactive alias (a2) — should NOT promote anything; a1 stays active
	if err := s.DeleteAlias(a2.ID); err != nil {
		t.Fatalf("delete alias 2: %v", err)
	}

	got1, _ := s.GetAlias(a1.ID)
	if !got1.IsActive {
		t.Fatal("expected a1 to remain active after deleting inactive sibling a2")
	}
}
