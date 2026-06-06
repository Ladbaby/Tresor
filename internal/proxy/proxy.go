package proxy

import (
	"net/http"
	"net/url"
)

// Mode determines how the outbound proxy is resolved.
type Mode string

const (
	ModeAuto    Mode = "auto"    // Windows registry > env vars > direct
	ModeEnv     Mode = "env"     // Environment variables only
	ModeWindows Mode = "windows" // Windows registry + env vars fallback
	ModeNone    Mode = "none"    // No proxy, direct connection
)

// ProxyFunc returns an http.Transport.Proxy-compatible function for the given mode.
func ProxyFunc(mode Mode) func(*http.Request) (*url.URL, error) {
	switch mode {
	case ModeNone:
		return nil // nil means no proxy (direct connection)
	case ModeEnv:
		return http.ProxyFromEnvironment
	case ModeWindows, ModeAuto:
		return func(req *http.Request) (*url.URL, error) {
			// Windows/system registry proxy has highest priority
			if u, ok := getSystemProxy(); ok {
				return u, nil
			}
			// Fall back to environment variables (HTTP_PROXY / HTTPS_PROXY)
			return http.ProxyFromEnvironment(req)
		}
	default:
		// Default to auto behavior
		return func(req *http.Request) (*url.URL, error) {
			if u, ok := getSystemProxy(); ok {
				return u, nil
			}
			return http.ProxyFromEnvironment(req)
		}
	}
}
