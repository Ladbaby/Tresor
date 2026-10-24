package engine

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"tresor/internal/store"
)

// Reserved execution metadata is never a provider or ordinary plugin header.
func stripIntegrationHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-tresor-execution-") || strings.EqualFold(k, "X-Tresor-Integration-Service") || strings.EqualFold(k, "X-Tresor-Inference-ID") {
			h.Del(k)
		}
	}
}
func executionFromRequest(r *http.Request) *IntegrationExecution {
	x, _ := IntegrationExecutionFromContext(r.Context())
	return x
}
func hasIntegrationHeaders(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-tresor-execution-") || strings.EqualFold(k, "X-Tresor-Integration-Service") || strings.EqualFold(k, "X-Tresor-Inference-ID") {
			return true
		}
	}
	return false
}

type integrationExecutionContextKey struct{}

// IntegrationExecution is the authenticated scope attached to managed execution requests.
type IntegrationExecution struct {
	ExecutionID, Service, Subject, AliasID, AssignmentID string
	Epoch                                                int64
	InferenceID                                          string
}

func IntegrationExecutionFromContext(ctx context.Context) (*IntegrationExecution, bool) {
	x, ok := ctx.Value(integrationExecutionContextKey{}).(*IntegrationExecution)
	return x, ok
}

// HandleIntegrationControl registers and revokes a capability for the authenticated service.
func (e *Engine) HandleIntegrationControl(w http.ResponseWriter, r *http.Request) {
	state := e.integrationState()
	if state == nil {
		integrationError(w, http.StatusUnauthorized, "execution_unauthorized")
		return
	}
	serviceName := r.Header.Get("X-Tresor-Integration-Service")
	service := e.configuredBinding(serviceName)
	if service == nil {
		service = state.services[serviceName]
	}
	if service == nil || service.ControlCredential() == "" || !constantCredential(service.ControlCredential(), bearerToken(r)) {
		integrationError(w, http.StatusUnauthorized, "execution_unauthorized")
		return
	}
	if r.URL.Path != "/api/integrations/executions" && !(r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/integrations/executions/")) {
		integrationError(w, http.StatusNotFound, "not_found")
		return
	}
	switch r.Method {
	case http.MethodPost:
		var input struct {
			ExecutionID  string `json:"execution_id"`
			Subject      string `json:"subject"`
			AliasID      string `json:"alias"`
			AssignmentID string `json:"assignment_id"`
			Epoch        int64  `json:"epoch"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input) != nil {
			integrationError(w, 400, "invalid_request")
			return
		}
		if input.ExecutionID == "" {
			input.ExecutionID = newUUID()
		}
		if alias, err := e.store.FindActiveAlias(input.AliasID); err != nil || alias == nil {
			integrationError(w, 400, "invalid_alias")
			return
		}
		capability, err := e.store.RegisterExecution(store.ExecutionRegistration{ExecutionID: input.ExecutionID, Service: serviceName, Subject: input.Subject, AliasID: input.AliasID, AssignmentID: input.AssignmentID, Epoch: input.Epoch})
		if err != nil {
			integrationError(w, 400, "invalid_request")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"execution_id": input.ExecutionID, "capability": capability})
	case http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/api/integrations/executions/")
		if id == "" || strings.Contains(id, "/") || id == r.URL.Path {
			integrationError(w, 400, "invalid_request")
			return
		}
		if err := e.store.RevokeServiceExecution(id, serviceName); err != nil {
			integrationError(w, 404, "execution_not_found")
			return
		}
		e.executionMu.Lock()
		for _, cancel := range e.activeExecutions[id] {
			cancel()
		}
		delete(e.activeExecutions, id)
		e.executionMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		integrationError(w, 405, "method_not_allowed")
	}
}

func (e *Engine) registerManagedExecution(r *http.Request) (*http.Request, func(), error) {
	ex := executionFromRequest(r)
	if ex == nil {
		return r, func() {}, nil
	}
	e.executionMu.Lock()
	defer e.executionMu.Unlock()
	// Reauthenticate under the cancellation lock to close the revoke/register race.
	current, err := e.store.ActiveExecution(ex.ExecutionID)
	if err != nil || current.Epoch != ex.Epoch {
		return r, nil, store.ErrIntegrationNotFound
	}
	ctx, cancel := context.WithCancel(r.Context())
	id := newUUID()
	if e.activeExecutions == nil {
		e.activeExecutions = map[string]map[string]context.CancelFunc{}
	}
	if e.activeExecutions[ex.ExecutionID] == nil {
		e.activeExecutions[ex.ExecutionID] = map[string]context.CancelFunc{}
	}
	e.activeExecutions[ex.ExecutionID][id] = cancel
	return r.WithContext(ctx), func() {
		cancel()
		e.executionMu.Lock()
		defer e.executionMu.Unlock()
		delete(e.activeExecutions[ex.ExecutionID], id)
		if len(e.activeExecutions[ex.ExecutionID]) == 0 {
			delete(e.activeExecutions, ex.ExecutionID)
		}
	}, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}
func constantCredential(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
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

// authenticateExecution strips reserved headers on every request and attaches authenticated scope when present.
func (e *Engine) authenticateExecution(r *http.Request, body []byte) (*http.Request, *gatewayError) {
	had := hasIntegrationHeaders(r.Header)
	inference := r.Header.Get("X-Tresor-Inference-ID")
	token := r.Header.Get("X-Tresor-Execution-Token")
	epochText := r.Header.Get("X-Tresor-Execution-Epoch")
	stripIntegrationHeaders(r.Header)
	if !had {
		return r, nil
	}
	fail := func() *gatewayError {
		return &gatewayError{http.StatusUnauthorized, "execution unauthorized", "execution unauthorized", "unauthorized", nil}
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions" {
		return r, fail()
	}
	if token == "" || len(r.Header.Values("Authorization")) > 1 {
		return r, fail()
	}
	x, err := e.store.AuthenticateExecution(token)
	if err != nil {
		return r, fail()
	}
	epoch, epochErr := strconv.ParseInt(epochText, 10, 64)
	if epochErr != nil || epoch < 0 || strconv.FormatInt(epoch, 10) != epochText || epoch != x.Epoch {
		return r, &gatewayError{http.StatusUnauthorized, "execution_stale", "execution_stale", "execution_stale", nil}
	}
	state := e.integrationState()
	if e.configuredBinding(x.Service) == nil && (state == nil || state.services[x.Service] == nil) {
		return r, fail()
	}
	model := extractModel(body, r.URL.Path)
	resolved, ge := e.store.FindActiveAlias(model)
	if ge != nil || resolved == nil || model != x.AliasID {
		return r, fail()
	}
	if len(inference) == 0 || len(inference) > 128 {
		return r, fail()
	}
	for _, c := range inference {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return r, fail()
		}
	}
	return r.WithContext(context.WithValue(r.Context(), integrationExecutionContextKey{}, &IntegrationExecution{ExecutionID: x.ExecutionID, Service: x.Service, Subject: x.Subject, AliasID: x.AliasID, AssignmentID: x.AssignmentID, Epoch: x.Epoch, InferenceID: inference})), nil
}
