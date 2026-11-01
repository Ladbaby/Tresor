package engine

import (
	"fmt"
	"net/http"

	"context"
	"encoding/base64"
	"io"
	"net/url"
	"strings"
	"tresor/internal/discovery"
	"tresor/internal/jsplugins"
	"tresor/internal/wire"
)

// dialectAdapter is the wire.Adapter implemented by a plugin package. It replaces
// the compiled-in provider dialects: the package declares its name and policy in
// the manifest, and shapes the request through the wirePrepare hook.
//
// The goja instance is built lazily on first use rather than in the resolver,
// because the resolver is also called for validation (API writes, profile
// installs, startup), where creating a VM would be wasteful.
type dialectAdapter struct {
	pkg     *jsplugins.Package
	decl    *jsplugins.DialectDeclaration
	newInst func() (*jsplugins.Instance, error)
}

func (a *dialectAdapter) Name() string { return a.decl.Name }

// NoReplay and Discovery read the manifest rather than invoking the hook: engine
// policy must be auditable without executing plugin code.
func (a *dialectAdapter) NoReplay() bool { return a.decl.NoReplay }

func (a *dialectAdapter) Discovery() string { return a.decl.Discovery }

// PrepareRequest runs the dialect's hooks once for this request. An instance is
// created here and discarded with the request, so no state carries between
// requests and an installed update cannot change an in-flight one.
func (a *dialectAdapter) PrepareRequest(r wire.Request) wire.Prepared {
	instance, err := a.newInst()
	if err != nil {
		return wire.Prepared{Err: fmt.Errorf("wire %s: %w", a.decl.Name, err)}
	}
	defer instance.ReleaseBuffers()

	body := map[string]any{"encoding": "utf8", "data": string(r.Body)}
	input := map[string]any{
		"body":    body,
		"request": requestHeaders(r.Original),
	}
	result, err := instance.Invoke("wirePrepare", input)
	if err != nil {
		return wire.Prepared{Err: fmt.Errorf("wire %s: %w", a.decl.Name, err)}
	}

	out := wire.Prepared{Body: r.Body}
	// A hook that returns no body leaves the request untouched, so a dialect that
	// only adds headers does not have to echo the body back. Binary output (a
	// compressed body) arrives base64-encoded, exactly like every other hook body.
	if encoded, ok := result["body"].(map[string]any); ok {
		data, _ := encoded["data"].(string)
		switch encoded["encoding"] {
		case "base64":
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				return wire.Prepared{Err: fmt.Errorf("wire %s: invalid base64 body: %w", a.decl.Name, err)}
			}
			out.Body = decoded
		case "utf8", "":
			out.Body = []byte(data)
		default:
			return wire.Prepared{Err: fmt.Errorf("wire %s: unknown body encoding %v", a.decl.Name, encoded["encoding"])}
		}
	}
	// An empty value means "remove this header", which a dialect needs for a
	// now-incorrect Content-Length after re-encoding a body. Ordinary hooks cannot
	// delete a header through a value map, so the convention lives here.
	if headers, ok := result["headers"].(map[string]any); ok && len(headers) > 0 {
		applied := make(map[string]string, len(headers))
		for k, v := range headers {
			if s, ok := v.(string); ok {
				applied[k] = s
			}
		}
		out.ApplyHeaders = func(req *http.Request) {
			for k, v := range applied {
				if v == "" {
					req.Header.Del(k)
					continue
				}
				req.Header.Set(k, v)
			}
		}
	}
	return out
}

// requestHeaders exposes the client's headers to the dialect, which needs them to
// decide things like a conversation identity. Host credentials are not included:
// the dialect has no business reading what the engine attached.
func requestHeaders(original *http.Request) map[string]any {
	if original == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(original.Header))
	for name, values := range original.Header {
		if protectedHeader(name, nil) {
			continue
		}
		copied := make([]any, 0, len(values))
		for _, v := range values {
			copied = append(copied, v)
		}
		out[name] = copied
	}
	return out
}

