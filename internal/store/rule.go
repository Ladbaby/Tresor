package store

import (
	"encoding/json"
	"fmt"
	"time"

	"tresor/internal/config"

	"github.com/google/uuid"
)

// Rule represents a conditional transform pipeline.
// It matches incoming requests based on path, model, and format criteria.
// When conditions are met, its pipeline of transformers is applied.
type Rule struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	PatternPath         string    `json:"pattern_path"`
	PatternModel        string    `json:"pattern_model,omitempty"`
	MatchFormat         []string  `json:"match_format"`
	MatchDownstreamFmt  []string  `json:"match_downstream_format"`
	MatchDownstreams    []string  `json:"match_downstreams"`
	PipelineConfig      string    `json:"pipeline_config"`
	IsEnabled           bool      `json:"is_enabled"`
	CreatedAt           time.Time `json:"created_at"`
}

// scanRule populates a Rule from a DB row scan. The vals slice should contain
// values in order: ID, Name, PatternPath, PatternModel, PipelineConfig, Enabled, CreatedAt,
// MatchFormat, MatchDownstreamFmt, MatchDownstreams.
func scanRule(vals *[]interface{}) *Rule {
	r := &Rule{
		ID:             toString((*vals)[0]),
		Name:           toString((*vals)[1]),
		PatternPath:    toString((*vals)[2]),
		PatternModel:   toString((*vals)[3]),
		PipelineConfig: toString((*vals)[5]),
		CreatedAt:      (*vals)[7].(time.Time),
	}
	enabled := int((*vals)[6].(int64))
	r.IsEnabled = enabled == 1

	// Parse JSON array columns
	for i, col := range []string{toString((*vals)[8]), toString((*vals)[9]), toString((*vals)[10])} {
		if col == "" || col == "[]" {
			switch i {
			case 0:
				r.MatchFormat = []string{}
			case 1:
				r.MatchDownstreamFmt = []string{}
			case 2:
				r.MatchDownstreams = []string{}
			}
		} else {
			var arr []string
			if err := json.Unmarshal([]byte(col), &arr); err != nil {
				switch i {
				case 0:
					r.MatchFormat = []string{}
				case 1:
					r.MatchDownstreamFmt = []string{}
				case 2:
					r.MatchDownstreams = []string{}
				}
			} else {
				switch i {
				case 0:
					r.MatchFormat = arr
				case 1:
					r.MatchDownstreamFmt = arr
				case 2:
					r.MatchDownstreams = arr
				}
			}
		}
	}
	return r
}

