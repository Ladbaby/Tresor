package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOAuthPolicyYAMLAndJSONRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	data := `downstreams:
  - id: fictional
    auth:
      type: oauth
      flow: auth_code
      request_nonce: true
      require_refresh_token: true
      default_token_lifetime_secs: 3600
      refresh_on_unknown_expiry: true
      allowed_endpoint_hosts: [fictional.invalid, '*.fictional.invalid']
      persist_discovered_endpoints: true
      refresh_skew_secs: 0
`
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Downstreams[0].Auth
	encoded, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var got DownstreamAuthCfg
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !got.RequestNonce || !got.RequireRefreshToken || !got.RefreshOnUnknownExpiry || !got.PersistDiscoveredEndpoints || got.DefaultTokenLifetimeSecs != 3600 || got.RefreshSkewSecs != 0 || len(got.AllowedEndpointHosts) != 2 {
		t.Fatalf("policy lost: %+v", got)
	}
}

func TestObsoleteGlobalIntegrationsRequireRuleConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, section string
		wantError     bool
	}{{"services", "integration_services:\n  old:\n    base_url: https://example.invalid\n", true}, {"grants", "integration_grants:\n  - plugin_id: old\n", true}, {"empty", "integration_services: {}\nintegration_grants: []\n", false}} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(file, []byte("bind_addr: 127.0.0.1:11510\ndb_path: ./test.db\n"+tc.section), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(file)
			if (err != nil) != tc.wantError {
				t.Fatalf("load error=%v", err)
			}
		})
	}
}

func TestPipelineConfigPreservedAndIDsNormalized(t *testing.T) {
	input := `[{"plugin_id":"p","config":{"token":"secret","nested":[1,true]},"failure_policy":"fail_closed"}]`
	var first, second []PipelineStep
	for _, steps := range []*[]PipelineStep{&first, &second} {
		if err := json.Unmarshal([]byte(input), steps); err != nil {
			t.Fatal(err)
		}
		if err := NormalizePipeline(steps); err != nil {
			t.Fatal(err)
		}
	}
	if first[0].ID == "" || first[0].ID == second[0].ID {
		t.Fatalf("bad generated IDs: %q %q", first[0].ID, second[0].ID)
	}
	id := first[0].ID
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded []PipelineStep
	if err := json.Unmarshal(encoded, &reloaded); err != nil {
		t.Fatal(err)
	}
	if err := NormalizePipeline(&reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded[0].ID != id || reloaded[0].Config["token"] != "secret" {
		t.Fatalf("round trip changed: %#v", reloaded[0])
	}
}

func TestValidatePipelineRejectsDuplicateIDs(t *testing.T) {
	steps := []PipelineStep{{ID: "same", PluginID: "a"}, {ID: "same", PluginID: "b"}}
	if err := ValidatePipeline(steps); err == nil {
		t.Fatal("expected duplicate IDs rejected")
	}
}

func TestNormalizePipelineJSONPreservesConfig(t *testing.T) {
	got, err := NormalizePipelineJSON(`[{"id":"step-a","plugin_id":"p","config":{"secret":"value"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `[{"id":"step-a","plugin_id":"p","config":{"secret":"value"}}]` {
		t.Fatalf("got %s", got)
	}
}

func TestLoad_PipelineSettingsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("rules:\n  - id: r\n    pipeline_config:\n      - plugin_id: p\n        config: {secret: token}\n        failure_policy: fail_closed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	step := cfg.Rules[0].PipelineConfig[0]
	if step.ID == "" || step.Config["secret"] != "token" || step.FailurePolicy != "fail_closed" {
		t.Fatalf("settings lost: %#v", step)
	}
}

func TestLoad_Defaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`bind_addr: ""`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BindAddr != "127.0.0.1:11510" || cfg.DBPath == "" {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
}
