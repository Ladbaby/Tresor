package jsplugins

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"

	"github.com/dop251/goja"
)

const MaxPluginLogs = 128
const MaxHookLogs = 16

type LogEvent struct {
	Level  string         `json:"level"`
	Code   string         `json:"code"`
	Fields map[string]any `json:"fields,omitempty"`
}

var logCode = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func (i *Instance) Identity() (string, string, string) {
	return i.pkg.Manifest.ID, i.pkg.Manifest.Version, i.pkg.Digest
}
func (i *Instance) fault(code string) {
	if i.hostFault == "" {
		i.hostFault = code
	}
	panic(i.vm.NewTypeError("Host contract violation"))
}
func (i *Instance) bindLogging() {
	i.vm.Set("__tresorLog", func(call goja.FunctionCall) goja.Value {
		if !i.invoking || !i.pkg.granted["log.v1"] {
			i.fault("permission_denied")
		}
		if len(call.Arguments) != 1 {
			i.fault("invalid_log")
		}
		raw, ok := call.Argument(0).Export().(string)
		if !ok || len(raw) > 2048 {
			i.fault("invalid_log")
		}
		var e LogEvent
		if err := StrictJSON([]byte(raw), &e); err != nil {
			i.fault("invalid_log")
		}
		if e.Level != "debug" && e.Level != "info" && e.Level != "warn" && e.Level != "error" {
			i.fault("invalid_log")
		}
		if !logCode.MatchString(e.Code) || len(e.Fields) > 8 {
			i.fault("invalid_log")
		}
		for key, v := range e.Fields {
			if !logCode.MatchString(key) {
				i.fault("invalid_log")
			}
			switch key {
			case "plugin", "id", "version", "digest", "rule", "step", "hook", "attempt", "stage":
				i.fault("invalid_log")
			}
			switch n := v.(type) {
			case bool:
			case float64:
				if math.IsInf(n, 0) || math.IsNaN(n) {
					i.fault("invalid_log")
				}
			default:
				i.fault("invalid_log")
			}
		}
		if len(i.logs) >= MaxHookLogs {
			i.fault("log_limit")
		}
		i.logs = append(i.logs, e)
		return goja.Undefined()
	})
	i.vm.Set("__tresorFaultNative", func(call goja.FunctionCall) goja.Value {
		code, ok := call.Argument(0).Export().(string)
		if !ok {
			i.fault("invalid_effect")
		}
		switch code {
		case "buffer_bytes", "buffer_handles", "buffer_fragments", "state_bytes", "input_limit", "invalid_effect", "permission_denied", "log_limit":
		default:
			code = "invalid_effect"
		}
		i.fault(code)
		return goja.Undefined()
	})
}
func (i *Instance) DrainLogs() []LogEvent {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := i.logs
	i.logs = nil
	return out
}
func (i *Instance) SafePolicyEligible() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for hook := range i.declared {
		if hook != "request" && hook != "responseHeaders" && hook != "response" {
			return false
		}
	}
	return true
}

// ValidatePolicy checks exports without granting privileged services.
func ValidatePolicy(pkg *Package, config map[string]any, policy string) error {
	if policy == "" || policy == "fail_closed" {
		return nil
	}
	if policy != "safe_passthrough" {
		return fmt.Errorf("unknown failure_policy")
	}
	i, err := NewInstance(pkg, config)
	if err != nil {
		return err
	}
	if !i.SafePolicyEligible() {
		return fmt.Errorf("safe_passthrough requires request/header/buffered-response-only hooks")
	}
	return nil
}
func encodeLog(e LogEvent) string { b, _ := json.Marshal(e); return string(b) }
