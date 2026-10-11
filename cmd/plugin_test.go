package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tresor/internal/config"
)

func TestReadPluginDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"id":"test/a"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.js"), []byte(`module.exports={};`), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := readPluginDirectory(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("read package: %v, %v", files, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "invalid.bin"), []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPluginDirectory(dir); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestPluginAPIRequestReportsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing CLI auth")
		}
		http.Error(w, "package rejected", http.StatusBadRequest)
	}))
	defer server.Close()
	cfg := &config.AppConfig{BindAddr: strings.TrimPrefix(server.URL, "http://"), AdminPassword: "test-secret"}
	_, err := pluginAPIRequest(cfg, http.MethodPost, "/api/plugin-packages/activate", map[string]string{"id": "test/a", "version": "1"})
	if err == nil || !strings.Contains(err.Error(), "package rejected") {
		t.Fatalf("HTTP failure not propagated: %v", err)
	}
}
