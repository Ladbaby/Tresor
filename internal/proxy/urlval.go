package proxy

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// blockedNetworks defines RFC-defined private/reserved address ranges.
var blockedNetworks = []string{
	"10.0.0.0/8",      // RFC-1918 private
	"172.16.0.0/12",   // RFC-1918 private
	"192.168.0.0/16",  // RFC-1918 private
	"169.254.0.0/16",  // link-local
	"127.0.0.0/8",     // loopback
	"0.0.0.0/8",       // unspecified
	"::1/128",         // IPv6 loopback
	"fc00::/7",        // IPv6 unique-local
	"fe80::/10",       // IPv6 link-local
}

var blockedCIDRs []*net.IPNet

func init() {
	blockedCIDRs = make([]*net.IPNet, 0, len(blockedNetworks))
	for _, n := range blockedNetworks {
		_, cidr, err := net.ParseCIDR(n)
		if err != nil {
			continue
		}
		blockedCIDRs = append(blockedCIDRs, cidr)
	}
}

// ValidateOutboundURL checks that the URL is safe for outbound requests.
// It rejects non-http/https schemes and literal private/reserved IP addresses.
// Hostname DNS resolution is best-effort (skipped on failure) so that
// unresolvable or fictional domains are not blocked at creation time.
// Returns nil if the URL is safe, or an error describing the rejection.
func ValidateOutboundURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Only allow http and https
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed; only http and https permitted", u.Scheme)
	}

	host := u.Hostname()

	// Check if it's a literal IP address
	ip := net.ParseIP(host)
	if ip != nil {
		return checkIP(ip)
	}

	// Resolve hostname (best-effort — skip if unresolvable)
	addrs, err := net.LookupHost(host)
	if err != nil {
		// Can't resolve — let the user try; the request will fail at runtime.
		return nil
	}

	for _, resolved := range addrs {
		rIP := net.ParseIP(resolved)
		if rIP != nil {
			if err := checkIP(rIP); err != nil {
				return err
			}
		}
	}

	return nil
}

// stripPort removes the port from a host:port string, handling IPv6 brackets.
func stripPort(host string) string {
	// Handle IPv6: [::1]:443 -> ::1
	host = strings.Trim(host, "[]")
	// Handle IPv4: example.com:443 -> example.com
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	return host
}

func checkIP(ip net.IP) error {
	for _, cidr := range blockedCIDRs {
		if cidr.Contains(ip) {
			return fmt.Errorf("address %s is in a blocked network", ip)
		}
	}
	return nil
}
