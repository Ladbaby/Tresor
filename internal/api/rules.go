package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"tresor/internal/store"
)

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
		writeJSON(w, http.StatusOK, rules)

	case http.MethodPost:
		var rule store.Rule
		if err := json.NewDecoder(req.Body).Decode(&rule); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if rule.Name == "" || rule.PatternPath == "" {
			writeError(w, http.StatusBadRequest, "name and pattern_path are required")
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
		if err := r.store.CreateRule(&rule); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = r.writeConfig()
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
		writeJSON(w, http.StatusOK, rule)

	case req.Method == http.MethodPut:
		var update struct {
			Name                  string   `json:"name"`
			PatternPath           string   `json:"pattern_path"`
			PatternModel          string   `json:"pattern_model"`
			MatchFormat           []string `json:"match_format"`
			MatchDownstreamFmt    []string `json:"match_downstream_format"`
			MatchDownstreams      []string `json:"match_downstreams"`
			PipelineConfig        string   `json:"pipeline_config"`
			IsEnabled             *bool    `json:"is_enabled"`
		}
		if err := json.NewDecoder(req.Body).Decode(&update); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		// Heuristic: if the payload supplies rule-content fields, do a full update;
		// otherwise treat it as a backward-compatible enabled-only toggle.
		if update.Name != "" || update.PatternPath != "" || update.PipelineConfig != "" ||
			update.MatchFormat != nil || update.MatchDownstreamFmt != nil || update.MatchDownstreams != nil {
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
			existing.PatternModel = update.PatternModel // allow setting to ""
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

		_ = r.writeConfig()
		rule, err := r.store.GetRule(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rule)

	case req.Method == http.MethodDelete:
		if err := r.store.DeleteRule(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = r.writeConfig()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
