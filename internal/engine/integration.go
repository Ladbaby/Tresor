package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
	"tresor/internal/integrations"
	"tresor/internal/jsplugins"
)

// RoutingContext is a credential-free value snapshot of the original resolution.
type RoutingContext struct {
	RequestedModel string            `json:"requestedModel"`
	Alias          *RoutingAlias     `json:"alias"`
	ResolvedModel  string            `json:"resolvedModel"`
	Downstream     RoutingDownstream `json:"downstream"`
}
type RoutingAlias struct {
	ID         string `json:"id"`
	InputModel string `json:"inputModel"`
	IsRegex    bool   `json:"isRegex"`
}
type RoutingDownstream struct {
	ID     string `json:"id"`
	Format string `json:"format"`
}

func routingContext(result *modelResult, format string) RoutingContext {
	c := RoutingContext{RequestedModel: result.model, ResolvedModel: result.resolvedModel, Downstream: RoutingDownstream{result.ds.ID, format}}
	if a := result.alias; a != nil {
		c.Alias = &RoutingAlias{a.ID, a.InputModelID, a.IsRegex}
	}
	return c
}
func jsonObject(value any) map[string]any {
	b, _ := json.Marshal(value)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// Service SSE is already in the client protocol. Bound each event, not the
// subscription lifetime, and never route it through provider converters.
func copyIntegrationStream(w *statusCaptureWriter, resp *http.Response) error {
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return fmt.Errorf("unsupported integration content type")
	}
	w.WriteHeader(resp.StatusCode)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), externalValueLimit)
	eventBytes := 0
	var event strings.Builder
	done := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "data: [DONE]" {
			done = true
		}
		eventBytes += len(line) + 1
		if eventBytes > externalValueLimit {
			_, _ = fmt.Fprint(w, "data: {\"error\":{\"code\":\"integration_unavailable\"}}\n\n")
			w.Flush()
			return fmt.Errorf("oversized integration event")
		}
		event.WriteString(line)
		event.WriteByte('\n')
		if line == "" {
			if _, err := fmt.Fprint(w, event.String()); err != nil {
				return err
			}
			event.Reset()
			eventBytes = 0
			w.Flush()
			if done {
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_, _ = fmt.Fprint(w, "\ndata: {\"error\":{\"code\":\"integration_unavailable\"}}\n\n")
		w.Flush()
		return err
	}
	if event.Len() > 0 || !done {
		_, _ = fmt.Fprint(w, "data: {\"error\":{\"code\":\"integration_unavailable\"}}\n\n")
		w.Flush()
		return fmt.Errorf("incomplete integration event")
	}
	return nil
}

type integrationSnapshot struct {
	services map[string]*integrations.Snapshot
	grants   []integrations.Grant
}
type integrationSnapshotKey struct{}

func (e *Engine) requestIntegrationState(r *http.Request) *integrationSnapshot {
	if s, ok := r.Context().Value(integrationSnapshotKey{}).(*integrationSnapshot); ok {
		return s
	}
	return e.integrationState()
}
func (e *Engine) integrationState() *integrationSnapshot {
	e.integrationMu.RLock()
	defer e.integrationMu.RUnlock()
	return e.integrations
}

// SetIntegrations is defined in export_test.go. The product path never calls
// it: service bindings are derived from each rule's pipeline_config step via
// configuredBinding, and installing/enabling a package already authorizes that
// package's declared capabilities.
func integrationError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}
func (s *integrationSnapshot) allows(p *ExternalPlugin, service string, op integrations.Operation) bool {
	if p.services != nil {
		client := p.services[service]
		auth, ok := p.runtime.(interface{ AllowsService(string, string) bool })
		return client != nil && ok && auth.AllowsService(service, string(op))
	}
	identity, ok := p.runtime.(interface{ SourceIdentity() (string, string) })
	if !ok || s == nil {
		return false
	}
	id, digest := identity.SourceIdentity()
	if auth, ok := p.runtime.(interface{ AllowsService(string, string) bool }); ok && !auth.AllowsService(service, string(op)) {
		return false
	}
	for _, g := range s.grants {
		if g.PluginID == id && g.SourceDigest == digest && g.Service == service {
			for _, o := range g.Operations {
				if o == op {
					return true
				}
			}
		}
	}
	return false
}

