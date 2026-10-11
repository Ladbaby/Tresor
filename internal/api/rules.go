package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"tresor/internal/engine"
	"tresor/internal/jsplugins"
	"tresor/internal/store"
)

// validateJSPipeline validates newly supplied user pipeline configuration.
func (r *Router) validateJSPipeline(raw string) error {
	if raw == "" {
		return nil
	}
	var steps []engine.PipelineStepConfig
	if err := json.Unmarshal([]byte(raw), &steps); err != nil {
		return fmt.Errorf("invalid pipeline: %w", err)
	}
	for index, step := range steps {
		if engine.IsInternalPlugin(r.engine.Registry(), step.PluginID) {
			return fmt.Errorf("step %d: %s is internal-only; API format conversion is automatic", index, step.PluginID)
		}
	}
	if r.pluginManager == nil {
		return nil
	}
	active, blocked := r.pluginManager.ExecutionSnapshot()
	for index, step := range steps {
		if step.FailurePolicy != "" && step.FailurePolicy != "fail_closed" && step.FailurePolicy != "safe_passthrough" {
			return fmt.Errorf("step %d: unknown failure_policy", index)
		}
		if reason := blocked[step.PluginID]; reason != "" {
			return fmt.Errorf("step %d: plugin %s %s", index, step.PluginID, reason)
		}
		pkg := active[step.PluginID]
		if pkg == nil {
			if jsplugins.ValidPackageID(step.PluginID) {
				return fmt.Errorf("step %d: JS plugin %s is not active", index, step.PluginID)
			}
			continue
		}
		if _, err := jsplugins.NewInstance(pkg, step.Config); err != nil {
			return fmt.Errorf("step %d: %w", index, err)
		}
		if err := jsplugins.ValidatePolicy(pkg, step.Config, step.FailurePolicy); err != nil {
			return fmt.Errorf("step %d: %w", index, err)
		}
	}
	return nil
}

// Rule-only display metadata does not alter stored DB/YAML pipelines.
type ruleView struct {
	store.Rule
	IgnoredPipelineSteps []int `json:"ignored_pipeline_steps,omitempty"`
}

func (r *Router) ruleView(rule store.Rule) ruleView {
	view := ruleView{Rule: rule}
	var steps []engine.PipelineStepConfig
	if json.Unmarshal([]byte(rule.PipelineConfig), &steps) == nil {
		for index, step := range steps {
			if engine.IsInternalPlugin(r.engine.Registry(), step.PluginID) {
				view.IgnoredPipelineSteps = append(view.IgnoredPipelineSteps, index)
			}
		}
	}
	return view
}

