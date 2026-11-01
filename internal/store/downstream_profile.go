package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// DownstreamProfile records how a downstream was created when it came from a
// provider profile. Applied is the exact set of field values the profile last
// wrote, which is what makes a later update non-destructive: a field the user
// has changed since no longer matches Applied, so the update leaves it alone.
type DownstreamProfile struct {
	DownstreamID string `json:"downstream_id"`
	ProfileID    string `json:"profile_id"`
	Version      int    `json:"version"`
	Digest       string `json:"digest"`
	Source       string `json:"source"`
	Applied      string `json:"applied"`
}

// GetDownstreamProfile returns the provenance row for a downstream, or nil when
// the downstream was not created from a profile.
func (s *Store) GetDownstreamProfile(downstreamID string) (*DownstreamProfile, error) {
	var p DownstreamProfile
	err := s.db.QueryRow(
		`SELECT downstream_id, profile_id, version, digest, source, applied
		 FROM downstream_profiles WHERE downstream_id = ?`, downstreamID).
		Scan(&p.DownstreamID, &p.ProfileID, &p.Version, &p.Digest, &p.Source, &p.Applied)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get downstream profile %s: %w", downstreamID, err)
	}
	return &p, nil
}

// SetDownstreamProfile creates or replaces a downstream's provenance row.
func (s *Store) SetDownstreamProfile(p *DownstreamProfile) error {
	if p == nil || p.DownstreamID == "" {
		return fmt.Errorf("set downstream profile: downstream id is required")
	}
	applied := p.Applied
	if applied == "" {
		applied = "{}"
	}
	_, err := s.db.Exec(
		`INSERT INTO downstream_profiles (downstream_id, profile_id, version, digest, source, applied, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(downstream_id) DO UPDATE SET
		   profile_id = excluded.profile_id,
		   version    = excluded.version,
		   digest     = excluded.digest,
		   source     = excluded.source,
		   applied    = excluded.applied,
		   updated_at = CURRENT_TIMESTAMP`,
		p.DownstreamID, p.ProfileID, p.Version, p.Digest, p.Source, applied)
	if err != nil {
		return fmt.Errorf("set downstream profile %s: %w", p.DownstreamID, err)
	}
	return nil
}

// ListDownstreamProfiles returns every provenance row keyed by downstream id, so
// a caller listing downstreams can annotate them without a query per row.
func (s *Store) ListDownstreamProfiles() (map[string]DownstreamProfile, error) {
	rows, err := s.db.Query(
		`SELECT downstream_id, profile_id, version, digest, source, applied FROM downstream_profiles`)
	if err != nil {
		return nil, fmt.Errorf("list downstream profiles: %w", err)
	}
	defer rows.Close()

	out := map[string]DownstreamProfile{}
	for rows.Next() {
		var p DownstreamProfile
		if err := rows.Scan(&p.DownstreamID, &p.ProfileID, &p.Version, &p.Digest, &p.Source, &p.Applied); err != nil {
			return nil, err
		}
		out[p.DownstreamID] = p
	}
	return out, rows.Err()
}

// DeleteDownstreamProfile removes a downstream's provenance row.
func (s *Store) DeleteDownstreamProfile(downstreamID string) error {
	if _, err := s.db.Exec(`DELETE FROM downstream_profiles WHERE downstream_id = ?`, downstreamID); err != nil {
		return fmt.Errorf("delete downstream profile %s: %w", downstreamID, err)
	}
	return nil
}

// AliasGroupExists reports whether any alias already uses inputModelID as its
// input model. Install uses it so a provider profile never rewrites routing the
// user already set up.
func (s *Store) AliasGroupExists(inputModelID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM aliases WHERE input_model_id = ?`, inputModelID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check alias group %s: %w", inputModelID, err)
	}
	return n > 0, nil
}

// MarshalApplied serializes a snapshot for the applied column, normalizing an
// empty value to "{}" so the column always holds valid JSON.
func MarshalApplied(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal applied snapshot: %w", err)
	}
	if len(b) == 0 || string(b) == "null" {
		return "{}", nil
	}
	return string(b), nil
}
