package store

import (
	"fmt"
	"time"

	"tresor/internal/config"

	"github.com/google/uuid"
)

// Alias represents a single input-model -> output-model mapping entry.
// Multiple aliases sharing the same InputModelID form a group.
type Alias struct {
	ID              string    `json:"id"`
	InputModelID    string    `json:"input_model_id"`
	DownstreamID    string    `json:"downstream_id"`
	DownstreamName  string    `json:"downstream_name,omitempty"`
	OutputModelID   string    `json:"output_model_id"`
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
}

// AliasGroup represents one alias group: an input model and its available options.
type AliasGroup struct {
	InputModelID string  `json:"input_model_id"`
	ActiveID     *string `json:"active_id,omitempty"`
	Options      []Alias `json:"options"`
}

// ListAliases returns all aliases ordered by rowid (insertion order), then created_at.
func (s *Store) ListAliases() ([]Alias, error) {
	rows, err := s.db.Query(
		`SELECT id, input_model_id, downstream_id, output_model_id, is_active, created_at
		 FROM aliases ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()

	var aliases []Alias
	for rows.Next() {
		var a Alias
		var active int
		if err := rows.Scan(&a.ID, &a.InputModelID, &a.DownstreamID, &a.OutputModelID, &active, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.IsActive = active == 1
		aliases = append(aliases, a)
	}
	return aliases, rows.Err()
}

// GetAlias returns a single alias by ID.
func (s *Store) GetAlias(id string) (*Alias, error) {
	var a Alias
	var active int
	err := s.db.QueryRow(
		`SELECT id, input_model_id, downstream_id, output_model_id, is_active, created_at
		 FROM aliases WHERE id = ?`, id).
		Scan(&a.ID, &a.InputModelID, &a.DownstreamID, &a.OutputModelID, &active, &a.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get alias %s: %w", id, err)
	}
	a.IsActive = active == 1
	return &a, nil
}

// CreateAlias inserts a new alias. Validates that the downstream exists.
// If IsActive is true, all other aliases with the same InputModelID are
// deactivated within a transaction.
func (s *Store) CreateAlias(a *Alias) error {
	if a.ID == "" {
		a.ID = uuid.New().String()[:8]
	}

	// Validate downstream exists
	if _, err := s.GetDownstream(a.DownstreamID); err != nil {
		return fmt.Errorf("downstream %s not found: %w", a.DownstreamID, err)
	}

	active := 0
	if a.IsActive {
		active = 1
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// If this alias should be active, deactivate all siblings first
	if a.IsActive {
		if _, err := tx.Exec("UPDATE aliases SET is_active = 0 WHERE input_model_id = ? AND id != ?",
			a.InputModelID, a.ID); err != nil {
			return fmt.Errorf("deactivate sibling aliases: %w", err)
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO aliases (id, input_model_id, downstream_id, output_model_id, is_active)
		 VALUES (?, ?, ?, ?, ?)`,
		a.ID, a.InputModelID, a.DownstreamID, a.OutputModelID, active); err != nil {
		return fmt.Errorf("create alias: %w", err)
	}

	return tx.Commit()
}

