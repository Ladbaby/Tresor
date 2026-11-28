package api

// Provider allowance endpoints.
//
// The whole feature is deliberately quiet: a downstream that declares no quota,
// or whose declaration cannot be read, is simply absent from the response. There
// is no "unsupported" flag and no error field for the browser to have to render,
// so a provider nobody has described looks exactly like a provider that does not
// report an allowance. That is the rule the UI depends on.

import (
	"encoding/json"
	"io"
	"net/http"

	"tresor/internal/quota"
)

// quotasResponse is the shape of GET /api/quotas.
type quotasResponse struct {
	// Quotas maps a downstream ID to its reading. A provider not in this map
	// reports nothing and must render nothing.
	Quotas map[string]quota.Reading `json:"quotas"`
}

// SetQuotaFetcher attaches the allowance fetcher. Nil (tests, or a build without
// allowance support) makes /api/quotas answer with an empty map.
func (r *Router) SetQuotaFetcher(f *quota.Fetcher) {
	r.quotaFetcher = f
}

// forgetQuota drops a provider's cached reading. It is called whenever a
// downstream's quota declaration or credentials change: a reading taken against
// the old declaration would otherwise be shown as if it described the new one.
func (r *Router) forgetQuota(id string) {
	if r.quotaFetcher != nil {
		r.quotaFetcher.Forget(id)
	}
}

// handleQuotas serves every current reading in one call.
//
// One request covers all providers because the UI shows them all at once: a
// per-provider call would turn opening the tab into a fan-out, and every extra
// request is another chance for something to be slow or fail.
//
// A stale reading is served as-is and refreshed in the background, so a poll
// never waits on a vendor. The fetcher only reaches the network for a provider
// whose reading has aged out, which means an idle dashboard makes no calls at
// all.
func (r *Router) handleQuotas(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	quotas := map[string]quota.Reading{}
	if r.quotaFetcher != nil {
		quotas = r.quotaFetcher.Snapshot()
	}
	writeJSON(w, http.StatusOK, quotasResponse{Quotas: quotas})
}

// handleQuotaDiscover asks a provider's endpoint what it reports and fills in the
// balance declaration from the answer.
//
// The user supplies an endpoint and nothing else: this walks the reply, and when
// one figure is clearly the balance it is saved and read immediately, so the chip
// appears with no further action. Several plausible figures come back as a ranked
// list to choose between — the paths are never shown, only what each would read.
func (r *Router) handleQuotaDiscover(w http.ResponseWriter, req *http.Request, id string) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.quotaFetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "allowance readings are not available")
		return
	}
	// The endpoint rides on the request: an endpoint alone is not a valid
	// declaration, so it cannot be saved before discovery has found a balance to
	// go with it.
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if req.Body != nil {
		_ = json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&body)
	}
	result, err := r.quotaFetcher.Discover(req.Context(), id, body.Endpoint)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if result.Adopted {
		r.forgetQuota(id)
	}
	writeJSON(w, http.StatusOK, result)
}

// handleQuotasRefreshAll reads every enabled provider that declares a quota, then
// serves the readings in the same shape as GET /api/quotas.
//
// Opening the provider list is the one moment a cached figure is not good enough:
// the list is being read for the first time, so "what was cached a while ago" is
// either empty or not about the key the user just pasted. Reading on open turns
// that into a single wait the person can see, instead of a blank pane that fills
// in at some unpredictable later moment.
func (r *Router) handleQuotasRefreshAll(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	quotas := map[string]quota.Reading{}
	if r.quotaFetcher != nil {
		quotas = r.quotaFetcher.RefreshAll(req.Context())
	}
	writeJSON(w, http.StatusOK, quotasResponse{Quotas: quotas})
}

func (r *Router) handleQuotaRefresh(w http.ResponseWriter, req *http.Request, id string) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.quotaFetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "allowance readings are not available")
		return
	}
	// Derive from the request so a browser that navigates away does not leave a
	// request hanging.
	reading, err := r.quotaFetcher.Refresh(req.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reading)
}
