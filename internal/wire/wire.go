// Package wire defines how a provider's non-generic request dialect is shaped:
// request body, correlation headers, model discovery, and engine policy. A
// downstream selects one by name with the `wire:` field on its auth block;
// omitting it means generic behavior.
//
// This package holds only the contract and the lookup. The dialect logic itself
// lives in plugin packages (see DialectDeclaration in internal/jsplugins), so
// adding a provider is a package install rather than a code change. SetSource
// installs the resolver that reaches those packages; nothing here knows any
// provider by name.
//
// Why an adapter and not config fields: some dialects cannot be described. A
// subscription backend needs a body fingerprint, a compressed body, a
// correlation id, a client-specific header, and a model manifest that only
// answers under a version-pinned identity. Encoding that as fields would mean
// `vendor_*` keys forever. Instead the schema stays at one field -- the name.
//
// Why explicit rather than inferred: the previous code detected one backend by
// looking for a literal header, so any provider that happened to send that header
// silently inherited that backend's no-replay policy. Selecting by name removes
// that class of bug.
package wire

import (
	"context"
	"net/http"
	"sort"
	"sync"
)

// Request is what an adapter may shape before the request is sent.
type Request struct {
	// Original is the client's request, for headers the adapter wants to see
	// (a stable session id, a beta opt-in).
	Original *http.Request
	// Body is the final outbound body, after format translation and pipeline
	// transforms.
	Body []byte
}

// Prepared is the adapter's output: a possibly reshaped body plus a hook that
// applies late headers once generic authentication is set.
type Prepared struct {
	Body []byte
	// ApplyHeaders runs after generic auth and declared extra headers. It may be
	// nil, meaning the adapter only reshapes the body.
	ApplyHeaders func(*http.Request)
	// Err reports a failure to shape the request, so the gateway can fail the
	// attempt with a specific message instead of forwarding a half-shaped body.
	Err error
}

// Adapter is one provider dialect.
type Adapter interface {
	// Name is the value of the `wire:` field that selects this adapter.
	Name() string
	// PrepareRequest shapes the outbound request. A nil-safe no-op is allowed.
	PrepareRequest(Request) Prepared
	// NoReplay reports whether retry-on-empty must be disabled. Replaying a
	// generation against a real subscription creates duplicate response objects
	// on the provider's side, so adapters for subscription backends return true.
	NoReplay() bool
	// Discovery names the model-discovery strategy this dialect needs, or "" to
	// let the declared api_formats decide. Model discovery belongs to the dialect
	// rather than to a config field: only a provider with a non-generic model
	// endpoint needs anything other than the default list.
	Discovery() string
}

// Discoverer is implemented by a dialect that answers its provider's model list
// with its own protocol. The host performs the HTTP request — a package never
// gets network access — but the URL, query, identity headers, and response
// parsing are the dialect's, because they are provider knowledge.
type Discoverer interface {
	DiscoverModels(ctx context.Context, baseURL, token string, extraHeaders map[string]string) ([]string, error)
}

// Source resolves dialect names to adapters outside this package, and reports
// which names it can provide. It is what lets a plugin package be a dialect.
type Source interface {
	For(name string) (Adapter, bool)
	Names() []string
}

var (
	mu     sync.RWMutex
	source Source
)

// SetSource installs the resolver consulted when no built-in matches. Passing nil
// removes it. Built-in adapters always win, so a package can never shadow one.
func SetSource(s Source) {
	mu.Lock()
	defer mu.Unlock()
	source = s
}

// ActiveSourceForTest returns the currently installed resolver. It exists so a
// test that installs a temporary source can restore the previous one rather than
// leaking process-wide state into tests that run later.
func ActiveSourceForTest() Source {
	mu.RLock()
	defer mu.RUnlock()
	return source
}

// For returns the adapter named by an auth block's wire field. An empty name means
// generic behavior and yields (nil, true). The second result is false for an
// unknown name, which config validation reports as a specific error rather than
// silently degrading to generic.
func For(name string) (Adapter, bool) {
	if name == "" {
		return nil, true
	}
	mu.RLock()
	s := source
	mu.RUnlock()
	if s == nil {
		return nil, false
	}
	return s.For(name)
}

// Names lists every available adapter name, sorted, for error messages, the
// version endpoint, and documentation.
func Names() []string {
	mu.RLock()
	s := source
	mu.RUnlock()
	if s == nil {
		return nil
	}
	out := append([]string{}, s.Names()...)
	sort.Strings(out)
	return out
}