// dialectSource resolves plugin-provided dialects so wire.For can find them. It
// reads the manager's execution snapshot, which is what pins one package version
// for the life of a request.
type dialectSource struct {
	snapshot func() map[string]*jsplugins.Package
}

func (s dialectSource) For(name string) (wire.Adapter, bool) {
	for _, pkg := range s.snapshot() {
		if pkg == nil || pkg.Manifest.Dialect == nil || pkg.Manifest.Dialect.Name != name {
			continue
		}
		pkg := pkg
		return &dialectAdapter{
			pkg:  pkg,
			decl: pkg.Manifest.Dialect,
			newInst: func() (*jsplugins.Instance, error) {
				return jsplugins.NewInstance(pkg, nil)
			},
		}, true
	}
	return nil, false
}

func (s dialectSource) Names() []string {
	var out []string
	for _, pkg := range s.snapshot() {
		if pkg != nil && pkg.Manifest.Dialect != nil {
			out = append(out, pkg.Manifest.Dialect.Name)
		}
	}
	return out
}

// NewDialectSource resolves dialect names through the plugin manager's execution
// snapshot, so a request keeps the package version it started with even if an
// install or activation happens mid-flight.
func NewDialectSource(manager interface {
	ExecutionSnapshot() (map[string]*jsplugins.Package, map[string]string)
}) wire.Source {
	return dialectSource{snapshot: func() map[string]*jsplugins.Package {
		packages, _ := manager.ExecutionSnapshot()
		return packages
	}}
}

// NewDialectAdapterForTest builds an adapter around a package. Exported for
// tests in this package that need to exercise the engine path without a Manager.
func NewDialectAdapterForTest(pkg *jsplugins.Package) wire.Adapter {
	p := pkg
	return &dialectAdapter{pkg: p, decl: p.Manifest.Dialect, newInst: func() (*jsplugins.Instance, error) {
		return jsplugins.NewInstance(p, nil)
	}}
}

// DiscoverModels asks the provider for its model list using the dialect's own
// protocol. The host performs the request so no package gains network access;
// the dialect supplies the path, query, identity headers, and the parsing.
func (a *dialectAdapter) DiscoverModels(ctx context.Context, baseURL, token string, extraHeaders map[string]string) ([]string, error) {
	instance, err := a.newInst()
	if err != nil {
		return nil, fmt.Errorf("wire %s: %w", a.decl.Name, err)
	}
	defer instance.ReleaseBuffers()

	plan, err := instance.Invoke("wireDiscover", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("wire %s discovery: %w", a.decl.Name, err)
	}
	path, _ := plan["path"].(string)
	if path == "" {
		path = "/models"
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + path
	if query, ok := plan["query"].(map[string]any); ok && len(query) > 0 {
		values := url.Values{}
		for k, v := range query {
			if s, ok := v.(string); ok && s != "" {
				values.Set(k, s)
			}
		}
		if encoded := values.Encode(); encoded != "" {
			endpoint += "?" + encoded
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("wire %s discovery: %w", a.decl.Name, err)
	}
	// The runtime's own identity headers (from the token manager) come first, so
	// the dialect's declared identity wins where they overlap.
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if headers, ok := plan["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok && s != "" {
				req.Header.Set(k, s)
			}
		}
	}

	resp, err := discoveryHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wire %s discovery: %w", a.decl.Name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	parsed, err := instance.Invoke("wireParseModels", map[string]any{
		"response": map[string]any{"status": resp.StatusCode, "body": string(body)},
	})
	if err != nil {
		return nil, fmt.Errorf("wire %s discovery: %w", a.decl.Name, err)
	}
	raw, _ := parsed["models"].([]any)
	models := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			models = append(models, s)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("wire %s discovery returned no models", a.decl.Name)
	}
	return models, nil
}

// discoveryHTTPClient performs a dialect's model-list request. The base URL is
// already validated when the downstream is saved.
var discoveryHTTPClient = &http.Client{Timeout: discovery.Timeout}
