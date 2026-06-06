package store

import (
	"encoding/json"
	"fmt"
	"time"

	"tresor/internal/config"

	"github.com/google/uuid"
)

// Rule represents an active interception/routing policy.
type Rule struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	PatternPath      string    `json:"pattern_path"`
	PatternModel     string    `json:"pattern_model,omitempty"`
	ActiveDownstream string    `json:"active_downstream"`
	PipelineConfig   string    `json:"pipeline_config"`
	IsEnabled        bool      `json:"is_enabled"`
	CreatedAt        time.Time `json:"created_at"`
}

// ListRules returns all rules.
func (s *Store) ListRules() ([]Rule, error) {
	rows, err := s.db.Query(
		`SELECT id, name, pattern_path, COALESCE(pattern_model,''), COALESCE(active_downstream,''),
		        pipeline_config, is_enabled, created_at
		 FROM rules ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var r Rule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Name, &r.PatternPath, &r.PatternModel,
			&r.ActiveDownstream, &r.PipelineConfig, &enabled, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.IsEnabled = enabled == 1
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// GetRule returns a single rule by ID.
func (s *Store) GetRule(id string) (*Rule, error) {
	var r Rule
	var enabled int
	err := s.db.QueryRow(
		`SELECT id, name, pattern_path, COALESCE(pattern_model,''), COALESCE(active_downstream,''),
		        pipeline_config, is_enabled, created_at
		 FROM rules WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.PatternPath, &r.PatternModel,
			&r.ActiveDownstream, &r.PipelineConfig, &enabled, &r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get rule %s: %w", id, err)
	}
	r.IsEnabled = enabled == 1
	return &r, nil
}

// CreateRule inserts a new rule.
func (s *Store) CreateRule(r *Rule) error {
	if r.ID == "" {
		r.ID = uuid.New().String()[:8]
	}

	enabled := 0
	if r.IsEnabled {
		enabled = 1
	}
	if r.PipelineConfig == "" {
		r.PipelineConfig = "[]"
	}

	_, err := s.db.Exec(
		`INSERT INTO rules (id, name, pattern_path, pattern_model, active_downstream, pipeline_config, is_enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.PatternPath, r.PatternModel, r.ActiveDownstream, r.PipelineConfig, enabled)
	if err != nil {
		return fmt.Errorf("create rule: %w", err)
	}
	return nil
}

// UpdateRuleDownstream changes the active downstream for a rule.
func (s *Store) UpdateRuleDownstream(ruleID, downstreamID string) error {
	res, err := s.db.Exec("UPDATE rules SET active_downstream = ? WHERE id = ?", downstreamID, ruleID)
	if err != nil {
		return fmt.Errorf("update rule downstream: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("rule %s not found", ruleID)
	}
	return nil
}

// UpdateRuleEnabled enables or disables a rule.
func (s *Store) UpdateRuleEnabled(ruleID string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.Exec("UPDATE rules SET is_enabled = ? WHERE id = ?", v, ruleID)
	if err != nil {
		return fmt.Errorf("update rule enabled: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("rule %s not found", ruleID)
	}
	return nil
}

// UpdateRule updates all mutable fields of a rule.
func (s *Store) UpdateRule(r *Rule) error {
	enabled := 0
	if r.IsEnabled {
		enabled = 1
	}
	res, err := s.db.Exec(
		`UPDATE rules SET name = ?, pattern_path = ?, pattern_model = ?,
		 active_downstream = ?, pipeline_config = ?, is_enabled = ?
		 WHERE id = ?`,
		r.Name, r.PatternPath, r.PatternModel, r.ActiveDownstream, r.PipelineConfig, enabled, r.ID)
	if err != nil {
		return fmt.Errorf("update rule: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("rule %s not found", r.ID)
	}
	return nil
}

// DeleteRule removes a rule.
func (s *Store) DeleteRule(id string) error {
	res, err := s.db.Exec("DELETE FROM rules WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("rule %s not found", id)
	}
	return nil
}

// FindMatchingRule finds the first enabled rule matching the given path and optional model.
// Returns nil if no rule matches.
// Priority: exact path+model > exact path (no model) > wildcard.
func (s *Store) FindMatchingRule(path, model string) (*Rule, error) {
	rows, err := s.db.Query(
		`SELECT id, name, pattern_path, COALESCE(pattern_model,''), COALESCE(active_downstream,''),
		        pipeline_config, is_enabled, created_at
		 FROM rules WHERE is_enabled = 1
		  AND (
		    (pattern_path = ? AND pattern_model = ?)
		    OR (pattern_path = ? AND (pattern_model = '' OR pattern_model IS NULL))
		    OR pattern_path = '*'
		  )
		 ORDER BY
		  CASE
		    WHEN pattern_path = ? AND pattern_model = ? THEN 0
		    WHEN pattern_path = ? AND (pattern_model = '' OR pattern_model IS NULL) THEN 1
		    WHEN pattern_path = '*' THEN 2
		  END
		 LIMIT 1`, path, model, path, path, model, path)
	if err != nil {
		return nil, fmt.Errorf("find matching rule: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, nil
	}

	var r Rule
	var enabled int
	if err := rows.Scan(&r.ID, &r.Name, &r.PatternPath, &r.PatternModel,
		&r.ActiveDownstream, &r.PipelineConfig, &enabled, &r.CreatedAt); err != nil {
		return nil, err
	}
	r.IsEnabled = enabled == 1
	return &r, nil
}

// upsertRules creates or updates rules from YAML config.
func (s *Store) upsertRules(rules []config.RuleCfg) error {
	if len(rules) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, r := range rules {
		// Marshal pipeline config to JSON string for storage
		pipelineJSON := "[]"
		if len(r.PipelineConfig) > 0 {
			data, err := json.Marshal(r.PipelineConfig)
			if err != nil {
				return fmt.Errorf("marshal pipeline_config for rule %s: %w", r.ID, err)
			}
			pipelineJSON = string(data)
		}

		enabled := 0
		if r.IsEnabled {
			enabled = 1
		}

		// Check if this rule already exists
		var exists bool
		if err := tx.QueryRow("SELECT COUNT(*) > 0 FROM rules WHERE id = ?", r.ID).Scan(&exists); err != nil {
			return fmt.Errorf("check rule %s: %w", r.ID, err)
		}

		if exists {
			if _, err := tx.Exec(
				`UPDATE rules SET name = ?, pattern_path = ?, pattern_model = ?,
				 active_downstream = ?, pipeline_config = ?, is_enabled = ?
				 WHERE id = ?`,
				r.Name, r.PatternPath, r.PatternModel, r.ActiveDownstream,
				pipelineJSON, enabled, r.ID); err != nil {
				return fmt.Errorf("update rule %s: %w", r.ID, err)
			}
		} else {
			if _, err := tx.Exec(
				`INSERT INTO rules (id, name, pattern_path, pattern_model, active_downstream, pipeline_config, is_enabled)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				r.ID, r.Name, r.PatternPath, r.PatternModel, r.ActiveDownstream,
				pipelineJSON, enabled); err != nil {
				return fmt.Errorf("insert rule %s: %w", r.ID, err)
			}
		}
	}

	return tx.Commit()
}
