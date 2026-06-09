package proxy

import (
	"fmt"
	"net"
	"net/url"
)

// ValidateOutboundURL checks that the URL is parseable and uses http/https.
// No network restrictions are applied — the admin decides where traffic goes.
func ValidateOutboundURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed; only http and https permitted", u.Scheme)
	}

	return nil
}

// IsBareIP returns true if the URL's host is a literal IP address.
// Used to surface a warning when creating or editing downstreams with
// bare-IP endpoints (no SSL, potentially interceptible).
func IsBareIP(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil
}
