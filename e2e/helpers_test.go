//go:build integration

package e2e

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedIconCache(cacheDir string, slugs []string) error {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}
	index := struct {
		Version string    `json:"version"`
		Fetched time.Time `json:"fetched"`
		Slugs   []string  `json:"slugs"`
		Source  string    `json:"source"`
	}{Version: "e2e", Fetched: time.Now().UTC(), Slugs: slugs, Source: "e2e-fixture"}
	b, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "index.json"), b, 0644); err != nil {
		return err
	}
	for _, slug := range slugs {
		url := "https://cdn.jsdelivr.net/npm/@lobehub/icons-static-svg@latest/icons/" + slug + ".svg"
		sum := sha256.Sum256([]byte(url))
		name := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(cacheDir, name+".svg"), []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"), 0644); err != nil {
			return err
		}
		meta, err := json.Marshal(map[string]string{"url": url, "content_type": "image/svg+xml", "fetched": time.Now().UTC().Format(time.RFC3339)})
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cacheDir, name+".meta"), meta, 0644); err != nil {
			return err
		}
	}
	return nil
}

// startTresor spins up a Tresor daemon with the given YAML config on port
// and returns the API base URL. Caller defers the returned cleanup.
func startTresor(t *testing.T, cfg string, port int) (apiBase string, cleanup func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "tresor-e2e-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	if !strings.Contains(cfg, "plugin_cache_dir:") {
		cfg = fmt.Sprintf("plugin_cache_dir: %s\n%s", filepath.ToSlash(filepath.Join(tmpDir, "plugin-cache")), cfg)
	}
	iconCacheDir := filepath.Join(tmpDir, "icon-cache")
	if strings.Contains(cfg, "icon_cache_dir:") {
		for _, line := range strings.Split(cfg, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "icon_cache_dir:") {
				iconCacheDir = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "icon_cache_dir:"))
				break
			}
		}
	} else {
		cfg = fmt.Sprintf("icon_cache_dir: %s\n%s", filepath.ToSlash(iconCacheDir), cfg)
	}
	if err := seedIconCache(iconCacheDir, []string{"openai", "claude-color", "minimax-color", "minimax", "totally"}); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("seed icon cache: %v", err)
	}
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("write config: %v", err)
	}
	probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("port %d is already occupied: %v", port, err)
	}
	_ = probe.Close()
	binary := os.Getenv("TRESOR_TEST_BINARY")
	if binary == "" {
		binary, _ = filepath.Abs("../tresor.exe")
	}
	cmd := exec.Command(binary, "run", "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("start daemon: %v", err)
	}
	apiBase = fmt.Sprintf("http://127.0.0.1:%d", port)
	cleanup = func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.RemoveAll(tmpDir)
	}
	t.Cleanup(cleanup)
	waitForHealth(t, apiBase, 5*time.Second)
	return apiBase, cleanup
}

// waitForHealth polls /api/health until 200 or timeout.
func waitForHealth(t *testing.T, apiBase string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := client.Get(apiBase + "/api/health")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-deadline:
			t.Fatalf("daemon not ready at %s", apiBase)
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// startMockServer binds a listener on 127.0.0.1:port and serves mux in a
// goroutine. Fails the test fast if the port is taken.
func startMockServer(t *testing.T, port int, mux http.Handler) *http.Server {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen %d: %v", port, err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return srv
}

// scanSSE reads an SSE response body and returns parsed events.
func scanSSE(body io.Reader) ([]sseEvent, error) {
	var events []sseEvent
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var event string
	var data strings.Builder
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if data.Len() > 0 {
				events = append(events, sseEvent{Type: event, Data: data.String()})
				data.Reset()
			}
			event = ""
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	if data.Len() > 0 {
		events = append(events, sseEvent{Type: event, Data: data.String()})
	}
	return events, scanner.Err()
}

type sseEvent struct {
	Type string
	Data string
}

func findEvent(events []sseEvent, eventType string) *sseEvent {
	for i, e := range events {
		if e.Type == eventType {
			return &events[i]
		}
	}
	return nil
}

func findEvents(events []sseEvent, eventType string) []sseEvent {
	var out []sseEvent
	for _, e := range events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}
