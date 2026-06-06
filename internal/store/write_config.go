package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"tresor/internal/config"

	"gopkg.in/yaml.v3"
)

// WriteConfig serializes the current database state to the YAML config file.
// It reads all downstreams, rules, and aliases from SQLite, populates the
// AppConfig struct (with grouped aliases), then writes it atomically (tmp + rename).
// If cfg.ConfigPath is empty, this is a no-op (no config file was used).
func (s *Store) WriteConfig(cfg *config.AppConfig) error {
	if cfg.ConfigPath == "" {
		return nil
	}

	// --- Downstreams with output_model_ids ---
	var downstreams []config.DownstreamCfg
	rows, err := s.db.Query(
		`SELECT id, name, base_url, api_key FROM downstreams ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("query downstreams: %w", err)
	}
	for rows.Next() {
		var d config.DownstreamCfg
		if err := rows.Scan(&d.ID, &d.Name, &d.BaseURL, &d.APIKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan downstream: %w", err)
		}
		d.OutputModelIDs = s.listOutputModelIDs(d.ID)
		if d.OutputModelIDs == nil {
			d.OutputModelIDs = []string{}
		}
		downstreams = append(downstreams, d)
	}
	rows.Close()

	// --- Rules ---
	var rules []config.RuleCfg
	rows, err = s.db.Query(
		`SELECT id, name, pattern_path, COALESCE(pattern_model,''), COALESCE(active_downstream,''),
			pipeline_config, is_enabled FROM rules ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("query rules: %w", err)
	}
	for rows.Next() {
		var r config.RuleCfg
		var pipelineJSON string
		var enabled int
		if err := rows.Scan(&r.ID, &r.Name, &r.PatternPath, &r.PatternModel,
			&r.ActiveDownstream, &pipelineJSON, &enabled); err != nil {
			rows.Close()
			return fmt.Errorf("scan rule: %w", err)
		}
		r.IsEnabled = enabled == 1

		// Parse pipeline_config JSON string to []PipelineStep
		if pipelineJSON != "" && pipelineJSON != "[]" {
			var steps []config.PipelineStep
			if err := json.Unmarshal([]byte(pipelineJSON), &steps); err != nil {
				rows.Close()
				return fmt.Errorf("parse pipeline_config for rule %s: %w", r.ID, err)
			}
			r.PipelineConfig = steps
		} else {
			r.PipelineConfig = []config.PipelineStep{}
		}

		rules = append(rules, r)
	}
	rows.Close()

	// --- Aliases grouped by input_model_id (no is_active in YAML) ---
	var aliasGroups []config.AliasGroupCfg

	groupMap := make(map[string]*config.AliasGroupCfg)
	var groupOrder []string

	rows, err = s.db.Query(
		`SELECT id, input_model_id, downstream_id, output_model_id FROM aliases ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("query aliases: %w", err)
	}
	for rows.Next() {
		var id, inputModelID, downstreamID, outputModelID string
		if err := rows.Scan(&id, &inputModelID, &downstreamID, &outputModelID); err != nil {
			rows.Close()
			return fmt.Errorf("scan alias: %w", err)
		}

		g, exists := groupMap[inputModelID]
		if !exists {
			g = &config.AliasGroupCfg{InputModelID: inputModelID}
			groupMap[inputModelID] = g
			groupOrder = append(groupOrder, inputModelID)
		}
		g.Options = append(g.Options, config.AliasOptionCfg{
			ID:            id,
			DownstreamID:  downstreamID,
			OutputModelID: outputModelID,
		})
	}
	rows.Close()

	for _, key := range groupOrder {
		g := groupMap[key]
		if len(g.Options) == 0 {
			continue
		}
		aliasGroups = append(aliasGroups, *g)
	}

	if aliasGroups == nil {
		aliasGroups = []config.AliasGroupCfg{}
	}

	// Replace config slices with DB state
	cfg.Downstreams = downstreams
	cfg.Rules = rules
	cfg.Aliases = aliasGroups

	// Build the YAML output: sort aliases by input_model_id for readability
	sort.Slice(cfg.Aliases, func(i, j int) bool {
		return cfg.Aliases[i].InputModelID < cfg.Aliases[j].InputModelID
	})

	// Marshal to YAML
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal YAML: %w", err)
	}

	// Atomic write: write to tmp file in same directory, then rename
	dir := filepath.Dir(cfg.ConfigPath)
	tmpFile := filepath.Join(dir, ".tresor-config.tmp")

	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("write tmp config: %w", err)
	}

	if err := os.Rename(tmpFile, cfg.ConfigPath); err != nil {
		os.Remove(tmpFile) // clean up on failure
		return fmt.Errorf("rename tmp to config: %w", err)
	}

	return nil
}
