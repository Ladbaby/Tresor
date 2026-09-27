package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tresor/internal/config"
)

// OAuthToken holds the OAuth credential state for one downstream.
// Rows are keyed by downstream_id: a downstream has at most one OAuth
// connection at a time. Secrets live in SQLite only — they are never
// written to the YAML config.
type OAuthToken struct {
	DownstreamID string `json:"downstream_id"`
	Provider     string `json:"provider"`
	Flow         string `json:"flow"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	// ExpiresAt is a unix-seconds expiry; 0 means "unknown" (treated as
	// not-expiring unless the token itself carries an exp claim).
	ExpiresAt int64 `json:"expires_at"`
	// Extra holds provider-specific values (e.g. ChatGPT account_id,
	// discovered token endpoint) as a free-form JSON object.
	Extra      map[string]interface{} `json:"extra,omitempty"`
	NeedsLogin bool                   `json:"needs_login"`
	UpdatedAt  time.Time              `json:"updated_at"`
}

// SaveOAuthToken upserts a token row for a downstream.
func (s *Store) SaveOAuthToken(t *OAuthToken) error {
	extraJSON := "{}"
	if t.Extra != nil {
		b, err := json.Marshal(t.Extra)
		if err != nil {
			return fmt.Errorf("marshal oauth extra: %w", err)
		}
		extraJSON = string(b)
	}
	if t.TokenType == "" {
		t.TokenType = "Bearer"
	}
	needsLogin := 0
	if t.NeedsLogin {
		needsLogin = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO oauth_tokens
		    (downstream_id, provider, flow, access_token, refresh_token,
		     token_type, expires_at, extra, needs_login, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(downstream_id) DO UPDATE SET
		    provider      = excluded.provider,
		    flow          = excluded.flow,
		    access_token  = excluded.access_token,
		    refresh_token = excluded.refresh_token,
		    token_type    = excluded.token_type,
		    expires_at    = excluded.expires_at,
		    extra         = excluded.extra,
		    needs_login   = excluded.needs_login,
		    updated_at    = CURRENT_TIMESTAMP
	`, t.DownstreamID, t.Provider, t.Flow, t.AccessToken, t.RefreshToken,
		t.TokenType, t.ExpiresAt, extraJSON, needsLogin)
	if err != nil {
		return fmt.Errorf("save oauth token for %s: %w", t.DownstreamID, err)
	}
	return nil
}

// SetOAuthNeedsLogin marks (or unmarks) a downstream's OAuth connection as
// requiring a fresh login. Used when a refresh fails permanently.
func (s *Store) SetOAuthNeedsLogin(downstreamID string, needsLogin bool) error {
	v := 0
	if needsLogin {
		v = 1
	}
	res, err := s.db.Exec("UPDATE oauth_tokens SET needs_login = ? WHERE downstream_id = ?", v, downstreamID)
	if err != nil {
		return fmt.Errorf("set oauth needs_login for %s: %w", downstreamID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no oauth token for downstream %s", downstreamID)
	}
	return nil
}

// GetOAuthToken returns the token row for a downstream. Returns (nil, nil)
// when the downstream has no OAuth connection.
func (s *Store) GetOAuthToken(downstreamID string) (*OAuthToken, error) {
	var t OAuthToken
	var refreshToken, extraJSON, tokenType, provider, flow, accessToken string
	var needsLogin int
	err := s.db.QueryRow(
		`SELECT downstream_id, provider, flow, access_token, refresh_token,
		        token_type, expires_at, extra, needs_login, updated_at
		 FROM oauth_tokens WHERE downstream_id = ?`, downstreamID).
		Scan(&t.DownstreamID, &provider, &flow, &accessToken, &refreshToken,
			&tokenType, &t.ExpiresAt, &extraJSON, &needsLogin, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get oauth token for %s: %w", downstreamID, err)
	}
	t.Provider = provider
	t.Flow = flow
	t.AccessToken = accessToken
	t.RefreshToken = refreshToken
	t.TokenType = tokenType
	t.NeedsLogin = needsLogin == 1
	t.Extra = map[string]interface{}{}
	if extraJSON != "" && extraJSON != "{}" {
		if err := json.Unmarshal([]byte(extraJSON), &t.Extra); err != nil {
			return nil, fmt.Errorf("parse oauth extra for %s: %w", downstreamID, err)
		}
	}
	return &t, nil
}

// DeleteOAuthToken removes a downstream's OAuth connection.
// Idempotent: no error when no row exists.
func (s *Store) DeleteOAuthToken(downstreamID string) error {
	_, err := s.db.Exec("DELETE FROM oauth_tokens WHERE downstream_id = ?", downstreamID)
	if err != nil {
		return fmt.Errorf("delete oauth token for %s: %w", downstreamID, err)
	}
	return nil
}

// ListOAuthDownstreams returns the IDs of all downstreams that have an
// OAuth token row. Used by the background refresher.
func (s *Store) ListOAuthDownstreams() ([]string, error) {
	rows, err := s.db.Query("SELECT downstream_id FROM oauth_tokens")
	if err != nil {
		return nil, fmt.Errorf("list oauth downstreams: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, rows.Err()
}

// SetDownstreamAuthJSON sets the downstream's unified auth column directly
// (used by the OAuth manager when starting a login or reverting to api_key).
func (s *Store) SetDownstreamAuthJSON(downstreamID, authJSON string) error {
	res, err := s.db.Exec(
		"UPDATE downstreams SET auth = ? WHERE id = ?",
		authJSON, downstreamID)
	if err != nil {
		return fmt.Errorf("set auth for downstream %s: %w", downstreamID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("downstream %s not found", downstreamID)
	}
	return nil
}

// SetDownstreamAuth sets the downstream's auth from a DownstreamAuthCfg.
// Convenience wrapper over SetDownstreamAuthJSON used by the OAuth manager
// and the API layer.
func (s *Store) SetDownstreamAuth(downstreamID string, auth *config.DownstreamAuthCfg) error {
	b, err := json.Marshal(auth)
	if err != nil {
		return fmt.Errorf("marshal auth for %s: %w", downstreamID, err)
	}
	return s.SetDownstreamAuthJSON(downstreamID, string(b))
}
