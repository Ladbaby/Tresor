package quota

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"tresor/internal/config"
)

// Balance is a provider's remaining money or credits.
type Balance struct {
	Value    float64 `json:"value"`
	Currency string  `json:"currency,omitempty"`
	// Text is the figure as it is shown, with the currency's sign: "¥110.00",
	// "12.40 credits". Rendered here so the browser does no formatting.
	Text string `json:"text"`
}

// Window is one rolling allowance.
type Window struct {
	Name string `json:"name"`
	// Text is the window as it is shown: "42% used · resets 14:30",
	// "4 / 5 videos".
	Text string `json:"text"`
	// Meter is how much of the window is spent, 0 to 1, for a progress bar.
	// Absent for a window whose share cannot be told.
	Meter *float64 `json:"meter,omitempty"`
	// UsedPercent is the share spent in percentage points, when the window is a
	// percentage one.
	UsedPercent *float64 `json:"used_percent,omitempty"`
	// Used and Limit are the counts of an amount window, and Unit their label.
	Used  *float64 `json:"used,omitempty"`
	Limit *float64 `json:"limit,omitempty"`
	Unit  string   `json:"unit,omitempty"`
	// ResetsAt is when the window starts again, nil when the vendor does not say.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// Reading is one provider's allowance as of a moment.
type Reading struct {
	// FetchedAt is when the reply this reading came from was read.
	FetchedAt time.Time `json:"fetched_at"`
	// Stale reports that the reading is older than the refresh interval: it is
	// still shown, and a refresh is on its way.
	Stale   bool     `json:"stale,omitempty"`
	Balance *Balance `json:"balance,omitempty"`
	Windows []Window `json:"windows,omitempty"`
}

// currencySigns maps the currency codes worth showing as a glyph.
var currencySigns = map[string]string{
	"USD": "$", "CNY": "¥", "RMB": "¥", "EUR": "€", "GBP": "£", "JPY": "¥",
}

// Build turns a raw JSON reply into a reading by the declaration that asked for
// it. It reports false when the reply yields nothing worth showing — no balance
// and no window — which is how a provider that answered "no plan" quietly stays
// hidden rather than appearing as an empty card.
func Build(cfg *config.QuotaCfg, body []byte, now time.Time) (Reading, bool) {
	if cfg == nil || cfg.IsEmpty() {
		return Reading{}, false
	}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return Reading{}, false
	}
	out := Reading{FetchedAt: now}
	out.Balance = buildBalance(cfg, root, body)
	out.Windows = buildWindows(cfg, root, now)
	if out.Balance == nil && len(out.Windows) == 0 {
		return Reading{}, false
	}
	return out, true
}

// buildBalance reads the declaration's balance, or nil when the reply does not
// carry one.
func buildBalance(cfg *config.QuotaCfg, root any, body []byte) *Balance {
	if cfg.Balance == "" {
		return nil
	}
	parsed, err := config.ParseQuotaPath(string(cfg.Balance))
	if err != nil {
		return nil
	}
	v, state := Eval(parsed, root)
	if state != Present {
		return nil
	}
	// A vendor that reports its balance as prose is shown as prose: there is no
	// number to divide or sign.
	if s, ok := Text(v); ok && parsed.Kind == config.QuotaPlain {
		if _, isNumber := Number(v); !isNumber {
			return &Balance{Text: s}
		}
	}
	n, ok := Number(v)
	if !ok {
		return nil
	}
	if cfg.Scale > 0 {
		n /= cfg.Scale
	}
	currency := strings.ToUpper(strings.TrimSpace(cfg.Currency))
	if currency == "" {
		// A declaration may leave the currency out — most vendors state it in the
		// reply anyway, beside the amount or once at the root. Reading it here
		// means a recipe does not have to guess, and a vendor that varies by
		// account (CNY for one user, USD for another) still shows the right sign.
		currency = siblingCurrency(root, string(cfg.Balance))
	}
	b := &Balance{Value: n, Currency: currency}
	sign := currencySigns[b.Currency]
	b.Text = groupedNumber(n, sign)
	return b
}

// groupedNumber renders an amount with thousands separators and, when there is
// one, the currency's sign: "¥1,234.50", "6,291".
func groupedNumber(n float64, sign string) string {
	rounded := math.Round(n*100) / 100
	s := strconv.FormatFloat(rounded, 'f', -1, 64)
	whole, frac, _ := strings.Cut(s, ".")
	neg := strings.HasPrefix(whole, "-")
	whole = strings.TrimPrefix(whole, "-")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if neg {
		whole = "-" + whole
	}
	if frac != "" {
		whole += "." + frac
	}
	return sign + whole
}