// UpdateAlias updates mutable fields of an existing alias.
// If setting IsActive to true, it deactivates all sibling aliases.
func (s *Store) UpdateAlias(a *Alias) error {
	active := 0
	if a.IsActive {
		active = 1
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// If activating, deactivate siblings first
	if a.IsActive {
		if _, err := tx.Exec("UPDATE aliases SET is_active = 0 WHERE input_model_id = ? AND id != ?",
			a.InputModelID, a.ID); err != nil {
			return fmt.Errorf("deactivate sibling aliases: %w", err)
		}
	}

	res, err := tx.Exec(
		`UPDATE aliases SET downstream_id = ?, output_model_id = ?, is_active = ?
		 WHERE id = ?`,
		a.DownstreamID, a.OutputModelID, active, a.ID)
	if err != nil {
		return fmt.Errorf("update alias: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias %s not found", a.ID)
	}

	return tx.Commit()
}

// DeleteAlias removes an alias. If it was the active one for its group,
// the next remaining sibling (by rowid order) is auto-promoted to active.
func (s *Store) DeleteAlias(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Read the alias before deleting it
	var inputModelID string
	var wasActive int
	err = tx.QueryRow(
		`SELECT input_model_id, is_active FROM aliases WHERE id = ?`, id).
		Scan(&inputModelID, &wasActive)
	if err != nil {
		return fmt.Errorf("get alias %s: %w", id, err)
	}

	// Delete the alias
	res, err := tx.Exec("DELETE FROM aliases WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete alias: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias %s not found", id)
	}

	// If it was active, promote the next sibling (by rowid order)
	if wasActive == 1 {
		var nextID string
		err = tx.QueryRow(
			`SELECT id FROM aliases WHERE input_model_id = ? ORDER BY rowid LIMIT 1`,
			inputModelID).Scan(&nextID)
		if err == nil {
			if _, err := tx.Exec("UPDATE aliases SET is_active = 1 WHERE id = ?", nextID); err != nil {
				return fmt.Errorf("promote sibling alias: %w", err)
			}
		}
		// If no sibling exists (err == sql: no rows), the group simply has no active mapping.
	}

	return tx.Commit()
}

// ActivateAlias sets the given alias as active for its group.
// All other aliases sharing the same InputModelID are deactivated.
func (s *Store) ActivateAlias(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Get the alias's input_model_id first
	var inputModelID string
	err = tx.QueryRow("SELECT input_model_id FROM aliases WHERE id = ?", id).Scan(&inputModelID)
	if err != nil {
		return fmt.Errorf("get alias %s: %w", id, err)
	}

	// Deactivate all aliases in this group
	if _, err := tx.Exec("UPDATE aliases SET is_active = 0 WHERE input_model_id = ?", inputModelID); err != nil {
		return fmt.Errorf("deactivate all in group: %w", err)
	}

	// Activate this one
	res, err := tx.Exec("UPDATE aliases SET is_active = 1 WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("activate alias: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias %s not found", id)
	}

	return tx.Commit()
}

// DeleteGroup removes all aliases sharing the same InputModelID.
// Returns the count of deleted aliases. Returns an error if none were found.
func (s *Store) DeleteGroup(inputModelID string) (int, error) {
	res, err := s.db.Exec("DELETE FROM aliases WHERE input_model_id = ?", inputModelID)
	if err != nil {
		return 0, fmt.Errorf("delete alias group %s: %w", inputModelID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete alias group %s: %w", inputModelID, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("alias group %s not found", inputModelID)
	}
	return int(n), nil
}

// FindActiveAlias returns the active alias for a given input model ID.
// Returns nil if no active alias exists for that input model.
func (s *Store) FindActiveAlias(inputModelID string) (*Alias, error) {
	var a Alias
	var active int
	err := s.db.QueryRow(
		`SELECT id, input_model_id, downstream_id, output_model_id, is_active, created_at
		 FROM aliases WHERE input_model_id = ? AND is_active = 1
		 LIMIT 1`, inputModelID).
		Scan(&a.ID, &a.InputModelID, &a.DownstreamID, &a.OutputModelID, &active, &a.CreatedAt)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("find active alias %s: %w", inputModelID, err)
	}
	a.IsActive = active == 1
	return &a, nil
}

// ListGroups returns aliases grouped by InputModelID.
func (s *Store) ListGroups() ([]AliasGroup, error) {
	aliases, err := s.ListAliases()
	if err != nil {
		return nil, err
	}

	groupMap := make(map[string]*AliasGroup)
	var order []string

	for _, a := range aliases {
		g, exists := groupMap[a.InputModelID]
		if !exists {
			g = &AliasGroup{InputModelID: a.InputModelID}
			groupMap[a.InputModelID] = g
			order = append(order, a.InputModelID)
		}
		g.Options = append(g.Options, a)
		if a.IsActive {
			g.ActiveID = &a.ID
		}
	}

	var groups []AliasGroup
	for _, key := range order {
		groups = append(groups, *groupMap[key])
	}

	return groups, nil
}

