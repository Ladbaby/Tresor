package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"tresor/internal/jsplugins"
	"tresor/internal/providers"
	"tresor/internal/proxy"
	"tresor/internal/store"
)

// maxProfileBytes bounds a posted provider profile. A profile is a few KB at
// most; anything larger is either a mistake or an attempt to make the daemon
// buffer something unreasonable.
const maxProfileBytes = 256 << 10

// handleProviders serves /api/providers and its sub-resources.
//
//	POST /api/providers/install       install a profile (inline definition or id)
//	GET  /api/providers               list installed providers with provenance
//	GET  /api/providers/{id}          one installed provider
//	POST /api/providers/{id}/update   apply a newer profile, non-destructively
//	DELETE /api/providers/{id}        uninstall (deletes the downstream)
//
// newInstaller builds an installer that can download the code a profile asks
// for. Without a plugin runtime it still installs the data half; a binding that
// carries a URL is then reported instead of silently dropped. Nil-safe so the
// handler works in tests that construct a Router without a runtime.
func (r *Router) newInstaller() *providers.Installer {
	installer := providers.NewInstaller(r.store, r.cfg)
	if r.pluginManager == nil {
		return installer
	}
	runtimeCfgMu.RLock()
	mode := proxy.Mode(runtimeCfg.ProxyMode)
	runtimeCfgMu.RUnlock()
	return installer.WithPackages(r.pluginManager, mode)
}

