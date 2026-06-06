//go:build windows

package proxy

import (
	"net/url"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// getSystemProxy reads the Windows system proxy from the registry.
// It checks HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings
// for ProxyEnable (DWORD=1) and ProxyServer (STRING).
func getSystemProxy() (*url.URL, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.READ)
	if err != nil {
		return nil, false
	}
	defer k.Close()

	enabled, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enabled != 1 {
		return nil, false
	}

	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return nil, false
	}

	server = strings.TrimSpace(server)
	if server == "" {
		return nil, false
	}

	u, err := url.Parse(server)
	if err != nil {
		return nil, false
	}

	return u, true
}
