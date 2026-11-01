package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Header value forms. Exactly one is set on a HeaderValue.
const (
	// HeaderLiteral is a fixed string, sent as-is. A bare YAML string is this
	// form, so existing `extra_headers: {X-Foo: bar}` configs keep working.
	HeaderLiteral = "literal"
	// HeaderFromClaim copies a value out of the access token's JWT claims, so a
	// header derived from the token (an account id, a tenant) follows a refresh
	// instead of going stale.
	HeaderFromClaim = "from_claim"
	// HeaderTemplate is filled from a closed set of host variables. It exists so
	// a provider that gates on client identity can be described without a
	// provider-specific Go function.
	HeaderTemplate = "template"
)

// TemplateVars are the only variables a template may reference. The set is
// closed on purpose: templates are data served by a catalog, so they must not be
// able to read the environment or evaluate expressions.
var TemplateVars = []string{"os", "os_release", "arch"}

// HeaderValue is one entry of a downstream's extra_headers map.
//
// It accepts three shapes in YAML and JSON:
//
//	X-Api-Version: "2024-01"              # literal (bare string)
//	originator: pi                        # literal
//	chatgpt-account-id:
//	  from_claim: "https://api.openai.com/auth.chatgpt_account_id"
//	User-Agent:
//	  template: "pi ({os} {os_release}; {arch})"
//
// Resolution to an actual string happens in internal/oauth, which is where the
// token and the host identity are known.
type HeaderValue struct {
	Literal   string `yaml:"literal,omitempty" json:"literal,omitempty"`
	FromClaim string `yaml:"from_claim,omitempty" json:"from_claim,omitempty"`
	Template  string `yaml:"template,omitempty" json:"template,omitempty"`
}

// Form reports which single form is set: HeaderLiteral, HeaderFromClaim,
// HeaderTemplate, or "" when none is.
func (h HeaderValue) Form() string {
	set := 0
	form := ""
	if h.Literal != "" {
		set, form = set+1, HeaderLiteral
	}
	if h.FromClaim != "" {
		set, form = set+1, HeaderFromClaim
	}
	if h.Template != "" {
		set, form = set+1, HeaderTemplate
	}
	if set != 1 {
		return ""
	}
	return form
}

// Validate reports why this value cannot be used, or nil when it can.
func (h HeaderValue) Validate(name string) error {
	switch h.Form() {
	case HeaderLiteral:
		return nil
	case HeaderFromClaim:
		if err := validateClaimPath(h.FromClaim); err != nil {
			return fmt.Errorf("extra_headers[%q].from_claim: %w", name, err)
		}
		return nil
	case HeaderTemplate:
		return validateHeaderTemplate(name, h.Template)
	default:
		return fmt.Errorf("extra_headers[%q]: set exactly one of literal, from_claim or template", name)
	}
}

// ClaimPathSeparator separates the segments of a from_claim path.
const ClaimPathSeparator = "."

// validateClaimPath checks a from_claim path.
//
// A path is either a dotted route from the claim root ("auth.tenant.id") or a
// single leaf name ("chatgpt_account_id"). The leaf form is what most providers
// need, because OAuth tokens routinely nest claims under a namespaced URL key
// that would be unpleasant to spell: OpenAI keeps chatgpt_account_id under
// "https://api.openai.com/auth". A single-segment path is therefore resolved by
// searching for that key at any depth; see oauth.ResolveHeaderValues.
func validateClaimPath(path string) error {
	segs := strings.Split(path, ClaimPathSeparator)
	if len(segs) == 1 && strings.TrimSpace(segs[0]) == "" {
		return fmt.Errorf("claim path is empty")
	}
	for _, seg := range segs {
		if seg == "" {
			return fmt.Errorf("claim path %q has an empty segment", path)
		}
		for _, r := range seg {
			if r <= ' ' || r == 0x7f {
				return fmt.Errorf("claim path %q has a segment with whitespace", path)
			}
		}
	}
	return nil
}

// validateHeaderTemplate checks that every {placeholder} is a known variable and
// that no other braces appear, so a template cannot smuggle in an expression.
func validateHeaderTemplate(name, tmpl string) error {
	rest := tmpl
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			break
		}
		shut := strings.IndexByte(rest[open:], '}')
		if shut < 0 {
			return fmt.Errorf("extra_headers[%q].template: unclosed %q", name, rest[open:])
		}
		varName := rest[open+1 : open+shut]
		if !containsString(TemplateVars, varName) {
			return fmt.Errorf("extra_headers[%q].template: unknown variable %q (allowed: %s)",
				name, varName, strings.Join(TemplateVars, ", "))
		}
		rest = rest[open+shut+1:]
	}
	if strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("extra_headers[%q].template: stray brace", name)
	}
	return nil
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ValidateHeaderValues checks a whole extra_headers map and returns a
// deterministic error (names sorted) so failures are reproducible.
func ValidateHeaderValues(headers map[string]HeaderValue) error {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("extra_headers: a header name is empty")
		}
		if err := headers[name].Validate(name); err != nil {
			return err
		}
	}
	return nil
}

// UnmarshalYAML accepts either a bare scalar (a literal value) or a mapping with
// one of literal/from_claim/template.
func (h *HeaderValue) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*h = HeaderValue{Literal: s}
		return nil
	case yaml.MappingNode:
		type plain HeaderValue
		var p plain
		if err := node.Decode(&p); err != nil {
			return err
		}
		*h = HeaderValue(p)
		return nil
	default:
		return fmt.Errorf("extra header value must be a string or a mapping, got %s", yamlKindName(node.Kind))
	}
}

// MarshalYAML writes a literal as a bare scalar so round-tripping an existing
// config produces the same YAML it came from.
func (h HeaderValue) MarshalYAML() (any, error) {
	if h.Form() == HeaderLiteral {
		return h.Literal, nil
	}
	type plain HeaderValue
	return plain(h), nil
}

// UnmarshalJSON mirrors UnmarshalYAML.
func (h *HeaderValue) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		*h = HeaderValue{}
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*h = HeaderValue{Literal: s}
		return nil
	}
	type plain HeaderValue
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*h = HeaderValue(p)
	return nil
}

// MarshalJSON mirrors MarshalYAML.
func (h HeaderValue) MarshalJSON() ([]byte, error) {
	if h.Form() == HeaderLiteral {
		return json.Marshal(h.Literal)
	}
	type plain HeaderValue
	return json.Marshal(plain(h))
}

func yamlKindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "an unsupported node"
	}
}
