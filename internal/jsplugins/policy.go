package jsplugins

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
}

func EffectiveLimits(large bool) Limits {
	body := MaxValueBytes
	if large {
		body = MaxCollectionBytes
	}
	return Limits{BodyBytes: body, EventBytes: body, StateBytes: MaxValueBytes,
		BufferBytes: body, BufferHandles: 65536, BufferFragments: 1 << 20, Events: MaxEvents,
		WireBytes: 6*body + (1 << 20)}
}

// InvocationError exposes only host-selected fault metadata. The cause remains
// available to internal errors.Is/As, never to logs or client messages.
type InvocationError struct {
	Code, Plugin, Hook string
	Cause              error
}

func (e *InvocationError) Error() string {
	return "plugin " + e.Plugin + " hook " + e.Hook + ": " + e.Code
}
func (e *InvocationError) Unwrap() error { return e.Cause }
