package store

import (
	"fmt"
)

// EnsureStatus reports what EnsureAliasGroupForModel found or did.
type EnsureStatus string

const (
	// EnsureCovered means the group already lists every provider that claims the
	// model, so routing is already explicit and nothing changed.
	EnsureCovered EnsureStatus = "covered"
	// EnsureExtended means the group existed but was missing one or more
	// providers that claim the model; those providers were added as inactive
	// options, and the active option was left alone so routing did not move.
	EnsureExtended EnsureStatus = "extended"
	// EnsureSingle means fewer than two providers list the model, so there is
	// nothing to disambiguate and no group was created. A one-option group
	// would pin routing and silently exclude providers added later.
	EnsureSingle EnsureStatus = "single"
	// EnsureNoEnabled means the only providers claiming the model are disabled.
	// A disabled provider is invisible to routing, so there is no winner to
	// keep active and no group was created.
	EnsureNoEnabled EnsureStatus = "no-enabled"
	// EnsureCreated means the group was created with the current winner active.
	EnsureCreated EnsureStatus = "created"
)

// EnsureResult is the outcome of EnsureAliasGroupForModel.
type EnsureResult struct {
	Status EnsureStatus `json:"status"`
	// Group is the alias group name, which is the model ID itself.
	Group string `json:"group"`
	// ActiveOptionID is the alias ID of the option left active after the call.
	// Only set when the group was created, or when an inert group was healed.
	ActiveOptionID string `json:"active_option_id,omitempty"`
	// AddedDownstreamIDs lists the providers appended to an existing group.
	AddedDownstreamIDs []string `json:"added_downstream_ids,omitempty"`
}

// sharedModelClaimant is one provider that lists a given model ID.
type sharedModelClaimant struct {
	downstreamID string
	enabled      bool
}

// EnsureAliasGroupForModel makes routing explicit for a modelID that several
// providers list, by maintaining an alias group named after the model with one
// option per claiming provider.
//
// The group disambiguates a model offered by several providers: a non-regex
// alias group is resolved before a provider's own output_model_ids, so the
// active option — rather than an implicit provider-ordering race — decides who
// answers.
//
// Two cases:
//   - No group: one is created, listing every claimant with the provider that
//     answers today first and active. Routing is unchanged; the choice merely
//     becomes visible and switchable.
//   - Group exists: any claimant missing from it is appended as an inactive
//     option. Without this, a provider that started offering the model later
//     would be silently shadowed by the group with no trace in the UI.
//
// The winner is the first enabled claimant, ordered exactly as
// FindDownstreamByOutputModel orders candidates, so "who answers today" is the
// same provider before and after the call.
//
// An existing group that is inert (default mode, no active option) is skipped by
// routing entirely and falls back to the ambiguous provider order this feature
// exists to remove. Such a group is activated at the winner, which leaves the
// responding provider exactly as it was.
//
// Regex groups are left untouched: their options and announced names are
// deliberate user structure, and appending a plain model ID to one would blur
// that.
//
// Concurrency: two simultaneous calls for the same model can race past the
// group lookup. The loser's inserts collide on the primary key and its rollback
// removes only what it added. Re-running is harmless, and a personal-use
// gateway does not warrant locking for it.
func (s *Store) EnsureAliasGroupForModel(modelID string) (EnsureResult, error) {
	claimants, err := s.listSharedModelClaimants(modelID)
	if err != nil {
		return EnsureResult{}, err
	}

	winner := ""
	for _, c := range claimants {
		if c.enabled {
			winner = c.downstreamID
			break
		}
	}

	group, err := s.findGroup(modelID)
	if err != nil {
		return EnsureResult{}, err
	}
	if group != nil {
		return s.extendAliasGroup(group, claimants, winner)
	}

	if len(claimants) < 2 {
		return EnsureResult{Status: EnsureSingle, Group: modelID}, nil
	}
	if winner == "" {
		return EnsureResult{Status: EnsureNoEnabled, Group: modelID}, nil
	}

	// Winner first, so CreateAlias assigns it option_order 0 and its is_active
	// flag makes it the group's sole active option.
	ordered := make([]string, 0, len(claimants))
	ordered = append(ordered, winner)
	for _, c := range claimants {
		if c.downstreamID != winner {
			ordered = append(ordered, c.downstreamID)
		}
	}

	var activeID string
	for i, dsID := range ordered {
		a := &Alias{
			InputModelID:  modelID,
			DownstreamID:  dsID,
			OutputModelID: modelID,
			IsActive:      i == 0,
		}
		if err := s.CreateAlias(a); err != nil {
			// Never leave a partial group behind: a group with missing options
			// misroutes or misleads. Known to be brand new here, so deleting it
			// cannot destroy user setup.
			if _, delErr := s.DeleteGroup(modelID); delErr != nil {
				return EnsureResult{}, fmt.Errorf("create alias group %q: %w (rollback also failed: %v)", modelID, err, delErr)
			}
			return EnsureResult{}, fmt.Errorf("create alias group %q: %w", modelID, err)
		}
		if i == 0 {
			activeID = a.ID
		}
	}

	return EnsureResult{Status: EnsureCreated, Group: modelID, ActiveOptionID: activeID}, nil
}

