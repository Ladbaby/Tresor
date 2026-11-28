package config

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"tresor/internal/proxy"
)

// QuotaCfg declares how one authenticated GET reads a provider's remaining
// balance and rate-limit windows.
//
// It carries no credential of its own: the request uses the downstream's
// existing auth block (an API key, or an OAuth token resolved at fetch time),
// exactly as fetching a model list does. A provider that cannot be read this way
// simply has no quota block, and Tresor then shows nothing about its allowance —
// no error, no warning, no empty card.
//
// Resolution happens in internal/quota; this type only declares what to ask and
// where in the reply to look.
type QuotaCfg struct {
	// URL is the quota endpoint: either an absolute http(s) URL, or a
	// root-relative path ("/api/user/self") resolved against the origin of the
	// downstream's effective base URL. The relative form keeps a region install
	// on its own host.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`

	// Balance names the remaining balance in the reply. Empty means the provider
	// reports windows only.
	Balance QuotaPath `yaml:"balance,omitempty" json:"balance,omitempty"`

	// Currency labels a monetary balance ("CNY", "USD"), shown with its sign.
	Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`

	// Scale divides the resolved balance before display. It exists because some
	// relays quote quota in their own units (a new-api relay counts 1 unit as
	// 500000 of a dollar); the divisor is a constant and never an expression, so
	// the path grammar stays closed.
	Scale float64 `yaml:"scale,omitempty" json:"scale,omitempty"`

	// Windows are rolling allowances shown as meters.
	Windows []QuotaWindowCfg `yaml:"windows,omitempty" json:"windows,omitempty"`
}

// QuotaWindowCfg is one rolling allowance of a provider's plan.
type QuotaWindowCfg struct {
	// Name is the window's label: "5h", "week", "video / day".
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Used names how much of the window is spent. For a percentage window it is
	// 0-100 points; for an amount window it is the count used.
	Used QuotaPath `yaml:"used,omitempty" json:"used,omitempty"`
	// Limit is an amount window's total ("4 / 5 videos"). Optional; a null at
	// this path means the allowance is uncapped.
	Limit QuotaPath `yaml:"limit,omitempty" json:"limit,omitempty"`
	// ResetsAt names when the window starts again: Unix seconds, Unix
	// milliseconds, or an RFC 3339 string.
	ResetsAt QuotaPath `yaml:"resets_at,omitempty" json:"resets_at,omitempty"`
	// Invert says the vendor reports what is LEFT rather than what is used, so
	// the share spent is 100 minus the reading (percent) or limit minus the
	// reading (amount).
	Invert bool `yaml:"invert,omitempty" json:"invert,omitempty"`
	// Unit labels an amount window's count: "requests", "videos".
	Unit string `yaml:"unit,omitempty" json:"unit,omitempty"`
}

// QuotaPath is one closed path expression: a dotted route into a JSON reply,
// optionally marked as money ("$") or a percentage ("%").
//
//	balance_infos.0.total_balance     a bare number, or a string holding one
//	$data.balance                     money, rendered with its currency sign
//	current_interval_used_percent%    0-100 percentage points
//
// The grammar is deliberately tiny and closed: no arithmetic, no functions, no
// wildcards, no filters. A declaration is data that a catalog can serve, so it
// must not be able to evaluate an expression. Parsing happens at declaration
// time with ParseQuotaPath and evaluation at fetch time in internal/quota.
type QuotaPath string

// QuotaPathKind is how a parsed path's value is read and shown.
type QuotaPathKind int

const (
	// QuotaPlain is a bare number (or a string passed through as-is).
	QuotaPlain QuotaPathKind = iota
	// QuotaCurrency is money, marked with a leading "$".
	QuotaCurrency
	// QuotaPercent is a share in percentage points, 0-100, marked with a
	// trailing "%". Note this is points, not a 0-1 fraction.
	QuotaPercent
)

// ParsedQuotaPath is a QuotaPath split into the marker it carries and the route
// to walk.
type ParsedQuotaPath struct {
	Kind QuotaPathKind
	// Segments is the dotted route. A segment of all digits indexes a list;
	// otherwise (or when the container is an object) it is a key.
	Segments []string
}

// ErrQuotaPathEmpty is returned for an unset path.
var ErrQuotaPathEmpty = errors.New("path is empty")

// quotaPathArithmetic explains the one thing users reach for that this grammar
// refuses, so a rejected path teaches the right fix.
const quotaPathArithmetic = "arithmetic is not supported; use scale for a fixed divisor, or declare an amount window with limit"

// ParseQuotaPath parses one quota path expression. It is the single definition of
// the grammar: validation here and evaluation in internal/quota both go through
// it, so the two can never disagree about what a path means.
func ParseQuotaPath(src string) (ParsedQuotaPath, error) {
	if strings.TrimSpace(src) == "" {
		return ParsedQuotaPath{}, ErrQuotaPathEmpty
	}
	if err := checkQuotaPathChars(src); err != nil {
		return ParsedQuotaPath{}, err
	}

	kind, rest := QuotaPlain, src
	if strings.HasPrefix(rest, "$") {
		kind, rest = QuotaCurrency, rest[1:]
	}
	if strings.HasSuffix(rest, "%") {
		if kind == QuotaCurrency {
			return ParsedQuotaPath{}, fmt.Errorf("path %q carries both a currency and a percentage marker", src)
		}
		kind, rest = QuotaPercent, rest[:len(rest)-1]
	}
	if rest == "" {
		return ParsedQuotaPath{}, fmt.Errorf("path %q has no key after its marker", src)
	}
	// A marker anywhere but its one legal position is a typo, not a path.
	if strings.ContainsAny(rest, "$%") {
		if strings.Contains(rest, "$") && strings.Contains(rest, "%") {
			return ParsedQuotaPath{}, fmt.Errorf("path %q carries both a currency and a percentage marker", src)
		}
		return ParsedQuotaPath{}, fmt.Errorf(
			"path %q may carry only one marker, at the start ($, money) or the end (%%, percentage)", src)
	}

	segments := strings.Split(rest, ".")
	for _, seg := range segments {
		if seg == "" {
			return ParsedQuotaPath{}, fmt.Errorf("path %q has an empty segment", src)
		}
		for _, r := range seg {
			if !quotaKeyRune(r) {
				return ParsedQuotaPath{}, fmt.Errorf("path %q has an unsupported character %q in segment %q", src, r, seg)
			}
		}
	}
	return ParsedQuotaPath{Kind: kind, Segments: segments}, nil
}

// checkQuotaPathChars rejects everything outside the grammar before the parser
// tries to interpret it, naming the mistakes worth naming.
//
// Division and addition are singled out because the path users most often reach
// for is a unit conversion ("data.quota / 500000"), and answering "unsupported
// character" would hide the actual fix. A '*' is left as a plain unsupported
// character: it reads as a wildcard far more often than as a multiplication.
func checkQuotaPathChars(src string) error {
	body := strings.TrimSuffix(strings.TrimPrefix(src, "$"), "%")
	if strings.ContainsAny(body, "/+") {
		return fmt.Errorf("path %q: %s", src, quotaPathArithmetic)
	}
	for _, r := range src {
		switch {
		case r > 0x7f:
			return fmt.Errorf("path %q has a non-ASCII character", src)
		case r <= ' ' || r == 0x7f:
			return fmt.Errorf("path %q has whitespace", src)
		case r != '-' && strings.ContainsRune("*()[]{}<>|&=?!~^@#\\\"'`,:;", r):
			return fmt.Errorf("path %q has an unsupported character %q", src, r)
		}
	}
	return nil
}

