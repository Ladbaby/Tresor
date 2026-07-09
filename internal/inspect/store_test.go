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
