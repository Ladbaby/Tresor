package store

import "fmt"

// settingsKeySessionToken is the key used in the settings table to store
// the persistent admin session token.
const settingsKeySessionToken = "session_token"

// SaveSessionToken persists the session token to the settings table.
// An empty string removes any existing token.
func (s *Store) SaveSessionToken(token string) error {
	if token == "" {
		_, err := s.db.Exec("DELETE FROM settings WHERE key = ?", settingsKeySessionToken)
		return err
	}
	_, err := s.db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", settingsKeySessionToken, token)
	return err
}

// LoadSessionToken retrieves the persisted session token from the settings table.
// Returns an empty string if no token exists.
func (s *Store) LoadSessionToken() (string, error) {
	var token string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", settingsKeySessionToken).Scan(&token)
	if err != nil {
		return "", fmt.Errorf("query session token: %w", err)
	}
	return token, nil
}
