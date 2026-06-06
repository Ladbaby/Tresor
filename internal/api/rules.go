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
		// Validate downstream exists if specified
		if rule.ActiveDownstream != "" {
			if _, err := r.store.GetDownstream(rule.ActiveDownstream); err != nil {
				writeError(w, http.StatusBadRequest, "active_downstream not found: "+rule.ActiveDownstream)
				return
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
	// Extract rule ID from path: /api/rules/{id} or /api/rules/{id}/switch
	path := strings.TrimPrefix(req.URL.Path, "/api/rules/")
	id := path
	action := ""

	if idx := strings.Index(path, "/"); idx >= 0 {
		id = path[:idx]
		action = path[idx+1:]
	}

	switch {
	case action == "switch" && req.Method == http.MethodPut:
		r.handleSwitchRule(w, req, id)
	case req.Method == http.MethodGet:
		rule, err := r.store.GetRule(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rule)

	case req.Method == http.MethodPut:
		var update struct {
			Name             string `json:"name"`
			PatternPath      string `json:"pattern_path"`
			PatternModel     string `json:"pattern_model"`
			ActiveDownstream string `json:"active_downstream"`
			PipelineConfig   string `json:"pipeline_config"`
			IsEnabled        *bool  `json:"is_enabled"`
		}
		if err := json.NewDecoder(req.Body).Decode(&update); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		// Heuristic: if the payload supplies rule-content fields, do a full update;
		// otherwise treat it as a backward-compatible enabled-only toggle.
		if update.Name != "" || update.PatternPath != "" || update.PipelineConfig != "" {
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
			if update.ActiveDownstream != "" {
				if _, err := r.store.GetDownstream(update.ActiveDownstream); err != nil {
					writeError(w, http.StatusBadRequest, "active_downstream not found: "+update.ActiveDownstream)
					return
				}
				existing.ActiveDownstream = update.ActiveDownstream
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

// handleSwitchRule handles PUT /api/rules/{id}/switch
func (r *Router) handleSwitchRule(w http.ResponseWriter, req *http.Request, id string) {
	var body struct {
		DownstreamID string `json:"downstream_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.DownstreamID == "" {
		writeError(w, http.StatusBadRequest, "downstream_id is required")
		return
	}

	// Validate downstream exists
	if _, err := r.store.GetDownstream(body.DownstreamID); err != nil {
		writeError(w, http.StatusNotFound, "downstream not found: "+body.DownstreamID)
		return
	}

	if err := r.store.UpdateRuleDownstream(id, body.DownstreamID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	_ = r.writeConfig()
	rule, err := r.store.GetRule(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rule)
}
