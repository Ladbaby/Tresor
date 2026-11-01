package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"tresor/internal/proxy"

	"sort"
	"tresor/internal/config"
	"tresor/internal/jsplugins"
)

// SetPluginManager wires trusted local-package management before serving.
func (r *Router) SetPluginManager(m *jsplugins.Manager) { r.pluginManager = m }

const maxPackageUpload = 8 << 20

func decodePackageRequest(w http.ResponseWriter, req *http.Request, value any) bool {
	req.Body = http.MaxBytesReader(w, req.Body, maxPackageUpload)
	data, err := io.ReadAll(req.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "package request exceeds upload limit")
		} else {
			writeError(w, http.StatusBadRequest, "invalid package request")
		}
		return false
	}
	if err := jsplugins.StrictJSON(data, value); err != nil {
		writeError(w, http.StatusBadRequest, "expected one unambiguous JSON request object")
		return false
	}
	return true
}

func (r *Router) handlePluginPackages(w http.ResponseWriter, req *http.Request) {
	if r.pluginManager == nil {
		writeError(w, http.StatusServiceUnavailable, "local plugin management unavailable")
		return
	}
	if req.Method == http.MethodPost {
		media, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		// Legacy headerless local installs remain supported; new operations require JSON.
		if req.Header.Get("Content-Type") != "" && (err != nil || media != "application/json") {
			writeError(w, 415, "JSON required")
			return
		}
		if origin := req.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Host != req.Host || (u.Scheme != "http" && u.Scheme != "https") {
				writeError(w, 403, "cross-origin package mutation prohibited")
				return
			}
		}
		if req.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, 403, "cross-site package mutation prohibited")
			return
		}
	}
	action := strings.TrimPrefix(req.URL.Path, "/api/plugin-packages")
	switch action {
	case "":
		if req.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		items := r.pluginManager.List()
		type packageView struct {
			ID          string `json:"id"`
			Version     string `json:"version"`
			Description string `json:"description"`
			Active      bool   `json:"active"`
			Compatible  bool   `json:"compatible"`
			Executable  bool   `json:"executable"`
			Diagnostic  string `json:"diagnostic,omitempty"`
			// ManagedBy names the provider-installed downstreams that depend on
			// this package. Such a package is not a rule plugin: it belongs to a
			// provider and the provider's recipe installed it, so it must not be
			// offered as a pipeline step or deleted out from under the provider.
			ManagedBy []string `json:"managed_by,omitempty"`
		}
		managed := r.managedPluginOwners()
		views := make([]packageView, 0, len(items))
		for _, p := range items {
			views = append(views, packageView{
				ID: p.ID, Version: p.Version, Description: p.Description,
				Active: p.Active, Compatible: p.Compatible, Executable: p.Executable,
				Diagnostic: p.Diagnostic, ManagedBy: managed[p.ID],
			})
		}
		writeJSON(w, http.StatusOK, views)
	case "/install":
		if req.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var input struct {
			Files map[string]string `json:"files"`
		}
		if !decodePackageRequest(w, req, &input) {
			return
		}
		manifest, err := r.pluginManager.Install(input.Files)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(manifest)
	case "/install-remote":
		if req.Method != http.MethodPost {
			writeError(w, 405, "method not allowed")
			return
		}
		if media, _, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err != nil || media != "application/json" {
			writeError(w, 415, "JSON required")
			return
		}
		var input jsplugins.RemoteInput
		if !decodePackageRequest(w, req, &input) {
			return
		}
		runtimeCfgMu.RLock()
		mode := proxy.Mode(runtimeCfg.ProxyMode)
		runtimeCfgMu.RUnlock()
		manifest, err := r.pluginManager.InstallRemote(req.Context(), input, mode)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, manifest)
	case "/enabled", "/delete":
		if req.Method != http.MethodPost {
			writeError(w, 405, "method not allowed")
			return
		}
		if media, _, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err != nil || media != "application/json" {
			writeError(w, 415, "JSON required")
			return
		}
		var input struct {
			ID      string `json:"id"`
			Version string `json:"version"`
			Enabled *bool  `json:"enabled,omitempty"`
		}
		if !decodePackageRequest(w, req, &input) {
			return
		}
		var err error
		if action == "/delete" {
			if input.Enabled != nil {
				writeError(w, 400, "enabled is not a delete field")
				return
			}
			err = r.pluginManager.Delete(input.ID, input.Version)
		} else {
			if input.Enabled == nil {
				writeError(w, 400, "enabled is required")
				return
			}
			err = r.pluginManager.SetEnabled(input.ID, input.Version, *input.Enabled)
		}
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "/activate":
		if req.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var input struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		}
		if !decodePackageRequest(w, req, &input) {
			return
		}
		if err := r.pluginManager.Activate(input.ID, input.Version); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": input.ID, "version": input.Version, "active": true})
	default:
		http.NotFound(w, req)
	}
}

// managedPluginOwners maps a plugin id to the downstreams that came from a
// provider profile and list that plugin in their pipeline. Derived rather than
// stored: the downstream's own recipe is the record of what it needs, so this
// cannot drift from the truth, and uninstalling the provider frees the plugin.
// A plugin the user added to the same downstream by hand is excluded, because
// only packages recorded by the profile install are "managed".
func (r *Router) managedPluginOwners() map[string][]string {
	owners := map[string][]string{}
	provenance, err := r.store.ListDownstreamProfiles()
	if err != nil || len(provenance) == 0 {
		return owners
	}
	downstreams, err := r.store.ListDownstreams()
	if err != nil {
		return owners
	}
	for _, d := range downstreams {
		if _, fromProfile := provenance[d.ID]; !fromProfile {
			continue
		}
		for _, step := range d.Plugins {
			id := step.PluginID
			if id == "" {
				id = step.ID
			}
			if id == "" {
				continue
			}
			if !contains(owners[id], d.ID) {
				owners[id] = append(owners[id], d.ID)
			}
		}
	}
	for id := range owners {
		sort.Strings(owners[id])
	}
	return owners
}

// packageIDsManagedBy lists the plugin packages that only downstream serves.
//
// Called after the downstream is deleted, so the ownership map has already lost it:
// whatever this downstream used to bind and nothing else claims is now orphaned.
// Deleting the last provider therefore removes the code it installed, instead of
// leaving a package behind that no provider owns — which then reappears in the
// ordinary plugin table as though the user had installed it.
func (r *Router) packageIDsManagedBy(downstreamID string, bound []config.PipelineStep) []string {
	if r.pluginManager == nil || len(bound) == 0 {
		return nil
	}
	owners := r.managedPluginOwners()
	seen := map[string]bool{}
	var out []string
	for _, step := range bound {
		id := step.PluginID
		if id == "" {
			id = step.ID
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		// Still claimed by another provider: keep it.
		if len(owners[id]) > 0 {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// removeOrphanedPlugins deletes the packages that belonged only to a downstream
// that no longer exists. Best effort: a failure is logged and never fails the
// delete the user asked for.
func (r *Router) removeOrphanedPlugins(bound []config.PipelineStep, downstreamID string) {
	for _, id := range r.packageIDsManagedBy(downstreamID, bound) {
		for _, pkg := range r.pluginManager.List() {
			if pkg.ID != id {
				continue
			}
			if err := r.pluginManager.Delete(pkg.ID, pkg.Version); err != nil {
				log.Printf("provider plugins: could not remove %s@%s after deleting %s: %v",
					pkg.ID, pkg.Version, downstreamID, err)
			}
		}
	}
}

// contains reports whether list already holds want.
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