// handleRules handles GET and POST on /api/rules.
func (r *Router) handleRules(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		rules, err := r.store.ListRules()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rules == nil {
			rules = []store.Rule{}
		}
		views := make([]ruleView, 0, len(rules))
		for _, rule := range rules {
			views = append(views, r.ruleView(rule))
		}
		writeJSON(w, http.StatusOK, views)

	case http.MethodPost:
		var payload struct {
			ID                 string    `json:"id"`
			Name               string    `json:"name"`
			PatternPath        string    `json:"pattern_path"`
			PatternModel       *string   `json:"pattern_model"`
			PatternModels      *[]string `json:"pattern_models"`
			MatchFormat        []string  `json:"match_format"`
			MatchDownstreamFmt []string  `json:"match_downstream_format"`
			MatchDownstreams   []string  `json:"match_downstreams"`
			PipelineConfig     string    `json:"pipeline_config"`
			IsEnabled          bool      `json:"is_enabled"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		rule := store.Rule{
			ID: payload.ID, Name: payload.Name, PatternPath: payload.PatternPath,
			MatchFormat: payload.MatchFormat, MatchDownstreamFmt: payload.MatchDownstreamFmt,
			MatchDownstreams: payload.MatchDownstreams, PipelineConfig: payload.PipelineConfig,
			IsEnabled: payload.IsEnabled,
		}
		if payload.PatternModels != nil {
			rule.PatternModels = *payload.PatternModels
		} else if payload.PatternModel != nil && *payload.PatternModel != "" {
			rule.PatternModels = []string{*payload.PatternModel}
		}
		if rule.Name == "" || rule.PatternPath == "" {
			writeError(w, http.StatusBadRequest, "name and pattern_path are required")
			return
		}
		if err := r.validateJSPipeline(rule.PipelineConfig); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Validate downstreams exist if specified
		if len(rule.MatchDownstreams) > 0 {
			for _, dsID := range rule.MatchDownstreams {
				if _, err := r.store.GetDownstream(dsID); err != nil {
					writeError(w, http.StatusBadRequest, "match_downstreams not found: "+dsID)
					return
				}
			}
		}
		// Initialize nil slices to empty for storage
		if rule.MatchFormat == nil {
			rule.MatchFormat = []string{}
		}
		if rule.MatchDownstreamFmt == nil {
			rule.MatchDownstreamFmt = []string{}
		}
		if rule.MatchDownstreams == nil {
			rule.MatchDownstreams = []string{}
		}
		if rule.PatternModels == nil {
			rule.PatternModels = []string{}
		}
		if err := r.store.CreateRule(&rule); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		r.requestConfigWrite()
		writeJSON(w, http.StatusCreated, rule)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRuleByID handles GET, PUT, DELETE on /api/rules/{id}
func (r *Router) handleRuleByID(w http.ResponseWriter, req *http.Request) {
	// Extract rule ID from path: /api/rules/{id}
	path := strings.TrimPrefix(req.URL.Path, "/api/rules/")
	id := path

	if idx := strings.Index(path, "/"); idx >= 0 {
		id = path[:idx]
	}

	switch {
	case req.Method == http.MethodGet:
		rule, err := r.store.GetRule(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, r.ruleView(*rule))

	case req.Method == http.MethodPut:
		var update struct {
			Name               string    `json:"name"`
			PatternPath        string    `json:"pattern_path"`
			PatternModel       *string   `json:"pattern_model"`
			PatternModels      *[]string `json:"pattern_models"`
			MatchFormat        []string  `json:"match_format"`
			MatchDownstreamFmt []string  `json:"match_downstream_format"`
			MatchDownstreams   []string  `json:"match_downstreams"`
			PipelineConfig     string    `json:"pipeline_config"`
			IsEnabled          *bool     `json:"is_enabled"`
		}
		if err := json.NewDecoder(req.Body).Decode(&update); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		// Heuristic: if the payload supplies rule-content fields, do a full update;
		// otherwise treat it as a backward-compatible enabled-only toggle.
		if update.Name != "" || update.PatternPath != "" || update.PipelineConfig != "" ||
			update.MatchFormat != nil || update.MatchDownstreamFmt != nil || update.MatchDownstreams != nil ||
			update.PatternModels != nil || update.PatternModel != nil {
			// Full rule update
			existing, err := r.store.GetRule(id)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}

			if update.Name != "" {
				existing.Name = update.Name
			}
			if update.PatternPath != "" {
				existing.PatternPath = update.PatternPath
			}
			// Legacy pattern_model is accepted on input, but responses are canonical.
			if update.PatternModels != nil {
				existing.PatternModels = *update.PatternModels
			} else if update.PatternModel != nil {
				existing.PatternModels = []string{}
				if *update.PatternModel != "" {
					existing.PatternModels = []string{*update.PatternModel}
				}
			}
			if update.MatchFormat != nil {
				existing.MatchFormat = update.MatchFormat
			}
			if update.MatchDownstreamFmt != nil {
				existing.MatchDownstreamFmt = update.MatchDownstreamFmt
			}
			if update.MatchDownstreams != nil {
				// Validate downstreams exist
				for _, dsID := range update.MatchDownstreams {
					if _, err := r.store.GetDownstream(dsID); err != nil {
						writeError(w, http.StatusBadRequest, "match_downstreams not found: "+dsID)
						return
					}
				}
				existing.MatchDownstreams = update.MatchDownstreams
			}
			if update.PipelineConfig != "" {
				if !json.Valid([]byte(update.PipelineConfig)) {
					writeError(w, http.StatusBadRequest, "pipeline_config must be valid JSON")
					return
				}
				existing.PipelineConfig = update.PipelineConfig
			}
			if update.IsEnabled != nil {
				existing.IsEnabled = *update.IsEnabled
			}

			// Only newly supplied pipelines reject legacy internal converter steps.
			if update.PipelineConfig != "" {
				if err := r.validateJSPipeline(existing.PipelineConfig); err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			if err := r.store.UpdateRule(existing); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		} else if update.IsEnabled != nil {
			// Partial update: enabled state only
			if err := r.store.UpdateRuleEnabled(id, *update.IsEnabled); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		} else {
			writeError(w, http.StatusBadRequest, "no fields to update")
			return
		}

		r.requestConfigWrite()
		rule, err := r.store.GetRule(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, r.ruleView(*rule))

	case req.Method == http.MethodDelete:
		if err := r.store.DeleteRule(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		r.requestConfigWrite()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