// buildWindows reads each declared window, dropping the ones the reply does not
// carry: a window a vendor omits is one the account does not have, not one at
// zero.
func buildWindows(cfg *config.QuotaCfg, root any, now time.Time) []Window {
	var out []Window
	for i := range cfg.Windows {
		if w, ok := buildWindow(&cfg.Windows[i], root, now); ok {
			out = append(out, w)
		}
	}
	return out
}

// buildWindow reads one window declaration against the reply.
func buildWindow(cfg *config.QuotaWindowCfg, root any, now time.Time) (Window, bool) {
	out := Window{Name: strings.TrimSpace(cfg.Name), Unit: strings.TrimSpace(cfg.Unit)}

	usedPath, err := config.ParseQuotaPath(string(cfg.Used))
	if err != nil {
		return Window{}, false
	}
	raw, state := Eval(usedPath, root)
	if state != Present {
		return Window{}, false
	}
	reading, ok := Number(raw)
	if !ok {
		return Window{}, false
	}

	// A limit turns the window into a count ("4 / 5 videos") and, when there is
	// one, into a share we can meter.
	var limit float64
	haveLimit := false
	if cfg.Limit != "" {
		if p, err := config.ParseQuotaPath(string(cfg.Limit)); err == nil {
			switch v, st := Eval(p, root); st {
			case Present:
				if n, ok := Number(v); ok && n > 0 {
					limit, haveLimit = n, true
				}
			case Null:
				// A null limit is an uncapped allowance: no share to show, but the
				// count still reads.
			}
		}
	}

	// The share spent, 0 to 1. A percentage window says it directly; an amount
	// window derives it from its limit.
	var meter *float64
	var usedPercent *float64
	switch {
	case usedPath.Kind == config.QuotaPercent:
		pct := clampPercent(reading)
		if cfg.Invert {
			pct = 100 - pct
		}
		m := pct / 100
		meter, usedPercent = &m, &pct
	case haveLimit:
		used := reading
		if cfg.Invert {
			used = limit - reading
		}
		used = math.Max(0, math.Min(limit, used))
		m := used / limit
		meter = &m
		out.Used, out.Limit = &used, &limit
	}

	// An amount window with no limit is a bare count ("12 requests"): nothing to
	// take a share of.
	if out.Used == nil && usedPath.Kind != config.QuotaPercent {
		count := reading
		out.Used = &count
	}

	if cfg.ResetsAt != "" {
		if p, err := config.ParseQuotaPath(string(cfg.ResetsAt)); err == nil {
			if v, st := Eval(p, root); st == Present {
				if t, ok := Time(v); ok {
					out.ResetsAt = &t
				}
			}
		}
	}

	out.Meter, out.UsedPercent = meter, usedPercent
	out.Text = windowText(out, now)
	return out, true
}

// clampPercent keeps a reading inside 0-100: vendors do occasionally report a
// slightly negative or over-100 figure, and a meter must not run off its track.
func clampPercent(v float64) float64 {
	return math.Max(0, math.Min(100, v))
}

// windowText renders a window the way it is shown, so the browser formats
// nothing: "42% used · resets 14:30", "4 / 5 videos · resets 08:00".
func windowText(w Window, now time.Time) string {
	var parts []string
	switch {
	case w.UsedPercent != nil:
		parts = append(parts, trimPercent(*w.UsedPercent)+"% used")
	case w.Used != nil && w.Limit != nil:
		count := groupedNumber(*w.Used, "") + " / " + groupedNumber(*w.Limit, "")
		if w.Unit != "" {
			count += " " + w.Unit
		}
		parts = append(parts, count)
	case w.Used != nil:
		count := groupedNumber(*w.Used, "")
		if w.Unit != "" {
			count += " " + w.Unit
		}
		parts = append(parts, count)
	}
	if w.ResetsAt != nil {
		parts = append(parts, "resets "+resetClock(*w.ResetsAt, now))
	}
	return strings.Join(parts, " · ")
}

// trimPercent renders a percentage without a trailing ".0" or noisy decimals.
func trimPercent(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

// resetClock says when a window starts again, in a form that stays readable: the
// time today, "tomorrow 09:00", a weekday within the week, else the date.
func resetClock(at, now time.Time) string {
	at, now = at.Local(), now.Local()
	day := func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	}
	switch days := int(math.Round(day(at).Sub(day(now)).Hours() / 24)); {
	case days <= 0:
		return at.Format("15:04")
	case days == 1:
		return "tomorrow " + at.Format("15:04")
	case days < 7:
		return at.Format("Mon 15:04")
	}
	return at.Format("Jan 2 15:04")
}

// String is for logs and tests.
func (r Reading) String() string {
	parts := make([]string, 0, len(r.Windows)+1)
	if r.Balance != nil {
		parts = append(parts, r.Balance.Text)
	}
	for _, w := range r.Windows {
		parts = append(parts, fmt.Sprintf("%s: %s", w.Name, w.Text))
	}
	return strings.Join(parts, "; ")
}
