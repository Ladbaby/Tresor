package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"tresor/internal/middleware"
	"tresor/internal/store"
)

const affinityTTL = time.Hour
const affinityLimit = 10000

type routeIdentityContext struct{}
type routePin struct {
	aliasID, target, mode, fingerprint string
	touched                            time.Time
	active                             int
	order                              int
}
type aliasCoordinator struct {
	mu    sync.Mutex
	now   func() time.Time
	pins  map[string]*routePin
	loads map[string]int
}

func newAliasCoordinator() *aliasCoordinator {
	return &aliasCoordinator{now: time.Now, pins: make(map[string]*routePin), loads: make(map[string]int)}
}
func hashRoute(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func routeFingerprint(mode string, options []routeCandidate) string {
	var b strings.Builder
	b.WriteString(mode)
	for _, x := range options {
		b.WriteByte(0)
		b.WriteString(x.aliasID)
		b.WriteByte(0)
		b.WriteString(x.target)
		b.WriteByte(0)
		b.WriteString(strconv.Itoa(x.order))
	}
	return hashRoute(b.String())
}

func (c *aliasCoordinator) prune() {
	now := c.now()
	for k, p := range c.pins {
		if p.active == 0 && !now.Before(p.touched.Add(affinityTTL)) {
			delete(c.pins, k)
		}
	}
	for len(c.pins) >= affinityLimit {
		oldest := ""
		var at time.Time
		for k, p := range c.pins {
			if p.active == 0 && (oldest == "" || p.touched.Before(at)) {
				oldest, at = k, p.touched
			}
		}
		if oldest == "" {
			break
		}
		delete(c.pins, oldest)
	}
}
func routeSessionKey(r *http.Request, body []byte, group, model, namespace string) string {
	identity := strings.TrimSpace(r.Header.Get("x-tresor-session-id"))
	if identity == "" {
		identity = strings.TrimSpace(r.Header.Get("session-id"))
	}
	if identity == "" {
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			for _, source := range []map[string]any{obj, object(obj["metadata"])} {
				for _, key := range []string{"conversation_id", "conversationId", "session_id", "sessionId"} {
					if s, ok := source[key].(string); ok && strings.TrimSpace(s) != "" {
						identity = strings.TrimSpace(s)
						break
					}
				}
				if identity != "" {
					break
				}
			}
		}
	}
	if identity == "" {
		identity = initialContent(body)
	}
	if identity == "" {
		return ""
	}
	if namespace == "" {
		namespace = middleware.ExtractClientIP(r)
	}
	data, _ := json.Marshal([]string{group, model, namespace, identity})
	return hashRoute(string(data))
}
func emptyInitialContent(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

func initialContent(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	prefix := map[string]any{}
	for _, k := range []string{"system", "instructions", "systemInstruction"} {
		if v, ok := obj[k]; ok {
			prefix[k] = v
		}
	}
	if text, ok := obj["input"].(string); ok && text != "" {
		prefix["first_user"] = text
		b, _ := json.Marshal(prefix)
		return string(b)
	}
	for _, key := range []string{"messages", "input", "contents"} {
		for _, v := range array(obj[key]) {
			item := object(v)
			role, _ := item["role"].(string)
			if role == "system" || role == "developer" {
				prefix[role] = item
				continue
			}
			if role == "user" || role == "human" {
				content := item["content"]
				if key == "contents" {
					content = item["parts"]
				}
				if content == nil || content == "" {
					continue
				}
				prefix["first_user"] = content
				b, _ := json.Marshal(prefix)
				return string(b)
			}
		}
	}
	return ""
}

type routeCandidate struct {
	aliasID, target string
	order           int
	alias           *store.Alias
	ds              *store.Downstream
}

func (e *Engine) resolveRoutedGroup(r *http.Request, body []byte, model string, g store.AliasGroup) (*modelResult, *gatewayError) {
	candidates := []routeCandidate{}
	for i, a := range g.Options {
		ds, err := e.store.GetDownstream(a.DownstreamID)
		if err != nil || !ds.IsEnabled {
			continue
		}
		a := a
		candidates = append(candidates, routeCandidate{a.ID, ds.ID + "\x00" + a.OutputModelID, i, &a, ds})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	namespace, _ := r.Context().Value(routeIdentityContext{}).(string)
	key := routeSessionKey(r, body, g.InputModelID, model, namespace)
	idx, release, token, sessionDone := e.routes.selectReserveSession(key, g.RouteMode, candidates)
	x := candidates[idx]
	return &modelResult{ds: x.ds, alias: x.alias, model: model, resolvedModel: x.alias.OutputModelID, body: rewriteModelInBody(body, x.alias.OutputModelID), originalBody: body, candidates: candidates, routeKey: key, routeMode: g.RouteMode, routeIndex: idx, release: release, routeToken: token, sessionDone: sessionDone}, nil
}
func (c *aliasCoordinator) selectReserve(key, mode string, options []routeCandidate) (int, func()) {
	idx, release, _, done := c.selectReserveSession(key, mode, options)
	return idx, func() { release(); done() }
}
func (c *aliasCoordinator) selectReserveSession(key, mode string, options []routeCandidate) (int, func(), *routePin, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	fingerprint := routeFingerprint(mode, options)
	chosen := 0
	var pin *routePin
	if key != "" {
		pin = c.pins[key]
		if pin != nil {
			valid := pin.mode == mode && pin.fingerprint == fingerprint
			for i, x := range options {
				if pin.aliasID == x.aliasID && pin.target == x.target && pin.mode == mode && pin.fingerprint == fingerprint {
					chosen = i
					valid = true
					break
				}
			}
			if !valid {
				delete(c.pins, key)
				pin = nil
			}
		}
	}
	if pin == nil && mode == "load_balance" {
		for i, x := range options {
			if c.loads[x.target] < c.loads[options[chosen].target] {
				chosen = i
			}
			if c.loads[x.target] == 0 {
				chosen = i
				break
			}
		}
	}
	if pin == nil && key != "" && len(c.pins) < affinityLimit {
		pin = &routePin{mode: mode, fingerprint: fingerprint, touched: c.now(), order: -1}
		if mode == "load_balance" {
			pin.aliasID, pin.target, pin.order = options[chosen].aliasID, options[chosen].target, options[chosen].order
		}
		c.pins[key] = pin
	}
	if pin != nil {
		pin.active++
		pin.touched = c.now()
	}
	target := options[chosen].target
	c.loads[target]++
	var once, sessionOnce sync.Once
	done := func() {
		sessionOnce.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if pin != nil {
				pin.active--
				pin.touched = c.now()
			}
		})
	}
	return chosen, func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.loads[target]--
			if c.loads[target] == 0 {
				delete(c.loads, target)
			}
		})
	}, pin, done
}
func (c *aliasCoordinator) reserve(target string) func() {
	c.mu.Lock()
	c.loads[target]++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.loads[target]--
			if c.loads[target] == 0 {
				delete(c.loads, target)
			}
		})
	}
}
func (c *aliasCoordinator) pin(key string, x routeCandidate, token *routePin, mode string, options []routeCandidate) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pins[key]
	if p == nil || p != token || p.fingerprint != routeFingerprint(mode, options) || !routeCandidatePresent(x, options) {
		return
	}
	if p.order > x.order {
		return
	}
	p.aliasID, p.target, p.order = x.aliasID, x.target, x.order
	p.touched = c.now()
}
func routeCandidatePresent(x routeCandidate, options []routeCandidate) bool {
	for _, option := range options {
		if option.aliasID == x.aliasID && option.target == x.target {
			return true
		}
	}
	return false
}

