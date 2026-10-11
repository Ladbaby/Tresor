package engine

import (
	"errors"
	"tresor/internal/jsplugins"
)

const maxDiagnostics = 128

type PluginDiagnostic struct {
	PluginID    string         `json:"plugin_id"`
	Version     string         `json:"version"`
	Digest      string         `json:"digest,omitempty"`
	RuleID      string         `json:"rule_id,omitempty"`
	Step        int            `json:"step"`
	Hook        string         `json:"hook"`
	Attempt     int            `json:"attempt"`
	Stage       string         `json:"stage"`
	Category    string         `json:"category,omitempty"`
	Code        string         `json:"code,omitempty"`
	Disposition string         `json:"disposition"`
	Level       string         `json:"level,omitempty"`
	Fields      map[string]any `json:"fields,omitempty"`
	Count       int            `json:"count"`
}
type PluginDiagnostics struct {
	Entries []PluginDiagnostic `json:"entries"`
	Dropped int                `json:"dropped"`
}

func (p *ExternalPlugin) diagnostic(hook, disposition string, err error, log *jsplugins.LogEvent) {
	if p.ctx == nil {
		return
	}
	ctx := p.ctx
	if ctx.Diagnostics == nil {
		ctx.Diagnostics = &PluginDiagnostics{Entries: []PluginDiagnostic{}}
	}
	id, version, digest := p.PluginName(), "", ""
	if identity, ok := p.runtime.(interface {
		Identity() (string, string, string)
	}); ok {
		id, version, digest = identity.Identity()
	}
	d := PluginDiagnostic{PluginID: id, Version: version, Digest: digest, RuleID: p.ruleID, Step: p.stepIndex, Hook: hook, Attempt: ctx.AttemptIndex, Stage: ctx.execution().Stage, Disposition: disposition, Count: 1}
	if err != nil {
		d.Category = "plugin_failure"
		d.Code = "invalid_effect"
		var fault *jsplugins.InvocationError
		if errors.As(err, &fault) {
			d.Code = fault.Code
		}
		var contract *ContractError
		if errors.As(err, &contract) {
			d.Category = contract.Category
			d.Code = contract.Code
		}
		switch d.Code {
		case "deadline":
			d.Category = "deadline"
		case "input_limit", "state_bytes", "buffer_bytes", "buffer_handles", "buffer_fragments", "log_limit":
			d.Category = "resource_limit"
		case "permission_denied":
			d.Category = "security"
		}
		d.Level = "error"
		if disposition == "passed_through" {
			d.Level = "warn"
		}
	}
	if log != nil {
		d.Code = log.Code
		d.Level = log.Level
		d.Fields = log.Fields
	}
	// Aggregate identical ordinary successes, never allocate one record per SSE event.
	if err == nil && log == nil {
		for index := range ctx.Diagnostics.Entries {
			existing := &ctx.Diagnostics.Entries[index]
			if existing.PluginID == d.PluginID && existing.RuleID == d.RuleID && existing.Step == d.Step && existing.Hook == d.Hook && existing.Attempt == d.Attempt && existing.Disposition == d.Disposition && existing.Code == "" {
				existing.Count++
				return
			}
		}
	}
	if len(ctx.Diagnostics.Entries) >= maxDiagnostics {
		ctx.Diagnostics.Dropped++
		return
	}
	ctx.Diagnostics.Entries = append(ctx.Diagnostics.Entries, d)
}
func (p *ExternalPlugin) invocationFailure(hook string, err error) error {
	if err == nil {
		return nil
	}
	var fault *jsplugins.InvocationError
	if !errors.As(err, &fault) {
		err = &jsplugins.InvocationError{Code: "invalid_effect", Plugin: p.PluginName(), Hook: hook, Cause: err}
	}
	p.diagnostic(hook, "failed_closed", err, nil)
	if p.ctx != nil {
		p.ctx.recordFailure(err)
	}
	return err
}
func (p *ExternalPlugin) safeFallback(hook string, err error) bool {
	if p.failurePolicy != "safe_passthrough" || p.ctx == nil {
		return false
	}
	ctx := p.ctx
	e := ctx.execution()
	if ctx.ClientStreaming || ctx.ExternalCollector != nil || e.Requested == "sse" || e.Observed == "sse" || e.Delivery == "collect" || ctx.Committed {
		return false
	}
	if hook != "request" && hook != "responseHeaders" && hook != "response" {
		return false
	}
	var fault *jsplugins.InvocationError
	return errors.As(err, &fault) && fault.Code == "plugin_exception"
}
