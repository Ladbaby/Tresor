package integrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Operation names are the only operations accepted by the named-service transport.
type Operation string

const (
	Prepare     Operation = "prepare"
	Invoke      Operation = "invoke"
	ExecuteTool Operation = "execute-tool"
)

var allowedOperations = map[Operation]bool{Prepare: true, Invoke: true, ExecuteTool: true}

// Service is a host-owned endpoint binding. Credentials are named by environment variable, never stored here.
type Service struct {
	BaseURL              string               `yaml:"base_url" json:"base_url"`
	CredentialEnv        string               `yaml:"credential_env,omitempty" json:"credential_env,omitempty"`
	ControlCredentialEnv string               `yaml:"control_credential_env,omitempty" json:"control_credential_env,omitempty"`
	Operations           map[Operation]string `yaml:"operations" json:"operations"`
}

// Grant scopes a plugin source digest to specific named service operations.
type Grant struct {
	PluginID     string      `yaml:"plugin_id" json:"plugin_id"`
	SourceDigest string      `yaml:"source_digest" json:"source_digest"`
	Service      string      `yaml:"service" json:"service"`
	Operations   []Operation `yaml:"operations" json:"operations"`
}

// ValidateURL restricts remote services to HTTPS and permits HTTP only on loopback.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid service base_url: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("service base_url must not contain credentials, query, or fragment")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("service base_url scheme must be https or loopback http")
	}
	if u.Hostname() == "" || u.Port() != "" && !validPort(u.Port()) || strings.ContainsAny(u.Host, "\\\\%") {
		return errors.New("service base_url requires a valid host and port")
	}
	if strings.ContainsAny(raw, "\r\n\t\x00") {
		return errors.New("service base_url contains control characters")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return errors.New("HTTP service base_url is allowed only for loopback hosts")
	}
	return nil
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}
func validEnvName(s string) bool {
	if s == "" {
		return true
	}
	if !((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z') || s[0] == '_') {
		return false
	}
	for _, r := range s[1:] {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// Validate checks the complete service binding including fixed operation paths.
func (s Service) Validate() error {
	if err := ValidateURL(s.BaseURL); err != nil {
		return err
	}
	if !validEnvName(s.CredentialEnv) || !validEnvName(s.ControlCredentialEnv) {
		return errors.New("service credential environment name is invalid")
	}
	if len(s.Operations) == 0 {
		return errors.New("service operations must be configured")
	}
	for op, p := range s.Operations {
		if !allowedOperations[op] {
			return fmt.Errorf("unsupported service operation %q", op)
		}
		if p == "" || p[0] != '/' || strings.ContainsAny(p, "\\?#%") || strings.Contains(p, "//") || strings.ContainsAny(p, "\r\n\t\x00") {
			return fmt.Errorf("operation %q path must be a fixed absolute path", op)
		}
		for _, segment := range strings.Split(p, "/") {
			if segment == "." || segment == ".." {
				return fmt.Errorf("operation %q path contains a dot segment", op)
			}
		}
	}
	return nil
}

// ValidateGrant verifies canonical SHA-256 digest and operation scope.
func (g Grant) Validate() error {
	parts := strings.Split(g.PluginID, "/")
	validPlugin := len(parts) <= 2
	for _, part := range parts {
		validPlugin = validPlugin && validName(part)
	}
	if !validPlugin || !validName(g.Service) {
		return errors.New("grant requires valid plugin_id and service names")
	}
	d := g.SourceDigest
	b, err := hex.DecodeString(d)
	if err != nil || len(b) != sha256.Size || strings.ToLower(d) != d {
		return errors.New("grant source_digest must be canonical lowercase SHA-256 hex")
	}
	if len(g.Operations) == 0 {
		return errors.New("grant operations must not be empty")
	}
	seen := map[Operation]bool{}
	for _, op := range g.Operations {
		if !allowedOperations[op] || seen[op] {
			return fmt.Errorf("invalid or duplicate grant operation %q", op)
		}
		seen[op] = true
	}
	return nil
}

// Envelope is the versioned JSON request and response wrapper.
type Envelope struct {
	Version string          `json:"version"`
	Payload json.RawMessage `json:"payload"`
}

const EnvelopeVersion = "integration.v1"

func cloneOperations(in map[Operation]string) map[Operation]string {
	out := make(map[Operation]string, len(in))
	for op, endpoint := range in {
		out[op] = endpoint
	}
	return out
}

// Snapshot constructs a host client with credentials captured exactly once.
type Snapshot struct {
	service                       Service
	client                        *http.Client
	credential, controlCredential string
	maxResponse                   int64
}

// NewClient validates and snapshots credentials; invoke calls have no whole-response timeout.
func NewClient(service Service, maxResponse int64) (*Snapshot, error) {
	return NewConfiguredClient(service, os.Getenv(service.CredentialEnv), os.Getenv(service.ControlCredentialEnv), maxResponse)
}

// NewConfiguredClient snapshots credentials supplied by a configured rule step.
func NewConfiguredClient(service Service, credential, controlCredential string, maxResponse int64) (*Snapshot, error) {
	if err := service.Validate(); err != nil {
		return nil, err
	}
	service.Operations = cloneOperations(service.Operations)
	if maxResponse <= 0 {
		maxResponse = 8 << 20
	}
	if strings.ContainsAny(credential+controlCredential, "\r\n") {
		return nil, errors.New("integration credential contains invalid characters")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Snapshot{service: service, client: client, credential: credential, controlCredential: controlCredential, maxResponse: maxResponse}, nil
}

// BindingIdentity identifies endpoint configuration without credentials.
func (s *Snapshot) BindingIdentity() string { b, _ := json.Marshal(s.service); return Digest(b) }

// Credential returns the captured service credential for host-owned authentication.
func (s *Snapshot) Credential() string { return s.credential }

// ControlCredential returns the separately captured control-plane credential.
func (s *Snapshot) ControlCredential() string { return s.controlCredential }

// Do sends a versioned JSON operation request. Only fixed configured endpoints are callable.
func (s *Snapshot) Do(ctx context.Context, op Operation, payload any) (*http.Response, []byte, error) {
	endpoint, ok := s.service.Operations[op]
	if !ok || !allowedOperations[op] {
		return nil, nil, fmt.Errorf("operation %q is not configured", op)
	}
	if s.service.CredentialEnv != "" && strings.TrimSpace(s.credential) == "" {
		return nil, nil, errors.New("configured service credential is unavailable")
	}
	if s.credential != "" && strings.ContainsAny(s.credential, "\r\n") {
		return nil, nil, errors.New("integration credential contains invalid characters")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	wrapped, err := json.Marshal(Envelope{Version: EnvelopeVersion, Payload: body})
	if err != nil {
		return nil, nil, err
	}
	const maxRequest = 8 << 20
	if len(wrapped) > maxRequest {
		return nil, nil, errors.New("integration request exceeds configured limit")
	}
	base, _ := url.Parse(s.service.BaseURL)
	target := *base
	target.Path = strings.TrimSuffix(base.Path, "/") + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(string(wrapped)))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.credential != "" {
		req.Header.Set("Authorization", "Bearer "+s.credential)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return resp, nil, errors.New("integration redirects are not accepted")
	}
	if op == Invoke && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return resp, nil, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.maxResponse+1))
	if err != nil {
		return resp, nil, err
	}
	if int64(len(data)) > s.maxResponse {
		return resp, nil, errors.New("integration response exceeds configured limit")
	}
	if op != Invoke {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
			return resp, nil, errors.New("integration response requires JSON")
		}
		var envelope Envelope
		if err := json.Unmarshal(data, &envelope); err != nil || envelope.Version != EnvelopeVersion || len(envelope.Payload) == 0 || !json.Valid(envelope.Payload) {
			return resp, nil, errors.New("invalid integration response envelope")
		}
	}
	return resp, data, nil
}

// Digest returns the canonical source digest representation used by grants.
func Digest(source []byte) string { sum := sha256.Sum256(source); return hex.EncodeToString(sum[:]) }
