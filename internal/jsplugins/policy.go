package jsplugins

import "time"

// Limits is selected by the host, never by package source. It bounds decoded
// values independently from JSON envelope expansion; it is not a memory sandbox.
type Limits struct {
	BodyBytes       int `json:"bodyBytes"`
	EventBytes      int `json:"eventBytes"`
	StateBytes      int `json:"stateBytes"`
	BufferBytes     int `json:"bufferBytes"`
	BufferHandles   int `json:"bufferHandles"`
	BufferFragments int `json:"bufferFragments"`
	Events          int `json:"events"`
	WireBytes       int `json:"wireBytes"`
	DeadlineMs      int `json:"deadlineMs"`
}

func EffectiveLimits(large bool) Limits {
	body, deadline := MaxValueBytes, 250
	if large {
		body, deadline = MaxCollectionBytes, 10000
	}
	return Limits{BodyBytes: body, EventBytes: body, StateBytes: MaxValueBytes,
		BufferBytes: body, BufferHandles: 65536, BufferFragments: 1 << 20, Events: MaxEvents,
		WireBytes: 6*body + (1 << 20), DeadlineMs: deadline}
}
func (l Limits) Deadline() time.Duration { return time.Duration(l.DeadlineMs) * time.Millisecond }

// InvocationError retains the original cause internally. Public details must
// use Code, not Error(), which may include plugin exception text.
type InvocationError struct {
	Code, Plugin, Hook string
	Cause              error
}

func (e *InvocationError) Error() string {
	return "plugin " + e.Plugin + " hook " + e.Hook + ": " + e.Cause.Error()
}
func (e *InvocationError) Unwrap() error { return e.Cause }
