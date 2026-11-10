package engine

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"tresor/internal/oauth"
	"tresor/internal/wire"
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
			return nil, &gatewayError{http.StatusUnauthorized, "provider " + ds.ID + " uses oauth but no oauth manager is configured", "provider is configured for OAuth but the daemon has no OAuth support enabled", "oauth not configured", nil}
		}
		token, extraHeaders, err := tm.ResolveValidToken(ds.ID)
		if err != nil {
			// Transient refresh failure is retryable by the caller, not a reason
			// to discard credentials or ask the user to reconnect.
			if errors.Is(err, oauth.ErrTransientRefresh) {
				msg := "provider token refresh is temporarily unavailable — please retry"
				return nil, &gatewayError{http.StatusServiceUnavailable, msg, msg, "oauth refresh temporarily unavailable", err}
			}
			msg := "provider not connected — finish the OAuth login in the Tresor dashboard (Providers tab)"
			return nil, &gatewayError{http.StatusUnauthorized, msg, msg, "oauth not connected", err}
		}
		ctx.TargetDownstream.APIKey = token
		ctx.ExtraHeaders = extraHeaders
	}

	// Declared extra headers apply to every auth type, so a provider relay can
	// say what it needs without a rule or a plugin. For an api_key downstream
	// the token argument is the key itself: literal and template values work,
	// and a claim-derived one finds no claim and contributes nothing.
	if !ds.IsOAuth() && ds.Auth != nil && len(ds.Auth.ExtraHeaders) > 0 {
		resolved, err := oauth.ResolveHeaderValues(ds.Auth.ExtraHeaders, ds.EffectiveAPIKey(), nil)
		if err != nil {
			log.Printf("engine: downstream %s: refusing declared extra_headers: %v", ds.ID, err)
		} else {
			ctx.ExtraHeaders = resolved
		}
	}

	// The wire adapter owns this provider's non-generic request dialect. It is
	// selected by name from config, never inferred from a header, so a provider
	// cannot accidentally inherit another provider's policy.
	if ds.Auth != nil && ds.Auth.Wire != "" {
		adapter, ok := wire.For(ds.Auth.Wire)
		if !ok {
			// A declared but unavailable dialect must not fall through to generic
			// forwarding: that would send the provider a request stripped of the
			// identity and policy the dialect exists to apply, and look like a
			// success. Fail the request instead, and let the message say why.
			available := "none installed"
			if names := wire.Names(); len(names) > 0 {
				available = strings.Join(names, ", ")
			}
			msg := fmt.Sprintf("provider %s needs the %q provider dialect, which is not installed "+
				"(available: %s). Install it from Providers > Fetch Providers.", ds.ID, ds.Auth.Wire, available)
			return ctx, &gatewayError{http.StatusInternalServerError, msg, msg, "provider dialect not installed", nil}
		}
		ctx.Wire = adapter
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
