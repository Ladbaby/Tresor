package store

// SaveSessionToken persists a single admin session token. It is idempotent
// (INSERT OR IGNORE), so calling it on an existing token is a no-op.
func (s *Store) SaveSessionToken(token string) error {
	if token == "" {
		return nil
	}
	_, err := s.db.Exec("INSERT OR IGNORE INTO sessions (token) VALUES (?)", token)
	return err
}

// DeleteSessionToken removes a single admin session token. An empty string
// removes every row in the sessions table (used by password change).
func (s *Store) DeleteSessionToken(token string) error {
	if token == "" {
		_, err := s.db.Exec("DELETE FROM sessions")
		return err
	}
	_, err := s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
	return err
}

// LoadAllSessionTokens returns every persisted admin session token. Used at
// daemon startup to rehydrate the in-memory token set so existing browser
// sessions survive restarts.
func (s *Store) LoadAllSessionTokens() ([]string, error) {
	rows, err := s.db.Query("SELECT token FROM sessions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tokens, nil
}