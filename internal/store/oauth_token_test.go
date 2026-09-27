package store

import (
	"testing"
	"time"

	"tresor/internal/config"
)

func TestOAuthToken_SaveGetDelete(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{Name: "ChatGPT", BaseURL: "https://chatgpt.com/backend-api/codex"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create downstream: %v", err)
	}

	// No token yet
	got, err := s.GetOAuthToken(ds.ID)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil token, got %+v", got)
	}

	tok := &OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "chatgpt",
		Flow:         "device",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Extra:        map[string]interface{}{"account_id": "acct_123"},
	}
	if err := s.SaveOAuthToken(tok); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err = s.GetOAuthToken(ds.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected token, got nil")
	}
	if got.AccessToken != "access-1" || got.RefreshToken != "refresh-1" {
		t.Fatalf("token mismatch: %+v", got)
	}
	if got.Provider != "chatgpt" || got.Flow != "device" {
		t.Fatalf("provider/flow mismatch: %+v", got)
	}
	if got.Extra["account_id"] != "acct_123" {
		t.Fatalf("extra mismatch: %+v", got.Extra)
	}
	if got.TokenType != "Bearer" {
		t.Fatalf("token type: %q", got.TokenType)
	}

	// Upsert
	tok.AccessToken = "access-2"
	if err := s.SaveOAuthToken(tok); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetOAuthToken(ds.ID)
	if got.AccessToken != "access-2" {
		t.Fatalf("upsert did not update: %q", got.AccessToken)
	}

	// ListOAuthDownstreams
	ids, err := s.ListOAuthDownstreams()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 1 || ids[0] != ds.ID {
		t.Fatalf("list: %v", ids)
	}

	// SetOAuthNeedsLogin
	if err := s.SetOAuthNeedsLogin(ds.ID, true); err != nil {
		t.Fatalf("set needs_login: %v", err)
	}
	got, _ = s.GetOAuthToken(ds.ID)
	if !got.NeedsLogin {
		t.Fatal("needs_login not set")
	}

	// Delete
	if err := s.DeleteOAuthToken(ds.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = s.GetOAuthToken(ds.ID)
	if got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}
}

func TestSetDownstreamAuth(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{Name: "X", BaseURL: "https://x.test"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, _ := s.GetDownstream(ds.ID)
	if got.Auth == nil || got.Auth.Type != "api_key" {
		t.Fatalf("default auth type: %+v", got.Auth)
	}

	auth := &config.DownstreamAuthCfg{Type: "oauth", Flow: "device", ClientID: "chatgpt"}
	if err := s.SetDownstreamAuth(ds.ID, auth); err != nil {
		t.Fatalf("set auth: %v", err)
	}
	got, _ = s.GetDownstream(ds.ID)
	if got.Auth == nil || got.Auth.Type != "oauth" || got.Auth.ClientID != "chatgpt" {
		t.Fatalf("auth: %+v", got)
	}

	// Unknown downstream
	if err := s.SetDownstreamAuth("nope", &config.DownstreamAuthCfg{Type: "api_key"}); err == nil {
		t.Fatal("expected error for unknown downstream")
	}
}

func TestDeleteDownstream_CascadesOAuthToken(t *testing.T) {
	s := newTestStore(t)

	ds := &Downstream{Name: "X", BaseURL: "https://x.test"}
	if err := s.CreateDownstream(ds); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SaveOAuthToken(&OAuthToken{
		DownstreamID: ds.ID,
		Provider:     "p",
		Flow:         "auth_code",
		AccessToken:  "a",
	}); err != nil {
		t.Fatalf("save token: %v", err)
	}

	if err := s.DeleteDownstream(ds.ID); err != nil {
		t.Fatalf("delete downstream: %v", err)
	}
	got, err := s.GetOAuthToken(ds.ID)
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if got != nil {
		t.Fatalf("oauth token should be cascaded, got %+v", got)
	}
}
