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
//                        or the generic dummy icon when no slug matches /
//                        the CDN is unreachable. The slot is never blank; the
//                        browser never has to substitute its own fallback.
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
	if len(data) == 0 || err != nil {
		// Either no pattern matched (every candidate slug was absent from
		// the CDN index) or the fetch failed (network error, 5xx). Both
		// states should yield a filled <img> slot, so fall back to the
		// generic dummy icon instead of 404-ing. The dummy is monochrome
		// enough to be visually distinguishable from a real provider
		// icon while still keeping the row's leading glyph consistent.
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
