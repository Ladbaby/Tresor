// Package inspect provides a small bounded in-memory store of raw gateway
// request/response bodies used by the Web UI's "Logs → click a row" inspector.
//
// Design choices:
//
//   - Pure in-memory, never persisted to disk. The CLAUDE.md and feature
//     requirements explicitly call this out: "cache them for some number of
//     latest requests in memory, otherwise Tresor may have serious performance
//     issues".
//
//   - Bounded by a max-entries cap. When the cap is reached, the oldest entry
//     is evicted. Insertion order is tracked with a slice of ids rather than
//     a heap: with caps in the dozens-to-hundreds the slice scan stays under
//     a microsecond and the code stays trivial. (LLM gateways are not
//     high-frequency caches; this is more than fast enough.)
//
//   - Captures are always the **pre-transformer** bytes. The engine copies
//     the raw incoming body and the raw downstream response body **before**
//     any plugin runs; this is the inspector's whole point, modeled after
//     claude-tap's trace viewer.
package inspect

import (
	"sync"
	"time"
)

// DefaultMaxEntries is the default cap when the caller doesn't override it.
// 100 entries × ~tens of KB per direction × 2 directions is a few MB worst
// case, well below the budget we want to dedicate to the inspector.
const DefaultMaxEntries = 100

// MaxBodyBytes is the per-direction cap on captured body size. Anything
// beyond this is truncated and the truncated flag is set on the entry. 1 MiB
// matches the cap used in the response handler in the engine; SSE streams
// beyond that get a "truncated" marker rather than a multi-megabyte copy.
const MaxBodyBytes = 1 << 20

// Entry is one captured request/response snapshot. Body bytes are stored as
// raw slices — the engine does not pre-parse them so the inspector can show
// the original wire bytes (including whitespace, key order, comments, etc.)
// for both JSON and SSE.
type Entry struct {
	// ID is the log entry id this snapshot belongs to. The Web UI calls
	// GET /api/logs/{id}/inspect with this id.
	ID int `json:"id"`

	// Timestamp is when the request arrived at the gateway. Matches the
	// RequestLogEntry.Timestamp for the same id.
	Timestamp time.Time `json:"timestamp"`

	// RequestBody is the raw incoming client body, before any plugin ran.
	// Empty when the request had no body (e.g. GET /v1/models).
	RequestBody []byte `json:"request_body,omitempty"`
	// ResponseBody is the raw downstream response body, before any response
	// plugin ran. Empty for streaming responses that were truncated to zero
	// bytes — those still set TruncatedResponse.
	ResponseBody []byte `json:"response_body,omitempty"`

	// RequestContentType and ResponseContentType are taken from the incoming
	// request's Content-Type and the downstream response's Content-Type. The
	// inspector uses these to decide between JSON pretty-printing, SSE
	// verbatim rendering, and plain text.
	RequestContentType  string `json:"request_content_type,omitempty"`
	ResponseContentType string `json:"response_content_type,omitempty"`

	// TruncatedRequest / TruncatedResponse are set when the corresponding
	// body exceeded MaxBodyBytes and the rest was dropped. The UI shows a
	// "truncated" badge so the operator knows they are looking at a partial
	// capture.
	TruncatedRequest  bool `json:"truncated_request,omitempty"`
	TruncatedResponse bool `json:"truncated_response,omitempty"`

	// Path, Method, Model, ResolvedModel, DownstreamID, DownstreamName, Status
	// mirror fields from RequestLogEntry so the inspector can render a
	// header without an extra /api/logs round-trip. DownstreamName is the
	// human-readable label (e.g. "OpenAI production") rather than the ID
	// ("openai-prod") so the inspector header is self-explanatory.
	Path           string `json:"path,omitempty"`
	Method         string `json:"method,omitempty"`
	Model          string `json:"model,omitempty"`
	ResolvedModel  string `json:"resolved_model,omitempty"`
	DownstreamID   string `json:"downstream_id,omitempty"`
	DownstreamName string `json:"downstream_name,omitempty"`
	Status         int    `json:"status,omitempty"`
	// ClientIP is the immediate peer's IP (port stripped) at the time
	// the request hit the gateway. Populated by the engine from
	// r.RemoteAddr; forwarded headers are intentionally ignored.
	ClientIP string `json:"client_ip,omitempty"`
}

// Store is the bounded ring of captured Entries keyed by log entry id. Safe
// for concurrent use.
type Store struct {
	mu      sync.RWMutex
	max     int
	entries map[int]*Entry
	// order tracks insertion ids in oldest-to-newest order. The slice head
	// is the next eviction candidate when the store is full.
	order []int
}

// New creates a Store that holds at most maxEntries snapshots. If maxEntries
// is <= 0 the caller is using the store in disabled mode and the methods
// become no-ops.
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

// Add inserts or replaces an entry. If the store is full, the oldest entry
// (by insertion time) is evicted first. Body bytes are copied defensively so
// later mutation by the caller cannot corrupt the stored snapshot.
//
// Safe to call when capture is disabled — callers gate this; the store
// itself never refuses a write.
func (s *Store) Add(e Entry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Defensive copies of body slices. The engine reuses its scratch
	// buffers between requests, so handing out aliases would let one
	// request's capture get clobbered by the next.
	if len(e.RequestBody) > 0 {
		req := make([]byte, len(e.RequestBody))
		copy(req, e.RequestBody)
		e.RequestBody = req
	}
	if len(e.ResponseBody) > 0 {
		resp := make([]byte, len(e.ResponseBody))
		copy(resp, e.ResponseBody)
		e.ResponseBody = resp
	}

	// If an entry with this id already exists (a re-Record for the same id,
	// which doesn't currently happen but is defensive), drop the old order
	// entry so we don't double-count it during eviction.
	if _, exists := s.entries[e.ID]; !exists {
		s.order = append(s.order, e.ID)
	} else {
		s.removeFromOrder(e.ID)
		s.order = append(s.order, e.ID)
	}
	s.entries[e.ID] = &e

	// Evict from the head until we are at or below the cap. We don't shrink
	// the order slice — it stays at max length, which is bounded.
	for len(s.entries) > s.max && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

// Get returns the captured entry for id, or false if it has been evicted or
// was never captured (capture flag off at the time the request came in).
func (s *Store) Get(id int) (*Entry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	if !ok {
		return nil, false
	}
	// Hand out a copy so the caller cannot mutate our retained state. The
	// body slices are already independent copies from Add, but copying the
	// struct header keeps the API consistent.
	cp := *e
	return &cp, true
}

// Len returns the current number of stored entries. Used by tests and the
// future UI status indicator.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Max returns the configured cap.
func (s *Store) Max() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.max
}

// SetMax adjusts the cap at runtime. Entries past the new cap are evicted
// in oldest-first order until the store fits.
func (s *Store) SetMax(n int) {
	if s == nil || n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.max = n
	for len(s.entries) > s.max && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

// removeFromOrder drops id from the order slice. The slice is small (bounded
// by max) so a linear scan is fine. Caller must hold s.mu.
func (s *Store) removeFromOrder(id int) {
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}