func toString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ListRules returns all rules.
func (s *Store) ListRules() ([]Rule, error) {
	rows, err := s.db.Query(
		`SELECT id, name, pattern_path, COALESCE(pattern_model,''),
			pipeline_config, is_enabled, created_at,
			COALESCE(match_format,'[]'), COALESCE(match_downstream_format,'[]'),
			COALESCE(match_downstreams,'[]')
		 FROM rules ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		vals := make([]interface{}, 11)
		if err := rows.Scan(&vals[0], &vals[1], &vals[2], &vals[3], &vals[5], &vals[6], &vals[7], &vals[8], &vals[9], &vals[10]); err != nil {
			return nil, err
		}
		rules = append(rules, *scanRule(&vals))
	}
	return rules, rows.Err()
}

// GetRule returns a single rule by ID.
func (s *Store) GetRule(id string) (*Rule, error) {
	vals := make([]interface{}, 11)
	err := s.db.QueryRow(
		`SELECT id, name, pattern_path, COALESCE(pattern_model,''),
			pipeline_config, is_enabled, created_at,
			COALESCE(match_format,'[]'), COALESCE(match_downstream_format,'[]'),
			COALESCE(match_downstreams,'[]')
		 FROM rules WHERE id = ?`, id).
		Scan(&vals[0], &vals[1], &vals[2], &vals[3], &vals[5], &vals[6], &vals[7], &vals[8], &vals[9], &vals[10])
	if err != nil {
		return nil, fmt.Errorf("get rule %s: %w", id, err)
	}
	return scanRule(&vals), nil
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

	mf := []string{}
	if len(r.MatchFormat) > 0 {
		mf = r.MatchFormat
	}
	mdf := []string{}
	if len(r.MatchDownstreamFmt) > 0 {
		mdf = r.MatchDownstreamFmt
	}
	mds := []string{}
	if len(r.MatchDownstreams) > 0 {
		mds = r.MatchDownstreams
	}

	mfJSON, _ := json.Marshal(mf)
	mdfJSON, _ := json.Marshal(mdf)
	mdsJSON, _ := json.Marshal(mds)

	_, err := s.db.Exec(
		`INSERT INTO rules (id, name, pattern_path, pattern_model, pipeline_config, is_enabled,
			match_format, match_downstream_format, match_downstreams)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.PatternPath, r.PatternModel, r.PipelineConfig, enabled,
		string(mfJSON), string(mdfJSON), string(mdsJSON))
	if err != nil {
		return fmt.Errorf("create rule: %w", err)
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

	mf := []string{}
	if len(r.MatchFormat) > 0 {
		mf = r.MatchFormat
	}
	mdf := []string{}
	if len(r.MatchDownstreamFmt) > 0 {
		mdf = r.MatchDownstreamFmt
	}
	mds := []string{}
	if len(r.MatchDownstreams) > 0 {
		mds = r.MatchDownstreams
	}

	mfJSON, _ := json.Marshal(mf)
	mdfJSON, _ := json.Marshal(mdf)
	mdsJSON, _ := json.Marshal(mds)

	res, err := s.db.Exec(
		`UPDATE rules SET name = ?, pattern_path = ?, pattern_model = ?,
		 pipeline_config = ?, is_enabled = ?,
		 match_format = ?, match_downstream_format = ?, match_downstreams = ?
		 WHERE id = ?`,
		r.Name, r.PatternPath, r.PatternModel, r.PipelineConfig, enabled,
		string(mfJSON), string(mdfJSON), string(mdsJSON), r.ID)
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

// MatchRule checks if this rule's conditions are satisfied by the given
// request context. Returns true if all non-empty filters pass.
func (r *Rule) MatchRule(inputFormat string, dsID string, dsFormats []string) bool {
	if len(r.MatchFormat) > 0 && !containsStr(r.MatchFormat, inputFormat) {
		return false
	}
	if len(r.MatchDownstreamFmt) > 0 && !containsAnyStr(r.MatchDownstreamFmt, dsFormats) {
		return false
	}
	if len(r.MatchDownstreams) > 0 && !containsStr(r.MatchDownstreams, dsID) {
		return false
	}
	return true
}

// containsStr checks if a string slice contains a specific value.
func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// containsAnyStr checks if any element of sliceA exists in sliceB.
func containsAnyStr(sliceA, sliceB []string) bool {
	for _, a := range sliceA {
		if containsStr(sliceB, a) {
			return true
		}
	}
	return false
}

// FindMatchingRules finds all enabled rules matching the given path and model,
// then filters by format criteria. Returns matching rules sorted by priority:
// exact path+model > exact path > wildcard.
func (s *Store) FindMatchingRules(path, model, inputFormat string, dsID string, dsFormats []string) ([]Rule, error) {
	rows, err := s.db.Query(
		`SELECT id, name, pattern_path, COALESCE(pattern_model,''),
			pipeline_config, is_enabled, created_at,
			COALESCE(match_format,'[]'), COALESCE(match_downstream_format,'[]'),
			COALESCE(match_downstreams,'[]')
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
		  END`, path, model, path, path, model, path)
	if err != nil {
		return nil, fmt.Errorf("find matching rules: %w", err)
	}
	defer rows.Close()

	var matches []Rule
	for rows.Next() {
		var vals = make([]interface{}, 11)
		if err := rows.Scan(&vals[0], &vals[1], &vals[2], &vals[3], &vals[5], &vals[6], &vals[7], &vals[8], &vals[9], &vals[10]); err != nil {
			return nil, err
		}
		r := scanRule(&vals)
		if r.MatchRule(inputFormat, dsID, dsFormats) {
			matches = append(matches, *r)
		}
	}
	if matches == nil {
		return []Rule{}, rows.Err()
	}
	return matches, rows.Err()
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

		// Marshal array fields to JSON
		mf := []string{}
		if len(r.MatchFormat) > 0 {
			mf = r.MatchFormat
		}
		mdf := []string{}
		if len(r.MatchDownstreamFmt) > 0 {
			mdf = r.MatchDownstreamFmt
		}
		mds := []string{}
		if len(r.MatchDownstreams) > 0 {
			mds = r.MatchDownstreams
		}
		mfJSON, _ := json.Marshal(mf)
		mdfJSON, _ := json.Marshal(mdf)
		mdsJSON, _ := json.Marshal(mds)

		// Check if this rule already exists
		var exists bool
		if err := tx.QueryRow("SELECT COUNT(*) > 0 FROM rules WHERE id = ?", r.ID).Scan(&exists); err != nil {
			return fmt.Errorf("check rule %s: %w", r.ID, err)
		}

		if exists {
			if _, err := tx.Exec(
				`UPDATE rules SET name = ?, pattern_path = ?, pattern_model = ?,
				 pipeline_config = ?, is_enabled = ?,
				 match_format = ?, match_downstream_format = ?, match_downstreams = ?
				 WHERE id = ?`,
				r.Name, r.PatternPath, r.PatternModel,
				pipelineJSON, enabled,
				string(mfJSON), string(mdfJSON), string(mdsJSON), r.ID); err != nil {
				return fmt.Errorf("update rule %s: %w", r.ID, err)
			}
		} else {
			if _, err := tx.Exec(
				`INSERT INTO rules (id, name, pattern_path, pattern_model, pipeline_config, is_enabled,
					match_format, match_downstream_format, match_downstreams)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				r.ID, r.Name, r.PatternPath, r.PatternModel, pipelineJSON, enabled,
				string(mfJSON), string(mdfJSON), string(mdsJSON)); err != nil {
				return fmt.Errorf("insert rule %s: %w", r.ID, err)
			}
		}
	}

	return tx.Commit()
}
