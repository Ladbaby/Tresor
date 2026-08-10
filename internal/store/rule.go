package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"tresor/internal/config"

	"github.com/google/uuid"
)

// Rule represents a conditional transform pipeline.
// It matches incoming requests based on path, model, and format criteria.
// When conditions are met, its pipeline of transformers is applied.
type Rule struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	PatternPath        string    `json:"pattern_path"`
	PatternModels      []string  `json:"pattern_models"`
	MatchFormat        []string  `json:"match_format"`
	MatchDownstreamFmt []string  `json:"match_downstream_format"`
	MatchDownstreams   []string  `json:"match_downstreams"`
	PipelineConfig     string    `json:"pipeline_config"`
	IsEnabled          bool      `json:"is_enabled"`
	CreatedAt          time.Time `json:"created_at"`
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// scanRule populates a Rule from scanned DB columns.
func scanRule(id, name, patternPath, pipelineConfig string, enabled int64, createdAt time.Time, matchFormat, matchDownstreamFmt, matchDownstreams, patternModelsRaw []byte) *Rule {
	r := &Rule{
		ID:             id,
		Name:           name,
		PatternPath:    patternPath,
		PipelineConfig: pipelineConfig,
		IsEnabled:      enabled == 1,
		CreatedAt:      createdAt,
	}

	// Parse JSON array columns using index-based assignment
	cols := [][]byte{matchFormat, matchDownstreamFmt, matchDownstreams}
	fields := [3]*[]string{&r.MatchFormat, &r.MatchDownstreamFmt, &r.MatchDownstreams}
	for i, col := range cols {
		if len(col) == 0 || string(col) == "[]" {
			continue
		}
		var arr []string
		if err := json.Unmarshal(col, &arr); err != nil {
			continue
		}
		*fields[i] = arr
	}

	// Parse pattern_models JSON array
	if len(patternModelsRaw) > 0 && string(patternModelsRaw) != "[]" {
		var arr []string
		if err := json.Unmarshal(patternModelsRaw, &arr); err == nil {
			r.PatternModels = arr
		} else {
			r.PatternModels = []string{}
		}
	} else {
		r.PatternModels = []string{}
	}

	r.PatternModels = normalizeStrings(r.PatternModels)

	if r.MatchFormat == nil {
		r.MatchFormat = []string{}
	}
	if r.MatchDownstreamFmt == nil {
		r.MatchDownstreamFmt = []string{}
	}
	if r.MatchDownstreams == nil {
		r.MatchDownstreams = []string{}
	}
	return r
}

