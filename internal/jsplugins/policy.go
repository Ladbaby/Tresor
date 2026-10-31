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
	// Persistent key-value ceilings, also enforced by the store itself. They are
	// published here so an author can size keys and values without probing.
	KVKeyBytes         int `json:"kvKeyBytes"`
	KVValueBytes       int `json:"kvValueBytes"`
	KVKeysPerNamespace int `json:"kvKeysPerNamespace"`
	KVTTLMs            int `json:"kvTTLMs"`
}

func EffectiveLimits(large bool) Limits {
	body := MaxValueBytes
	if large {
		body = MaxCollectionBytes
	}
	return Limits{BodyBytes: body, EventBytes: body, StateBytes: MaxValueBytes,
		BufferBytes: body, BufferHandles: 65536, BufferFragments: 1 << 20, Events: MaxEvents,
		WireBytes:  6*body + (1 << 20),
		KVKeyBytes: kvMaxKeyBytes, KVValueBytes: kvMaxValueBytes,
		KVKeysPerNamespace: kvMaxKeysPerNS, KVTTLMs: int(kvMaxTTL.Milliseconds())}
}

// InvocationError exposes only host-selected fault metadata. The cause remains
// available to internal errors.Is/As, never to logs or client messages.
// Initialization failures are sanitized too: plugin source is author-controlled
// but may embed credentials or echoed input, so its error text is not safe to
// publish. Authors get import and manifest diagnostics from `plugin lint`
// instead, which analyzes source statically.
type InvocationError struct {
	Code, Plugin, Hook string
	Cause              error
}

func (e *InvocationError) Error() string {
	return "plugin " + e.Plugin + " hook " + e.Hook + ": " + e.Code
}
func (e *InvocationError) Unwrap() error { return e.Cause }
