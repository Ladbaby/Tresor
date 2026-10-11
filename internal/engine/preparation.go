package engine

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"tresor/internal/oauth"
)

// prepareDownstreamContext resolves request-specific credentials and format
// facts without taking ownership of logging, attempt cleanup or publication.
func (e *Engine) prepareDownstreamContext(r *http.Request, result *modelResult, inputFormat string) (*PipelineContext, *gatewayError) {
	ds := result.ds
	ctx := &PipelineContext{
		TargetDownstream: &Downstream{
			ID:          ds.ID,
			Name:        ds.Name,
			BaseURL:     ds.BaseURL,
			APIKey:      ds.EffectiveAPIKey(),
			ApiFormats:  ds.ApiFormats,
			FormatURLs:  ds.FormatURLs,
			FormatPaths: ds.FormatPaths,
			Auth:        ds.Auth,
		},
		ClientFormat:    inputFormat,
		ClientStreaming: isStreamRequest(result.body) || strings.HasSuffix(r.URL.Path, ":streamGenerateContent"),
		Variables:       make(map[string]interface{}),
	}
	if ds.IsOAuth() {
		tm := e.getTokenManager()
		if tm == nil {
			return nil, &gatewayError{http.StatusUnauthorized, "downstream " + ds.ID + " uses oauth but no oauth manager is configured", "provider is configured for OAuth but the daemon has no OAuth support enabled", "oauth not configured", nil}
		}
		token, extraHeaders, err := tm.ResolveValidToken(ds.ID)
		if err != nil {
			// Transient refresh failure is retryable by the caller, not a reason
			// to discard credentials or ask the user to reconnect.
			if errors.Is(err, oauth.ErrTransientRefresh) {
				msg := "provider token refresh is temporarily unavailable — please retry"
				return nil, &gatewayError{http.StatusServiceUnavailable, msg, msg, "oauth refresh temporarily unavailable", err}
			}
			msg := "provider not connected — finish the OAuth login in the Tresor dashboard (Downstreams tab)"
			return nil, &gatewayError{http.StatusUnauthorized, msg, msg, "oauth not connected", err}
		}
		ctx.TargetDownstream.APIKey = token
		ctx.OAuthExtraHeaders = extraHeaders
		// Backend identity gates fingerprint and no-replay policy only;
		// generic transport selection is independent of it.
		_, ctx.CodexBackend = extraHeaders["chatgpt-account-id"]
	}

	// A multi-format downstream echoes a supported input format; otherwise
	// automatic translation targets its first declared format.
	if inputFormat != "" && len(ds.ApiFormats) > 0 && slices.Contains(ds.ApiFormats, inputFormat) {
		ctx.DownstreamFormat = inputFormat
	} else if len(ds.ApiFormats) > 0 {
		ctx.DownstreamFormat = ds.ApiFormats[0]
	}
	return ctx, nil
}
