// Package discovery holds the strategies Tresor uses to ask a provider what
// models it serves. A strategy is selected per downstream; the default is the
// OpenAI-style list, which is what most providers and every OpenAI-compatible
// relay (one-api, LiteLLM, sub2api) answer.
//
// Keeping these here rather than inline in the API handler means the rules for
// "which endpoint, which auth header, which response shape" live in one place per
// dialect, and a provider profile from the catalog can name a strategy instead of
// the UI guessing from api_formats.
package discovery

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Strategy discovers the model ids a provider serves.
type Strategy interface {
	// Name is the value of the `models.discovery` profile field.
	Name() string
	// Models lists model ids. baseURL is the downstream's base URL (per-format
	// override already applied), token the access token or API key, and formats
	// the declared api_formats, which decide the auth header to send.
	Models(ctx context.Context, in Input) ([]string, error)
}

// Input is everything a strategy may need.
type Input struct {
	BaseURL string
	Token   string
	Formats []string
	// Client is the HTTP client to use; nil means a default with Timeout.
	Client *http.Client
	// ExtraHeaders are the downstream's resolved declared headers, so a provider
	// that gates its model list on identity can say so.
	ExtraHeaders map[string]string
}

// Timeout bounds a discovery request. It is generous because a slow provider
// should not look like a provider with no models.
const Timeout = 15 * time.Second

// Client returns the client to use, defaulting to a bounded one.
func (in Input) ClientOrDefault() *http.Client {
	if in.Client != nil {
		return in.Client
	}
	return &http.Client{Timeout: Timeout}
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Strategy{}
)

// Register installs a strategy. A duplicate name panics at startup.
func Register(s Strategy) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if s.Name() == "" {
		panic("discovery: strategy has no name")
	}
	if _, exists := registry[s.Name()]; exists {
		panic("discovery: duplicate strategy " + s.Name())
	}
	registry[s.Name()] = s
}

// For returns the named strategy. An empty name yields the default (openai_list),
// so a downstream that declares nothing behaves as it always has.
func For(name string) (Strategy, error) {
	if name == "" {
		name = OpenaiList
	}
	registryMu.RLock()
	defer registryMu.RUnlock()
	s, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("models.discovery: unknown strategy %q (available: %v)", name, Names())
	}
	return s, nil
}

// Names lists registered strategies, sorted.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
