package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// handleIconRefresh forces an out-of-band sync of the local CDN icon
// index. Useful when:
//   - The user just added a new alias whose icon isn't in the cached index.
//   - The CDN shipped a new slug set since the last periodic sync.
//   - Network was down at startup and the disk-cached index is stale.
//
// URL: POST /api/icons/refresh
// Auth: required (admin session cookie).
//
// Response 200:
//   {"version": "1.91.0", "slug_count": 871}
//
// Response 502 with a plain-text error body when the sync fails.
func (r *Router) handleIconRefresh(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.iconFetcher == nil {
		http.Error(w, "icon fetcher not configured", http.StatusServiceUnavailable)
		return
	}

	// Bound the sync so a slow CDN can't pin this handler open. 30 s
	// matches indexSyncTimeout internally; we set it explicitly here so
	// the client also gets a clear deadline.
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	v, n, err := r.iconFetcher.RefreshIndex(ctx)
	if err != nil {
		http.Error(w, "icon index sync failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":    v,
		"slug_count": n,
	})
}