// prepareIntegration runs read-only declarations on original client-format input.
// It collects all claims before invoking a service or forwarding to a provider.
func (e *Engine) prepareIntegration(w *statusCaptureWriter, r *http.Request, result *modelResult, pipeline *Pipeline, entry *RequestLogEntry, start time.Time) bool {
	var participants []*ExternalPlugin
	seen := map[string]bool{}
	for _, p := range pipeline.ExternalSteps {
		key := p.PluginName()
		if identity, ok := p.runtime.(interface{ SourceIdentity() (string, string) }); ok {
			id, digest := identity.SourceIdentity()
			key = id + "\x00" + digest
		}
		if p.stepID != "" {
			key = p.ruleID + "\x00" + p.stepID
		}
		if p.runtime.HasHook("prepare") && !seen[key] {
			participants = append(participants, p)
			seen[key] = true
		}
	}
	if len(participants) == 0 {
		if executionFromRequest(r) != nil {
			integrationError(w, 401, "execution_unauthorized")
			return true
		}
		return false
	}
	s := e.requestIntegrationState(r)
	format := detectInputFormat(r.URL.Path)
	downstreamFormat := format
	if len(result.ds.ApiFormats) > 0 {
		downstreamFormat = result.ds.ApiFormats[0]
		for _, f := range result.ds.ApiFormats {
			if f == format {
				downstreamFormat = f
			}
		}
	}
	ctx := &PipelineContext{ClientFormat: format, DownstreamFormat: downstreamFormat, ClientStreaming: isStreamRequest(result.originalBody), Variables: map[string]interface{}{}}
	outcome := "error"
	transfer := false
	defer func() {
		if transfer {
			return
		}
		for _, p := range participants {
			if p.begun && !p.finished {
				p.finished = true
				input := p.input(ctx)
				input["outcome"] = ctx.termination(outcome)
				_, finishErr := p.call("finish", input)
				if finishErr != nil {
					entry.Error = "integration_unavailable"
				}
			}
			p.releaseBuffers()
		}
	}()
	fail := func(code string) bool {
		integrationError(w, http.StatusBadGateway, code)
		entry.Status = w.status
		entry.Error = code
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return true
	}
	var owner *ExternalPlugin
	var decision jsplugins.Decision
	input := map[string]any{"context": ctx.invocationContext(), "routing": jsonObject(routingContext(result, downstreamFormat)), "execution": nil, "request": map[string]any{"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery, "headers": headerSnapshot(r.Header)}, "body": payload(result.originalBody)}
	if ex := executionFromRequest(r); ex != nil {
		input["execution"] = map[string]any{"id": ex.ExecutionID, "subject": ex.Subject, "assignment": ex.AssignmentID, "epoch": ex.Epoch}
	}
	for _, p := range participants {
		clients, err := p.configuredServices()
		if err != nil {
			return fail("integration_unavailable")
		}
		p.services = clients
		if ex := executionFromRequest(r); ex != nil && clients != nil {
			owns := false
			for name := range clients {
				if ex.Service == p.executionService(name) {
					owns = true
				}
			}
			if !owns {
				continue
			}
		}
		if len(clients) > 0 {
			local := map[string]any{}
			for name := range clients {
				local[name] = map[string]any{"binding_id": stepServiceBinding(p.ruleID, p.stepID, name)}
			}
			input["bindings"] = local
		} else {
			delete(input, "bindings")
		}
		p.ctx = ctx
		p.request = r
		if err := p.runtime.Reset(); err != nil {
			return fail("integration_unavailable")
		}
		p.begun = true
		prep, err := p.call("prepare", input)
		if err != nil {
			return fail("integration_unavailable")
		}
		if kind, _ := prep["kind"].(string); kind == "pass" {
			if _, err := jsplugins.ValidateDecision(prep); err != nil {
				return fail("integration_conflict")
			}
			continue
		}
		declaration, err := jsplugins.ValidatePreparation(prep)
		if err != nil {
			return fail("integration_conflict")
		}
		if ex := executionFromRequest(r); ex != nil && ex.Service != p.executionService(declaration.Service) {
			return fail("execution_unauthorized")
		}
		client := s.serviceFor(p, declaration.Service)
		if !s.allows(p, declaration.Service, integrations.Prepare) || client == nil {
			return fail("execution_unauthorized")
		}
		response, data, err := client.Do(r.Context(), integrations.Prepare, declaration.Payload)
		if err != nil || response.StatusCode != http.StatusOK {
			return fail("integration_unavailable")
		}
		var envelope integrations.Envelope
		if json.Unmarshal(data, &envelope) != nil || envelope.Version != integrations.EnvelopeVersion {
			return fail("integration_unavailable")
		}
		var serviceResult any
		if json.Unmarshal(envelope.Payload, &serviceResult) != nil {
			return fail("integration_unavailable")
		}
		prepared := make(map[string]any, len(input)+1)
		for k, v := range input {
			prepared[k] = v
		}
		prepared["result"] = serviceResult
		out, err := p.call("prepared", prepared)
		if err != nil {
			return fail("integration_unavailable")
		}
		d, err := jsplugins.ValidateDecision(out)
		if err != nil {
			return fail("integration_conflict")
		}
		if d.Kind == "pass" {
			continue
		}
		if d.Service != declaration.Service {
			return fail("execution_unauthorized")
		}
		if owner != nil {
			return fail("integration_conflict")
		}
		owner = p
		decision = d
	}
	if owner == nil {
		if executionFromRequest(r) != nil {
			return fail("execution_unauthorized")
		}
		outcome = "success"
		transfer = true
		for _, p := range participants {
			p.prepared = true
		}
		return false
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions") {
		return fail("unsupported_managed_route")
	}
	if decision.Kind == "managedTools" {
		handled := e.executeManagedTools(w, r, result, pipeline, owner, decision, entry, start)
		if w.status < 400 {
			outcome = "success"
		}
		return handled
	}
	if executionFromRequest(r) != nil {
		return fail("execution_unauthorized")
	}
	if !s.allows(owner, decision.Service, integrations.Invoke) || s.serviceFor(owner, decision.Service) == nil {
		return fail("execution_unauthorized")
	}
	result.release()
	result.release = func() {}
	if result.sessionDone != nil {
		result.sessionDone()
		result.sessionDone = nil
	}
	response, data, err := s.serviceFor(owner, decision.Service).Do(r.Context(), integrations.Invoke, decision.Payload)
	if err != nil {
		return fail("integration_unavailable")
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return fail("integration_unavailable")
	}
	contentType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || (contentType != "application/json" && contentType != "text/event-stream") {
		return fail("integration_unavailable")
	}
	w.Header().Set("Content-Type", contentType)
	if id := response.Header.Get("X-Tresor-Task-ID"); id != "" && len(id) <= 256 && !strings.ContainsAny(id, "\r\n\x00") {
		w.Header().Set("X-Tresor-Task-ID", id)
	}
	if contentType == "application/json" {
		if !json.Valid(data) {
			return fail("integration_unavailable")
		}
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(data)
	} else {
		if response.Body == nil {
			return fail("integration_unavailable")
		}
		defer response.Body.Close()
		err = copyIntegrationStream(w, response)
	}
	if err == nil {
		outcome = "success"
	} else {
		entry.Error = "integration_unavailable"
	}
	entry.Status = w.status
	entry.Duration = DurationMs(time.Since(start))
	e.logger.Record(entry)
	return true
}
