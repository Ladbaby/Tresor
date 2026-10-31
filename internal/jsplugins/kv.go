package jsplugins

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Plugin key-value store. A bounded, persistent, per-plugin-namespaced string
// map that outlives a plugin VM: unlike input.state (which is reset on every
// request and retry attempt) this store is a Go object the VM never clears, so
// a plugin can keep cross-request state such as a stable per-conversation id.
// Keys are namespaced by the host to the owning plugin's manifest id; JS never
// supplies the namespace. It is a cache, not a database: values are small
// strings, each namespace is capped with LRU eviction, and entries may carry an
// optional TTL.
//
// Durability is deliberately asynchronous. Request-path operations (Get, Set,
// GetOrSet, Delete, PurgeNamespace) only touch memory and mark the store dirty;
// a background flusher rewrites the file at most once per kvFlushInterval and
// once more on Stop. A Get therefore never performs I/O and never dirties the
// store, which keeps reads off the disk entirely: the previous design rewrote
// and fsynced the whole file under the store mutex on every call (measured at
// milliseconds per operation). Because reads never write, no request-path
// operation can return a disk error either; flush failures are logged and the
// store is re-dirtied for the next tick. The cost is that a hard crash can lose
// up to kvFlushInterval of writes, which is acceptable for a cache.

const (
	kvMaxKeyBytes   = 256
	kvMaxValueBytes = 4096
	kvMaxKeysPerNS  = 2048
	kvMaxTTL        = 30 * 24 * time.Hour
	kvMaxFileBytes  = 16 << 20
	kvFileVersion   = 1
	// kvEvictSlack is how far below the key cap a full namespace is trimmed, so
	// the O(n) least-recently-used scan runs once per slack insertions rather
	// than on every insert at the cap.
	kvEvictSlack = kvMaxKeysPerNS / 8
)

// kvFlushInterval is the debounce window for persistence. A variable so tests
// can shorten it.
var kvFlushInterval = time.Second

// kvEntry is one stored value. Exp is an absolute expiry in unix milliseconds,
// or 0 for no expiry. At is the last-write time in unix milliseconds and is
// persisted only to seed eviction order across restarts. seq is the live
// recency rank: it is bumped on read as well as on write, so an entry that is
// read every request stays recent. It is never serialized, so tracking it costs
// no flush.
type kvEntry struct {
	V   string `json:"v"`
	At  int64  `json:"at"`
	Exp int64  `json:"exp"`
	seq uint64
}

// kvFile is the on-disk shape of the store.
type kvFile struct {
	Version    int                           `json:"version"`
	Namespaces map[string]map[string]kvEntry `json:"namespaces"`
}

// KVStore is the bounded persistent store. The zero value is not usable; call
// NewKVStore or use the process-wide shared store. A store with an empty path
// is memory-only (used by unit tests and non-daemon contexts) and runs no
// flusher goroutine.
type KVStore struct {
	mu    sync.Mutex
	path  string
	now   func() int64
	ns    map[string]map[string]*kvEntry
	seq   uint64
	dirty bool
	stop  chan struct{}
	done  chan struct{}
}

// kvFlushInterval is a variable so tests can shorten it.
var kvFlushIntervalBox = kvFlushInterval

// NewKVStore loads the store at path, creating an empty one when the file is
// absent. A corrupt, oversized, or version-mismatched file is discarded with a
// logged warning and the store starts empty rather than failing daemon startup.
func NewKVStore(path string) (*KVStore, error) {
	s := &KVStore{path: path, now: func() int64 { return time.Now().UnixMilli() }, ns: map[string]map[string]*kvEntry{}}
	if path != "" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("create plugin kv directory: %w", err)
			}
		}
	}
	if err := s.load(); err != nil {
		log.Printf("warning: plugin kv store discarded: %v", err)
		s.ns = map[string]map[string]*kvEntry{}
		s.seq = 0
	}
	// Expired entries are dropped once at load; afterwards only the flusher
	// sweeps, so a read never mutates the store.
	s.sweepLocked(s.now())
	if path != "" {
		s.stop, s.done = make(chan struct{}), make(chan struct{})
		go s.runFlusher()
	}
	return s, nil
}

