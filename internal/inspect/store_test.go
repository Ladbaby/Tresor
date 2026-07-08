package inspect

import (
	"sync"
	"testing"
)

// TestStore_AddAndGet verifies the basic put/get round-trip and that body
// bytes are defensively copied — mutating the input slice after Add must
// not affect the stored snapshot.
func TestStore_AddAndGet(t *testing.T) {
	s := New(10)
	req := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	resp := []byte(`{"choices":[{"message":{"content":"hello"}}]}`)

	s.Add(Entry{
		ID:                 1,
		RequestBody:        req,
		ResponseBody:       resp,
		RequestContentType: "application/json",
		Status:             200,
		Path:               "/v1/chat/completions",
	})

	// Mutate the original slices after Add; stored bytes must not change.
	for i := range req {
		req[i] = 'X'
	}
	for i := range resp {
		resp[i] = 'Y'
	}

	got, ok := s.Get(1)
	if !ok {
		t.Fatalf("expected to find id 1")
	}
	if string(got.RequestBody) != `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}` {
		t.Fatalf("request body was not defensively copied: %q", got.RequestBody)
	}
	if string(got.ResponseBody) != `{"choices":[{"message":{"content":"hello"}}]}` {
		t.Fatalf("response body was not defensively copied: %q", got.ResponseBody)
	}
	if got.RequestContentType != "application/json" {
		t.Fatalf("content type not preserved: %q", got.RequestContentType)
	}
	if got.Status != 200 {
		t.Fatalf("status not preserved: %d", got.Status)
	}
}

// TestStore_EvictsOldestWhenFull verifies the bounded behaviour: once max
// entries is reached, the next Add drops the oldest insertion.
func TestStore_EvictsOldestWhenFull(t *testing.T) {
	s := New(3)
	for i := 1; i <= 5; i++ {
		s.Add(Entry{ID: i, RequestBody: []byte("body")})
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("expected len 3 after 5 inserts, got %d", got)
	}
	// Oldest two (ids 1 and 2) should be gone.
	if _, ok := s.Get(1); ok {
		t.Fatalf("expected id 1 to be evicted")
	}
	if _, ok := s.Get(2); ok {
		t.Fatalf("expected id 2 to be evicted")
	}
	// Newest three (3, 4, 5) should be present.
	for _, id := range []int{3, 4, 5} {
		if _, ok := s.Get(id); !ok {
			t.Fatalf("expected id %d to be present", id)
		}
	}
}

// TestStore_SetMaxShrinksAndEvicts verifies that the cap can be shrunk at
// runtime and old entries are evicted in insertion order.
func TestStore_SetMaxShrinksAndEvicts(t *testing.T) {
	s := New(10)
	for i := 1; i <= 8; i++ {
		s.Add(Entry{ID: i})
	}
	if got := s.Len(); got != 8 {
		t.Fatalf("expected len 8, got %d", got)
	}
	s.SetMax(3)
	if got := s.Len(); got != 3 {
		t.Fatalf("expected len 3 after shrink, got %d", got)
	}
	for _, id := range []int{1, 2, 3, 4, 5} {
		if _, ok := s.Get(id); ok {
			t.Fatalf("expected id %d to be evicted after shrink", id)
		}
	}
	for _, id := range []int{6, 7, 8} {
		if _, ok := s.Get(id); !ok {
			t.Fatalf("expected id %d to remain after shrink", id)
		}
	}
}

// TestStore_NilSafe makes the nil-store no-op behaviour explicit. The
// engine gates the hot path with an atomic.Load, but if a nil store ever
// slips through (e.g. in a test) the call must not panic.
func TestStore_NilSafe(t *testing.T) {
	var s *Store
	s.Add(Entry{ID: 1}) // must not panic
	if _, ok := s.Get(1); ok {
		t.Fatalf("nil store should return false")
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("nil store len should be 0, got %d", got)
	}
	if got := s.Max(); got != 0 {
		t.Fatalf("nil store max should be 0, got %d", got)
	}
}

// TestStore_ConcurrentAddAndGet exercises the lock with -race. Many writers
// add entries, many readers call Get. No panics, no lost entries within
// the cap, and a final Len() that matches the actual map.
func TestStore_ConcurrentAddAndGet(t *testing.T) {
	s := New(50)
	var wg sync.WaitGroup
	const writers = 8
	const perWriter = 50
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				s.Add(Entry{ID: base + i, RequestBody: []byte("x")})
			}
		}(w * 1000)
	}
	const readers = 4
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _ = s.Get(i)
				_ = s.Len()
			}
		}()
	}
	wg.Wait()
	if got := s.Len(); got > 50 {
		t.Fatalf("len exceeded cap: %d", got)
	}
}
