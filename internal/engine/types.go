package engine

import (
	"net/http"

	"tresor/internal/config"
	"tresor/internal/store"
)

// Downstream is a resolved target endpoint.
type Downstream struct {
	ID         string
	Name       string
	BaseURL    string
	// APIKey is the effective credential forwarded to the downstream. For
	// api_key auth it is the static key; for oauth it is the live access
	// token resolved at request time (substituted in place).
	APIKey     string
	ApiFormats []string
	// FormatURLs maps API format names to per-format base URLs.
	// Empty/nil map means "use BaseURL for all formats".
	FormatURLs map[string]string
	// FormatPaths maps API format names to the request path used when
	// forwarding in that format. When set for the active format the outbound
	// path is base_url + format_path and the client's own path is ignored;
	// when unset the engine appends the client's path. Empty/nil = no
	// override for any format.
	FormatPaths map[string]string
	// Auth holds the per-downstream auth config (api_key or oauth).
	Auth *config.DownstreamAuthCfg
}

// PipelineContext carries state through the transformation pipeline.
type PipelineContext struct {
	TargetDownstream *Downstream
	// DownstreamFormat is the API format the downstream will actually receive
	// (after any auto-translation). Used by forwardRequest to select the
	// correct per-format URL from TargetDownstream.FormatURLs, and is also
	// used for empty-response detection / stream format detection.
	DownstreamFormat string
	// OAuthExtraHeaders are additional headers required by the OAuth
	// provider (e.g. originator, user-agent). Applied in forwardRequest
	// after the standard auth header is set.
	OAuthExtraHeaders map[string]string
	Variables        map[string]interface{}
}

// TokenResolver resolves a currently-valid OAuth access token for a
// downstream (refreshing as needed). Implemented by the oauth.Manager.
type TokenResolver interface {
	ResolveValidToken(downstreamID string) (token string, extraHeaders map[string]string, err error)
}

// RequestTransformer modifies an outgoing request before it is forwarded.
type RequestTransformer interface {
	TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error)
}

// ResponseTransformer modifies a full response body before it is returned to the client.
type ResponseTransformer interface {
	TransformResponse(resp *http.Response, body []byte, ctx *PipelineContext) ([]byte, error)
}

// SSEChunk represents a single SSE event: an optional event type and the data
// payload (without "data: " prefix). For protocols without named events (e.g.,
// OpenAI), EventType is empty.
type SSEChunk struct {
	EventType string // e.g. "message_start", "content_block_delta" — empty for unnamed events
	Data      []byte // the JSON payload
}

// StreamResponseTransformer transforms a single SSE event chunk. Plugins that do
// not support streaming should return the chunk unchanged.
type StreamResponseTransformer interface {
	TransformStreamChunk(chunk SSEChunk, ctx *PipelineContext) (SSEChunk, error)
}

// SafeFlush calls flusher.Flush() with a panic guard. A broken or hijacked
// client connection can make Flush() panic; the flush is best-effort so we
// return false rather than crashing the calling goroutine (which often
// serves a live proxy request).
// ponytail: each call site does the same defer-recover dance — keep one copy.
func SafeFlush(flusher http.Flusher) (ok bool) {
	defer func() { _ = recover() }()
	flusher.Flush()
	return true
}

// PluginNamer is implemented by plugins that want to provide an explicit,
// stable type name for deduplication without relying on reflection.
type PluginNamer interface {
	PluginName() string
}

// PluginFactory creates a plugin instance from a configuration map.
type PluginFactory func(config map[string]interface{}) (interface{}, error)

// gatewayError carries structured error information for consistent logging and
// client-facing error responses across the proxy handler.
type gatewayError struct {
	status  int    // HTTP status code
	logMsg  string // detailed message for server logs
	httpMsg string // client-facing error message
	errLabel string // short label for log entry
	cause   error  // underlying error (nil if none)
}

// modelResult holds the output of model resolution: which downstream to forward
// to, any alias that matched, and the body to use for the pipeline.
type modelResult struct {
	ds            *store.Downstream
	alias         *store.Alias
	model         string // input model name
	resolvedModel string // model name after alias rewrite
	body          []byte // body to use for pipeline
}
