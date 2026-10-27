package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// Reserved execution metadata is never a provider or ordinary plugin header.
// It is retained only so a stale client cannot spoof identity into the managed
// tool loop; there is no capability token to authenticate any more.
func stripIntegrationHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-tresor-execution-") || strings.EqualFold(k, "X-Tresor-Integration-Service") || strings.EqualFold(k, "X-Tresor-Inference-ID") {
			h.Del(k)
		}
	}
}

// IntegrationExecution is the identity + continuation key carried by managed
// tool-loop requests. It is NOT an authorization grant: there is no capability
// token and no liveness registration. Subject is the bot identity and AliasID
// the requested bot alias, so a tool handler can scope memory and contacts;
// ExecutionID/Epoch/InferenceID key the durable managed transcript and ledger.
type IntegrationExecution struct {
	ExecutionID, Service, Subject, AliasID, AssignmentID string
	Epoch                                                int64
	InferenceID                                          string
}

type integrationExecutionContextKey struct{}
type inferenceReplayKey struct{}

// captureInferenceReplay preserves an optional client-supplied replay key before
// reserved headers are stripped. It is not a credential: it only stabilizes the
// managed transcript key so identical retries replay.
func captureInferenceReplay(r *http.Request) *http.Request {
	v := r.Header.Get("X-Tresor-Inference-ID")
	if v == "" || len(v) > 128 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), inferenceReplayKey{}, v))
}

func inferenceReplayFrom(r *http.Request) string {
	v, _ := r.Context().Value(inferenceReplayKey{}).(string)
	return v
}

func IntegrationExecutionFromContext(ctx context.Context) (*IntegrationExecution, bool) {
	x, ok := ctx.Value(integrationExecutionContextKey{}).(*IntegrationExecution)
	return x, ok
}

func executionFromRequest(r *http.Request) *IntegrationExecution {
	x, _ := IntegrationExecutionFromContext(r.Context())
	return x
}

// withSynthesizedExecution derives the managed tool-loop scope directly from the
// requested model (the bot alias). A caller needs no capability token and no
// pre-registration: identity comes from routing. The continuation key is
// deterministic in the request identity so an identical retry replays against
// the same managed transcript instead of re-running effects.
func withSynthesizedExecution(r *http.Request, service, subject, aliasID string) *http.Request {
	if subject == "" || aliasID == "" {
		return r
	}
	// An optional client-supplied replay key keeps the managed transcript stable
	// across retries of the same logical generation. It carries no authority.
	replay := inferenceReplayFrom(r)
	if replay == "" || len(replay) > 128 {
		replay = newUUID()
	}
	base := subject + "\x00" + aliasID + "\x00" + replay
	sum := sha256.Sum256([]byte(base))
	execID := hex.EncodeToString(sum[:])
	ex := &IntegrationExecution{
		ExecutionID:  execID,
		Service:      service,
		Subject:      subject,
		AliasID:      aliasID,
		AssignmentID: "chat",
		Epoch:        1,
		InferenceID:  "chat-" + execID,
	}
	return r.WithContext(context.WithValue(r.Context(), integrationExecutionContextKey{}, ex))
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// bearerToken reads the Authorization bearer credential. Used by the binding
// resolve endpoint to authenticate a plugin's control credential.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}
