package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// Strategy names. These cover the generic protocol shapes only: a provider whose
// model endpoint is non-generic answers through its own dialect package instead
// (see wire.Discoverer), so no vendor name appears here.
const (
	// OpenaiList is the default: GET {base}/models (and {base}/v1/models),
	// probing the auth header each declared format needs.
	OpenaiList = "openai_list"
	// GeminiList is the Google Generative AI shape, mounted at /v1beta/models or
	// bare /models.
	GeminiList = "gemini_list"
)

func init() {
	Register(openaiList{})
	Register(geminiList{})
}

// apiKeyError is the message every strategy uses for a missing credential, so the
// UI's error text does not depend on which strategy ran.
func apiKeyError() error {
	return fmt.Errorf("no API key configured — add an API key before fetching models")
}

// maxBody bounds a models response so a misbehaving endpoint cannot exhaust
// memory.
const maxBody = 4 << 20

// readBody reads a bounded body and closes it.
func readBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return b
}

// ---- openai_list ----

type openaiList struct{}

func (openaiList) Name() string { return OpenaiList }

// Models probes the OpenAI-style model list. A downstream declaring several
// formats (minimax serves openai and anthropic) may answer on either scheme, so
// each declared format's auth header is tried and the first parseable body wins.
func (openaiList) Models(ctx context.Context, in Input) ([]string, error) {
	if in.Token == "" {
		return nil, apiKeyError()
	}
	base := strings.TrimSuffix(in.BaseURL, "/")
	client := in.ClientOrDefault()

	endpoints := []string{
		base + "/models",    // OpenAI-style /v1/models (base_url already includes /v1)
		base + "/v1/models", // some providers need an explicit /v1 prefix
	}

	var lastError string
	for _, url := range endpoints {
		for _, scheme := range authSchemes(in.Formats) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				continue
			}
			scheme.apply(req, in.Token)
			applyExtra(req, in.ExtraHeaders)

			resp, err := client.Do(req)
			if err != nil {
				lastError = "unable to connect to provider"
				continue
			}
			body := readBody(resp)

			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				lastError = "authentication failed — check the API key"
				continue // wrong auth style for this endpoint; try the next scheme
			}
			if resp.StatusCode >= 400 {
				lastError = "provider returned unexpected response"
				continue
			}

			if models := parseOpenaiList(body); len(models) > 0 {
				return models, nil
			}
			lastError = "unrecognized response format"
			break // body decoded but no models: try the next URL, not another scheme
		}
	}
	return nil, fmt.Errorf("%s. No working models endpoint found", lastError)
}

// parseOpenaiList accepts the two shapes seen in the wild: {"data":[{"id":…}]} and
// a bare array of strings.
func parseOpenaiList(body []byte) []string {
	var openaiResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &openaiResp); err == nil && len(openaiResp.Data) > 0 {
		out := make([]string, 0, len(openaiResp.Data))
		for _, m := range openaiResp.Data {
			if m.ID != "" {
				out = append(out, m.ID)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	var strArr []string
	if err := json.Unmarshal(body, &strArr); err == nil && len(strArr) > 0 {
		return strArr
	}
	return nil
}

// authScheme is one way of attaching credentials to a probe.
type authScheme struct {
	name  string
	apply func(*http.Request, string)
}

var (
	bearerScheme = authScheme{
		name:  "bearer",
		apply: func(r *http.Request, key string) { r.Header.Set("Authorization", "Bearer "+key) },
	}
	anthropicScheme = authScheme{
		name: "anthropic",
		apply: func(r *http.Request, key string) {
			r.Header.Set("x-api-key", key)
			r.Header.Set("anthropic-version", "2023-06-01")
		},
	}
)

// authSchemes picks the styles to probe from the declared formats. An empty list
// falls back to Bearer, which is what an unconfigured relay expects.
func authSchemes(formats []string) []authScheme {
	if len(formats) == 0 {
		return []authScheme{bearerScheme}
	}
	seen := map[string]bool{}
	var out []authScheme
	for _, f := range formats {
		switch f {
		case "openai", "openai_responses":
			if !seen["bearer"] {
				out = append(out, bearerScheme)
				seen["bearer"] = true
			}
		case "anthropic":
			if !seen["anthropic"] {
				out = append(out, anthropicScheme)
				seen["anthropic"] = true
			}
		}
	}
	if len(out) == 0 {
		return []authScheme{bearerScheme}
	}
	return out
}

// applyExtra applies the downstream's declared extra headers, so a provider that
// gates its model list on identity can still be asked.
func applyExtra(req *http.Request, extra map[string]string) {
	for k, v := range extra {
		req.Header.Set(k, v)
	}
}

// ---- gemini_list ----

type geminiList struct{}

func (geminiList) Name() string { return GeminiList }

// Models probes Google's generateContent model list. The official endpoint is
// {base}/v1beta/models, but providers occasionally mount it at bare /models, so
// both are tried.
func (geminiList) Models(ctx context.Context, in Input) ([]string, error) {
	if in.Token == "" {
		return nil, apiKeyError()
	}
	base := strings.TrimSuffix(in.BaseURL, "/")
	client := in.ClientOrDefault()

	for _, url := range []string{base + "/v1beta/models", base + "/models"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("x-goog-api-key", in.Token)
		applyExtra(req, in.ExtraHeaders)

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body := readBody(resp)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("authentication failed — check the API key")
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if models := parseGeminiList(body); len(models) > 0 {
				return models, nil
			}
		}
	}
	return nil, fmt.Errorf("gemini /v1beta/models and /models both returned no models in expected format")
}

// parseGeminiList reads {"models":[{"name":"models/gemini-2.5-pro"}]} and strips
// the "models/" prefix the API puts on each name.
func parseGeminiList(body []byte) []string {
	var resp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Models) == 0 {
		return nil
	}
	out := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		if name := strings.TrimPrefix(m.Name, "models/"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Pick chooses a strategy from a downstream's declared formats when no explicit
// one is configured, so existing configs keep their behavior: a gemini-only
// downstream uses gemini_list, everything else openai_list.
func Pick(formats []string) string {
	if slices.Contains(formats, "gemini") {
		return GeminiList
	}
	return OpenaiList
}
