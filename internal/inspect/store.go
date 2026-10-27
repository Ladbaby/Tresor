// Package inspect is a bounded in-memory store of raw gateway
// request/response bodies for the Web UI's Logs inspector.
//
// Ordinary captures retain pre-transformer client and downstream wire bodies.
// Integration captures retain the client exchange plus an optional ordered flow
// of actual upstream generations and host-tool interactions.
package inspect

import (
	"encoding/json"
	"sync"
	"time"
)

// Capture and retention limits bound inspector memory, not forwarded responses.
const (
	DefaultMaxEntries = 100
	MaxBodyBytes      = 16 << 20
	MaxRetainedBytes  = 128 << 20
)

// Event is an ordered observation of managed execution, never a credential envelope.
type Event struct {
	Kind        string `json:"kind"`
	Generation  int    `json:"generation,omitempty"`
	ToolName    string `json:"tool_name,omitempty"`
	CallID      string `json:"call_id,omitempty"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body,omitempty"`
	Error       string `json:"error,omitempty"`
	Replayed    bool   `json:"replayed,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

type Entry struct {
	UsageScope          string          `json:"usage_scope,omitempty"`
	Usage               json.RawMessage `json:"usage,omitempty"`
	Flow                []Event         `json:"flow,omitempty"`
	FlowTruncated       bool            `json:"flow_truncated,omitempty"`
	TaskID              string          `json:"task_id,omitempty"`
	ExecutionID         string          `json:"execution_id,omitempty"`
	InferenceID         string          `json:"inference_id,omitempty"`
	ID                  int             `json:"id"`
	Timestamp           time.Time       `json:"timestamp"`
	RequestBody         []byte          `json:"request_body,omitempty"`
	ResponseBody        []byte          `json:"response_body,omitempty"`
	RequestContentType  string          `json:"request_content_type,omitempty"`
	ResponseContentType string          `json:"response_content_type,omitempty"`
	TruncatedResponse   bool            `json:"truncated_response,omitempty"`
	Path                string          `json:"path,omitempty"`
	Method              string          `json:"method,omitempty"`
	Model               string          `json:"model,omitempty"`
	ResolvedModel       string          `json:"resolved_model,omitempty"`
	DownstreamID        string          `json:"downstream_id,omitempty"`
	DownstreamName      string          `json:"downstream_name,omitempty"`
	Status              int             `json:"status,omitempty"`
	ClientIP            string          `json:"client_ip,omitempty"`
	// Decode metrics are final raw-upstream observations, independent of body truncation.
	DecodeTokens            *int64   `json:"decode_tokens"`
	DecodeDurationMS        *float64 `json:"decode_duration_ms"`
	DecodeTokensPerSecond   *float64 `json:"decode_tokens_per_second"`
	DecodeUnavailableReason string   `json:"decode_unavailable_reason,omitempty"`
	// RequestFormat is the API format the client used on the wire for the
	// incoming request (one of "openai", "anthropic", "openai_responses",
	// "gemini", or "" when unknown). It is the format the inspector should
	// use when parsing RequestBody.
	RequestFormat string `json:"request_format,omitempty"`
	// DownstreamFormat is the API format the downstream actually returned
	// on the wire for the response (typically result.ds.ApiFormats[0] at
	// the time of the request). It is the format the inspector should use
	// when parsing ResponseBody — which may differ from RequestFormat when
	// the engine ran an auto-translator.
	DownstreamFormat string `json:"downstream_format,omitempty"`
}

type Store struct {
	mu            sync.RWMutex
	max           int
	maxBytes      int
	retainedBytes int
	entries       map[int]*Entry
	order         []int
}

func New(maxEntries int) *Store {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Store{
		max:      maxEntries,
		maxBytes: MaxRetainedBytes,
		entries:  make(map[int]*Entry, maxEntries),
		order:    make([]int, 0, maxEntries),
	}
}

func entryBytes(e *Entry) int {
	n := len(e.Usage) + len(e.RequestBody) + len(e.ResponseBody) + len(e.TaskID) + len(e.ExecutionID) + len(e.InferenceID)
	for _, event := range e.Flow {
		n += len(event.Body) + len(event.Kind) + len(event.ToolName) + len(event.CallID) + len(event.ContentType) + len(event.Error) + 128
	}
	return n
}

func cloneEntry(e Entry) Entry {
	e.Usage = append(json.RawMessage(nil), e.Usage...)
	e.RequestBody = append([]byte(nil), e.RequestBody...)
	e.ResponseBody = append([]byte(nil), e.ResponseBody...)
	e.Flow = append([]Event(nil), e.Flow...)
	return e
}

func (s *Store) Add(e Entry) {
	e = cloneEntry(e)
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.entries[e.ID]; ok {
		s.retainedBytes -= entryBytes(previous)
		for i, id := range s.order {
			if id == e.ID {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.entries[e.ID] = &e
	s.retainedBytes += entryBytes(&e)
	s.order = append(s.order, e.ID)
	for len(s.entries) > s.max || s.retainedBytes > s.maxBytes {
		oldest := s.entries[s.order[0]]
		s.retainedBytes -= entryBytes(oldest)
		delete(s.entries, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *Store) Get(id int) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	if !ok {
		return nil, false
	}
	cp := cloneEntry(*e)
	return &cp, true
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
