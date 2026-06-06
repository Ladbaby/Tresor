//go:build !windows

package proxy

import "net/url"

// getSystemProxy is a stub for non-Windows platforms.
// On Windows, the build-tagged system_windows.go reads the registry instead.
func getSystemProxy() (*url.URL, bool) {
	return nil, false
}
