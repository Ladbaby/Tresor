package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"tresor/internal/proxy"

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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
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
	case "/install-remote", "/approve", "/revoke":
		if req.Method != http.MethodPost {
			writeError(w, 405, "method not allowed")
			return
		}
		if media, _, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err != nil || media != "application/json" {
			writeError(w, 415, "JSON required")
			return
		}
		if action == "/install-remote" {
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
			return
		}
		var input struct {
			ID          string   `json:"id"`
			Version     string   `json:"version"`
			Digest      string   `json:"digest"`
			Permissions []string `json:"permissions"`
		}
		if !decodePackageRequest(w, req, &input) {
			return
		}
		var err error
		if action == "/approve" {
			err = r.pluginManager.Approve(input.ID, input.Version, input.Digest, input.Permissions)
		} else {
			err = r.pluginManager.Revoke(input.ID, input.Version, input.Digest, input.Permissions)
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
