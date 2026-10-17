package inspect

import (
	"sync"
	"testing"
)

// TestStore_AddAndGet verifies the basic put/get round-trip.
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

	got, ok := s.Get(1)
	if !ok {
		t.Fatalf("expected to find id 1")
	}
	if string(got.RequestBody) != `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}` {
		t.Fatalf("request body mismatch: %q", got.RequestBody)
	}
	if string(got.ResponseBody) != `{"choices":[{"message":{"content":"hello"}}]}` {
		t.Fatalf("response body mismatch: %q", got.ResponseBody)
	}
	if got.RequestContentType != "application/json" {
		t.Fatalf("content type not preserved: %q", got.RequestContentType)
	}
	if got.Status != 200 {
		t.Fatalf("status not preserved: %d", got.Status)
	}
}

// TestStore_EvictsOldestWhenFull verifies bounded behaviour.
func TestStore_EvictsOldestWhenFull(t *testing.T) {
	s := New(3)
	for i := 1; i <= 5; i++ {
		s.Add(Entry{ID: i, RequestBody: []byte("body")})
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("expected len 3 after 5 inserts, got %d", got)
	}
	if _, ok := s.Get(1); ok {
		t.Fatalf("expected id 1 to be evicted")
	}
	if _, ok := s.Get(2); ok {
		t.Fatalf("expected id 2 to be evicted")
	}
	for _, id := range []int{3, 4, 5} {
		if _, ok := s.Get(id); !ok {
			t.Fatalf("expected id %d to be present", id)
		}
	}
}

func TestStore_EvictsByPayloadBytes(t *testing.T) {
	s := New(10)
	s.maxBytes = 10
	s.Add(Entry{ID: 1, RequestBody: []byte("123"), ResponseBody: []byte("456")})
	s.Add(Entry{ID: 2, ResponseBody: []byte("78901")})
	if _, ok := s.Get(1); ok || s.retainedBytes != 5 || s.Len() != 1 {
		t.Fatalf("byte eviction failed: len=%d bytes=%d", s.Len(), s.retainedBytes)
	}
	// An entry larger than the entire budget must not remain retained.
	s.Add(Entry{ID: 3, ResponseBody: make([]byte, 11)})
	if s.Len() != 0 || s.retainedBytes != 0 {
		t.Fatalf("oversized entry retained: len=%d bytes=%d", s.Len(), s.retainedBytes)
	}
}

func TestStore_ReplacementAccountingAndOrder(t *testing.T) {
	s := New(2)
	s.maxBytes = 10
	s.Add(Entry{ID: 1, ResponseBody: []byte("123456")})
	s.Add(Entry{ID: 2, ResponseBody: []byte("78")})
	s.Add(Entry{ID: 1, ResponseBody: []byte("x")})
	if s.retainedBytes != 3 || len(s.order) != 2 || s.order[0] != 2 {
		t.Fatalf("replacement accounting/order: bytes=%d order=%v", s.retainedBytes, s.order)
	}
	s.Add(Entry{ID: 3, ResponseBody: []byte("abc")})
	if _, ok := s.Get(2); ok || s.retainedBytes != 4 || s.Len() != 2 {
		t.Fatalf("replacement eviction: bytes=%d order=%v", s.retainedBytes, s.order)
	}
	if _, ok := s.Get(1); !ok {
		t.Fatal("replacement was incorrectly evicted")
	}
}

// TestStore_ConcurrentAddAndGet exercises the lock with -race.
func TestStore_ConcurrentAddAndGet(t *testing.T) {
	s := New(50)
	var wg sync.WaitGroup
	const writers = 8
	const perWriter = 50
	for w := range writers {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := range perWriter {
				s.Add(Entry{ID: base + i, RequestBody: []byte("x")})
			}
		}(w * 1000)
	}
	const readers = 4
	for range readers {
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