// ListRules returns all rules.
func (s *Store) ListRules() ([]Rule, error) {
	rows, err := s.db.Query(
		`SELECT id, name, pattern_path,
			pipeline_config, is_enabled, created_at,
			COALESCE(match_format,'[]'), COALESCE(match_downstream_format,'[]'),
			COALESCE(match_downstreams,'[]'), COALESCE(pattern_models,'[]')
		 FROM rules ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var id, name, patternPath, pipelineConfig string
		var enabled int64
		var createdAt time.Time
		var matchFormat, matchDownstreamFmt, matchDownstreams, patternModelsRaw []byte
		if err := rows.Scan(&id, &name, &patternPath, &pipelineConfig, &enabled, &createdAt, &matchFormat, &matchDownstreamFmt, &matchDownstreams, &patternModelsRaw); err != nil {
			return nil, err
		}
		rules = append(rules, *scanRule(id, name, patternPath, pipelineConfig, enabled, createdAt, matchFormat, matchDownstreamFmt, matchDownstreams, patternModelsRaw))
	}
	return rules, rows.Err()
}

// GetRule returns a single rule by ID.
func (s *Store) GetRule(id string) (*Rule, error) {
	var rowId, name, patternPath, pipelineConfig string
	var enabled int64
	var createdAt time.Time
	var matchFormat, matchDownstreamFmt, matchDownstreams, patternModelsRaw []byte
	err := s.db.QueryRow(
		`SELECT id, name, pattern_path,
			pipeline_config, is_enabled, created_at,
			COALESCE(match_format,'[]'), COALESCE(match_downstream_format,'[]'),
			COALESCE(match_downstreams,'[]'), COALESCE(pattern_models,'[]')
		 FROM rules WHERE id = ?`, id).
		Scan(&rowId, &name, &patternPath, &pipelineConfig, &enabled, &createdAt, &matchFormat, &matchDownstreamFmt, &matchDownstreams, &patternModelsRaw)
	if err != nil {
		return nil, fmt.Errorf("get rule %s: %w", id, err)
	}
	return scanRule(rowId, name, patternPath, pipelineConfig, enabled, createdAt, matchFormat, matchDownstreamFmt, matchDownstreams, patternModelsRaw), nil
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
	pm := normalizeStrings(r.PatternModels)

	mfJSON, _ := json.Marshal(mf)
	mdfJSON, _ := json.Marshal(mdf)
	mdsJSON, _ := json.Marshal(mds)
	pmJSON, _ := json.Marshal(pm)

	_, err := s.db.Exec(
		`INSERT INTO rules (id, name, pattern_path, pipeline_config, is_enabled,
			match_format, match_downstream_format, match_downstreams, pattern_models)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.PatternPath, r.PipelineConfig, enabled,
		string(mfJSON), string(mdfJSON), string(mdsJSON), string(pmJSON))
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
	pm := normalizeStrings(r.PatternModels)

	mfJSON, _ := json.Marshal(mf)
	mdfJSON, _ := json.Marshal(mdf)
	mdsJSON, _ := json.Marshal(mds)
	pmJSON, _ := json.Marshal(pm)

	res, err := s.db.Exec(
		`UPDATE rules SET name = ?, pattern_path = ?,
		 pattern_models = ?, pipeline_config = ?, is_enabled = ?,
		 match_format = ?, match_downstream_format = ?, match_downstreams = ?
		 WHERE id = ?`,
		r.Name, r.PatternPath, string(pmJSON), r.PipelineConfig, enabled,
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

// matchingRules returns enabled exact/wildcard path rules whose model filter
// intersects candidates. Model values are ORed; MatchRule applies the other
// fields with AND semantics. Results retain creation order within each tier.
func (s *Store) matchingRules(path string, candidates []string, inputFormat, dsID string, dsFormats []string) ([]Rule, error) {
	rows, err := s.db.Query(
		`SELECT id, name, pattern_path,
			pipeline_config, is_enabled, created_at,
			COALESCE(match_format,'[]'), COALESCE(match_downstream_format,'[]'),
			COALESCE(match_downstreams,'[]'), COALESCE(pattern_models,'[]')
		 FROM rules WHERE is_enabled = 1 AND (pattern_path = ? OR pattern_path = '*')
		 ORDER BY created_at, rowid`, path)
	if err != nil {
		return nil, fmt.Errorf("find matching rules: %w", err)
	}
	defer rows.Close()

	type rankedRule struct {
		rule     Rule
		priority int
	}
	var ranked []rankedRule
	for rows.Next() {
		var id, name, patternPath, pipelineConfig string
		var enabled int64
		var createdAt time.Time
		var matchFormat, matchDownstreamFmt, matchDownstreams, patternModelsRaw []byte
		if err := rows.Scan(&id, &name, &patternPath, &pipelineConfig, &enabled, &createdAt, &matchFormat, &matchDownstreamFmt, &matchDownstreams, &patternModelsRaw); err != nil {
			return nil, err
		}
		r := scanRule(id, name, patternPath, pipelineConfig, enabled, createdAt, matchFormat, matchDownstreamFmt, matchDownstreams, patternModelsRaw)
		modelSpecific := len(r.PatternModels) > 0
		if modelSpecific && !containsAnyStr(r.PatternModels, candidates) {
			continue
		}
		if !r.MatchRule(inputFormat, dsID, dsFormats) {
			continue
		}

		priority := 3
		switch {
		case r.PatternPath == path && modelSpecific:
			priority = 0
		case r.PatternPath == "*" && modelSpecific:
			priority = 1
		case r.PatternPath == path:
			priority = 2
		}
		ranked = append(ranked, rankedRule{rule: *r, priority: priority})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].priority < ranked[j].priority
	})
	matches := make([]Rule, len(ranked))
	for i := range ranked {
		matches[i] = ranked[i].rule
	}
	return matches, nil
}

// FindMatchingRules finds all enabled rules matching the given path and model.
func (s *Store) FindMatchingRules(path, model, inputFormat string, dsID string, dsFormats []string) ([]Rule, error) {
	candidates := []string{}
	if model != "" {
		candidates = append(candidates, model)
	}
	return s.matchingRules(path, candidates, inputFormat, dsID, dsFormats)
}

// FindMatchingRulesWithCandidates is the engine-aware variant. A rule matches
// when any pattern_models entry exactly equals any engine candidate. Empty
// pattern_models retains any-model behavior.
func (s *Store) FindMatchingRulesWithCandidates(path string, candidates []string, inputFormat string, dsID string, dsFormats []string) ([]Rule, error) {
	return s.matchingRules(path, candidates, inputFormat, dsID, dsFormats)
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
		pm := normalizeStrings(r.PatternModels)
		mfJSON, _ := json.Marshal(mf)
		mdfJSON, _ := json.Marshal(mdf)
		mdsJSON, _ := json.Marshal(mds)
		pmJSON, _ := json.Marshal(pm)

		// Check if this rule already exists
		var exists bool
		if err := tx.QueryRow("SELECT COUNT(*) > 0 FROM rules WHERE id = ?", r.ID).Scan(&exists); err != nil {
			return fmt.Errorf("check rule %s: %w", r.ID, err)
		}

		if exists {
			if _, err := tx.Exec(
				`UPDATE rules SET name = ?, pattern_path = ?,
				 pattern_models = ?, pipeline_config = ?, is_enabled = ?,
				 match_format = ?, match_downstream_format = ?, match_downstreams = ?
				 WHERE id = ?`,
				r.Name, r.PatternPath, string(pmJSON),
				pipelineJSON, enabled,
				string(mfJSON), string(mdfJSON), string(mdsJSON), r.ID); err != nil {
				return fmt.Errorf("update rule %s: %w", r.ID, err)
			}
		} else {
			if _, err := tx.Exec(
				`INSERT INTO rules (id, name, pattern_path, pattern_models, pipeline_config, is_enabled,
					match_format, match_downstream_format, match_downstreams)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				r.ID, r.Name, r.PatternPath, string(pmJSON), pipelineJSON, enabled,
				string(mfJSON), string(mdfJSON), string(mdsJSON)); err != nil {
				return fmt.Errorf("insert rule %s: %w", r.ID, err)
			}
		}
	}

	return tx.Commit()
}
