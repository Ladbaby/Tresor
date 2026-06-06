package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"tresor/internal/store"
)

// handleAliases handles GET and POST on /api/aliases.
func (r *Router) handleAliases(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		groups, err := r.store.ListGroups()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if groups == nil {
			groups = []store.AliasGroup{}
		}
		// Enrich with downstream names for the UI
		enrichAliasGroups(r.store, groups)
		writeJSON(w, http.StatusOK, groups)

	case http.MethodPost:
		var alias store.Alias
		if err := json.NewDecoder(req.Body).Decode(&alias); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if alias.InputModelID == "" || alias.OutputModelID == "" || alias.DownstreamID == "" {
			writeError(w, http.StatusBadRequest, "input_model_id, output_model_id, and downstream_id are required")
			return
		}
		// Validate downstream exists
		if _, err := r.store.GetDownstream(alias.DownstreamID); err != nil {
			writeError(w, http.StatusBadRequest, "downstream not found: "+alias.DownstreamID)
			return
		}
		if err := r.store.CreateAlias(&alias); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = r.writeConfig()
		writeJSON(w, http.StatusCreated, alias)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAliasByID handles GET, PUT, DELETE on /api/aliases/{id} and activate.
func (r *Router) handleAliasByID(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/api/aliases/")
	id := path
	action := ""

	if idx := strings.Index(path, "/"); idx >= 0 {
		id = path[:idx]
		action = path[idx+1:]
	}

	switch {
	case action == "activate" && req.Method == http.MethodPut:
		r.handleActivateAlias(w, req, id)

	case req.Method == http.MethodGet:
		alias, err := r.store.GetAlias(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, alias)

	case req.Method == http.MethodPut:
		var update store.Alias
		if err := json.NewDecoder(req.Body).Decode(&update); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		update.ID = id

		// Validate downstream exists if being updated
		if update.DownstreamID != "" {
			if _, err := r.store.GetDownstream(update.DownstreamID); err != nil {
				writeError(w, http.StatusBadRequest, "downstream not found: "+update.DownstreamID)
				return
			}
		}

		if err := r.store.UpdateAlias(&update); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		_ = r.writeConfig()

		alias, err := r.store.GetAlias(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, alias)

	case req.Method == http.MethodDelete:
		if err := r.store.DeleteAlias(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = r.writeConfig()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAliasGroup handles DELETE on /api/aliases/group/{inputModelId}.
// Deletes all alias options sharing the same InputModelID.
func (r *Router) handleAliasGroup(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	inputModelID := strings.TrimPrefix(req.URL.Path, "/api/aliases/group/")
	if inputModelID == "" {
		writeError(w, http.StatusBadRequest, "input_model_id is required")
		return
	}

	n, err := r.store.DeleteGroup(inputModelID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	_ = r.writeConfig()
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "deleted", "count": n})
}

// handleActivateAlias handles PUT /api/aliases/{id}/activate
func (r *Router) handleActivateAlias(w http.ResponseWriter, req *http.Request, id string) {
	if err := r.store.ActivateAlias(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	_ = r.writeConfig()

	alias, err := r.store.GetAlias(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, alias)
}

// enrichAliasGroups fills in the downstream name for each option in every group.
func enrichAliasGroups(s *store.Store, groups []store.AliasGroup) {
	// Build a downstream ID -> name map
	downstreams, err := s.ListDownstreams()
	if err != nil {
		return
	}
	dsMap := make(map[string]string)
	for _, d := range downstreams {
		dsMap[d.ID] = d.Name
	}

	for i := range groups {
		for j := range groups[i].Options {
			groups[i].Options[j].DownstreamName = dsMap[groups[i].Options[j].DownstreamID]
		}
	}
}

// writeConfig triggers a synchronous YAML write-back after mutating the DB.
// Errors are returned so callers can surface a warning in the API response.
func (r *Router) writeConfig() error {
	if err := r.store.WriteConfig(r.cfg); err != nil {
		log.Printf("warning: failed to write config YAML: %v", err)
		return err
	}
	return nil
}
