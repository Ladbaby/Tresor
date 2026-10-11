package engine

import (
	"net/http"

	"github.com/google/uuid"
)

// codexRequest carries backend-specific wire preparation between body creation
// and late header application. Generic forwarding does not use these effects.
type codexRequest struct {
	body          []byte
	correlationID string
	compressed    bool
}

func prepareCodexRequest(original *http.Request, body []byte) codexRequest {
	// Stable client identity wins, followed by a stable prompt prefix. A random
	// fallback is correlation-only and must never become a prompt cache key.
	sessionID := codexCacheKey(original)
	if sessionID == "" {
		sessionID = codexStableCacheKey(body)
	}
	correlationID := sessionID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	prepared := codexRequest{body: codexFingerprintBody(body, sessionID), correlationID: correlationID}
	if zstdEncoder != nil && isStreamRequest(prepared.body) {
		compressed := zstdEncoder.EncodeAll(prepared.body, nil)
		if len(compressed) > 0 && len(compressed) < len(prepared.body) {
			prepared.body = compressed
			prepared.compressed = true
		}
	}
	return prepared
}

// applyHeaders runs after generic authentication and OAuth metadata, preserving
// caller beta opt-ins while overriding correlation headers with the chosen ID.
func (p codexRequest) applyHeaders(req *http.Request) {
	if p.compressed {
		req.Header.Set("Content-Encoding", "zstd")
		req.Header.Del("Content-Length")
	}
	if req.Header.Get("OpenAI-Beta") == "" {
		req.Header.Set("OpenAI-Beta", "responses=experimental")
	}
	req.Header.Set("session-id", p.correlationID)
	req.Header.Set("x-client-request-id", p.correlationID)
}
