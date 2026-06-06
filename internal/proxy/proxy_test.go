package proxy

import (
	"net/http"
	"os"
	"testing"
)

func TestProxyFunc_ModeNone(t *testing.T) {
	fn := ProxyFunc(ModeNone)
	if fn != nil {
		t.Fatal("expected nil proxy func for ModeNone")
	}
}

func TestProxyFunc_DefaultMode(t *testing.T) {
	fn := ProxyFunc(Mode("unknown"))
	if fn == nil {
		t.Fatal("expected non-nil proxy func for unknown mode (should default to auto)")
	}
}

// TestProxyFunc_EnvIntegration tests all proxy modes that depend on
// environment variables. All checks are in a single test function because
// Go's os.Setenv/os.Unsetenv has known issues on Windows where the process
// environment block is not atomically restored between test functions.
func TestProxyFunc_EnvIntegration(t *testing.T) {
	// Set up: HTTPS_PROXY for all tests below.
	proxyURL := "http://test-proxy.example.com:3128"
	t.Setenv("HTTPS_PROXY", proxyURL)

	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/chat", nil)

	// ModeEnv should use the env var directly.
	t.Run("ModeEnv", func(t *testing.T) {
		fn := ProxyFunc(ModeEnv)
		if fn == nil {
			t.Fatal("expected non-nil proxy func for ModeEnv")
		}
		u, err := fn(req)
		if err != nil {
			t.Fatalf("proxy func error: %v", err)
		}
		if u == nil {
			t.Fatal("expected proxy URL with HTTPS_PROXY set")
		}
		if u.Host != "test-proxy.example.com:3128" {
			t.Fatalf("expected host test-proxy.example.com:3128, got %s", u.Host)
		}
	})

	// ModeAuto should fall back to env vars (no Windows system proxy in test env).
	t.Run("ModeAuto", func(t *testing.T) {
		fn := ProxyFunc(ModeAuto)
		u, err := fn(req)
		if err != nil {
			t.Fatalf("proxy func error: %v", err)
		}
		if u == nil {
			t.Fatal("expected fallback to env var proxy")
		}
		if u.Host != "test-proxy.example.com:3128" {
			t.Fatalf("expected host test-proxy.example.com:3128, got %s", u.Host)
		}
	})

	// ModeWindows should fall back to env vars (no Windows system proxy in test env).
	t.Run("ModeWindows", func(t *testing.T) {
		fn := ProxyFunc(ModeWindows)
		u, err := fn(req)
		if err != nil {
			t.Fatalf("proxy func error: %v", err)
		}
		if u == nil {
			t.Fatal("expected fallback to env var proxy")
		}
		if u.Host != "test-proxy.example.com:3128" {
			t.Fatalf("expected host test-proxy.example.com:3128, got %s", u.Host)
		}
	})

	// ModeNone should return nil regardless of env vars.
	t.Run("ModeNone_IgnoresEnv", func(t *testing.T) {
		fn := ProxyFunc(ModeNone)
		if fn != nil {
			t.Fatal("expected nil proxy func for ModeNone")
		}
	})
}

// TestMain ensures proxy env vars are cleared before tests run.
func TestMain(m *testing.M) {
	os.Unsetenv("HTTP_PROXY")
	os.Unsetenv("http_proxy")
	os.Unsetenv("HTTPS_PROXY")
	os.Unsetenv("https_proxy")
	os.Unsetenv("NO_PROXY")
	os.Unsetenv("no_proxy")
	m.Run()
}
