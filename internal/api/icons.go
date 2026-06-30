package api

import (
	"log"
	"net/http"
	"net/url"
	"strings"
)

// handleIcon serves a cached model icon by model ID.
//
// URL: GET /api/icons/{modelID}
//   - {modelID} is URL-encoded; we decode it before matching against patterns.
//   - Public endpoint, mounted alongside /api/health and /api/version — the
//     browser hits this directly via <img src="...">.
//
// Response:
//   200 image/svg+xml  — bytes from the cache (lazily fetched on first miss)
//                        or the generic dummy icon when no slug matches and
//                        the CDN has no file at the candidate slugs.
func (r *Router) handleIcon(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.iconFetcher == nil {
		http.NotFound(w, req)
		return
	}

	raw := strings.TrimPrefix(req.URL.Path, "/api/icons/")
	raw = strings.TrimSuffix(raw, "/")
	if raw == "" {
		http.NotFound(w, req)
		return
	}
	modelID, err := url.PathUnescape(raw)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		http.NotFound(w, req)
		return
	}

	data, ct, err := r.iconFetcher.Icon(modelID)
	if err != nil || len(data) == 0 {
		// Distinguish "no pattern matched" (no log) from "fetch failed" (warn).
		// Either way, fall back to the generic dummy icon so the <img> slot
		// stays filled instead of cascading an onerror=hide chain.
		if err != nil {
			log.Printf("icons: fetch failed for %q: %v", modelID, err)
		}
		data, ct = DefaultIcon()
		// Shorter max-age than real icons: the dummy is branding-generic and
		// may be replaced with a custom art asset at any release boundary.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	if ct == "" {
		ct = "image/svg+xml"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}
