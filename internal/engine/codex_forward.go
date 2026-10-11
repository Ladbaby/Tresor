package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"net/http"
	"strings"
)

// zstdEncoder compresses ChatGPT/Codex request bodies to match the reference
// client (the Codex backend accepts Content-Encoding: zstd on the SSE endpoint).
// Built once and reused; klauspost's zstd Encoder is safe for concurrent Use.
var zstdEncoder = newZstdEncoder()

func newZstdEncoder() *zstd.Encoder {
	// SpeedDefault maps to zstd level 3, the level the reference client uses.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil
	}
	return enc
}

// codexFingerprintBody injects the reference client's Codex request fields
// into a Responses-API request body:
//   - store: false — opt out of server-side conversation storage.
//   - include: ["reasoning.encrypted_content"] and prompt_cache_key (set to
//     the session id) — only added to a body that is actually a Responses
//     request (one carrying an "input" array), so a Chat-format body is left
//     apart from store.
//
// Only fields the client did not already set are added, and the input is
// returned unchanged if it is not a JSON object.
func codexFingerprintBody(body []byte, sessionID string) []byte {
	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	if _, ok := obj["store"]; !ok {
		obj["store"] = false
		changed = true
	}
	if _, hasInput := obj["input"]; hasInput {
		if _, ok := obj["include"]; !ok {
			obj["include"] = []interface{}{"reasoning.encrypted_content"}
			changed = true
		}
		if sessionID != "" {
			if _, ok := obj["prompt_cache_key"]; !ok {
				obj["prompt_cache_key"] = sessionID
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// codexCacheKey returns the stable, client-supplied conversation identity for
// the Codex cache key. A stateless gateway cannot safely invent a durable
// conversation identity: content-derived keys would merge unrelated clients
// with equivalent prompts, while a random key would change every turn. When
// no identity is supplied, return an empty string so the downstream's own
// cache matching remains authoritative.
func codexCacheKey(original *http.Request) string {
	if sessionID := original.Header.Get("session-id"); sessionID != "" {
		return sessionID
	}
	return original.Header.Get("x-client-request-id")
}

// codexStableCacheKey derives a deterministic, stable cache key for a Codex
// conversation from its stable prefix. It is used when the client supplies no
// session identity (e.g. the Anthropic client, which never sends a
// session-id), so the gateway cannot forward a per-request UUID.
//
// Why: the ChatGPT/Codex backend is a fleet of pods, and the reference
// gateway (sub2api) uses a stable prompt_cache_key / session-id as a
// sticky-routing + cache-affinity signal — the same conversation keeps landing
// on the pod whose prefix cache is hot. A fresh UUID per request makes every
// turn look like a brand-new conversation, so the pod the request lands on
// often has no cached prefix and the turn misses (this is what produces the
// erratic per-turn cache hit rate). Deriving the key from the stable prefix
// reproduces the reference behavior while still separating distinct
// conversations, and it stays constant for the whole conversation because a
// conversation only ever appends to its history.
//
// The key is derived from model + instructions + tools (tool names, in order)
// + the first user message — the parts that are identical across every turn —
// and deliberately NOT from later messages or the newest user turn. A client
// that sends no stable prefix at all yields "" (no key) so we never fabricate
// an identity we cannot distinguish.
func codexStableCacheKey(body []byte) string {
	var parsed struct {
		Model        string          `json:"model"`
		Instructions string          `json:"instructions"`
		Tools        json.RawMessage `json:"tools"`
		Input        json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Model == "" {
		return ""
	}

	var toolNames []string
	if len(parsed.Tools) > 0 {
		var tools []map[string]any
		if err := json.Unmarshal(parsed.Tools, &tools); err == nil {
			for _, t := range tools {
				toolNames = append(toolNames, fmt.Sprintf("%v", t["name"]))
			}
		}
	}

	// First real user text in the conversation (stable across turns).
	var firstUser string
	if len(parsed.Input) > 0 {
		var input []map[string]any
		if err := json.Unmarshal(parsed.Input, &input); err == nil {
			for _, it := range input {
				if role, _ := it["role"].(string); role == "user" {
					if c, ok := it["content"].(string); ok && c != "" {
						firstUser = c
						break
					}
					if c, ok := it["content"].([]any); ok {
						var parts []string
						for _, p := range c {
							if pm, ok := p.(map[string]any); ok {
								if txt, ok := pm["text"].(string); ok && txt != "" {
									parts = append(parts, txt)
								}
							}
						}
						if s := strings.Join(parts, "\n"); s != "" {
							firstUser = s
							break
						}
					}
				}
			}
		}
	}
	if firstUser == "" {
		// Nothing stable to key on — do not invent an identity.
		return ""
	}

	h := sha256.New()
	fmt.Fprintf(h, "codex-cache-key:v1|model=%s|instructions=%s|tools=%s|first_user=%s",
		parsed.Model, parsed.Instructions, strings.Join(toolNames, ","), firstUser)
	return "cc_" + hex.EncodeToString(h.Sum(nil))[:32]
}
