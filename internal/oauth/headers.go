package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"

	"tresor/internal/config"
)

// This file resolves a downstream's declarative extra_headers into the concrete
// header map sent upstream. It is the single place where a header value becomes a
// string, so a provider's wire shaping is described in config and nowhere else.
//
// Resolution happens against the FINAL access token, after any refresh: a claim
// copied into a header (an account id, a tenant) must follow a rotated token
// instead of going stale.

// Template variable values. The set is closed (config.TemplateVars); these are
// the only substitutions a template may use, so a catalog-supplied template can
// never read the environment or evaluate an expression.
var (
	hostOnce      sync.Once
	hostOS        string
	hostArch      string
	hostOSRelease string
)

func hostVars() (os, arch, release string) {
	hostOnce.Do(func() {
		hostOS = runtime.GOOS
		hostArch = runtime.GOARCH
		hostOSRelease = osRelease()
	})
	return hostOS, hostArch, hostOSRelease
}

// ResolveHeaderValues turns declarative header values into a flat header map.
//
// Precedence matches the pre-refactor behavior: a value already present in
// `base` wins, so an operator's static override is never clobbered by a claim.
// A from_claim entry whose claim is absent contributes nothing, rather than an
// empty header — the same rule that kept non-ChatGPT tokens from gaining an empty
// chatgpt-account-id.
func ResolveHeaderValues(values map[string]config.HeaderValue, accessToken string, base map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(base)+len(values))
	for k, v := range base {
		out[k] = v
	}
	if len(values) == 0 {
		return out, nil
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic, and keeps error messages stable

	for _, name := range names {
		if _, exists := out[name]; exists {
			continue // operator override wins
		}
		value, ok, err := resolveHeaderValue(values[name], accessToken)
		if err != nil {
			return nil, fmt.Errorf("extra_headers[%q]: %w", name, err)
		}
		if ok {
			out[name] = value
		}
	}
	return out, nil
}

// resolveHeaderValue renders one value. ok is false when the value legitimately
// produces no header (an absent claim).
func resolveHeaderValue(v config.HeaderValue, accessToken string) (string, bool, error) {
	switch v.Form() {
	case config.HeaderLiteral:
		return v.Literal, v.Literal != "", nil
	case config.HeaderTemplate:
		s, err := renderTemplate(v.Template)
		return s, err == nil, err
	case config.HeaderFromClaim:
		// A token this code cannot read is not an error: the value simply
		// contributes nothing. Treating it as an error would discard every other
		// declared header, so an api_key relay that declares one claim-based header
		// alongside a literal identity header would silently lose the literal too.
		// Opaque (non-JWT) access tokens are a real class, not a mistake.
		if accessToken == "" {
			return "", false, nil
		}
		claims, err := tokenClaims(accessToken)
		if err != nil {
			return "", false, nil
		}
		s, ok := lookupClaim(claims, v.FromClaim)
		return s, ok, nil
	default:
		return "", false, fmt.Errorf("set exactly one of literal, from_claim or template")
	}
}

// renderTemplate substitutes the closed variable set. An unknown variable is an
// error rather than a literal brace, so a typo surfaces instead of shipping.
func renderTemplate(tmpl string) (string, error) {
	osName, arch, release := hostVars()
	replacer := strings.NewReplacer(
		"{os}", osName,
		"{arch}", arch,
		"{os_release}", release,
	)
	out := replacer.Replace(tmpl)
	if strings.ContainsAny(out, "{}") {
		return "", fmt.Errorf("template has an unresolved variable: %q", tmpl)
	}
	return out, nil
}

// tokenClaims decodes the JWT payload of an access token. It does not verify the
// signature: the token came from the provider's own token endpoint and is used
// only to copy a claim into a header, never to make a trust decision.
func tokenClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("access token is not a JWT; cannot read a claim from it")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		// Some tokens pad the segment; retry with the standard decoder.
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil, fmt.Errorf("access token payload is not base64url")
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("access token payload is not a JSON claims object")
	}
	return claims, nil
}

// lookupClaim finds a claim by path.
//
// A dotted path descends from the root. A single-segment path is first looked up
// at the root and then searched for at any depth, because OAuth tokens commonly
// nest claims under a namespaced URL key: OpenAI keeps chatgpt_account_id under
// "https://api.openai.com/auth". Searching a unique leaf keeps the common
// declaration short without needing the URL in config.
func lookupClaim(claims map[string]any, path string) (string, bool) {
	if v, ok := claimAt(claims, path); ok {
		return v, true
	}
	if !strings.Contains(path, config.ClaimPathSeparator) {
		return findClaim(claims, path, 0)
	}
	return "", false
}

// claimAt resolves a dotted path from the root.
//
// A claim name may itself contain dots: the conventional namespaced form is a URL,
// so the canonical ChatGPT claim is the key "https://api.openai.com/auth" holding
// "chatgpt_account_id", reached by the path
// "https://api.openai.com/auth.chatgpt_account_id". Splitting that path naively on
// dots can never match, so every split is tried from the longest possible root key
// down. Both a URL-qualified path and a plain "auth.chatgpt_account_id" resolve.
func claimAt(claims map[string]any, path string) (string, bool) {
	segs := strings.Split(path, config.ClaimPathSeparator)
	for i := len(segs); i >= 1; i-- {
		head := strings.Join(segs[:i], config.ClaimPathSeparator)
		raw, ok := claims[head]
		if !ok {
			continue
		}
		cur := raw
		for _, seg := range segs[i:] {
			m, ok := cur.(map[string]any)
			if !ok {
				return "", false
			}
			cur, ok = m[seg]
			if !ok {
				return "", false
			}
		}
		return claimString(cur)
	}
	return "", false
}

// claimMaxDepth bounds the depth-first search so a pathological token cannot make
// resolution expensive.
const claimMaxDepth = 8

// findClaim searches for a leaf key at any depth. A value found more than once is
// ambiguous, and ambiguity is reported as "not found" so we never guess which
// account id to send.
func findClaim(node any, leaf string, depth int) (string, bool) {
	if depth > claimMaxDepth {
		return "", false
	}
	m, ok := node.(map[string]any)
	if !ok {
		return "", false
	}
	if raw, ok := m[leaf]; ok {
		if s, ok := claimString(raw); ok {
			return s, true
		}
	}
	found := ""
	count := 0
	for _, raw := range m {
		if s, ok := findClaim(raw, leaf, depth+1); ok {
			found = s
			count++
		}
	}
	if count == 1 {
		return found, true
	}
	return "", false
}

// claimString accepts only string claims that are safe to put in a header. A
// numeric or object claim is not silently stringified.
func claimString(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", false // never allow header injection from token contents
	}
	return s, true
}
