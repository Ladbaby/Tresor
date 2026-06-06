package store

import (
	"database/sql"
	"fmt"

	"tresor/internal/config"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database connection and provides access to rules and downstreams.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database and runs migrations.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for use in other store sub-packages.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS downstreams (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			base_url   TEXT NOT NULL,
			api_key    TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS rules (
			id                TEXT PRIMARY KEY,
			name              TEXT NOT NULL UNIQUE,
			pattern_path      TEXT NOT NULL,
			pattern_model     TEXT,
			active_downstream TEXT REFERENCES downstreams(id),
			pipeline_config   TEXT NOT NULL DEFAULT '[]',
			is_enabled        INTEGER DEFAULT 1,
			created_at        DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rules_enabled ON rules(is_enabled)`,
		`CREATE TABLE IF NOT EXISTS aliases (
			id              TEXT PRIMARY KEY,
			input_model_id  TEXT NOT NULL,
			downstream_id   TEXT NOT NULL REFERENCES downstreams(id),
			output_model_id TEXT NOT NULL,
			is_active       INTEGER DEFAULT 0,
			created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_aliases_input ON aliases(input_model_id)`,
		// output_model_ids table: tracks known model names per downstream
		`CREATE TABLE IF NOT EXISTS output_model_ids (
			downstream_id TEXT NOT NULL REFERENCES downstreams(id),
			model_id      TEXT NOT NULL,
			PRIMARY KEY (downstream_id, model_id)
		)`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate query %q: %w", q[:40], err)
		}
	}
	return nil
}

// SeedDefaults populates the database with default downstreams and aliases if empty.
// This is called as a fallback when no YAML config data is provided.
// Rules are optional — model resolution uses aliases and downstream output_model_ids.
func (s *Store) SeedDefaults() error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM downstreams").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	type defaultDS struct {
		ID, Name, BaseURL, APIKey string
		Models                     []string
	}
	defaults := []defaultDS{
		{"openai-gpt4o", "OpenAI GPT-4o", "https://api.openai.com/v1", "", []string{"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo"}},
		{"anthropic-sonnet", "Anthropic Claude Sonnet", "https://api.anthropic.com", "", []string{"claude-sonnet-4-20250514"}},
		{"anthropic-haiku", "Anthropic Claude Haiku", "https://api.anthropic.com", "", []string{"claude-haiku-4.5"}},
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmtDS, err := tx.Prepare("INSERT INTO downstreams (id, name, base_url, api_key) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmtDS.Close()

	stmtModel, err := tx.Prepare("INSERT OR IGNORE INTO output_model_ids (downstream_id, model_id) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("seed model prepare: %w", err)
	}
	defer stmtModel.Close()

	for _, d := range defaults {
		if _, err := stmtDS.Exec(d.ID, d.Name, d.BaseURL, d.APIKey); err != nil {
			return fmt.Errorf("seed downstream %s: %w", d.ID, err)
		}
		for _, m := range d.Models {
			if _, err := stmtModel.Exec(d.ID, m); err != nil {
				return fmt.Errorf("seed model %s for downstream %s: %w", m, d.ID, err)
			}
		}
	}

	// Seed sample alias groups
	defaultAliases := []struct {
		ID, InputModel, DownstreamID, OutputModel string
		Active                                      int
	}{
		{"alias-gpt4o-openai", "gpt-4o", "openai-gpt4o", "gpt-4o", 1},
		{"alias-gpt4o-anthropic", "gpt-4o", "anthropic-sonnet", "claude-sonnet-4-20250514", 0},
		{"alias-sonnet-anthropic", "claude-sonnet", "anthropic-sonnet", "claude-sonnet-4-20250514", 1},
	}

	stmtAlias, err := tx.Prepare("INSERT INTO aliases (id, input_model_id, downstream_id, output_model_id, is_active) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("seed alias prepare: %w", err)
	}
	defer stmtAlias.Close()

	for _, a := range defaultAliases {
		if _, err := stmtAlias.Exec(a.ID, a.InputModel, a.DownstreamID, a.OutputModel, a.Active); err != nil {
			return fmt.Errorf("seed alias %s: %w", a.ID, err)
		}
	}

	return tx.Commit()
}

// LoadConfigData upserts downstreams, rules, and aliases from the YAML config
// into the SQLite database. Existing rows (matched by ID) are updated; new rows
// are inserted. If all three slices are empty, SeedDefaults is called as fallback.
func (s *Store) LoadConfigData(cfg *config.AppConfig) error {
	if len(cfg.Downstreams) == 0 && len(cfg.Rules) == 0 && len(cfg.Aliases) == 0 {
		return s.SeedDefaults()
	}

	if err := s.upsertDownstreams(cfg.Downstreams); err != nil {
		return fmt.Errorf("load downstreams: %w", err)
	}
	if err := s.upsertRules(cfg.Rules); err != nil {
		return fmt.Errorf("load rules: %w", err)
	}
	if err := s.upsertAliases(cfg.Aliases); err != nil {
		return fmt.Errorf("load aliases: %w", err)
	}

	return nil
}