func (r *Router) handleProviders(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/api/providers")
	path = strings.Trim(path, "/")
	segments := []string{}
	if path != "" {
		segments = strings.Split(path, "/")
	}

	switch {
	case len(segments) == 0:
		if req.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		r.listInstalledProviders(w)

	case segments[0] == "install":
		if req.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		r.installProvider(w, req)

	case len(segments) == 1:
		switch req.Method {
		case http.MethodGet:
			r.getInstalledProvider(w, segments[0])
		case http.MethodDelete:
			// This endpoint uninstalls a provider. A hand-made downstream is not a
			// provider, and deleting it here would destroy a user's own provider
			// through a route that cannot recreate it — use the downstreams API.
			prov, err := r.store.GetDownstreamProfile(segments[0])
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if prov == nil {
				writeError(w, http.StatusConflict,
					"downstream "+segments[0]+" was not installed from a provider profile; delete it from the Downstreams tab")
				return
			}
			existing, _ := r.store.GetDownstream(segments[0])
			if err := r.store.DeleteDownstream(segments[0]); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if existing != nil {
				r.removeOrphanedPlugins(existing.Plugins, segments[0])
			}
			r.requestConfigWrite()
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case len(segments) == 2 && segments[1] == "update":
		if req.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		r.updateProvider(w, req, segments[0])

	default:
		http.NotFound(w, req)
	}
}

// installedProvider is a downstream annotated with its provenance.
type installedProvider struct {
	store.Downstream
	Profile *store.DownstreamProfile `json:"profile,omitempty"`
}

func (r *Router) listInstalledProviders(w http.ResponseWriter) {
	downstreams, err := r.store.ListDownstreams()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	provenance, err := r.store.ListDownstreamProfiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]installedProvider, 0, len(downstreams))
	for _, d := range downstreams {
		maskDownstreamAPIKey(&d)
		item := installedProvider{Downstream: d}
		if p, ok := provenance[d.ID]; ok {
			p := p
			item.Profile = &p
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (r *Router) getInstalledProvider(w http.ResponseWriter, id string) {
	d, err := r.store.GetDownstream(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	maskDownstreamAPIKey(d)
	item := installedProvider{Downstream: *d}
	if p, err := r.store.GetDownstreamProfile(id); err == nil && p != nil {
		item.Profile = p
	}
	writeJSON(w, http.StatusOK, item)
}

// installRequest is the body of POST /api/providers/install. Exactly one of
// Profile or ProfileID selects what to install.
type installRequest struct {
	// Profile is an inline tresor-provider-v1 definition, which is how a local
	// or user-authored provider is installed.
	Profile json.RawMessage `json:"profile,omitempty"`
	// ProfileID installs an entry from the catalog. Wired in with the catalog.
	ProfileID    string `json:"profile_id,omitempty"`
	DownstreamID string `json:"downstream_id,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// AllowPlugins is the user's consent to install the profile's plugin code.
	AllowPlugins bool `json:"allow_plugins,omitempty"`
	// Region selects which of the profile's endpoint choices to install at. Empty
	// takes the profile's default. The id is validated against the profile, so an
	// unknown one fails the install rather than quietly picking another region.
	Region string `json:"region,omitempty"`
}

func (r *Router) installProvider(w http.ResponseWriter, req *http.Request) {
	var input installRequest
	body, err := io.ReadAll(io.LimitReader(req.Body, maxProfileBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) > maxProfileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "provider profile is too large")
		return
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(input.Profile) == 0 && input.ProfileID == "" {
		writeError(w, http.StatusBadRequest, "one of profile or profile_id is required")
		return
	}
	// Either an inline definition (local or user-authored) or a catalog id.
	var profile *providers.Profile
	if input.ProfileID != "" {
		if r.catalog == nil {
			writeError(w, http.StatusServiceUnavailable, "the provider catalog is not available; post an inline profile instead")
			return
		}
		p, err := r.catalog.FetchProfile(req.Context(), input.ProfileID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		profile = p
	} else {
		p, err := providers.Parse(input.Profile)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		profile = p
	}

	installer := r.newInstaller()
	res, err := installer.Install(profile, providers.InstallOptions{
		DownstreamID: input.DownstreamID,
		APIKey:       input.APIKey,
		ClientSecret: input.ClientSecret,
		AllowPlugins: input.AllowPlugins,
		HostFeatures: jsplugins.SupportedFeatures(),
		HostVersion:  r.version,
		Region:       input.Region,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (r *Router) updateProvider(w http.ResponseWriter, req *http.Request, downstreamID string) {
	var input installRequest
	body, err := io.ReadAll(io.LimitReader(req.Body, maxProfileBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) > maxProfileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "provider profile is too large")
		return
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(input.Profile) == 0 {
		writeError(w, http.StatusBadRequest, "profile is required")
		return
	}
	profile, err := providers.Parse(input.Profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	installer := r.newInstaller()
	res, err := installer.Update(downstreamID, profile, providers.InstallOptions{
		APIKey:       input.APIKey,
		ClientSecret: input.ClientSecret,
		AllowPlugins: input.AllowPlugins,
		HostFeatures: jsplugins.SupportedFeatures(),
		HostVersion:  r.version,
		Region:       input.Region,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// catalogEntryView is a catalog entry plus what this daemon makes of it.
type catalogEntryView struct {
	providers.CatalogEntry
	Available bool     `json:"available"`
	Reason    string   `json:"reason,omitempty"`
	Installed []string `json:"installed,omitempty"`
	// NeedsPlugins tells the UI to ask for consent before installing: a profile
	// that binds a plugin pulls in executable code, not just data.
	NeedsPlugins bool `json:"needs_plugins,omitempty"`
}

// handleProviderCatalog serves the provider catalog.
//
//	GET  /api/provider-catalog           list entries with availability
//	POST /api/provider-catalog/refresh   fetch the remote index now
//	GET  /api/provider-catalog/{id}      one entry's full profile
func (r *Router) handleProviderCatalog(w http.ResponseWriter, req *http.Request) {
	path := strings.Trim(strings.TrimPrefix(req.URL.Path, "/api/provider-catalog"), "/")
	if r.catalog == nil {
		if path == "" && req.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{
				"entries": []any{},
				"source":  "unavailable",
				"note":    "the provider catalog is not available in this build",
			})
			return
		}
		writeError(w, http.StatusServiceUnavailable, "the provider catalog is not available")
		return
	}

	switch {
	case path == "":
		if req.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		r.listCatalog(w)

	case path == "refresh":
		if req.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := r.catalog.Refresh(req.Context()); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "refreshed", "source": r.catalog.Source()})

	default:
		if req.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		profile, err := r.catalog.FetchProfile(req.Context(), path)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"profile": profile})
	}
}

// listCatalog annotates each entry with availability and which downstreams
// already came from it, so the UI can offer "Installed" rather than reinstalling.
func (r *Router) listCatalog(w http.ResponseWriter) {
	entries := r.catalog.Entries()

	installed := map[string][]string{}
	if provenance, err := r.store.ListDownstreamProfiles(); err == nil {
		for downstreamID, p := range provenance {
			installed[p.ProfileID] = append(installed[p.ProfileID], downstreamID)
		}
	}

	out := make([]catalogEntryView, 0, len(entries))
	for _, e := range entries {
		avail := r.catalog.Availability(e)
		view := catalogEntryView{CatalogEntry: e, Available: avail.Available, Reason: avail.Reason, NeedsPlugins: e.NeedsPlugins}
		if ids, ok := installed[e.ID]; ok {
			sort.Strings(ids)
			view.Installed = ids
		}
		out = append(out, view)
	}
	resp := map[string]any{
		"entries": out,
		"source":  r.catalog.Source(),
		"ready":   r.catalog.Ready(),
	}
	// An empty catalog must explain itself, or it looks like there are no providers.
	if err := r.catalog.LastError(); err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}
