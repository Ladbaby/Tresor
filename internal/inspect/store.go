// Package inspect is a bounded in-memory store of raw gateway
// request/response bodies for the Web UI's Logs inspector.
//
// Captures are always the **pre-transformer** bytes — the engine copies
// the raw incoming body and the raw downstream response body **before**
// any plugin runs.
package inspect

import (
	"sync"
	"time"
)

// DefaultMaxEntries / MaxBodyBytes bound memory usage.
const (
	DefaultMaxEntries = 100
	MaxBodyBytes      = 1 << 20
)

type Entry struct {
	ID                  int       `json:"id"`
	Timestamp           time.Time `json:"timestamp"`
	RequestBody         []byte    `json:"request_body,omitempty"`
	ResponseBody        []byte    `json:"response_body,omitempty"`
	RequestContentType  string    `json:"request_content_type,omitempty"`
	ResponseContentType string    `json:"response_content_type,omitempty"`
	TruncatedResponse   bool      `json:"truncated_response,omitempty"`
	Path                string    `json:"path,omitempty"`
	Method              string    `json:"method,omitempty"`
	Model               string    `json:"model,omitempty"`
	ResolvedModel       string    `json:"resolved_model,omitempty"`
	DownstreamID        string    `json:"downstream_id,omitempty"`
	DownstreamName      string    `json:"downstream_name,omitempty"`
	Status              int       `json:"status,omitempty"`
	ClientIP            string    `json:"client_ip,omitempty"`
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
	mu      sync.RWMutex
	max     int
	entries map[int]*Entry
	order   []int
}

func New(maxEntries int) *Store {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Store{
		max:     maxEntries,
		entries: make(map[int]*Entry, maxEntries),
		order:   make([]int, 0, maxEntries),
	}
}

func (s *Store) Add(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[e.ID] = &e
	s.order = append(s.order, e.ID)
	for len(s.entries) > s.max {
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
	cp := *e
	return &cp, true
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