func isNetworkFailure(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func (e *Engine) tryFallback(r *http.Request, result *modelResult, status int, message string, err error) bool {
	if result.routeMode != "fallback" || result.routeIndex+1 >= len(result.candidates) || r.Context().Err() != nil || !fallbackStatus(status, message) {
		return false
	}
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !isNetworkFailure(err)) {
		return false
	}
	result.retry = true
	return true
}

func (e *Engine) confirmRoute(result *modelResult) {
	if result.routeMode != "fallback" || result.alias == nil || result.routeIndex < 0 || result.routeIndex >= len(result.candidates) {
		return
	}
	// A successful request may complete after the alias group has been mutated.
	// Re-read current routing state before allowing it to establish affinity.
	groups, err := e.store.ListGroups()
	if err != nil {
		return
	}
	var current []routeCandidate
	for _, group := range groups {
		if group.InputModelID != result.alias.InputModelID || group.RouteMode != result.routeMode {
			continue
		}
		for _, alias := range group.Options {
			ds, getErr := e.store.GetDownstream(alias.DownstreamID)
			if getErr == nil && ds.IsEnabled {
				current = append(current, routeCandidate{aliasID: alias.ID, target: ds.ID + "\x00" + alias.OutputModelID, order: len(current), alias: &alias, ds: ds})
			}
		}
		break
	}
	if routeFingerprint(result.routeMode, current) != routeFingerprint(result.routeMode, result.candidates) {
		return
	}
	e.routes.pin(result.routeKey, result.candidates[result.routeIndex], result.routeToken, result.routeMode, result.candidates)
}
func fallbackStatus(status int, message string) bool {
	if contextOverflow(message) {
		return false
	}
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return false
}
func contextOverflow(message string) bool {
	s := strings.ToLower(message)
	for _, term := range []string{"context_length_exceeded", "context_window_exceeded", "context size has been exceeded", "maximum context length", "context window", "context limit", "context overflow", "too many tokens", "prompt is too long", "input is too long"} {
		if strings.Contains(s, term) {
			return true
		}
	}
	return false
}
func proxyNamespace(r *http.Request) string {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.Header.Get("x-api-key")
	}
	if token == "" {
		token = r.Header.Get("x-goog-api-key")
	}
	if token == "" && strings.HasPrefix(r.URL.Path, "/v1beta/") {
		token = r.URL.Query().Get("key")
	}
	return hashRoute(token)
}
func withRouteNamespace(r *http.Request, namespace string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), routeIdentityContext{}, namespace))
}
