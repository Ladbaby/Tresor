package engine

import (
	"net/http"
	"sync"
	"time"
	"tresor/internal/inspect"
	"tresor/internal/proxy"
	"tresor/internal/store"
)

// PluginRegistry is the interface for looking up plugin factories.
// The concrete implementation is in internal/plugins.
type PluginRegistry interface {
	CreatePlugin(pluginID string, config map[string]interface{}) (interface{}, error)
	ListPlugins() []PluginInfo
}

// PluginInfo describes a registered plugin.
type PluginInfo struct {
	ID           string      `json:"id"`
	Description  string      `json:"description"`
	ConfigSchema interface{} `json:"config_schema,omitempty"`
}

// Engine is the core proxy handler. It matches incoming requests against
// rules, builds pipelines, and forwards transformed requests to downstreams.
type Engine struct {
	store     *store.Store
	registry  PluginRegistry
	client    *http.Client
	proxyAuth *proxyAuth
	logger    *RequestLogger

	// payloadStore receives the pre-transformer bytes from HandleProxy for
	// the inspector. Set once at startup; presence implies capture is on,
	// absence implies capture is off (so the engine never allocates a
	// scratch buffer for the disabled path).
	payloadStore *inspect.Store

	// retryOnEmpty enables automatic retry when a downstream returns an empty
	// response. When enabled, the gateway re-sends the request with exponential
	// backoff (up to retryMaxCount retries) if the downstream LLM produces no
	// content (e.g., empty choices, no text blocks).
	retryOnEmpty bool

	// tokenManager resolves valid OAuth access tokens for downstreams that
	// use OAuth auth (downstreams.auth_method = "oauth"). Nil when no OAuth
	// manager is wired (tests, or a config with no oauth_providers).
	tokenMu      sync.RWMutex
	tokenManager TokenResolver

	// statsBufMu guards statsBuf. The buffer accumulates per-request
	// usage_stats entries that the background flusher drains every
	// statsFlushInterval (default 60s). This collapses many small writes
	// into one bulk transaction — dramatically reducing fsync pressure on
	// SSDs at high request rates.
	statsBufMu sync.Mutex
	statsBuf   []store.StatsBatchEntry

	// statsStop signals the flusher goroutine to exit on engine shutdown.
	statsStop chan struct{}
	// statsDone is closed by the flusher when it has finished its last flush.
	statsDone chan struct{}
}

// New creates a new Engine.
func New(s *store.Store) *Engine {
	e := &Engine{
		store:     s,
		client:    &http.Client{},
		logger:    NewRequestLogger(),
		statsStop: make(chan struct{}),
		statsDone: make(chan struct{}),
	}
	go e.runStatsFlusher()
	return e
}

// SetLogger sets the request logger on the engine.
func (e *Engine) SetLogger(l *RequestLogger) {
	e.logger = l
}

// GetLogger returns the request logger.
func (e *Engine) GetLogger() *RequestLogger {
	return e.logger
}

// SetRegistry sets the plugin registry on the engine (called during initialization).
func (e *Engine) SetRegistry(r PluginRegistry) {
	e.registry = r
}

// SetProxyMode configures the outbound HTTP client's proxy behavior and transport settings.
// It replaces the default http.Client with one that uses a custom Transport
// respecting the given proxy mode (auto, env, windows, none).
//
// DisableCompression is set to true so Go does not silently decompress upstream
// responses. Some downstreams return Brotli-encoded streams (Content-Encoding: br)
// which Go's stdlib cannot decode, and when the body is streamed to the client
// (SSE) the raw compressed bytes leak through. Instead, we ask the downstream
// for plain text via Accept-Encoding: identity, set in forwardRequest.
func (e *Engine) SetProxyMode(mode proxy.Mode) {
	transport := &http.Transport{
		Proxy:               proxy.ProxyFunc(mode),
		IdleConnTimeout:     30 * time.Second, // Close idle connections after 30s of inactivity
		MaxIdleConns:        25,               // Total idle connection pool
		MaxIdleConnsPerHost: 5,                // Per-downstream idle pool
		DisableCompression:  true,
	}
	e.client = &http.Client{
		Transport: transport,
	}
}

// SetProxyAuthKeys configures API key authentication for incoming proxy requests.
// If keys is empty or nil, authentication is disabled and all requests are allowed.
func (e *Engine) SetProxyAuthKeys(keys []string) {
	if len(keys) == 0 {
		e.proxyAuth = nil
		return
	}
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}
	e.proxyAuth = &proxyAuth{enabled: true, keys: keySet}
}

// SetPayloadStore wires the in-memory payload store that the inspector
// reads from. The engine calls store.Add on every recorded request keyed
// by the log entry id. Pass nil to disable payload capture.
func (e *Engine) SetPayloadStore(s *inspect.Store) {
	e.payloadStore = s
}

// SetRetryOnEmpty enables or disables automatic retry when a downstream
// returns an empty response.
func (e *Engine) SetRetryOnEmpty(enabled bool) {
	e.retryOnEmpty = enabled
}

// SetTokenManager wires the OAuth token resolver. Pass nil to disable OAuth
// support.
func (e *Engine) SetTokenManager(m TokenResolver) {
	e.tokenMu.Lock()
	e.tokenManager = m
	e.tokenMu.Unlock()
}

// getTokenManager returns the wired OAuth token resolver (nil if none).
func (e *Engine) getTokenManager() TokenResolver {
	e.tokenMu.RLock()
	defer e.tokenMu.RUnlock()
	return e.tokenManager
}

// Registry returns the current plugin registry.
func (e *Engine) Registry() PluginRegistry {
	return e.registry
}

// Store returns the store for admin API access.
func (e *Engine) Store() *store.Store {
	return e.store
}
