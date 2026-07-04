package store

import (
	"os"
	"path/filepath"
	"testing"

	"tresor/internal/config"
	"gopkg.in/yaml.v3"
)

func TestWriteConfig_EmptyPath_NoOp(t *testing.T) {
	s := newTestStore(t)

	cfg := &config.AppConfig{ConfigPath: ""}
	err := s.WriteConfig(cfg)
	if err != nil {
		t.Fatalf("expected nil for empty config path, got %v", err)
	}
}

func TestWriteConfig_RoundTrip(t *testing.T) {
	s := newTestStore(t)

	// Create a temp YAML file as the config path
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Populate DB with known data
	ds := &Downstream{ID: "ds-test", Name: "Test Provider", BaseURL: "https://test.com", APIKey: "sk-123"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	rule := &Rule{ID: "rule-test", Name: "Test Rule", PatternPath: "/v1/chat/completions", MatchDownstreams: []string{"ds-test"}, PipelineConfig: "[{\"plugin_id\":\"custom_header\"}]", IsEnabled: true}
	if err := s.CreateRule(rule); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	alias := &Alias{InputModelID: "gpt-4o", DownstreamID: "ds-test", OutputModelID: "claude-sonnet", IsActive: true}
	if err := s.CreateAlias(alias); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	// Write config
	cfg := &config.AppConfig{ConfigPath: configPath}
	if err := s.WriteConfig(cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Read back and verify
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	var parsed config.AppConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	if len(parsed.Downstreams) != 1 {
		t.Fatalf("expected 1 downstream, got %d", len(parsed.Downstreams))
	}
	if parsed.Downstreams[0].ID != "ds-test" {
		t.Fatalf("expected downstream id ds-test, got %q", parsed.Downstreams[0].ID)
	}
	if parsed.Downstreams[0].APIKey != "sk-123" {
		t.Fatalf("expected api_key sk-123, got %q", parsed.Downstreams[0].APIKey)
	}

	if len(parsed.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(parsed.Rules))
	}
	if parsed.Rules[0].ID != "rule-test" {
		t.Fatalf("expected rule id rule-test, got %q", parsed.Rules[0].ID)
	}

	if len(parsed.Aliases) != 1 {
		t.Fatalf("expected 1 alias group, got %d", len(parsed.Aliases))
	}
	if parsed.Aliases[0].InputModelID != "gpt-4o" {
		t.Fatalf("expected input_model_id gpt-4o, got %q", parsed.Aliases[0].InputModelID)
	}
}

func TestWriteConfig_OverwritesOldData(t *testing.T) {
	s := newTestStore(t)

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// First write with one downstream
	ds1 := &Downstream{ID: "ds-1", Name: "First", BaseURL: "https://first.com"}
	if err := s.CreateDownstream(ds1); err != nil {
		t.Fatalf("create downstream 1: %v", err)
	}

	cfg := &config.AppConfig{ConfigPath: configPath}
	if err := s.WriteConfig(cfg); err != nil {
		t.Fatalf("first write config: %v", err)
	}

	// Add a second downstream
	ds2 := &Downstream{ID: "ds-2", Name: "Second", BaseURL: "https://second.com"}
	if err := s.CreateDownstream(ds2); err != nil {
		t.Fatalf("create downstream 2: %v", err)
	}

	// Second write
	if err := s.WriteConfig(cfg); err != nil {
		t.Fatalf("second write config: %v", err)
	}

	// Verify file contains both downstreams
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	var parsed config.AppConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	if len(parsed.Downstreams) != 2 {
		t.Fatalf("expected 2 downstreams after second write, got %d", len(parsed.Downstreams))
	}
}

func TestWriteConfig_AtomicWrite(t *testing.T) {
	s := newTestStore(t)

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	tmpFile := filepath.Join(tmpDir, ".tresor-config.tmp")

	ds := &Downstream{ID: "ds-atomic", Name: "Atomic", BaseURL: "https://atomic.com"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	cfg := &config.AppConfig{ConfigPath: configPath}
	if err := s.WriteConfig(cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Verify tmp file was cleaned up
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Fatal("tmp file should not exist after successful write")
	}

	// Verify actual config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatal("config file should exist after successful write")
	}
}