// load reads the file into memory and seeds per-namespace recency order from
// the persisted last-write times. It returns an error only when the file should
// be discarded.
func (s *KVStore) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read plugin kv store: %w", err)
	}
	if len(data) > kvMaxFileBytes {
		return fmt.Errorf("file exceeds %d bytes", kvMaxFileBytes)
	}
	var file kvFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("corrupt store: %w", err)
	}
	if file.Version != kvFileVersion {
		return fmt.Errorf("unsupported version %d", file.Version)
	}
	for ns, entries := range file.Namespaces {
		m := make(map[string]*kvEntry, len(entries))
		for k, e := range entries {
			e := e
			m[k] = &e
		}
		// Restore recency order from persisted write times: the oldest write
		// gets the smallest seq, so it is the first evicted.
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if m[keys[i]].At != m[keys[j]].At {
				return m[keys[i]].At < m[keys[j]].At
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			m[k].seq = s.nextSeq()
		}
		s.ns[ns] = m
	}
	return nil
}

// nextSeq returns the next recency rank. Callers hold s.mu.
func (s *KVStore) nextSeq() uint64 {
	s.seq++
	return s.seq
}

// snapshotLocked copies the store into its on-disk shape. Callers hold s.mu. The
// copy is a shallow map rebuild so JSON encoding, which is the expensive part,
// happens outside the lock.
func (s *KVStore) snapshotLocked() (kvFile, int) {
	s.sweepLocked(s.now())
	file := kvFile{Version: kvFileVersion, Namespaces: make(map[string]map[string]kvEntry, len(s.ns))}
	total := 0
	for ns, entries := range s.ns {
		m := make(map[string]kvEntry, len(entries))
		for k, e := range entries {
			m[k] = kvEntry{V: e.V, At: e.At, Exp: e.Exp}
		}
		file.Namespaces[ns] = m
		total += len(entries)
	}
	return file, total
}

// evictToFit sheds entries until the encoded store fits the byte ceiling. The
// ceiling applies to the encoded result rather than to entry counts alone,
// because JSON escaping can inflate a value, so a store we write can never
// exceed the size load() refuses.
//
// Each round copies under the lock, then encodes outside it, so a large store
// never blocks plugin requests for the duration of a marshal. Eviction is
// batched in proportion to the overshoot: removing one entry per round would be
// quadratic (a maximally-escaped 16 MiB store took over 40 seconds to trim a few
// hundred entries).
func (s *KVStore) evictToFit() ([]byte, error) {
	for {
		s.mu.Lock()
		file, total := s.snapshotLocked()
		s.mu.Unlock()

		data, err := json.Marshal(file)
		if err != nil {
			return nil, err
		}
		if len(data) <= kvMaxFileBytes || total == 0 {
			return data, nil
		}
		overshoot := float64(len(data)-kvMaxFileBytes) / float64(len(data))
		batch := int(math.Ceil(overshoot * float64(total)))
		if batch < 1 {
			batch = 1
		}
		s.mu.Lock()
		for i := 0; i < batch; i++ {
			if !s.evictOldestLocked("") {
				break
			}
		}
		s.mu.Unlock()
	}
}

// trimLocked enforces the per-namespace key cap after an insert. Callers hold
// s.mu and own entries. A full namespace is trimmed all the way down to
// kvMaxKeysPerNS-kvEvictSlack, so the expensive least-recently-used selection is
// paid once every kvEvictSlack insertions instead of on every insert at the cap.
func (s *KVStore) trimLocked(ns string, entries map[string]*kvEntry) {
	if len(entries) <= kvMaxKeysPerNS {
		return
	}
	s.evictOldestBatchLocked(ns, len(entries)-(kvMaxKeysPerNS-kvEvictSlack))
}

// evictOldestBatchLocked removes the count least-recently-used entries of ns in a
// single pass. Selecting victims one at a time is quadratic, which made a batch
// trim cost roughly a full scan per evicted key.
func (s *KVStore) evictOldestBatchLocked(ns string, count int) {
	entries := s.ns[ns]
	if count <= 0 || len(entries) == 0 {
		return
	}
	if count > len(entries) {
		count = len(entries)
	}
	type candidate struct {
		key string
		seq uint64
	}
	candidates := make([]candidate, 0, len(entries))
	for k, e := range entries {
		candidates = append(candidates, candidate{k, e.seq})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].seq < candidates[j].seq })
	for _, c := range candidates[:count] {
		delete(entries, c.key)
	}
	if len(entries) == 0 {
		delete(s.ns, ns)
	}
}

