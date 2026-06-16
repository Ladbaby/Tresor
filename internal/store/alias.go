package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"sync"
	"time"

	"tresor/internal/config"

	"github.com/google/uuid"
)

// Alias represents a single input-model -> output-model mapping entry.
// Multiple aliases sharing the same InputModelID form a group.
// If IsRegex is true, InputModelID is treated as a regular expression.
type Alias struct {
	ID              string    `json:"id"`
	InputModelID    string    `json:"input_model_id"`
	DownstreamID    string    `json:"downstream_id"`
	DownstreamName  string    `json:"downstream_name,omitempty"`
	OutputModelID   string    `json:"output_model_id"`
	IsActive        bool      `json:"is_active"`
	IsRegex         bool      `json:"is_regex"`
	GroupOrder      int       `json:"group_order"`
	CreatedAt       time.Time `json:"created_at"`
}

// AliasGroup represents one alias group: an input model and its available options.
type AliasGroup struct {
	InputModelID string  `json:"input_model_id"`
	IsRegex      bool    `json:"is_regex"`
	ActiveID     *string `json:"active_id,omitempty"`
	Options      []Alias `json:"options"`
}

// ListAliases returns all aliases ordered by group_order, then rowid.
func (s *Store) ListAliases() ([]Alias, error) {
	rows, err := s.db.Query(
		`SELECT id, input_model_id, downstream_id, output_model_id, is_active, is_regex, group_order, created_at
		 FROM aliases ORDER BY group_order, rowid`)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()

	var aliases []Alias
	for rows.Next() {
		var a Alias
		var active, isRegex, groupOrder int
		if err := rows.Scan(&a.ID, &a.InputModelID, &a.DownstreamID, &a.OutputModelID, &active, &isRegex, &groupOrder, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.IsActive = active == 1
		a.IsRegex = isRegex == 1
		a.GroupOrder = groupOrder
		aliases = append(aliases, a)
	}
	return aliases, rows.Err()
}

// GetAlias returns a single alias by ID.
func (s *Store) GetAlias(id string) (*Alias, error) {
	var a Alias
	var active, isRegex, groupOrder int
	err := s.db.QueryRow(
		`SELECT id, input_model_id, downstream_id, output_model_id, is_active, is_regex, group_order, created_at
		 FROM aliases WHERE id = ?`, id).
		Scan(&a.ID, &a.InputModelID, &a.DownstreamID, &a.OutputModelID, &active, &isRegex, &groupOrder, &a.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get alias %s: %w", id, err)
	}
	a.IsActive = active == 1
	a.IsRegex = isRegex == 1
	a.GroupOrder = groupOrder
	return &a, nil
}

// CreateAlias inserts a new alias. Validates that the downstream exists.
// If IsActive is true, all other aliases with the same InputModelID are
// deactivated within a transaction.
// If the group doesn't exist yet, group_order is auto-assigned (max + 1).
func (s *Store) CreateAlias(a *Alias) error {
	if a.ID == "" {
		a.ID = uuid.New().String()[:8]
	}

	// Validate downstream exists
	if _, err := s.GetDownstream(a.DownstreamID); err != nil {
		return fmt.Errorf("downstream %s not found: %w", a.DownstreamID, err)
	}

	// Validate regex pattern if is_regex is set
	if a.IsRegex {
		if _, err := compileRegex(a.InputModelID); err != nil {
			return fmt.Errorf("invalid regex pattern %q: %w", a.InputModelID, err)
		}
	}

	active := 0
	if a.IsActive {
		active = 1
	}
	isRegex := 0
	if a.IsRegex {
		isRegex = 1
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Determine group_order: if group already exists, reuse its order; otherwise assign max + 1
	groupOrder := a.GroupOrder
	if groupOrder == 0 {
		var count int
		err := tx.QueryRow("SELECT COUNT(*) FROM aliases WHERE input_model_id = ?", a.InputModelID).Scan(&count)
		if err == nil && count > 0 {
			// Group exists — reuse its existing order and inherit its is_regex status
			err = tx.QueryRow("SELECT group_order FROM aliases WHERE input_model_id = ? LIMIT 1", a.InputModelID).Scan(&groupOrder)
			// Inherit the group's is_regex (overrides whatever was passed in)
			var groupIsRegex int
			if err2 := tx.QueryRow("SELECT is_regex FROM aliases WHERE input_model_id = ? LIMIT 1", a.InputModelID).Scan(&groupIsRegex); err2 == nil {
				isRegex = groupIsRegex
			}
		}
		if groupOrder == 0 {
			// New group — assign max order + 1
			var maxOrder int
			err = tx.QueryRow("SELECT COALESCE(MAX(group_order),0) FROM aliases").Scan(&maxOrder)
			if err != nil {
				return fmt.Errorf("get max group_order: %w", err)
			}
			groupOrder = maxOrder + 1
		}
	}

	// If this alias should be active, deactivate all siblings first
	if a.IsActive {
		if _, err := tx.Exec("UPDATE aliases SET is_active = 0 WHERE input_model_id = ? AND id != ?",
			a.InputModelID, a.ID); err != nil {
			return fmt.Errorf("deactivate sibling aliases: %w", err)
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO aliases (id, input_model_id, downstream_id, output_model_id, is_active, is_regex, group_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.InputModelID, a.DownstreamID, a.OutputModelID, active, isRegex, groupOrder); err != nil {
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
	isRegex := 0
	if a.IsRegex {
		isRegex = 1
	}

	// Validate regex pattern if is_regex is set
	if a.IsRegex {
		if _, err := compileRegex(a.InputModelID); err != nil {
			return fmt.Errorf("invalid regex pattern %q: %w", a.InputModelID, err)
		}
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
		`UPDATE aliases SET downstream_id = ?, output_model_id = ?, is_active = ?, is_regex = ?, group_order = ?
		 WHERE id = ?`,
		a.DownstreamID, a.OutputModelID, active, isRegex, a.GroupOrder, a.ID)
	if err != nil {
		return fmt.Errorf("update alias: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias %s not found", a.ID)
	}

	// Sync is_regex to all siblings in the group (group-level property)
	if _, err := tx.Exec("UPDATE aliases SET is_regex = ? WHERE input_model_id = ?", isRegex, a.InputModelID); err != nil {
		return fmt.Errorf("sync is_regex to siblings: %w", err)
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
// After deletion, remaining groups are re-numbered sequentially.
func (s *Store) DeleteGroup(inputModelID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Get the group_order before deleting
	var groupOrder int
	err = tx.QueryRow("SELECT group_order FROM aliases WHERE input_model_id = ? LIMIT 1", inputModelID).Scan(&groupOrder)
	if err != nil {
		return 0, fmt.Errorf("alias group %s not found", inputModelID)
	}

	// Delete all aliases in this group
	res, err := tx.Exec("DELETE FROM aliases WHERE input_model_id = ?", inputModelID)
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

	// Compact: shift all groups with higher order down by one
	if _, err := tx.Exec("UPDATE aliases SET group_order = group_order - 1 WHERE group_order > ?", groupOrder); err != nil {
		return 0, fmt.Errorf("compact group_order: %w", err)
	}

	return int(n), tx.Commit()
}

// FindActiveAlias returns the active alias for a given input model ID.
// First tries exact match on input_model_id. If no exact match, tries
// regex match against active aliases where is_regex = 1.
// Returns nil if no active alias exists for that input model.
func (s *Store) FindActiveAlias(inputModelID string) (*Alias, error) {
	// Try exact match first
	a, err := findActiveAliasExact(s.db, inputModelID)
	if err != nil {
		return nil, err
	}
	if a != nil {
		return a, nil
	}

	// Try regex match
	return findActiveAliasRegex(s.db, inputModelID)
}

// findActiveAliasExact looks for an exact input_model_id match among active aliases.
func findActiveAliasExact(db *sql.DB, inputModelID string) (*Alias, error) {
	var a Alias
	var active, isRegex, groupOrder int
	err := db.QueryRow(
		`SELECT id, input_model_id, downstream_id, output_model_id, is_active, is_regex, group_order, created_at
		 FROM aliases WHERE input_model_id = ? AND is_active = 1
		 LIMIT 1`, inputModelID).
		Scan(&a.ID, &a.InputModelID, &a.DownstreamID, &a.OutputModelID, &active, &isRegex, &groupOrder, &a.CreatedAt)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("find active alias %s: %w", inputModelID, err)
	}
	a.IsActive = active == 1
	a.IsRegex = isRegex == 1
	a.GroupOrder = groupOrder
	return &a, nil
}

// findActiveAliasRegex looks for a regex-matching active alias.
// Returns the first active alias whose input_model_id pattern (treated as regex)
// matches the model. Matches both directly-regex aliases and active aliases in
// groups where any option has is_regex = 1 — this covers the case where a
// non-regex alias was activated (e.g. via web UI) in a regex group.
// Precompiled regex patterns are cached to avoid recompiling on every request.
func findActiveAliasRegex(db *sql.DB, model string) (*Alias, error) {
	rows, err := db.Query(
		`SELECT a.id, a.input_model_id, a.downstream_id, a.output_model_id, a.is_active, a.is_regex, a.group_order, a.created_at
		 FROM aliases a
		 WHERE a.is_active = 1
		   AND (a.is_regex = 1
		     OR EXISTS (
		       SELECT 1 FROM aliases a2
		       WHERE a2.input_model_id = a.input_model_id
		         AND a2.is_regex = 1
		     ))
		 ORDER BY a.group_order, a.rowid`)
	if err != nil {
		return nil, fmt.Errorf("query regex aliases: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var a Alias
		var active, isRegex, groupOrder int
		if err := rows.Scan(&a.ID, &a.InputModelID, &a.DownstreamID, &a.OutputModelID, &active, &isRegex, &groupOrder, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.IsActive = active == 1
		a.IsRegex = isRegex == 1
		a.GroupOrder = groupOrder

		re, err := compileRegex(a.InputModelID)
		if err != nil {
			// Skip aliases with invalid regex patterns
			continue
		}
		if re.MatchString(model) {
			return &a, nil
		}
	}
	return nil, rows.Err()
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
		if a.IsRegex {
			g.IsRegex = true
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
// Group array position determines group_order.
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

	for i, g := range groups {
		if len(g.Options) == 0 {
			continue
		}

		// YAML array position determines group_order (1-based)
		groupOrder := i + 1

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
				isRegex := 0
				if g.IsRegex {
					isRegex = 1
				}
				if _, err := tx.Exec(
					`UPDATE aliases SET downstream_id = ?, output_model_id = ?, is_regex = ?, group_order = ? WHERE id = ?`,
					o.DownstreamID, o.OutputModelID, isRegex, groupOrder, o.ID); err != nil {
					return fmt.Errorf("update alias %s: %w", o.ID, err)
				}
			} else {
				isRegex := 0
				if g.IsRegex {
					isRegex = 1
				}
				if _, err := tx.Exec(
					`INSERT INTO aliases (id, input_model_id, downstream_id, output_model_id, is_active, is_regex, group_order)
					 VALUES (?, ?, ?, ?, ?, ?, ?)`,
					o.ID, g.InputModelID, o.DownstreamID, o.OutputModelID, 0, isRegex, groupOrder); err != nil {
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

// ReorderGroups updates group_order for all alias groups to match the given
// input_model_id ordering. Validates that all groups exist. Groups not in the
// list keep their existing order.
func (s *Store) ReorderGroups(inputModelIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Validate all groups exist first
	for _, mid := range inputModelIDs {
		var count int
		err := tx.QueryRow("SELECT COUNT(*) FROM aliases WHERE input_model_id = ?", mid).Scan(&count)
		if err != nil || count == 0 {
			return fmt.Errorf("alias group %s not found", mid)
		}
	}

	// Assign new group_order values sequentially
	for i, mid := range inputModelIDs {
		order := i + 1
		if _, err := tx.Exec("UPDATE aliases SET group_order = ? WHERE input_model_id = ?", order, mid); err != nil {
			return fmt.Errorf("update group_order for %s: %w", mid, err)
		}
	}

	return tx.Commit()
}

// regexCache caches compiled regex patterns to avoid recompiling on every request.
var regexCache = struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}{m: make(map[string]*regexp.Regexp)}

// compileRegex compiles a regex pattern and caches the result.
func compileRegex(pattern string) (*regexp.Regexp, error) {
	regexCache.Lock()
	defer regexCache.Unlock()

	if re, ok := regexCache.m[pattern]; ok {
		return re, nil
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexCache.m[pattern] = re
	return re, nil
}
