package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// ConfigurationIdentity is a host-private keyed fingerprint, stable across restarts.
// The key remains in SQLite and is never written to portable configuration.
func (s *Store) ConfigurationIdentity(data []byte) (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO settings(key,value) VALUES ('plugin_configuration_key',?)`, hex.EncodeToString(key)); err != nil {
		return "", err
	}
	var encoded string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='plugin_configuration_key'`).Scan(&encoded); err != nil {
		return "", err
	}
	key, err := hex.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return "", errors.New("invalid host configuration key")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
