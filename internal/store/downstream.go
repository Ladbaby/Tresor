package store

import (
	"fmt"
	"sort"
	"time"

	"encoding/json"

	"github.com/google/uuid"
)

// Downstream represents a target endpoint or model provider.
type Downstream struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	BaseURL        string    `json:"base_url"`
	APIKey         string    `json:"api_key,omitempty"`
	ApiFormats     []string  `json:"api_formats"`
	OutputModelIDs []string  `json:"output_model_ids,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// ListDownstreams returns all downstreams with their output model IDs.
func (s *Store) ListDownstreams() ([]Downstream, error) {
	rows, err := s.db.Query(
		`SELECT id, name, base_url, api_key, api_formats, created_at FROM downstreams ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list downstreams: %w", err)
	}
	defer rows.Close()

	var ds []Downstream
	for rows.Next() {
		var d Downstream
		var formatsJSON string
		if err := rows.Scan(&d.ID, &d.Name, &d.BaseURL, &d.APIKey, &formatsJSON, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.ApiFormats = []string{}
		if formatsJSON != "" && formatsJSON != "[]" {
			json.Unmarshal([]byte(formatsJSON), &d.ApiFormats)
		}
		d.OutputModelIDs = s.listOutputModelIDs(d.ID)
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

// GetDownstream returns a single downstream by ID with output model IDs.
func (s *Store) GetDownstream(id string) (*Downstream, error) {
	var d Downstream
	var formatsJSON string
	err := s.db.QueryRow(
		`SELECT id, name, base_url, api_key, api_formats, created_at FROM downstreams WHERE id = ?`, id).
		Scan(&d.ID, &d.Name, &d.BaseURL, &d.APIKey, &formatsJSON, &d.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get downstream %s: %w", id, err)
	}
	d.ApiFormats = []string{}
	if formatsJSON != "" && formatsJSON != "[]" {
		json.Unmarshal([]byte(formatsJSON), &d.ApiFormats)
	}
	d.OutputModelIDs = s.listOutputModelIDs(d.ID)
	return &d, nil
}

// CreateDownstream inserts a new downstream and its output model IDs.
func (s *Store) CreateDownstream(d *Downstream) error {
	if d.ID == "" {
		d.ID = uuid.New().String()[:8]
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	formats := d.ApiFormats
	if formats == nil {
		formats = []string{}
	}
	formatsJSON, _ := json.Marshal(formats)
	_, err = tx.Exec(
		`INSERT INTO downstreams (id, name, base_url, api_key, api_formats) VALUES (?, ?, ?, ?, ?)`,
		d.ID, d.Name, d.BaseURL, d.APIKey, string(formatsJSON))
	if err != nil {
		return fmt.Errorf("create downstream: %w", err)
	}

	if len(d.OutputModelIDs) > 0 {
		stmt, err := tx.Prepare("INSERT INTO output_model_ids (downstream_id, model_id) VALUES (?, ?)")
		if err != nil {
			return fmt.Errorf("prepare model insert: %w", err)
		}
		defer stmt.Close()
		for _, m := range d.OutputModelIDs {
			if _, err := stmt.Exec(d.ID, m); err != nil {
				return fmt.Errorf("insert model %s: %w", m, err)
			}
		}
	}

	return tx.Commit()
}

// UpdateDownstream updates an existing downstream's mutable fields.
func (s *Store) UpdateDownstream(d *Downstream) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	formats := d.ApiFormats
	if formats == nil {
		formats = []string{}
	}
	formatsJSON, _ := json.Marshal(formats)
	res, err := tx.Exec(
		`UPDATE downstreams SET name = ?, base_url = ?, api_key = ?, api_formats = ? WHERE id = ?`,
		d.Name, d.BaseURL, d.APIKey, string(formatsJSON), d.ID)
	if err != nil {
		return fmt.Errorf("update downstream: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("downstream %s not found", d.ID)
	}

	// Replace output_model_ids: delete old, insert new
	if _, err := tx.Exec("DELETE FROM output_model_ids WHERE downstream_id = ?", d.ID); err != nil {
		return fmt.Errorf("clear model ids: %w", err)
	}
	if len(d.OutputModelIDs) > 0 {
		stmt, err := tx.Prepare("INSERT INTO output_model_ids (downstream_id, model_id) VALUES (?, ?)")
		if err != nil {
			return fmt.Errorf("prepare model insert: %w", err)
		}
		defer stmt.Close()
		for _, m := range d.OutputModelIDs {
			if _, err := stmt.Exec(d.ID, m); err != nil {
				return fmt.Errorf("insert model %s: %w", m, err)
			}
		}
	}

	return tx.Commit()
}

// DeleteDownstream removes a downstream and its output model IDs. Rules
// referencing it will have their active_downstream set to empty.
func (s *Store) DeleteDownstream(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Nullify rules referencing this downstream
	if _, err := tx.Exec("UPDATE rules SET active_downstream = '' WHERE active_downstream = ?", id); err != nil {
		return err
	}
	// Delete all aliases referencing this downstream
	if _, err := tx.Exec("DELETE FROM aliases WHERE downstream_id = ?", id); err != nil {
		return err
	}
	// Delete output model IDs for this downstream
	if _, err := tx.Exec("DELETE FROM output_model_ids WHERE downstream_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM downstreams WHERE id = ?", id); err != nil {
		return err
	}

	return tx.Commit()
}

// AddOutputModelID adds a model ID to a downstream's known models.
func (s *Store) AddOutputModelID(downstreamID, modelID string) error {
	if _, err := s.GetDownstream(downstreamID); err != nil {
		return fmt.Errorf("downstream %s not found: %w", downstreamID, err)
	}
	_, err := s.db.Exec("INSERT OR IGNORE INTO output_model_ids (downstream_id, model_id) VALUES (?, ?)",
		downstreamID, modelID)
	if err != nil {
		return fmt.Errorf("add output model: %w", err)
	}
	return nil
}

// RemoveOutputModelID removes a model ID from a downstream's known models.
func (s *Store) RemoveOutputModelID(downstreamID, modelID string) error {
	if _, err := s.GetDownstream(downstreamID); err != nil {
		return fmt.Errorf("downstream %s not found: %w", downstreamID, err)
	}
	res, err := s.db.Exec("DELETE FROM output_model_ids WHERE downstream_id = ? AND model_id = ?",
		downstreamID, modelID)
	if err != nil {
		return fmt.Errorf("remove output model: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model %s not found for downstream %s", modelID, downstreamID)
	}
	return nil
}

// ListAllModels collects all known model IDs from every downstream's output_model_ids,
// plus all alias input_model_ids and output_model_ids, and returns them as a deduplicated sorted list.
func (s *Store) ListAllModels() ([]string, error) {
	modelSet := make(map[string]struct{})

	// Collect models from downstream output_model_ids
	downstreams, err := s.ListDownstreams()
	if err != nil {
		return nil, fmt.Errorf("list downstreams for models: %w", err)
	}
	for _, d := range downstreams {
		for _, m := range d.OutputModelIDs {
			modelSet[m] = struct{}{}
		}
	}

	// Collect models from aliases (both input and output)
	aliases, err := s.ListAliases()
	if err != nil {
		return nil, fmt.Errorf("list aliases for models: %w", err)
	}
	for _, a := range aliases {
		modelSet[a.InputModelID] = struct{}{}
		modelSet[a.OutputModelID] = struct{}{}
	}

	// Convert to sorted slice
	models := make([]string, 0, len(modelSet))
	for m := range modelSet {
		models = append(models, m)
	}
	sort.Strings(models)
	return models, nil
}

// FindDownstreamByOutputModel returns the first downstream whose
// output_model_ids contains the given model name. Returns nil, nil
// when no downstream claims the model. If multiple downstreams claim
// the same model, the one with the earliest created_at wins (deterministic).
func (s *Store) FindDownstreamByOutputModel(model string) (*Downstream, error) {
	var d Downstream
	var formatsJSON string
	err := s.db.QueryRow(
		`SELECT d.id, d.name, d.base_url, d.api_key, d.api_formats, d.created_at
		 FROM downstreams d
		 JOIN output_model_ids o ON o.downstream_id = d.id
		 WHERE o.model_id = ?
		 ORDER BY d.created_at ASC
		 LIMIT 1`, model).
		Scan(&d.ID, &d.Name, &d.BaseURL, &d.APIKey, &formatsJSON, &d.CreatedAt)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("find downstream by output model %s: %w", model, err)
	}
	d.ApiFormats = []string{}
	if formatsJSON != "" && formatsJSON != "[]" {
		json.Unmarshal([]byte(formatsJSON), &d.ApiFormats)
	}
	d.OutputModelIDs = s.listOutputModelIDs(d.ID)
	return &d, nil
}

// listOutputModelIDs returns the sorted list of known output model IDs for a downstream.
func (s *Store) listOutputModelIDs(downstreamID string) []string {
	rows, err := s.db.Query("SELECT model_id FROM output_model_ids WHERE downstream_id = ? ORDER BY model_id", downstreamID)
	if err != nil {
		return []string{}
	}
	defer rows.Close()

	var models []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return []string{}
		}
		models = append(models, m)
	}
	if models == nil {
		return []string{}
	}
	return models
}