// quotaKeyRune reports whether r may appear in a path segment: letters, digits,
// underscore and hyphen, which is what real API keys are spelled with.
func quotaKeyRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_', r == '-':
		return true
	}
	return false
}

// Validate reports why this path cannot be used, or nil when it can.
func (p QuotaPath) Validate(field string) error {
	if _, err := ParseQuotaPath(string(p)); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// Kind is the marker this path carries, or QuotaPlain for an unparsable one.
func (p QuotaPath) Kind() QuotaPathKind {
	parsed, err := ParseQuotaPath(string(p))
	if err != nil {
		return QuotaPlain
	}
	return parsed.Kind
}

// IsEmpty reports whether the declaration says nothing at all, which is how the
// API clears one: a `{}` from the UI normalizes to nil.
func (q *QuotaCfg) IsEmpty() bool {
	if q == nil {
		return true
	}
	return q.URL == "" && q.Balance == "" && q.Currency == "" && q.Scale == 0 && len(q.Windows) == 0
}

// Validate reports why this quota declaration cannot be used, or nil when it can.
// field is the dotted prefix the declaration sits at, so an error names its place
// ("provider.quota.windows[1].used").
func (q *QuotaCfg) Validate(field string) error {
	if q == nil {
		return nil
	}
	if q.IsEmpty() {
		return nil
	}
	if _, err := q.ResolveURL("https://example.invalid"); err != nil {
		return fmt.Errorf("%s.url: %w", field, err)
	}
	if q.Balance == "" && len(q.Windows) == 0 {
		return fmt.Errorf("%s: needs a balance or at least one window", field)
	}
	if q.Balance != "" {
		if err := q.Balance.Validate(field + ".balance"); err != nil {
			return err
		}
	}
	// Scale is only ever applied to a balance; silently ignoring it on a
	// windows-only declaration would hide a mistake.
	if q.Scale != 0 {
		if math.IsNaN(q.Scale) || math.IsInf(q.Scale, 0) || q.Scale < 0 {
			return fmt.Errorf("%s.scale: must be a finite positive number", field)
		}
		if q.Balance == "" {
			return fmt.Errorf("%s.scale: divides the balance, which this declaration does not set", field)
		}
	}
	if q.Currency != "" {
		if q.Balance == "" {
			return fmt.Errorf("%s.currency: labels a balance, which this declaration does not set", field)
		}
		if q.Balance.Kind() != QuotaCurrency {
			return fmt.Errorf("%s.currency: needs a balance marked as money (\"$…\")", field)
		}
		if strings.TrimSpace(q.Currency) == "" {
			return fmt.Errorf("%s.currency: must not be blank", field)
		}
	}
	for i := range q.Windows {
		if err := q.Windows[i].Validate(fmt.Sprintf("%s.windows[%d]", field, i)); err != nil {
			return err
		}
	}
	return nil
}

// Validate reports why this window declaration cannot be used, or nil when it can.
func (w *QuotaWindowCfg) Validate(field string) error {
	if strings.TrimSpace(w.Name) == "" {
		return fmt.Errorf("%s.name: a window needs a label", field)
	}
	if w.Used == "" {
		return fmt.Errorf("%s.used: a window needs the value to read", field)
	}
	if err := w.Used.Validate(field + ".used"); err != nil {
		return err
	}
	if w.Limit != "" {
		if err := w.Limit.Validate(field + ".limit"); err != nil {
			return err
		}
		// A share of a share is not a thing anyone means.
		if w.Limit.Kind() == QuotaPercent {
			return fmt.Errorf("%s.limit: must not be a percentage", field)
		}
	}
	if w.ResetsAt != "" {
		if err := w.ResetsAt.Validate(field + ".resets_at"); err != nil {
			return err
		}
	}
	// Inverting a bare number has no defined meaning: there is nothing to
	// subtract it from. A percentage inverts against 100, an amount against its
	// limit.
	if w.Invert && w.Used.Kind() != QuotaPercent && w.Limit == "" {
		return fmt.Errorf("%s.invert: needs a percentage (%%) or a limit to invert against", field)
	}
	return nil
}

// ResolveURL resolves the declaration's URL against baseOrigin, the origin of the
// downstream's effective base URL. An absolute URL is returned as written; a
// root-relative one is joined to baseOrigin so a region install asks its own host.
func (q *QuotaCfg) ResolveURL(baseOrigin string) (string, error) {
	raw := strings.TrimSpace(q.URL)
	if raw == "" {
		return "", errors.New("a quota declaration needs the endpoint to ask")
	}
	if strings.HasPrefix(raw, "/") {
		if strings.HasPrefix(raw, "//") {
			return "", errors.New("must not be protocol-relative (start with //)")
		}
		origin := strings.TrimSuffix(strings.TrimSpace(baseOrigin), "/")
		if origin == "" {
			return "", errors.New("is a relative path but the downstream has no base URL to resolve it against")
		}
		return origin + raw, nil
	}
	if err := proxy.ValidateOutboundURL(raw); err != nil {
		return "", err
	}
	return raw, nil
}
