package store

import (
	"fmt"
)

// UpdateAliasGroup updates group-level routing mode and/or option ordering.
// A nil mode or nil order leaves that property unchanged.
func (s *Store) UpdateAliasGroup(input string, mode *string, order []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var ids []string
	rows, err := tx.Query("SELECT id FROM aliases WHERE input_model_id = ? ORDER BY option_order, rowid", input)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(ids) == 0 {
		return fmt.Errorf("alias group %s not found", input)
	}

	var previousMode string
	if err := tx.QueryRow("SELECT route_mode FROM aliases WHERE input_model_id = ? LIMIT 1", input).Scan(&previousMode); err != nil {
		return err
	}
	if order != nil {
		if len(order) != len(ids) {
			return fmt.Errorf("option_order must contain every option exactly once")
		}
		known := make(map[string]bool, len(ids))
		for _, id := range ids {
			known[id] = true
		}
		for _, id := range order {
			if !known[id] {
				return fmt.Errorf("option_order must contain every option exactly once")
			}
			delete(known, id)
		}
		for i, id := range order {
			if _, err := tx.Exec("UPDATE aliases SET option_order = ? WHERE id = ?", i, id); err != nil {
				return err
			}
		}
	}
	if mode != nil {
		normalized := *mode
		if normalized == "" {
			normalized = "default"
		}
		if normalized != "default" && normalized != "load_balance" && normalized != "fallback" {
			return fmt.Errorf("invalid route_mode %q", normalized)
		}
		if normalized != "default" {
			if _, err := tx.Exec("UPDATE aliases SET is_active = 0 WHERE input_model_id = ?", input); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE aliases SET route_mode = ? WHERE input_model_id = ?", normalized, input); err != nil {
			return err
		}
		if *mode != "default" {
			if _, err := tx.Exec("UPDATE aliases SET is_active = 0 WHERE input_model_id = ?", input); err != nil {
				return err
			}
		} else if previousMode != "default" {
			var activeID string
			err := tx.QueryRow(`SELECT a.id FROM aliases a JOIN downstreams d ON d.id = a.downstream_id WHERE a.input_model_id = ? AND d.is_enabled = 1 ORDER BY a.option_order, a.rowid LIMIT 1`, input).Scan(&activeID)
			if err != nil {
				err = tx.QueryRow("SELECT id FROM aliases WHERE input_model_id = ? ORDER BY option_order, rowid LIMIT 1", input).Scan(&activeID)
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE aliases SET is_active = CASE WHEN id = ? THEN 1 ELSE 0 END WHERE input_model_id = ?", activeID, input); err != nil {
				return err
			}
		}
	}

	s.invalidateRegexCache()
	return tx.Commit()
}
