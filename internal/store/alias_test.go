package store

import (
	"reflect"
	"testing"
)

func TestStore_CRUD_Aliases(t *testing.T) {
	s := newTestStore(t)

	// Create a downstream first for the alias to reference
	ds := &Downstream{
		ID:    "test-ds",
		Name:  "Test DS",
		BaseURL: "https://test.api.com",
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

	ds := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com"}
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

	ds := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com"}
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

	ds := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com"}
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

	ds1 := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com"}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com"}
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

	ds1 := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com"}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream ds1: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com"}
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

	ds := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com"}
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

	ds1 := &Downstream{ID: "ds1", Name: "DS1", BaseURL: "https://test1.com"}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream ds1: %v", err)
	}
	ds2 := &Downstream{ID: "ds2", Name: "DS2", BaseURL: "https://test2.com"}
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

// --- Announced-names tests ---

// createRegexGroupWithOption is a small helper to spin up a regex alias group
// with a single active option for announced-names testing.
func createRegexGroupWithOption(t *testing.T, s *Store, pattern, downstreamID, outputModelID string, names []string) *Alias {
	t.Helper()
	if err := s.CreateDownstream(&Downstream{ID: downstreamID, Name: downstreamID, BaseURL: "https://x"}); err != nil {
		t.Fatalf("create downstream %s: %v", downstreamID, err)
	}
	a := &Alias{
		InputModelID:   pattern,
		DownstreamID:   downstreamID,
		OutputModelID:  outputModelID,
		IsActive:       true,
		IsRegex:        true,
		AnnouncedNames: names,
	}
	if err := s.CreateAlias(a); err != nil {
		t.Fatalf("create regex alias %q: %v", pattern, err)
	}
	return a
}

func TestStore_AnnouncedNames_Valid(t *testing.T) {
	s := newTestStore(t)
	createRegexGroupWithOption(t, s, `claude-sonnet.*`, "ds-a", "claude-sonnet-4", []string{
		"claude-sonnet-4-5",
		"claude-sonnet-5",
	})

	// Routing: requested name resolves to the active option of the announcing group
	got, err := s.FindActiveAlias("claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("find active alias: %v", err)
	}
	if got == nil {
		t.Fatal("expected announced-name hit, got nil")
	}
	if got.InputModelID != "claude-sonnet.*" {
		t.Fatalf("expected input_model_id %q, got %q", "claude-sonnet.*", got.InputModelID)
	}
	if got.OutputModelID != "claude-sonnet-4" {
		t.Fatalf("expected output_model_id %q, got %q", "claude-sonnet-4", got.OutputModelID)
	}

	// Listing: the AliasGroup exposes announced_names
	groups, err := s.ListGroups()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if !reflect.DeepEqual(groups[0].AnnouncedNames, []string{"claude-sonnet-4-5", "claude-sonnet-5"}) {
		t.Fatalf("unexpected announced_names: %#v", groups[0].AnnouncedNames)
	}
}

