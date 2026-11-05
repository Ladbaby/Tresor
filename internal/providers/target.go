package providers

import (
	"net/url"
	"strings"
)

// TargetURL is the URL a request in format lands on, given a downstream's
// base_url, format_urls and format_paths. The gateway forwards through it (see
// engine.forwardRequest) and a profile is checked against a real endpoint through
// it, so a published recipe is exercised on the same rule the gateway applies.
//
// Two modes, matching what an operator sees in the UI:
//
//  1. A per-format path override wins, and the client's own path is ignored. The
//     result is that override on the format's base URL, which is what a provider
//     expects regardless of what the client called.
//  2. Otherwise the client's path is appended, after stripping a prefix the base
//     URL already carries, so "https://host/v1" plus "/v1/chat/completions" does
//     not become "/v1/v1/chat/completions".
//
// An empty format, base URL, client path or nil map all fall through harmlessly,
// because a downstream that declares nothing must keep behaving as it always has.
func TargetURL(baseURL string, formatURLs, formatPaths map[string]string, format, clientPath string) string {
	base := baseURL
	if format != "" && formatURLs != nil {
		if u := formatURLs[format]; u != "" {
			base = u
		}
	}
	base = strings.TrimRight(base, "/")

	path := ""
	if format != "" && formatPaths != nil {
		path = formatPaths[format]
	}
	if path == "" {
		path = clientPath
		if parsedBase, err := url.Parse(base); err == nil && parsedBase.Path != "" {
			prefix := strings.TrimSuffix(parsedBase.Path, "/")
			if strings.HasPrefix(path, prefix) {
				path = strings.TrimPrefix(path, prefix)
			}
		}
	}
	return base + path
}
