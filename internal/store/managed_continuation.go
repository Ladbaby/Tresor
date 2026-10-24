package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// ListManagedExecutionSegments returns the durable transcript anchors for an execution
// across inference requests and epochs, in their creation order. Callers still validate
// visible anchors before substituting the private full transcript.
func (s *Store) ListManagedExecutionSegments(executionID string) ([]ManagedSegment, error) {
	rows, err := s.db.Query(`SELECT s.execution_id, s.epoch, s.inference_id, s.segment_order, s.anchor_hash, s.full_json,
 CASE WHEN i.status='complete' THEN s.visible_json ELSE 'null' END, s.provenance, s.pending_client_ids
 FROM managed_segments s JOIN managed_inferences i ON i.execution_id=s.execution_id AND i.epoch=s.epoch AND i.inference_id=s.inference_id
 WHERE s.execution_id=? ORDER BY s.rowid`, executionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManagedSegment
	for rows.Next() {
		var seg ManagedSegment
		var pending string
		if err := rows.Scan(&seg.ExecutionID, &seg.Epoch, &seg.InferenceID, &seg.Order, &seg.AnchorHash, &seg.FullJSON, &seg.VisibleJSON, &seg.Provenance, &pending); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(pending), &seg.PendingClientIDs); err != nil {
			return nil, err
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

func (s *Store) BindManagedClientResult(execution string, epoch int64, id string, result []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prior []byte
	err = tx.QueryRow(`SELECT result FROM managed_client_results WHERE execution_id=? AND epoch=? AND call_id=?`, execution, epoch, id).Scan(&prior)
	if err == nil {
		if string(prior) != string(result) {
			return ErrInferenceConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO managed_client_results(execution_id,epoch,call_id,result) VALUES(?,?,?,?)`, execution, epoch, id, result); err != nil {
		return err
	}
	return tx.Commit()
}

// FinalizeManagedInference atomically publishes a settled projection and replay bytes.
func (s *Store) FinalizeManagedInference(seg ManagedSegment, response []byte) error {
	pending, err := json.Marshal(seg.PendingClientIDs)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE managed_segments SET full_json=?,visible_json=?,provenance=?,pending_client_ids=? WHERE execution_id=? AND epoch=? AND inference_id=? AND segment_order=?`, seg.FullJSON, seg.VisibleJSON, seg.Provenance, string(pending), seg.ExecutionID, seg.Epoch, seg.InferenceID, seg.Order)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("managed segment missing")
	}
	result, err = tx.Exec(`UPDATE managed_inferences SET response=?,response_revision=response_revision+1,status='complete' WHERE execution_id=? AND epoch=? AND inference_id=? AND status='running'`, response, seg.ExecutionID, seg.Epoch, seg.InferenceID)
	if err != nil {
		return err
	}
	n, err = result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("invalid managed transition")
	}
	return tx.Commit()
}

func (s *Store) UpdateManagedSegment(seg ManagedSegment) error {
	pending, err := json.Marshal(seg.PendingClientIDs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE managed_segments SET full_json=?,visible_json=?,provenance=?,pending_client_ids=? WHERE execution_id=? AND epoch=? AND inference_id=? AND segment_order=?`, seg.FullJSON, seg.VisibleJSON, seg.Provenance, string(pending), seg.ExecutionID, seg.Epoch, seg.InferenceID, seg.Order)
	return err
}

// GetManagedToolCall fetches a durable tool result by its execution-scoped identity.
func (s *Store) GetManagedToolCall(executionID string, epoch int64, inferenceID string, generation int, callID string) (*ManagedToolCall, error) {
	var call ManagedToolCall
	var callErr sql.NullString
	err := s.db.QueryRow(`SELECT argument_hash, operation_id, status, result, error FROM managed_tool_calls
		WHERE execution_id=? AND epoch=? AND inference_id=? AND generation=? AND call_id=?`,
		executionID, epoch, inferenceID, generation, callID).Scan(&call.ArgumentHash, &call.OperationID, &call.Status, &call.Result, &callErr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	call.ExecutionID, call.Epoch, call.InferenceID = executionID, epoch, inferenceID
	call.Generation, call.CallID, call.Error = generation, callID, callErr.String
	return &call, nil
}