// upsertAliases creates or updates aliases from YAML config (grouped format).
// In the grouped YAML, the first option in each group is considered active.
func (s *Store) upsertAliases(groups []config.AliasGroupCfg) error {
	if len(groups) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Collect all option IDs that exist in YAML so we can clean up DB-only ones later.
	yamlOptionIDs := make(map[string]bool)
	for _, g := range groups {
		for _, o := range g.Options {
			yamlOptionIDs[o.ID] = true
		}
	}

	for _, g := range groups {
		if len(g.Options) == 0 {
			continue
		}

		// Check if there's already an active alias in the DB for this group
		// (from a previous runtime session). Preserve it only if it will survive
		// cleanup (i.e. its ID is in the YAML config). Otherwise fall back to the
		// first YAML option.
		var existingActiveID string
		err := tx.QueryRow("SELECT id FROM aliases WHERE input_model_id = ? AND is_active = 1 LIMIT 1", g.InputModelID).Scan(&existingActiveID)

		// activeID tracks which alias to activate after upserting all options.
		var activeID string

		if err == nil && existingActiveID != "" && yamlOptionIDs[existingActiveID] {
			// A runtime-active alias exists AND it's in YAML — preserve it, skip deactivation.
			activeID = ""
		} else {
			// No active alias, or the active one isn't in YAML (will be cleaned up):
			// use YAML position-based activation (first option is the default).
			activeID = g.Options[0].ID

			// Deactivate all existing aliases in this group first
			if _, err := tx.Exec("UPDATE aliases SET is_active = 0 WHERE input_model_id = ?", g.InputModelID); err != nil {
				return fmt.Errorf("deactivate group %s: %w", g.InputModelID, err)
			}
		}

		for _, o := range g.Options {
			// Check if this option already exists in the DB
			var exists bool
			if err := tx.QueryRow("SELECT COUNT(*) > 0 FROM aliases WHERE id = ?", o.ID).Scan(&exists); err != nil {
				return fmt.Errorf("check alias %s: %w", o.ID, err)
			}

			if exists {
				if _, err := tx.Exec(
					`UPDATE aliases SET downstream_id = ?, output_model_id = ? WHERE id = ?`,
					o.DownstreamID, o.OutputModelID, o.ID); err != nil {
					return fmt.Errorf("update alias %s: %w", o.ID, err)
				}
			} else {
				if _, err := tx.Exec(
					`INSERT INTO aliases (id, input_model_id, downstream_id, output_model_id, is_active)
					 VALUES (?, ?, ?, ?, ?)`,
					o.ID, g.InputModelID, o.DownstreamID, o.OutputModelID, 0); err != nil {
					return fmt.Errorf("insert alias %s: %w", o.ID, err)
				}
			}
		}

		// Activate the chosen option (only if no runtime-active alias was preserved)
		if activeID != "" {
			if _, err := tx.Exec("UPDATE aliases SET is_active = 1 WHERE id = ?", activeID); err != nil {
				return fmt.Errorf("activate alias %s: %w", activeID, err)
			}
		}
	}

	// Delete aliases in DB that are not in YAML config (for groups present in YAML).
	// Aliases from groups NOT in YAML are preserved.
	for _, g := range groups {
		// Find options in this group that exist in DB but not in YAML
		rows, err := tx.Query("SELECT id FROM aliases WHERE input_model_id = ?", g.InputModelID)
		if err != nil {
			return fmt.Errorf("query group %s: %w", g.InputModelID, err)
		}
		var toDelete []string
		for rows.Next() {
			var dbID string
			if err := rows.Scan(&dbID); err != nil {
				rows.Close()
				return err
			}
			if !yamlOptionIDs[dbID] {
				toDelete = append(toDelete, dbID)
			}
		}
		rows.Close()

		for _, id := range toDelete {
			if _, err := tx.Exec("DELETE FROM aliases WHERE id = ?", id); err != nil {
				return fmt.Errorf("delete stale alias %s: %w", id, err)
			}
		}
	}

	return tx.Commit()
}