// extendAliasGroup appends the claimants missing from an existing group. It
// never reorders existing options, and only touches the active flag when the
// group was inert.
func (s *Store) extendAliasGroup(group *AliasGroup, claimants []sharedModelClaimant, winner string) (EnsureResult, error) {
	modelID := group.InputModelID
	if group.IsRegex {
		return EnsureResult{Status: EnsureCovered, Group: modelID}, nil
	}

	listed := make(map[string]bool, len(group.Options))
	for _, o := range group.Options {
		listed[o.DownstreamID] = true
	}
	var missing []string
	for _, c := range claimants {
		if !listed[c.downstreamID] {
			missing = append(missing, c.downstreamID)
		}
	}

	// A default-mode group with no active option is inert: routing skips it.
	// Non-default modes select their own candidates and reject explicit
	// activation, so they are never healed here.
	inert := group.RouteMode == "default" && group.ActiveID == nil && winner != ""

	if len(missing) == 0 && !inert {
		return EnsureResult{Status: EnsureCovered, Group: modelID}, nil
	}

	added := make([]string, 0, len(missing))
	rollback := func(cause error) (EnsureResult, error) {
		// Remove only what this call added: the rest of the group is user setup
		// that must survive a partial failure.
		for _, id := range added {
			_ = s.DeleteAlias(id)
		}
		return EnsureResult{}, fmt.Errorf("extend alias group %q: %w", modelID, cause)
	}
	for _, dsID := range missing {
		a := &Alias{
			InputModelID:  modelID,
			DownstreamID:  dsID,
			OutputModelID: modelID,
		}
		if err := s.CreateAlias(a); err != nil {
			return rollback(err)
		}
		added = append(added, a.ID)
	}

	result := EnsureResult{
		Status:             EnsureExtended,
		Group:              modelID,
		AddedDownstreamIDs: missing,
	}
	if inert {
		// Activate whichever option belongs to the current winner, whether it
		// was already in the group or was just appended.
		refreshed, err := s.findGroup(modelID)
		if err != nil {
			return rollback(err)
		}
		if refreshed != nil {
			for _, o := range refreshed.Options {
				if o.DownstreamID == winner {
					if err := s.ActivateAlias(o.ID); err != nil {
						return rollback(err)
					}
					result.ActiveOptionID = o.ID
					break
				}
			}
		}
	}
	return result, nil
}

// findGroup returns the alias group named modelID, or nil when none exists.
func (s *Store) findGroup(modelID string) (*AliasGroup, error) {
	groups, err := s.ListGroups()
	if err != nil {
		return nil, err
	}
	for i := range groups {
		if groups[i].InputModelID == modelID {
			return &groups[i], nil
		}
	}
	return nil, nil
}

// listSharedModelClaimants returns every provider that lists modelID, ordered so
// the first enabled entry is the provider FindDownstreamByOutputModel would
// pick. Disabled providers are included: they do not route, but they still claim
// the model, and omitting them would hide them from the user once re-enabled.
func (s *Store) listSharedModelClaimants(modelID string) ([]sharedModelClaimant, error) {
	rows, err := s.db.Query(
		`SELECT d.id, d.is_enabled
		 FROM downstreams d
		 JOIN output_model_ids o ON o.downstream_id = d.id
		 WHERE o.model_id = ?
		 ORDER BY d.created_at ASC, d.id ASC`, modelID)
	if err != nil {
		return nil, fmt.Errorf("list providers for model %q: %w", modelID, err)
	}
	defer rows.Close()

	var claimants []sharedModelClaimant
	for rows.Next() {
		var c sharedModelClaimant
		var enabled int
		if err := rows.Scan(&c.downstreamID, &enabled); err != nil {
			return nil, fmt.Errorf("scan provider for model %q: %w", modelID, err)
		}
		c.enabled = enabled == 1
		claimants = append(claimants, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list providers for model %q: %w", modelID, err)
	}
	return claimants, nil
}
