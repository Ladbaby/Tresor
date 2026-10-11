package jsplugins

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"tresor/internal/proxy"
)

const MaxBundleBytes = 6*MaxSourceBytes + (1 << 20)

type RemoteInput struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256,omitempty"`
	Signature string `json:"signature,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
}

func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && strings.ToLower(s) == s
}
func SignatureMessage(digest string) []byte {
	return []byte("tresor-plugin-package-v1\n" + digest + "\n")
}

func VerifyBundle(data []byte, input RemoteInput) (map[string]string, Provenance, error) {
	p := Provenance{Source: "remote"}
	u, urlErr := url.Parse(input.URL)
	if urlErr != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || len(input.URL) > 4096 {
		return nil, p, fmt.Errorf("public HTTPS URL without credentials/fragment required")
	}
	if input.SHA256 != "" && !validDigest(input.SHA256) {
		return nil, p, fmt.Errorf("expected SHA-256 must be lowercase hexadecimal")
	}
	if (input.Signature == "") != (input.PublicKey == "") {
		return nil, p, fmt.Errorf("signature and publisher key must be supplied together")
	}
	if len(data) > MaxBundleBytes {
		return nil, p, fmt.Errorf("bundle exceeds limit")
	}
	var bundle struct {
		Files map[string]string `json:"files"`
	}
	if err := StrictJSON(data, &bundle); err != nil {
		return nil, p, fmt.Errorf("invalid bundle: %w", err)
	}
	digest, err := SourceDigest(bundle.Files)
	if err != nil {
		return nil, p, err
	}
	if input.SHA256 != "" && digest != input.SHA256 {
		return nil, p, fmt.Errorf("package integrity mismatch")
	}
	if input.Signature != "" {
		key, e := base64.StdEncoding.Strict().DecodeString(input.PublicKey)
		if e != nil || len(key) != ed25519.PublicKeySize {
			return nil, p, fmt.Errorf("invalid Ed25519 public key")
		}
		sig, e := base64.StdEncoding.Strict().DecodeString(input.Signature)
		if e != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, SignatureMessage(digest), sig) {
			return nil, p, fmt.Errorf("publisher signature verification failed")
		}
		fingerprint := sha256.Sum256(key)
		p.Publisher = hex.EncodeToString(fingerprint[:])
		p.Signed = true
	}
	// A calculated digest identifies source; only caller-pinned integrity or
	// supplied-key signature checks establish additional verification.
	p.Verified = input.SHA256 != "" || p.Signed
	if u, e := url.Parse(input.URL); e == nil {
		u.RawQuery = ""
		u.User = nil
		p.URL = u.String()
	}
	return bundle.Files, p, nil
}
func publicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, prefix := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96"} {
		if netip.MustParsePrefix(prefix).Contains(a) {
			return false
		}
	}
	return true
}

// DownloadBundle has no production private-address bypass. Tests inject the
// resolver/dialer into downloadBundle; proxy routing is administrator-trusted.
func DownloadBundle(ctx context.Context, input RemoteInput, mode proxy.Mode) ([]byte, error) {
	return downloadBundle(ctx, input, mode, net.DefaultResolver.LookupIPAddr, (&net.Dialer{Timeout: 10 * time.Second}).DialContext, nil)
}
func downloadBundle(ctx context.Context, input RemoteInput, mode proxy.Mode, resolve func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error), configure func(*http.Transport)) ([]byte, error) {
	if input.SHA256 != "" && !validDigest(input.SHA256) || (input.Signature == "") != (input.PublicKey == "") {
		return nil, fmt.Errorf("optional digest must be valid and signature/key must be paired")
	}
	u, err := url.Parse(input.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || len(input.URL) > 4096 {
		return nil, fmt.Errorf("public HTTPS URL without credentials/fragment required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Pins are immutable once established, including when a chain revisits a
	// hostname. Transport dialing can run concurrently with redirect checks.
	var pinsMu sync.RWMutex
	pins := make(map[string][]net.IPAddr)
	validateDestination := func(target *url.URL) error {
		if target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || target.Opaque != "" || len(target.String()) > 4096 {
			return fmt.Errorf("public HTTPS URL without credentials/fragment required")
		}
		host := strings.ToLower(target.Hostname())
		pinsMu.RLock()
		_, pinned := pins[host]
		pinsMu.RUnlock()
		if pinned {
			return nil
		}
		ips, err := resolve(ctx, target.Hostname())
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("destination resolution failed")
		}
		for _, ip := range ips {
			if !publicIP(ip.IP) {
				return fmt.Errorf("non-public package destination prohibited")
			}
		}
		pinsMu.Lock()
		pins[host] = ips
		pinsMu.Unlock()
		return nil
	}
	if err := validateDestination(u); err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: proxy.ProxyFunc(mode), DisableCompression: true, MaxResponseHeaderBytes: 64 << 10, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		pinsMu.RLock()
		ips, pinned := pins[strings.ToLower(host)]
		pinsMu.RUnlock()
		if pinned {
			var last error
			for _, ip := range ips {
				conn, e := dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if e == nil {
					return conn, nil
				}
				last = e
			}
			return nil, last
		}
		// Only configured proxy endpoints may reach this branch.
		return dial(ctx, network, address)
	}
	if configure != nil {
		configure(transport)
	}
	defer transport.CloseIdleConnections()
	var redirectErr error
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			redirectErr = fmt.Errorf("package redirect limit exceeded (maximum 5)")
		} else if err := validateDestination(req.URL); err != nil {
			redirectErr = fmt.Errorf("package redirect rejected: %w", err)
		}
		return redirectErr
	}}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid download request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		// Do wraps errors with the URL, which may contain signed query secrets.
		if redirectErr != nil {
			return nil, redirectErr
		}
		return nil, fmt.Errorf("package download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return nil, fmt.Errorf("package response must be successful and uncompressed")
	}
	if resp.ContentLength > MaxBundleBytes {
		return nil, fmt.Errorf("bundle exceeds limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("package body read failed")
	}
	if len(data) > MaxBundleBytes {
		return nil, fmt.Errorf("bundle exceeds limit")
	}
	return data, nil
}
func (m *Manager) InstallRemote(ctx context.Context, input RemoteInput, mode proxy.Mode) (Manifest, error) {
	data, err := DownloadBundle(ctx, input, mode)
	if err != nil {
		return Manifest{}, err
	}
	return m.InstallVerified(data, input)
}
func (m *Manager) InstallVerified(data []byte, input RemoteInput) (Manifest, error) {
	files, provenance, err := VerifyBundle(data, input)
	if err != nil {
		return Manifest{}, err
	}
	return m.install(files, provenance)
}