// evictOldestLocked removes the least-recently-used entry, in ns when ns is
// non-empty or across every namespace otherwise. Callers hold s.mu.
func (s *KVStore) evictOldestLocked(onlyNS string) bool {
	var victimNS, victimKey string
	victimSeq := ^uint64(0)
	found := false
	for ns, entries := range s.ns {
		if onlyNS != "" && ns != onlyNS {
			continue
		}
		for k, e := range entries {
			if !found || e.seq < victimSeq {
				victimNS, victimKey, victimSeq, found = ns, k, e.seq, true
			}
		}
	}
	if !found {
		return false
	}
	entries := s.ns[victimNS]
	delete(entries, victimKey)
	if len(entries) == 0 {
		delete(s.ns, victimNS)
	}
	return true
}

// sweepLocked drops expired entries and caps each namespace in LRU order.
// Callers hold s.mu. It runs only at load and during a flush, never on a read,
// so Get stays side-effect free. It does not set dirty: a sweep during a flush is
// persisted by the write that triggered it, and eviction during a mutation is
// covered by the mutation itself.
func (s *KVStore) sweepLocked(now int64) {
	for ns, entries := range s.ns {
		for k, e := range entries {
			if e.Exp > 0 && e.Exp <= now {
				delete(entries, k)
			}
		}
		if len(entries) > kvMaxKeysPerNS {
			s.evictOldestBatchLocked(ns, len(entries)-kvMaxKeysPerNS)
		}
		if len(entries) == 0 {
			delete(s.ns, ns)
		}
	}
}

// validateKVEntry rejects out-of-bounds keys, values, and TTLs. The caller turns
// these into a plugin type error.
func validateKVEntry(key, value string, ttlMs int64) error {
	if len(key) == 0 || len(key) > kvMaxKeyBytes {
		return fmt.Errorf("kv key out of range")
	}
	if len(value) > kvMaxValueBytes {
		return fmt.Errorf("kv value out of range")
	}
	if ttlMs < 0 || ttlMs > kvMaxTTL.Milliseconds() {
		return fmt.Errorf("kv ttl out of range")
	}
	return nil
}

// expiryFor converts a relative TTL in milliseconds to an absolute expiry.
func (s *KVStore) expiryFor(now, ttlMs int64) int64 {
	if ttlMs <= 0 {
		return 0
	}
	return now + ttlMs
}

// Get returns the value for key in ns and marks the entry most recently used.
// The second result reports whether the key was present and unexpired. It
// never performs I/O and never dirties the store.
func (s *KVStore) Get(ns, key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, ok := s.ns[ns]
	if !ok {
		return "", false
	}
	e, ok := entries[key]
	if !ok {
		return "", false
	}
	now := s.now()
	if e.Exp > 0 && e.Exp <= now {
		// Leave the entry for the flusher's sweep; report a miss so a caller
		// never observes an expired value.
		return "", false
	}
	e.seq = s.nextSeq()
	return e.V, true
}