func TestStore_AnnouncedNames_RegexMismatch(t *testing.T) {
	s := newTestStore(t)
	ds := &Downstream{ID: "ds-x", Name: "ds-x", BaseURL: "https://x"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	a := &Alias{
		InputModelID:   `^claude-sonnet.*$`,
		DownstreamID:   "ds-x",
		OutputModelID:  "claude-sonnet-4",
		IsActive:       true,
		IsRegex:        true,
		AnnouncedNames: []string{"gpt-4o"},
	}
	if err := s.CreateAlias(a); err == nil {
		t.Fatal("expected error when announced name does not match regex, got nil")
	}
}

func TestStore_AnnouncedNames_DuplicateRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateDownstream(&Downstream{ID: "ds-dup", Name: "ds-dup", BaseURL: "https://x"}); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	a := &Alias{
		InputModelID:   `claude-sonnet.*`,
		DownstreamID:   "ds-dup",
		OutputModelID:  "claude-sonnet-4",
		IsActive:       true,
		IsRegex:        true,
		AnnouncedNames: []string{"claude-sonnet-4-5", "claude-sonnet-4-5"},
	}
	if err := s.CreateAlias(a); err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM aliases`).Scan(&count); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 aliases (duplicate rejected), got %d", count)
	}
}

func TestStore_AnnouncedNames_ConflictWithDownstreamModel(t *testing.T) {
	s := newTestStore(t)
	ds := &Downstream{ID: "ds-c", Name: "ds-c", BaseURL: "https://x"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	if err := s.AddOutputModelID("ds-c", "gpt-4o"); err != nil {
		t.Fatalf("add model id: %v", err)
	}
	a := &Alias{
		InputModelID:   `gpt-.*`,
		DownstreamID:   "ds-c",
		OutputModelID:  "gpt-4o-mini",
		IsActive:       true,
		IsRegex:        true,
		AnnouncedNames: []string{"gpt-4o"},
	}
	if err := s.CreateAlias(a); err == nil {
		t.Fatal("expected conflict error with downstream output_model_id, got nil")
	}
}

func TestStore_AnnouncedNames_ConflictWithNonRegexAlias(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateDownstream(&Downstream{ID: "ds-nr", Name: "ds-nr", BaseURL: "https://x"}); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	// Existing non-regex alias for "gpt-4o"
	if err := s.CreateAlias(&Alias{
		InputModelID:  "gpt-4o",
		DownstreamID:  "ds-nr",
		OutputModelID: "gpt-4o",
		IsActive:      true,
	}); err != nil {
		t.Fatalf("create non-regex alias: %v", err)
	}
	// Try to create a regex alias that announces "gpt-4o"
	a := &Alias{
		InputModelID:   `gpt-.*`,
		DownstreamID:   "ds-nr",
		OutputModelID:  "gpt-4o-mini",
		IsActive:       true,
		IsRegex:        true,
		AnnouncedNames: []string{"gpt-4o"},
	}
	if err := s.CreateAlias(a); err == nil {
		t.Fatal("expected conflict error with non-regex alias, got nil")
	}
}

func TestStore_AnnouncedNames_ConflictWithOtherRegexGroup(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateDownstream(&Downstream{ID: "ds-r1", Name: "ds-r1", BaseURL: "https://x"}); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	// First regex group with pattern claude-.*
	if err := s.CreateAlias(&Alias{
		InputModelID:  `claude-.*`,
		DownstreamID:  "ds-r1",
		OutputModelID: "claude-sonnet",
		IsActive:      true,
		IsRegex:       true,
	}); err != nil {
		t.Fatalf("create first regex group: %v", err)
	}
	// Second regex group whose pattern would match the first group's input_model_id "claude-.*"
	a := &Alias{
		InputModelID:   `claude-opus.*`,
		DownstreamID:   "ds-r1",
		OutputModelID:  "claude-opus",
		IsActive:       true,
		IsRegex:        true,
		AnnouncedNames: []string{"claude-sonnet-4-5"},
	}
	if err := s.CreateAlias(a); err == nil {
		t.Fatal("expected conflict error with another regex group's announced name, got nil")
	}
}

func TestStore_AnnouncedNames_RejectsNonRegexGroup(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateDownstream(&Downstream{ID: "ds-n", Name: "ds-n", BaseURL: "https://x"}); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	a := &Alias{
		InputModelID:   "gpt-4o",
		DownstreamID:   "ds-n",
		OutputModelID:  "gpt-4o",
		IsActive:       true,
		IsRegex:        false,
		AnnouncedNames: []string{"gpt-4o-mini"},
	}
	if err := s.CreateAlias(a); err == nil {
		t.Fatal("expected error for non-regex group with announced_names, got nil")
	}
}

func TestStore_AnnouncedNames_HotSwitchRoutesAnnouncedName(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateDownstream(&Downstream{ID: "ds-h", Name: "ds-h", BaseURL: "https://x"}); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	// Two options in the same regex group
	a1 := &Alias{
		InputModelID:   `claude-.*`,
		DownstreamID:   "ds-h",
		OutputModelID:  "claude-sonnet",
		IsActive:       true,
		IsRegex:        true,
		AnnouncedNames: []string{"claude-x"},
	}
	if err := s.CreateAlias(a1); err != nil {
		t.Fatalf("create a1: %v", err)
	}
	a2 := &Alias{
		InputModelID:  `claude-.*`,
		DownstreamID:  "ds-h",
		OutputModelID: "claude-haiku",
		IsActive:      false,
		IsRegex:       true,
	}
	if err := s.CreateAlias(a2); err != nil {
		t.Fatalf("create a2: %v", err)
	}
	// Active is a1 → announce routes there
	got, _ := s.FindActiveAlias("claude-x")
	if got == nil || got.OutputModelID != "claude-sonnet" {
		t.Fatalf("expected claude-sonnet, got %#v", got)
	}
	// Hot-switch to a2
	if err := s.ActivateAlias(a2.ID); err != nil {
		t.Fatalf("activate a2: %v", err)
	}
	got, _ = s.FindActiveAlias("claude-x")
	if got == nil || got.OutputModelID != "claude-haiku" {
		t.Fatalf("after hot-switch expected claude-haiku, got %#v", got)
	}
}

func TestStore_AnnouncedNames_PropagatesAcrossSiblings(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateDownstream(&Downstream{ID: "ds-p", Name: "ds-p", BaseURL: "https://x"}); err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	a1 := &Alias{
		InputModelID:   `claude-.*`,
		DownstreamID:   "ds-p",
		OutputModelID:  "claude-sonnet",
		IsActive:       true,
		IsRegex:        true,
		AnnouncedNames: []string{"claude-x"},
	}
	if err := s.CreateAlias(a1); err != nil {
		t.Fatalf("create a1: %v", err)
	}
	// Add a sibling — same announced_names should propagate to a1 (and a2)
	a2 := &Alias{
		InputModelID:  `claude-.*`,
		DownstreamID:  "ds-p",
		OutputModelID: "claude-haiku",
		IsActive:      false,
		IsRegex:       true,
	}
	if err := s.CreateAlias(a2); err != nil {
		t.Fatalf("create a2: %v", err)
	}
	got1, _ := s.GetAlias(a1.ID)
	if !reflect.DeepEqual(got1.AnnouncedNames, []string{"claude-x"}) {
		t.Fatalf("expected a1 to retain announced_names after sibling insert, got %#v", got1.AnnouncedNames)
	}
	got2, _ := s.GetAlias(a2.ID)
	if !reflect.DeepEqual(got2.AnnouncedNames, []string{"claude-x"}) {
		t.Fatalf("expected a2 to inherit announced_names, got %#v", got2.AnnouncedNames)
	}
}
