package store

import (
	"database/sql"
	"encoding/json"
)

// ManagedInferenceSnapshot retrieves immutable admission provenance without bodies.
func (s *Store) ManagedInferenceSnapshot(execution string, epoch int64, inference string) (json.RawMessage, error) {
	var snapshot string
	err := s.db.QueryRow(`SELECT snapshot_json FROM managed_inferences WHERE execution_id=? AND epoch=? AND inference_id=?`, execution, epoch, inference).Scan(&snapshot)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(snapshot), nil
}

// ManagedSegmentPending reports whether a visible call still lacks a bound outcome.
func (s *Store) ManagedSegmentPending(seg ManagedSegment) (bool, error) {
	for _, id := range seg.PendingClientIDs {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM managed_client_results WHERE execution_id=? AND epoch=? AND call_id=?`, seg.ExecutionID, seg.Epoch, id).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
	}
	return false, nil
}

// ManagedInferenceState is used to distinguish uncertain checkpoints from settled history.
func (s *Store) ManagedInferenceState(execution string, epoch int64, inference string) (string, error) {
	var status string
	err := s.db.QueryRow(`SELECT status FROM managed_inferences WHERE execution_id=? AND epoch=? AND inference_id=?`, execution, epoch, inference).Scan(&status)
	if err == sql.ErrNoRows {
		return "", ErrIntegrationNotFound
	}
	return status, err
}
