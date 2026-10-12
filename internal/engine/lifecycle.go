package engine

import (
	"context"
	"errors"
	"net/http"
	"tresor/internal/jsplugins"
)

type ContractError struct{ Category, Code string }

func (e *ContractError) Error() string { return e.Category + ": " + e.Code }
func contractError(category, code string) error {
	return &ContractError{Category: category, Code: code}
}

type Session struct{ Started, Ended bool }

func (s *Session) Start() bool {
	if s.Started || s.Ended {
		return false
	}
	s.Started = true
	return true
}
func (s *Session) End() bool {
	if !s.Started || s.Ended {
		return false
	}
	s.Ended = true
	return true
}

// AttemptExecution holds host facts. Hook snapshots never expose this object.
type AttemptExecution struct {
	Phase, Stage, CurrentFormat                                 string
	Requested, Observed, Classification, ReportedType, Delivery string
	RawContent                                                  bool
	Large                                                       bool
	FailureStage                                                string
	Failure                                                     error
	Statistics                                                  Statistics
}
type Statistics struct {
	RawBytes       int64 `json:"rawBytes"`
	RawEvents      int64 `json:"rawEvents"`
	OutputBytes    int64 `json:"outputBytes"`
	OutputEvents   int64 `json:"outputEvents"`
	CollectedBytes int64 `json:"collectedBytes"`
}

func (ctx *PipelineContext) execution() *AttemptExecution {
	if ctx.Execution == nil {
		ctx.Execution = &AttemptExecution{Phase: "request", Stage: "request", CurrentFormat: ctx.ClientFormat, Requested: "auto", Observed: "unknown", Classification: "unknown", Delivery: "unknown"}
	}
	return ctx.Execution
}
func (ctx *PipelineContext) recordFailure(err error) {
	if err != nil && ctx.execution().Failure == nil {
		ctx.execution().Failure = err
		ctx.execution().FailureStage = ctx.execution().Stage
	}
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (ctx *PipelineContext) invocationContext() map[string]any {
	e := ctx.execution()
	client, planned := "buffered", "buffered"
	if ctx.ClientStreaming {
		client, planned = "streaming", "streaming"
	}
	var owner any
	if ctx.ExternalCollector != nil {
		planned = "collect"
		owner = ctx.ExternalCollector.PluginName()
	}
	return map[string]any{"phase": e.Phase, "stage": e.Stage, "formats": map[string]any{"client": nullable(ctx.ClientFormat), "downstream": nullable(ctx.DownstreamFormat), "current": nullable(e.CurrentFormat)},
		"transport": map[string]any{"carrier": "http", "client": client, "requested": e.Requested, "observed": e.Observed, "reportedContentType": nullable(e.ReportedType), "classification": e.Classification, "plannedDelivery": planned, "effectiveDelivery": e.Delivery, "collector": owner}, "limits": jsplugins.EffectiveLimits(e.Large)}
}
func (ctx *PipelineContext) termination(reason string) map[string]any {
	e := ctx.execution()
	var detail any
	if e.Failure != nil {
		category, code := "transport_read", "read_failure"
		if errors.Is(e.Failure, context.Canceled) || errors.Is(e.Failure, context.DeadlineExceeded) {
			category, code = "cancelled", "cancelled"
		}
		var invocation *jsplugins.InvocationError
		var status interface{ UpstreamStatus() int }
		stage := e.FailureStage
		if stage == "" {
			stage = e.Stage
		}
		d := map[string]any{"stage": stage, "message": "Request attempt failed"}
		if errors.As(e.Failure, &invocation) {
			category, code = "plugin_failure", invocation.Code
			d["plugin"] = invocation.Plugin
			d["hook"] = invocation.Hook
			if code == "buffer_bytes" || code == "buffer_handles" || code == "buffer_fragments" || code == "state_bytes" || code == "input_limit" {
				category = "resource_limit"
			}
		}
		if errors.As(e.Failure, &status) {
			category, code = "upstream", "upstream_error"
			d["status"] = status.UpstreamStatus()
		}
		var contract *ContractError
		if errors.As(e.Failure, &contract) {
			category, code = contract.Category, contract.Code
		}
		d["category"], d["code"] = category, code
		detail = d
	}
	return map[string]any{"reason": reason, "error": detail, "statistics": e.Statistics}
}
func (ctx *PipelineContext) observeResponse(resp *http.Response, streamed bool) {
	e := ctx.execution()
	e.ReportedType = resp.Header.Get("Content-Type")
	e.CurrentFormat = ctx.DownstreamFormat
	e.Classification = "header"
	e.Observed = "buffered"
	e.Delivery = "buffered"
	if streamed {
		e.Observed = "sse"
		e.Delivery = "streaming"
		if e.Requested == "sse" {
			e.Classification = "declared"
		}
		if ctx.ExternalCollector != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			e.Delivery = "collect"
			e.Large = true
		}
	}
}

// FormatTransition avoids converter-ID switches and advances only on success.
type FormatTransition interface {
	RequestFormat() string
	ResponseFormat() string
}

// FormatPreserver identifies non-converters without teaching the engine their IDs.
type FormatPreserver interface{ PreservesFormat() bool }

func advanceFormat(ctx *PipelineContext, step any, request bool) {
	if t, ok := step.(FormatTransition); ok {
		if request {
			ctx.execution().CurrentFormat = t.RequestFormat()
		} else {
			ctx.execution().CurrentFormat = t.ResponseFormat()
		}
	} else if t, ok := step.(FormatPreserver); !ok || !t.PreservesFormat() {
		ctx.execution().CurrentFormat = ""
	}
}
