package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"tresor/internal/config"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database connection and provides access to rules and downstreams.
type Store struct {
	db *sql.DB

	// regexAliasCache caches the list of active regex aliases for fast lookup.
	// Invalidated on any alias mutation (CreateAlias, UpdateAlias, DeleteAlias,
	// DeleteGroup, ReorderGroups, upsertAliases).
	regexAliasCache struct {
		sync.RWMutex
		entries []cachedRegexAlias
	}
}

// cachedRegexAlias holds a single cached active regex alias.
type cachedRegexAlias struct {
	ID             string
	InputModelID   string
	DownstreamID   string
	OutputModelID  string
	IsRegex        bool
	GroupOrder     int
	CreatedAt      time.Time
	CompiledPattern *regexp.Regexp
}

// Open opens (or creates) the SQLite database and runs migrations.
func Open(dbPath string) (*Store, error) {
	// Ensure parent directory exists so SQLite can create the file
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

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

// invalidateRegexCache clears the regex alias cache.
// Call this after any alias mutation (Create, Update, Delete, Reorder, Upsert).
func (s *Store) invalidateRegexCache() {
	s.regexAliasCache.Lock()
	s.regexAliasCache.entries = nil
	s.regexAliasCache.Unlock()
}

// populateRegexCache loads active regex aliases from the database into the cache.
// Returns the populated cache entries.
func (s *Store) populateRegexCache() ([]cachedRegexAlias, error) {
	rows, err := s.db.Query(
		`SELECT a.id, a.input_model_id, a.downstream_id, a.output_model_id, a.is_active, a.is_regex, a.group_order, a.created_at
		 FROM aliases a
		 WHERE a.is_active = 1
		   AND (a.is_regex = 1
		     OR EXISTS (
		       SELECT 1 FROM aliases a2
		       WHERE a2.input_model_id = a.input_model_id
		         AND a2.is_regex = 1
		     ))
		 ORDER BY a.group_order, a.rowid`)
	if err != nil {
		return nil, fmt.Errorf("query regex aliases: %w", err)
	}
	defer rows.Close()

	var entries []cachedRegexAlias
	for rows.Next() {
		var id, inputModelID, downstreamID, outputModelID string
		var active, isRegex, groupOrder int
		var createdAt time.Time
		if err := rows.Scan(&id, &inputModelID, &downstreamID, &outputModelID, &active, &isRegex, &groupOrder, &createdAt); err != nil {
			return nil, err
		}
		re, err := compileRegex(inputModelID)
		if err != nil {
			// Skip aliases with invalid regex patterns
			continue
		}
		entries = append(entries, cachedRegexAlias{
			ID:             id,
			InputModelID:   inputModelID,
			DownstreamID:   downstreamID,
			OutputModelID:  outputModelID,
			IsRegex:        isRegex == 1,
			GroupOrder:     groupOrder,
			CreatedAt:      createdAt,
			CompiledPattern: re,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.regexAliasCache.Lock()
	s.regexAliasCache.entries = entries
	s.regexAliasCache.Unlock()
	return entries, nil
}

// findActiveAliasRegexCached looks for a regex-matching active alias from the cache.
// Returns nil if no match is found. Populates the cache on first use.
func (s *Store) findActiveAliasRegexCached(model string) (*Alias, error) {
	s.regexAliasCache.RLock()
	entries := s.regexAliasCache.entries
	s.regexAliasCache.RUnlock()

	// Populate cache if empty
	if entries == nil {
		var err error
		entries, err = s.populateRegexCache()
		if err != nil {
			return nil, err
		}
	}

	for _, e := range entries {
		if e.CompiledPattern.MatchString(model) {
			return &Alias{
				ID:           e.ID,
				InputModelID: e.InputModelID,
				DownstreamID: e.DownstreamID,
				OutputModelID: e.OutputModelID,
				IsActive:     true,
				IsRegex:      e.IsRegex,
				GroupOrder:   e.GroupOrder,
				CreatedAt:    e.CreatedAt,
			}, nil
		}
	}
	return nil, nil
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
			name              TEXT NOT NULL,
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

	// Remove UNIQUE constraint from rules.name (allows duplicate rule names)
	if s.hasUniqueIndex("rules", "name") {
		if err := s.migrateRemoveRulesUnique(); err != nil {
			return fmt.Errorf("migrate remove rules UNIQUE: %w", err)
		}
	}

	// Add api_format column only if it doesn't already exist (legacy migration)
	if !s.columnExists("downstreams", "api_format") {
		if _, err := s.db.Exec(`ALTER TABLE downstreams ADD COLUMN api_format TEXT DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate add api_format: %w", err)
		}
	}

	// Add api_formats column only if it doesn't already exist
	if !s.columnExists("downstreams", "api_formats") {
		if _, err := s.db.Exec(`ALTER TABLE downstreams ADD COLUMN api_formats TEXT DEFAULT '[]'`); err != nil {
			return fmt.Errorf("migrate add api_formats: %w", err)
		}
		// Migrate existing api_format values to api_formats JSON array
		rows, err := s.db.Query(`SELECT id, api_format FROM downstreams WHERE api_format != ''`)
		if err != nil {
			return fmt.Errorf("query api_format for migration: %w", err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			rows.Close()
			return fmt.Errorf("begin migration tx: %w", err)
		}
		stmt, err := tx.Prepare(`UPDATE downstreams SET api_formats = ? WHERE id = ?`)
		if err != nil {
			tx.Rollback()
			rows.Close()
			return fmt.Errorf("prepare migration stmt: %w", err)
		}
		defer stmt.Close()
		for rows.Next() {
			var id, format string
			if err := rows.Scan(&id, &format); err != nil {
				tx.Rollback()
				rows.Close()
				return fmt.Errorf("scan migration row: %w", err)
			}
			jsonBytes, err := json.Marshal([]string{format})
			if err != nil {
				tx.Rollback()
				rows.Close()
				return fmt.Errorf("marshal format for migration: %w", err)
			}
			if _, err := stmt.Exec(string(jsonBytes), id); err != nil {
				tx.Rollback()
				rows.Close()
				return fmt.Errorf("update api_formats for %s: %w", id, err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration row iteration: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration: %w", err)
		}
	}

	// Drop legacy api_format column if it exists and api_formats is now present
	if s.columnExists("downstreams", "api_format") && s.columnExists("downstreams", "api_formats") {
		if _, err := s.db.Exec(`ALTER TABLE downstreams DROP COLUMN api_format`); err != nil {
			return fmt.Errorf("migrate drop api_format: %w", err)
		}
	}

	// --- Rules: add format/downstream filter columns ---
	// Add match_format column
	if !s.columnExists("rules", "match_format") {
		if _, err := s.db.Exec(`ALTER TABLE rules ADD COLUMN match_format TEXT DEFAULT '[]'`); err != nil {
			return fmt.Errorf("migrate add match_format: %w", err)
		}
	}
	// Add match_downstream_format column
	if !s.columnExists("rules", "match_downstream_format") {
		if _, err := s.db.Exec(`ALTER TABLE rules ADD COLUMN match_downstream_format TEXT DEFAULT '[]'`); err != nil {
			return fmt.Errorf("migrate add match_downstream_format: %w", err)
		}
	}
	// Add match_downstreams column
	if !s.columnExists("rules", "match_downstreams") {
		if _, err := s.db.Exec(`ALTER TABLE rules ADD COLUMN match_downstreams TEXT DEFAULT '[]'`); err != nil {
			return fmt.Errorf("migrate add match_downstreams: %w", err)
		}
	}

	// Migrate legacy active_downstream -> match_downstreams
	if s.columnExists("rules", "active_downstream") {
		// Backfill: convert single-value active_downstream to JSON array
		rows, err := s.db.Query(`SELECT id, active_downstream FROM rules WHERE active_downstream != '' AND (match_downstreams IS NULL OR match_downstreams = '[]' OR match_downstreams = '')`)
		if err != nil {
			return fmt.Errorf("query active_downstream for migration: %w", err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			rows.Close()
			return fmt.Errorf("begin migration tx: %w", err)
		}
		stmt, err := tx.Prepare(`UPDATE rules SET match_downstreams = ? WHERE id = ?`)
		if err != nil {
			tx.Rollback()
			rows.Close()
			return fmt.Errorf("prepare migration stmt: %w", err)
		}
		defer stmt.Close()
		for rows.Next() {
			var id, ad string
			if err := rows.Scan(&id, &ad); err != nil {
				tx.Rollback()
				rows.Close()
				return fmt.Errorf("scan migration row: %w", err)
			}
			jsonBytes, err := json.Marshal([]string{ad})
			if err != nil {
				tx.Rollback()
				rows.Close()
				return fmt.Errorf("marshal match_downstreams for %s: %w", id, err)
			}
			if _, err := stmt.Exec(string(jsonBytes), id); err != nil {
				tx.Rollback()
				rows.Close()
				return fmt.Errorf("update match_downstreams for %s: %w", id, err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration row iteration: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration: %w", err)
		}

		// Drop the old index
		if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_rules_enabled`); err != nil {
			return fmt.Errorf("migrate drop idx_rules_enabled: %w", err)
		}

		// Drop legacy active_downstream column
		if _, err := s.db.Exec(`ALTER TABLE rules DROP COLUMN active_downstream`); err != nil {
			return fmt.Errorf("migrate drop active_downstream: %w", err)
		}
	}

	// Add is_regex column to aliases table (for regex input model matching)
	if !s.columnExists("aliases", "is_regex") {
		if _, err := s.db.Exec(`ALTER TABLE aliases ADD COLUMN is_regex INTEGER DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate add aliases.is_regex: %w", err)
		}
	}

	// Add group_order column to aliases table (for drag-and-drop group reordering)
	if !s.columnExists("aliases", "group_order") {
		if _, err := s.db.Exec(`ALTER TABLE aliases ADD COLUMN group_order INTEGER DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate add aliases.group_order: %w", err)
		}
		// Backfill: get distinct groups in rowid order, assign sequential orders
		rows, err := s.db.Query(`SELECT DISTINCT input_model_id FROM aliases ORDER BY (SELECT MIN(rowid) FROM aliases a2 WHERE a2.input_model_id = aliases.input_model_id)`)
		if err != nil {
			return fmt.Errorf("query aliases for backfill: %w", err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			rows.Close()
			return fmt.Errorf("begin backfill tx: %w", err)
		}
		order := 1
		for rows.Next() {
			var mid string
			if err := rows.Scan(&mid); err != nil {
				tx.Rollback()
				rows.Close()
				return fmt.Errorf("scan backfill row: %w", err)
			}
			if _, err := tx.Exec("UPDATE aliases SET group_order = ? WHERE input_model_id = ?", order, mid); err != nil {
				tx.Rollback()
				rows.Close()
				return fmt.Errorf("backfill group_order for %s: %w", mid, err)
			}
			order++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			tx.Rollback()
			return fmt.Errorf("backfill iteration: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit backfill: %w", err)
		}
	}

	// Recreate index
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_rules_enabled ON rules(is_enabled)`); err != nil {
		return fmt.Errorf("migrate recreate idx_rules_enabled: %w", err)
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
		ID         string
		Name       string
		BaseURL    string
		APIKey     string
		ApiFormats []string
		Models     []string
	}
	defaults := []defaultDS{
		{"openai-gpt4o", "OpenAI GPT-4o", "https://api.openai.com/v1", "", []string{"openai"}, []string{"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo"}},
		{"anthropic-sonnet", "Anthropic Claude Sonnet", "https://api.anthropic.com", "", []string{"anthropic"}, []string{"claude-sonnet-4-20250514"}},
		{"anthropic-haiku", "Anthropic Claude Haiku", "https://api.anthropic.com", "", []string{"anthropic"}, []string{"claude-haiku-4.5"}},
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmtDS, err := tx.Prepare("INSERT INTO downstreams (id, name, base_url, api_key, api_formats) VALUES (?, ?, ?, ?, ?)")
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
		formats := d.ApiFormats
		if formats == nil {
			formats = []string{}
		}
		formatsJSON, _ := json.Marshal(formats)
		if _, err := stmtDS.Exec(d.ID, d.Name, d.BaseURL, d.APIKey, string(formatsJSON)); err != nil {
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
		GroupOrder                                  int
	}{
		{"alias-gpt4o-openai", "gpt-4o", "openai-gpt4o", "gpt-4o", 1, 1},
		{"alias-gpt4o-anthropic", "gpt-4o", "anthropic-sonnet", "claude-sonnet-4-20250514", 0, 1},
		{"alias-sonnet-anthropic", "claude-sonnet", "anthropic-sonnet", "claude-sonnet-4-20250514", 1, 2},
	}

	stmtAlias, err := tx.Prepare("INSERT INTO aliases (id, input_model_id, downstream_id, output_model_id, is_active, is_regex, group_order) VALUES (?, ?, ?, ?, ?, 0, ?)")
	if err != nil {
		return fmt.Errorf("seed alias prepare: %w", err)
	}
	defer stmtAlias.Close()

	for _, a := range defaultAliases {
		if _, err := stmtAlias.Exec(a.ID, a.InputModel, a.DownstreamID, a.OutputModel, a.Active, a.GroupOrder); err != nil {
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

// columnExists checks if a column exists on a table using PRAGMA table_info.
func (s *Store) columnExists(table, column string) bool {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dk string
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &dk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

// hasUniqueIndex checks if a UNIQUE auto-index exists for a column on a table.
func (s *Store) hasUniqueIndex(table, column string) bool {
	pattern := "sqlite_autoindex_" + table + "%"
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type = 'index' AND name LIKE ?", pattern)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var indexName string
		if err := rows.Scan(&indexName); err != nil {
			continue
		}
		var colName string
		if err := s.db.QueryRow("SELECT name FROM pragma_index_info(?) ORDER BY seqno", indexName).Scan(&colName); err != nil {
			continue
		}
		if colName == column {
			return true
		}
	}
	return false
}

// migrateRemoveRulesUnique recreates the rules table without the UNIQUE constraint on name.
func (s *Store) migrateRemoveRulesUnique() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Determine which columns exist on the current table
	cols := s.listColumns("rules")

	// Build SELECT list: use existing columns or literal defaults
	selectCols := []string{"id", "name", "pattern_path"}
	if containsStr(cols, "pattern_model") {
		selectCols = append(selectCols, "pattern_model")
	} else {
		selectCols = append(selectCols, "''")
	}
	if containsStr(cols, "active_downstream") {
		selectCols = append(selectCols, "active_downstream")
	} else {
		selectCols = append(selectCols, "''")
	}
	selectCols = append(selectCols, "pipeline_config", "is_enabled", "created_at")
	if containsStr(cols, "match_format") {
		selectCols = append(selectCols, "match_format")
	} else {
		selectCols = append(selectCols, "'[]'")
	}
	if containsStr(cols, "match_downstream_format") {
		selectCols = append(selectCols, "match_downstream_format")
	} else {
		selectCols = append(selectCols, "'[]'")
	}
	if containsStr(cols, "match_downstreams") {
		selectCols = append(selectCols, "match_downstreams")
	} else {
		selectCols = append(selectCols, "'[]'")
	}

	// Target column names for INSERT (all columns the new table has)
	insertCols := []string{"id", "name", "pattern_path", "pattern_model", "active_downstream", "pipeline_config", "is_enabled", "created_at", "match_format", "match_downstream_format", "match_downstreams"}

	selectQuery := "SELECT " + strings.Join(selectCols, ", ") + " FROM rules"

	// Create new table without UNIQUE constraint (includes all columns)
	if _, err := tx.Exec(`CREATE TABLE rules_new (
		id                TEXT PRIMARY KEY,
		name              TEXT NOT NULL,
		pattern_path      TEXT NOT NULL,
		pattern_model     TEXT,
		active_downstream TEXT REFERENCES downstreams(id),
		pipeline_config   TEXT NOT NULL DEFAULT '[]',
		is_enabled        INTEGER DEFAULT 1,
		created_at        DATETIME DEFAULT CURRENT_TIMESTAMP,
		match_format      TEXT DEFAULT '[]',
		match_downstream_format TEXT DEFAULT '[]',
		match_downstreams TEXT DEFAULT '[]'
	)`); err != nil {
		return err
	}

	// Copy data
	if _, err := tx.Exec("INSERT INTO rules_new (" + strings.Join(insertCols, ", ") + ") " + selectQuery); err != nil {
		return err
	}

	// Drop old table and rename
	if _, err := tx.Exec("DROP TABLE rules"); err != nil {
		return err
	}
	if _, err := tx.Exec("ALTER TABLE rules_new RENAME TO rules"); err != nil {
		return err
	}

	// Recreate index
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_rules_enabled ON rules(is_enabled)`); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) listColumns(table string) []string {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dk string
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &dk); err != nil {
			return nil
		}
		cols = append(cols, name)
	}
	return cols
}

// upsertDownstreams creates or updates downstreams from YAML config.
func (s *Store) upsertDownstreams(downstreams []config.DownstreamCfg) error {
	if len(downstreams) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, d := range downstreams {
		formats := d.ApiFormats
		if formats == nil {
			formats = []string{}
		}
		formatsJSON, err := json.Marshal(formats)
		if err != nil {
			return fmt.Errorf("marshal api_formats for %s: %w", d.ID, err)
		}

		// Check if this downstream already exists
		var exists bool
		if err := tx.QueryRow("SELECT COUNT(*) > 0 FROM downstreams WHERE id = ?", d.ID).Scan(&exists); err != nil {
			return fmt.Errorf("check downstream %s: %w", d.ID, err)
		}

		if exists {
			if _, err := tx.Exec(
				"UPDATE downstreams SET name = ?, base_url = ?, api_key = ?, api_formats = ? WHERE id = ?",
				d.Name, d.BaseURL, d.APIKey, string(formatsJSON), d.ID); err != nil {
				return fmt.Errorf("update downstream %s: %w", d.ID, err)
			}
			// Replace output_model_ids from YAML
			if _, err := tx.Exec("DELETE FROM output_model_ids WHERE downstream_id = ?", d.ID); err != nil {
				return fmt.Errorf("clear model ids for %s: %w", d.ID, err)
			}
			if len(d.OutputModelIDs) > 0 {
				stmt, err := tx.Prepare("INSERT INTO output_model_ids (downstream_id, model_id) VALUES (?, ?)")
				if err != nil {
					return fmt.Errorf("prepare model insert: %w", err)
				}
				for _, m := range d.OutputModelIDs {
					if _, err := stmt.Exec(d.ID, m); err != nil {
						stmt.Close()
						return fmt.Errorf("insert model %s: %w", m, err)
					}
				}
				stmt.Close()
			}
		} else {
			if _, err := tx.Exec(
				"INSERT INTO downstreams (id, name, base_url, api_key, api_formats) VALUES (?, ?, ?, ?, ?)",
				d.ID, d.Name, d.BaseURL, d.APIKey, string(formatsJSON)); err != nil {
				return fmt.Errorf("insert downstream %s: %w", d.ID, err)
			}
			if len(d.OutputModelIDs) > 0 {
				stmt, err := tx.Prepare("INSERT INTO output_model_ids (downstream_id, model_id) VALUES (?, ?)")
				if err != nil {
					return fmt.Errorf("prepare model insert: %w", err)
				}
				for _, m := range d.OutputModelIDs {
					if _, err := stmt.Exec(d.ID, m); err != nil {
						stmt.Close()
						return fmt.Errorf("insert model %s: %w", m, err)
					}
				}
				stmt.Close()
			}
		}
	}

	return tx.Commit()
}
