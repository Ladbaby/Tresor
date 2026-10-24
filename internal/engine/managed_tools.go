package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tresor/internal/integrations"
	"tresor/internal/jsplugins"
	"tresor/internal/store"
)

type managedExecutionLock struct {
	gate  chan struct{}
	users int
}

// executeManagedTools runs managed Chat generations directly against the pinned
// downstream. It never re-enters HandleProxy, retries, or transforms the transcript.
func (e *Engine) executeManagedTools(w *statusCaptureWriter, r *http.Request, result *modelResult, pipeline *Pipeline, owner *ExternalPlugin, decision jsplugins.Decision, entry *RequestLogEntry, start time.Time) bool {
	s := e.requestIntegrationState(r)
	ex := executionFromRequest(r)
	fail := func(status int, code string) bool {
		integrationError(w, status, code)
		entry.Status, entry.Error, entry.Duration = status, code, DurationMs(time.Since(start))
		e.logger.Record(entry)
		return true
	}
	if ex == nil || s == nil || ex.Service != owner.executionService(decision.Service) || ex.AliasID == "" || !s.allows(owner, decision.Service, integrations.ExecuteTool) || s.serviceFor(owner, decision.Service) == nil {
		return fail(http.StatusUnauthorized, "execution_unauthorized")
	}
	lockKey := ex.ExecutionID + ":" + strconv.FormatInt(ex.Epoch, 10)
	e.executionMu.Lock()
	if e.managedLocks == nil {
		e.managedLocks = map[string]*managedExecutionLock{}
	}
	lock := e.managedLocks[lockKey]
	if lock == nil {
		lock = &managedExecutionLock{gate: make(chan struct{}, 1)}
		e.managedLocks[lockKey] = lock
	}
	lock.users++
	e.executionMu.Unlock()
	defer func() {
		e.executionMu.Lock()
		defer e.executionMu.Unlock()
		lock.users--
		if lock.users == 0 {
			delete(e.managedLocks, lockKey)
		}
	}()
	select {
	case lock.gate <- struct{}{}:
		defer func() { <-lock.gate }()
	case <-r.Context().Done():
		return fail(409, "operation_uncertain")
	}
	if current, err := e.store.ActiveExecution(ex.ExecutionID); err != nil || current.Epoch != ex.Epoch {
		return fail(401, "execution_unauthorized")
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions") || decision.Codec != "openai-chat.v1" {
		return fail(http.StatusBadRequest, "unsupported_managed_route")
	}
	if result.ds == nil || !containsFormat(result.ds.ApiFormats, "openai") {
		return fail(http.StatusBadRequest, "unsupported_managed_route")
	}
	// Until transforms explicitly declare transcript compatibility, reject the
	// combination rather than allowing plugins to rewrite hidden history.
	if pipeline != nil {
		for _, p := range pipeline.ExternalSteps {
			if p.runtime.HasHook("request") || p.runtime.HasHook("response") || p.runtime.HasHook("streamStart") || p.runtime.HasHook("streamEvent") || p.runtime.HasHook("streamEnd") || p.runtime.HasHook("upstreamStart") || p.runtime.HasHook("upstreamEvent") || p.runtime.HasHook("upstreamEnd") {
				return fail(http.StatusBadRequest, "unsupported_managed_route")
			}
		}
	}
	if len(result.originalBody) == 0 {
		return fail(http.StatusBadRequest, "unsupported_managed_route")
	}
	clientStreaming := isStreamRequest(result.originalBody)
	prior, priorErr := e.store.ListManagedExecutionSegments(ex.ExecutionID)
	if priorErr != nil {
		return fail(502, "operation_uncertain")
	}
	request, err := ParseManagedChatRequest(result.originalBody)
	if err != nil {
		return fail(http.StatusBadRequest, "unsupported_managed_route")
	}
	clientNames := make([]string, 0)
	var toolDefs []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	_ = json.Unmarshal(request.Raw["tools"], &toolDefs)
	for _, td := range toolDefs {
		clientNames = append(clientNames, td.Function.Name)
	}
	hostNames := make([]string, len(decision.Tools))
	for i, tool := range decision.Tools {
		hostNames[i] = tool.Name
	}
	if err := ValidateToolNames(clientNames, hostNames); err != nil {
		return fail(http.StatusBadRequest, "tool_name_collision")
	}
	canonical, err := canonicalJSON(result.originalBody)
	if err != nil {
		return fail(http.StatusBadRequest, "unsupported_managed_route")
	}
	route, _ := json.Marshal(map[string]any{"downstream": result.ds.ID, "model": result.resolvedModel, "codec": decision.Codec})
	packageID, digest := "", ""
	if identity, ok := owner.runtime.(interface{ SourceIdentity() (string, string) }); ok {
		packageID, digest = identity.SourceIdentity()
	}
	privateConfig, configErr := json.Marshal(owner.stepConfig)
	if configErr != nil {
		return fail(502, "integration_unavailable")
	}
	configIdentity, identityErr := e.store.ConfigurationIdentity(privateConfig)
	if identityErr != nil {
		return fail(502, "integration_unavailable")
	}
	snapshot, _ := json.Marshal(map[string]any{"plugin": packageID, "digest": digest, "service": owner.executionService(decision.Service), "config": configIdentity, "tools": decision.Tools, "binding": s.serviceFor(owner, decision.Service).BindingIdentity()})
	managed, created, err := e.store.BeginManagedInference(store.ManagedInference{ExecutionID: ex.ExecutionID, Epoch: ex.Epoch, InferenceID: ex.InferenceID, Status: "running", Codec: decision.Codec, Route: route, Snapshot: snapshot}, canonical)
	if err != nil {
		return fail(http.StatusConflict, "integration_conflict")
	}
	var recovery []ChatMessage
	if !created && managed != nil && managed.Status != "complete" {
		if !semanticEqual(managed.Snapshot, snapshot) || !semanticEqual(managed.Route, route) {
			return fail(409, "continuation_mismatch")
		}
		checkpoints, checkpointErr := e.store.ListManagedSegments(ex.ExecutionID, ex.InferenceID, ex.Epoch)
		if checkpointErr != nil || len(checkpoints) != 1 || json.Unmarshal(checkpoints[0].FullJSON, &recovery) != nil || len(recovery) == 0 {
			return fail(409, "operation_uncertain")
		}
		var role string
		_ = json.Unmarshal(recovery[len(recovery)-1]["role"], &role)
		if role != "assistant" {
			return fail(409, "operation_uncertain")
		}
		calls, parseErr := ParseToolCalls(recovery[len(recovery)-1], hostNames)
		if parseErr != nil || len(calls) == 0 {
			return fail(409, "operation_uncertain")
		}
	}
	if !created && len(recovery) == 0 {
		// Never re-run an uncertain generation or tool effect. Only finalized
		// durable responses are replayable.
		stored := managed
		if stored == nil || stored.Status != "complete" || len(stored.Response) == 0 {
			return fail(http.StatusConflict, "operation_uncertain")
		}
		if err := writeManagedProjection(w, stored.Response, clientStreaming); err != nil {
			entry.Error = "integration_unavailable"
		}
		entry.Status = http.StatusOK
		entry.Duration = DurationMs(time.Since(start))
		e.logger.Record(entry)
		return true
	}
	ctx, ge := e.prepareDownstreamContext(r, result, "openai")
	if ge != nil {
		return fail(ge.status, "integration_unavailable")
	}
	ctx.DownstreamFormat = "openai"
	historySegments := []store.ManagedSegment{}
	for _, seg := range prior {
		if seg.InferenceID != ex.InferenceID {
			historySegments = append(historySegments, seg)
		}
		if seg.Epoch == ex.Epoch && len(seg.PendingClientIDs) > 0 && seg.InferenceID != ex.InferenceID {
			pending, pendingErr := e.store.ManagedSegmentPending(seg)
			if pendingErr != nil {
				return fail(409, "continuation_mismatch")
			}
			if !pending {
				continue
			}
			old, err := e.store.ManagedInferenceSnapshot(ex.ExecutionID, ex.Epoch, seg.InferenceID)
			if err != nil || !compatibleManagedSnapshot(old, snapshot) {
				return fail(409, "continuation_mismatch")
			}
		}
	}
	messages, historyErr := reconstructManaged(request.Messages, historySegments, ex.Epoch)
	if historyErr != nil {
		return fail(409, "continuation_mismatch")
	}
	transcript := []ChatMessage{}
	segment := store.ManagedSegment{ExecutionID: ex.ExecutionID, Epoch: ex.Epoch, InferenceID: ex.InferenceID, Order: 0, FullJSON: json.RawMessage(`[]`), VisibleJSON: json.RawMessage(`null`), Provenance: `{}`}
	saveProjection := func(visible ChatMessage, pending []string, hosts map[string]ChatMessage) error {
		segment.FullJSON, _ = json.Marshal(transcript)
		segment.VisibleJSON, _ = json.Marshal(visible)
		segment.PendingClientIDs = pending
		provenance, _ := json.Marshal(hosts)
		segment.Provenance = string(provenance)
		return e.store.UpdateManagedSegment(segment)
	}
	if created {
		if err := e.store.SaveManagedSegment(segment); err != nil {
			return fail(502, "operation_uncertain")
		}
	}
	// Bind client outcomes so later inferences cannot replace settled results.
	for _, m := range request.Messages {
		var role, id string
		_ = json.Unmarshal(m["role"], &role)
		if role != "tool" {
			continue
		}
		_ = json.Unmarshal(m["tool_call_id"], &id)
		raw, _ := json.Marshal(m)
		if err := e.store.BindManagedClientResult(ex.ExecutionID, ex.Epoch, id, raw); err != nil {
			return fail(409, "continuation_mismatch")
		}
	}
	// Inject host declarations once, preserving every client declaration.
	var allTools []json.RawMessage
	if raw := request.Raw["tools"]; len(raw) > 0 {
		if json.Unmarshal(raw, &allTools) != nil {
			return fail(400, "unsupported_managed_route")
		}
	}
	for _, tool := range decision.Tools {
		raw, marshalErr := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters}})
		if marshalErr != nil {
			return fail(400, "unsupported_managed_route")
		}
		allTools = append(allTools, raw)
	}
	request.Raw["tools"], _ = json.Marshal(allTools)
	if decision.Instructions != "" {
		instruction, _ := json.Marshal(decision.Instructions)
		messages = append([]ChatMessage{{"role": json.RawMessage(`"system"`), "content": instruction}}, messages...)
	}
	// Managed history is durable and intentionally fails closed if prior anchors
	// cannot be reconstructed. The first implementation stores each final turn.
	generation := 0
	if len(recovery) > 0 {
		transcript = append(transcript, recovery[:len(recovery)-1]...)
		messages = append(messages, transcript...)
		for _, m := range transcript {
			var role string
			_ = json.Unmarshal(m["role"], &role)
			if role == "assistant" {
				generation++
			}
		}
	}
	for {
		if active, err := e.store.ActiveExecution(ex.ExecutionID); err != nil || active.Epoch != ex.Epoch {
			return fail(401, "execution_stale")
		}
		if r.Context().Err() != nil {
			return fail(http.StatusBadGateway, "operation_uncertain")
		}
		bodyMap := make(map[string]json.RawMessage, len(request.Raw)+2)
		for k, v := range request.Raw {
			bodyMap[k] = append(json.RawMessage(nil), v...)
		}
		mb, _ := json.Marshal(messages)
		bodyMap["messages"] = mb
		bodyMap["model"], _ = json.Marshal(result.resolvedModel)
		bodyMap["stream"] = json.RawMessage("false")
		body, _ := json.Marshal(bodyMap)
		innerReq := r.Clone(r.Context())
		innerReq.Header = r.Header.Clone()
		innerReq.Header.Del("Authorization")
		innerReq.Header.Del("X-API-Key")
		innerReq.Header.Set("Content-Type", "application/json")
		var data []byte
		responseStatus := http.StatusOK
		if len(recovery) > 0 {
			message := recovery[len(recovery)-1]
			data, _ = json.Marshal(map[string]any{"choices": []any{map[string]any{"message": message, "index": 0, "finish_reason": "tool_calls"}}})
			recovery = nil
		} else {
			resp, cancel, err := e.forwardRequest(innerReq, body, ctx)
			if err != nil {
				cancel()
				return fail(http.StatusBadGateway, "integration_unavailable")
			}
			var readErr error
			if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				data, readErr = collectManagedSSE(resp.Body)
			} else {
				data, readErr = io.ReadAll(io.LimitReader(resp.Body, externalCollectionLimit+1))
			}
			resp.Body.Close()
			cancel()
			if readErr != nil || len(data) > externalCollectionLimit {
				return fail(http.StatusBadGateway, "integration_unavailable")
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fail(http.StatusBadGateway, "integration_unavailable")
			}
			responseStatus = resp.StatusCode
		}
		if r.Context().Err() != nil {
			return fail(502, "operation_uncertain")
		}
		if active, err := e.store.ActiveExecution(ex.ExecutionID); err != nil || active.Epoch != ex.Epoch {
			return fail(401, "execution_stale")
		}
		if _, err := ValidateOneChoiceResponse(data); err != nil {
			return fail(http.StatusBadGateway, "unsupported_managed_route")
		}
		var root map[string]json.RawMessage
		_ = json.Unmarshal(data, &root)
		var choices []map[string]json.RawMessage
		_ = json.Unmarshal(root["choices"], &choices)
		var message ChatMessage
		_ = json.Unmarshal(choices[0]["message"], &message)
		calls, err := ParseToolCalls(message, hostNames)
		if err != nil {
			return fail(http.StatusBadGateway, "unsupported_managed_route")
		}
		if len(calls) == 0 {
			transcript = append(transcript, message)
			if err := saveProjection(message, nil, nil); err != nil {
				return fail(502, "operation_uncertain")
			}
			messages = append(messages, message)
			projection, _ := json.Marshal(root)
			if err := e.store.FinalizeManagedInference(segment, projection); err != nil {
				return fail(http.StatusBadGateway, "operation_uncertain")
			}
			if err := writeManagedProjection(w, projection, clientStreaming); err != nil {
				entry.Error = "integration_unavailable"
			}
			entry.Status = responseStatus
			entry.Duration = DurationMs(time.Since(start))
			e.logger.Record(entry)
			return true
		}
		for _, call := range calls {
			if r.Context().Err() != nil {
				return fail(502, "operation_uncertain")
			}
			if _, err := e.store.ActiveExecution(ex.ExecutionID); err != nil {
				return fail(401, "execution_unauthorized")
			}
			if !call.HostOwned {
				declared := false
				for _, name := range clientNames {
					if name == call.Name {
						declared = true
					}
				}
				if !declared {
					return fail(502, "unsupported_managed_route")
				}
			}
		}
		transcript = append(transcript, message)
		segment.FullJSON, _ = json.Marshal(transcript)
		if err := e.store.UpdateManagedSegment(segment); err != nil {
			return fail(502, "operation_uncertain")
		}
		messages = append(messages, message)
		hostResults := map[string]ChatMessage{}
		var clientCalls []ToolCall
		for _, call := range calls {
			if r.Context().Err() != nil {
				return fail(502, "operation_uncertain")
			}
			if _, err := e.store.ActiveExecution(ex.ExecutionID); err != nil {
				return fail(401, "execution_unauthorized")
			}
			if !call.HostOwned {
				clientCalls = append(clientCalls, call)
				continue
			}
			arg, argErr := canonicalJSON([]byte(call.Arguments))
			if argErr != nil {
				return fail(502, "unsupported_managed_route")
			}
			identity, _ := json.Marshal([]any{ex.ExecutionID, ex.Epoch, ex.InferenceID, generation, call.ID, json.RawMessage(arg)})
			sum := sha256.Sum256(identity)
			opID := hex.EncodeToString(sum[:])
			ledger, fresh, err := e.store.BeginManagedToolCall(store.ManagedToolCall{ExecutionID: ex.ExecutionID, Epoch: ex.Epoch, InferenceID: ex.InferenceID, Generation: generation, CallID: call.ID, OperationID: opID, Status: "running"}, arg)
			if err != nil {
				return fail(http.StatusBadGateway, "operation_uncertain")
			}
			if !fresh && ledger.Status == "complete" {
				var cached ChatMessage
				if json.Unmarshal(ledger.Result, &cached) != nil {
					return fail(502, "operation_uncertain")
				}
				messages = append(messages, cached)
				hostResults[call.ID] = cached
				continue
			}
			var tool jsplugins.HostTool
			for _, t := range decision.Tools {
				if t.Name == call.Name {
					tool = t
					break
				}
			}
			payload := map[string]any{"execution": ex, "operation_id": opID, "tool": tool.Name, "arguments": json.RawMessage(call.Arguments), "reconcile": !fresh}
			serviceResp, resultBytes, err := s.serviceFor(owner, decision.Service).Do(r.Context(), integrations.ExecuteTool, payload)
			// A valid versioned 4xx tool response is a domain outcome, not ambiguous IO.
			if err != nil || serviceResp == nil || serviceResp.StatusCode >= 500 || serviceResp.StatusCode < 200 || serviceResp.StatusCode >= 300 && serviceResp.StatusCode < 400 {
				_ = e.store.CompleteManagedToolCall(ex.ExecutionID, ex.InferenceID, call.ID, ex.Epoch, generation, "uncertain", nil, "service unavailable")
				return fail(http.StatusBadGateway, "operation_uncertain")
			}
			var envelope integrations.Envelope
			if json.Unmarshal(resultBytes, &envelope) != nil || envelope.Version != integrations.EnvelopeVersion || !json.Valid(envelope.Payload) {
				return fail(http.StatusBadGateway, "integration_unavailable")
			}
			toolMsg, _ := json.Marshal(map[string]any{"role": "tool", "tool_call_id": call.ID, "content": string(envelope.Payload)})
			if err := e.store.CompleteManagedToolCall(ex.ExecutionID, ex.InferenceID, call.ID, ex.Epoch, generation, "complete", toolMsg, ""); err != nil {
				return fail(http.StatusBadGateway, "operation_uncertain")
			}
			var tm ChatMessage
			_ = json.Unmarshal(toolMsg, &tm)
			messages = append(messages, tm)
			hostResults[call.ID] = tm
		}
		if len(clientCalls) > 0 {
			projected, _, projectionErr := ProjectAssistantMessage(message, hostNames)
			if projectionErr != nil {
				return fail(502, "unsupported_managed_route")
			}
			pending := []string{}
			for _, call := range clientCalls {
				pending = append(pending, call.ID)
			}
			if err := saveProjection(projected, pending, hostResults); err != nil {
				return fail(502, "operation_uncertain")
			}
			var outChoices []json.RawMessage
			_ = json.Unmarshal(root["choices"], &outChoices)
			choice := map[string]json.RawMessage{}
			_ = json.Unmarshal(outChoices[0], &choice)
			choice["message"], _ = json.Marshal(projected)
			choice["finish_reason"], _ = json.Marshal("tool_calls")
			outChoices[0], _ = json.Marshal(choice)
			root["choices"], _ = json.Marshal(outChoices)
			out, _ := json.Marshal(root)
			if err := e.store.FinalizeManagedInference(segment, out); err != nil {
				return fail(http.StatusBadGateway, "operation_uncertain")
			}
			if err := writeManagedProjection(w, out, clientStreaming); err != nil {
				entry.Error = "integration_unavailable"
			}
			entry.Status = responseStatus
			entry.Duration = DurationMs(time.Since(start))
			e.logger.Record(entry)
			return true
		}
		for _, call := range calls {
			transcript = append(transcript, hostResults[call.ID])
		}
		generation++
	}
}

func compatibleManagedSnapshot(a, b []byte) bool {
	var old, current map[string]json.RawMessage
	if json.Unmarshal(a, &old) != nil || json.Unmarshal(b, &current) != nil {
		return false
	}
	for _, key := range []string{"plugin", "digest", "service", "tools", "binding", "config"} {
		if len(old[key]) == 0 && len(current[key]) == 0 {
			continue
		}
		if !semanticEqual(old[key], current[key]) {
			return false
		}
	}
	return true
}

func containsFormat(formats []string, value string) bool {
	for _, f := range formats {
		if strings.EqualFold(f, value) {
			return true
		}
	}
	return false
}

// canonicalJSON produces deterministic object-key ordering for durable identity.
func canonicalJSON(raw []byte) ([]byte, error) {
	if !json.Valid(raw) {
		return nil, io.ErrUnexpectedEOF
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