// Set stores value for key in ns, replacing any existing entry. ttlMs is a
// relative lifetime in milliseconds; 0 means no expiry.
func (s *KVStore) Set(ns, key, value string, ttlMs int64) error {
	if err := validateKVEntry(key, value, ttlMs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entries, ok := s.ns[ns]
	if !ok {
		entries = make(map[string]*kvEntry, 1)
		s.ns[ns] = entries
	}
	entries[key] = &kvEntry{V: value, At: now, Exp: s.expiryFor(now, ttlMs), seq: s.nextSeq()}
	s.trimLocked(ns, entries)
	s.dirty = true
	return nil
}

// GetOrSet returns the value for key in ns, storing value and returning it when
// no unexpired entry exists. The lookup and the insert happen under one lock
// acquisition, so concurrent callers observe exactly one winner: this is why a
// plugin can mint a stable per-conversation id without a get-then-set race.
func (s *KVStore) GetOrSet(ns, key, value string, ttlMs int64) (string, error) {
	if err := validateKVEntry(key, value, ttlMs); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if entries, ok := s.ns[ns]; ok {
		if e, ok := entries[key]; ok && (e.Exp == 0 || e.Exp > now) {
			e.seq = s.nextSeq()
			return e.V, nil
		}
	}
	entries, ok := s.ns[ns]
	if !ok {
		entries = make(map[string]*kvEntry, 1)
		s.ns[ns] = entries
	}
	entries[key] = &kvEntry{V: value, At: now, Exp: s.expiryFor(now, ttlMs), seq: s.nextSeq()}
	s.trimLocked(ns, entries)
	s.dirty = true
	return value, nil
}

// Delete removes key from ns and reports whether it was present.
func (s *KVStore) Delete(ns, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, ok := s.ns[ns]
	if !ok {
		return false
	}
	if _, ok := entries[key]; !ok {
		return false
	}
	delete(entries, key)
	if len(entries) == 0 {
		delete(s.ns, ns)
	}
	s.dirty = true
	return true
}

// PurgeNamespace removes every entry under ns.
func (s *KVStore) PurgeNamespace(ns string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ns[ns]; !ok {
		return
	}
	delete(s.ns, ns)
	s.dirty = true
}

// runFlusher rewrites the file when the store has changed, at most once per
// interval, and once more on stop. It mirrors the engine stats flusher.
func (s *KVStore) runFlusher() {
	defer close(s.done)
	ticker := time.NewTicker(kvFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			s.flushNow()
			return
		case <-ticker.C:
			s.flushNow()
		}
	}
}

// flushNow writes the store through atomically when it is dirty. Only the
// in-memory copy is taken under s.mu; encoding, eviction, and the write happen
// outside it, so neither a large marshal nor an fsync blocks a plugin request.
func (s *KVStore) flushNow() {
	s.mu.Lock()
	if !s.dirty || s.path == "" {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	s.mu.Unlock()

	data, err := s.evictToFit()
	if err != nil {
		log.Printf("warning: plugin kv store marshal failed: %v", err)
		s.markDirty()
		return
	}
	if err := atomicFile(s.path, data); err != nil {
		log.Printf("warning: plugin kv store flush failed: %v", err)
		s.markDirty()
	}
}

// markDirty flags the store for another flush attempt.
func (s *KVStore) markDirty() {
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
}

// Stop performs a final flush and waits for the flusher to exit. It is safe to
// call more than once and on a memory-only store.
func (s *KVStore) Stop() {
	if s.stop == nil {
		return
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

// sharedKV is the process-wide store. It is created lazily so unit tests and
// non-daemon contexts (lint, pack) get an in-memory store without wiring.
var sharedKV atomic.Pointer[KVStore]

// InitSharedKV points the process-wide store at path and starts its flusher.
// Call once at daemon startup before any plugin is loaded. Failures are logged
// and degrade to a memory-only store: this is a cache, so an unusable store
// directory must not stop the gateway from serving requests.
func InitSharedKV(path string) {
	s, err := NewKVStore(path)
	if err != nil {
		log.Printf("warning: plugin kv store unavailable, continuing in memory: %v", err)
		s, err = NewKVStore("")
		if err != nil {
			s = &KVStore{now: func() int64 { return time.Now().UnixMilli() }, ns: map[string]map[string]*kvEntry{}}
		}
	}
	sharedKV.Store(s)
}

// StopSharedKV flushes and stops the process-wide store. Safe to call when
// InitSharedKV was never called or when called repeatedly.
func StopSharedKV() {
	if s := sharedKV.Load(); s != nil {
		s.Stop()
	}
}

// SetSharedKVForTesting installs s as the process-wide store and returns a
// restore function. Tests use it so plugin instances built by LoadPackage and
// NewInstance never share the ambient process store.
func SetSharedKVForTesting(s *KVStore) (restore func()) {
	previous := sharedKV.Load()
	sharedKV.Store(s)
	return func() {
		if previous != nil {
			sharedKV.Store(previous)
		}
	}
}

// shared returns the process-wide store, creating an in-memory one on first use
// when InitSharedKV was never called.
func shared() *KVStore {
	if s := sharedKV.Load(); s != nil {
		return s
	}
	s, _ := NewKVStore("")
	if s == nil {
		s = &KVStore{now: func() int64 { return time.Now().UnixMilli() }, ns: map[string]map[string]*kvEntry{}}
	}
	sharedKV.CompareAndSwap(nil, s)
	return sharedKV.Load()
}
