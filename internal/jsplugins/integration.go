package jsplugins

import (
	"encoding/json"
	"fmt"
)

type PrepareInput struct {
	Context   map[string]any `json:"context"`
	Routing   map[string]any `json:"routing"`
	Execution any            `json:"execution"`
	Request   map[string]any `json:"request"`
	Body      map[string]any `json:"body"`
}
type Preparation struct {
	Service   string `json:"service"`
	Operation string `json:"operation"`
	Payload   any    `json:"payload"`
}
type Decision struct {
	Kind         string        `json:"kind"`
	Service      string        `json:"service,omitempty"`
	Operation    string        `json:"operation,omitempty"`
	Payload      any           `json:"payload,omitempty"`
	Codec        string        `json:"codec,omitempty"`
	Instructions string        `json:"instructions,omitempty"`
	Tools        []HostTool    `json:"tools,omitempty"`
	Stream       *StreamPolicy `json:"stream,omitempty"`
}

// StreamPolicy shapes the content of a streaming managed response. The
// client's stream request always governs the transport itself.
type StreamPolicy struct {
	Heartbeat        *bool `json:"heartbeat,omitempty"`
	ShowIntermediate *bool `json:"show_intermediate,omitempty"`
}

// WantsHeartbeat reports whether waiting heartbeats should be emitted on a
// streaming response. Absent means no heartbeats.
func (d Decision) WantsHeartbeat() bool {
	return d.Stream != nil && d.Stream.Heartbeat != nil && *d.Stream.Heartbeat
}

// ShowsIntermediate reports whether intermediate turn texts should be
// forwarded on a streaming response. Absent means final answer only.
func (d Decision) ShowsIntermediate() bool {
	return d.Stream != nil && d.Stream.ShowIntermediate != nil && *d.Stream.ShowIntermediate
}

type HostTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Operation   string         `json:"operation"`
}

func ValidatePreparation(value map[string]any) (Preparation, error) {
	var p Preparation
	b, _ := json.Marshal(value)
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("invalid preparation")
	}
	if p.Service == "" || p.Operation != "prepare" || p.Payload == nil || len(value) != 3 {
		return p, fmt.Errorf("invalid preparation")
	}
	return p, nil
}
func ValidateDecision(value map[string]any) (Decision, error) {
	var d Decision
	b, _ := json.Marshal(value)
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("invalid decision")
	}
	if len(b) > 1<<20 {
		return d, fmt.Errorf("decision too large")
	}
	allowed := map[string]bool{"kind": true}
	switch d.Kind {
	case "intercept":
		allowed["service"] = true
		allowed["operation"] = true
		allowed["payload"] = true
	case "managedTools":
		for _, key := range []string{"service", "codec", "instructions", "tools", "stream"} {
			allowed[key] = true
		}
	}
	for key := range value {
		if !allowed[key] {
			return d, fmt.Errorf("unknown decision field")
		}
	}
	switch d.Kind {
	case "pass":
		if len(value) != 1 {
			return d, fmt.Errorf("invalid pass decision")
		}
	case "intercept":
		if d.Service == "" || d.Operation != "invoke" || d.Payload == nil {
			return d, fmt.Errorf("invalid intercept decision")
		}
	case "managedTools":
		if d.Service == "" || d.Codec != "openai-chat.v1" || len(d.Tools) == 0 {
			return d, fmt.Errorf("invalid managed tools decision")
		}
		if v, ok := value["stream"]; ok {
			var probe map[string]json.RawMessage
			raw, _ := json.Marshal(v)
			if json.Unmarshal(raw, &probe) != nil || probe == nil {
				return d, fmt.Errorf("invalid stream policy")
			}
			for key, field := range probe {
				// Strictly boolean: null, numbers, or strings are rejected.
				if key != "heartbeat" && key != "show_intermediate" {
					return d, fmt.Errorf("invalid stream policy")
				}
				if flag := string(field); flag != "true" && flag != "false" {
					return d, fmt.Errorf("invalid stream policy")
				}
			}
		}
		if len(d.Tools) > 128 || len(d.Instructions) > 65536 {
			return d, fmt.Errorf("managed tool limits exceeded")
		}
		seen := map[string]bool{}
		for _, t := range d.Tools {
			if t.Name == "" || seen[t.Name] || t.Operation != "execute-tool" || t.Parameters == nil {
				return d, fmt.Errorf("invalid host tool")
			}
			if len(t.Name) > 64 || len(t.Description) > 8192 || t.Parameters["type"] != "object" {
				return d, fmt.Errorf("invalid host tool schema")
			}
			for _, c := range t.Name {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
					return d, fmt.Errorf("invalid host tool name")
				}
			}
			seen[t.Name] = true
		}
	default:
		return d, fmt.Errorf("unknown decision kind")
	}
	return d, nil
}
func (i *Instance) Prepare(input PrepareInput) (map[string]any, error) {
	return i.Invoke("prepare", map[string]any{"context": input.Context, "routing": input.Routing, "execution": input.Execution, "request": input.Request, "body": input.Body})
}
func (i *Instance) Prepared(input PrepareInput, result any) (Decision, error) {
	out, err := i.Invoke("prepared", map[string]any{"context": input.Context, "routing": input.Routing, "execution": input.Execution, "request": input.Request, "body": input.Body, "result": result})
	if err != nil {
		return Decision{}, err
	}
	return ValidateDecision(out)
}
