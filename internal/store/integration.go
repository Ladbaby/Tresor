package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
)

var (
	ErrIntegrationNotFound = errors.New("integration execution not found or inactive")
	ErrInferenceConflict   = errors.New("managed inference identity conflict")
	ErrToolCallConflict    = errors.New("managed tool call conflict")
)

type ManagedInference struct {
	ExecutionID      string
	Epoch            int64
	InferenceID      string
	PayloadHash      []byte
	Status           string
	Codec            string
	Route            json.RawMessage
	Snapshot         json.RawMessage
	ResponseRevision int64
	Response         []byte
}

type ManagedSegment struct {
	ExecutionID, InferenceID          string
	Epoch                             int64
	Order                             int
	AnchorHash, FullJSON, VisibleJSON []byte
	Provenance                        string
	PendingClientIDs                  []string
}

type ManagedToolCall struct {
	ExecutionID, InferenceID string
	Epoch                    int64
	Generation               int
	CallID                   string
	ArgumentHash             []byte
	OperationID              string
	Status                   string
	Result                   []byte
	Error                    string
}

func (s *Store) BeginManagedInference(in ManagedInference, canonicalPayload []byte) (*ManagedInference, bool, error) {
	if in.ExecutionID == "" || in.InferenceID == "" || in.Epoch < 0 || in.Status == "" || in.Codec == "" {
		return nil, false, errors.New("invalid managed inference")
	}
	h := sha256.Sum256(canonicalPayload)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var old ManagedInference
	var route, snapshot string
	err = tx.QueryRow(`SELECT execution_id,epoch,inference_id,payload_hash,status,codec,route_json,snapshot_json,response_revision,response FROM managed_inferences WHERE execution_id=? AND epoch=? AND inference_id=?`, in.ExecutionID, in.Epoch, in.InferenceID).Scan(&old.ExecutionID, &old.Epoch, &old.InferenceID, &old.PayloadHash, &old.Status, &old.Codec, &route, &snapshot, &old.ResponseRevision, &old.Response)
	if err == nil {
		if subtle.ConstantTimeCompare(old.PayloadHash, h[:]) != 1 {
			return nil, false, ErrInferenceConflict
		}
		old.Route = json.RawMessage(route)
		old.Snapshot = json.RawMessage(snapshot)
		return &old, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if len(in.Route) == 0 {
		in.Route = json.RawMessage(`{}`)
	}
	if len(in.Snapshot) == 0 {
		in.Snapshot = json.RawMessage(`{}`)
	}
	_, err = tx.Exec(`INSERT INTO managed_inferences(execution_id,epoch,inference_id,payload_hash,status,codec,route_json,snapshot_json,response_revision,response) VALUES(?,?,?,?,?,?,?,?,?,?)`, in.ExecutionID, in.Epoch, in.InferenceID, h[:], in.Status, in.Codec, string(in.Route), string(in.Snapshot), in.ResponseRevision, in.Response)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	in.PayloadHash = h[:]
	return &in, true, nil
}

func (s *Store) SaveManagedSegment(seg ManagedSegment) error {
	if seg.ExecutionID == "" || seg.InferenceID == "" || seg.Order < 0 || seg.Provenance == "" || !json.Valid(seg.FullJSON) || !json.Valid(seg.VisibleJSON) {
		return errors.New("invalid managed segment")
	}
	if seg.AnchorHash == nil {
		seg.AnchorHash = []byte{}
	}
	pending, err := json.Marshal(seg.PendingClientIDs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO managed_segments(execution_id,epoch,inference_id,segment_order,anchor_hash,full_json,visible_json,provenance,pending_client_ids) VALUES(?,?,?,?,?,?,?,?,?)`, seg.ExecutionID, seg.Epoch, seg.InferenceID, seg.Order, seg.AnchorHash, seg.FullJSON, seg.VisibleJSON, seg.Provenance, string(pending))
	return err
}

func (s *Store) ListManagedSegments(executionID, inferenceID string, epoch int64) ([]ManagedSegment, error) {
	rows, err := s.db.Query(`SELECT segment_order,anchor_hash,full_json,visible_json,provenance,pending_client_ids FROM managed_segments WHERE execution_id=? AND epoch=? AND inference_id=? ORDER BY segment_order`, executionID, epoch, inferenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManagedSegment
	for rows.Next() {
		var x ManagedSegment
		var pending string
		x.ExecutionID = executionID
		x.InferenceID = inferenceID
		x.Epoch = epoch
		if err := rows.Scan(&x.Order, &x.AnchorHash, &x.FullJSON, &x.VisibleJSON, &x.Provenance, &pending); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(pending), &x.PendingClientIDs); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) BeginManagedToolCall(c ManagedToolCall, canonicalArguments []byte) (*ManagedToolCall, bool, error) {
	if c.ExecutionID == "" || c.InferenceID == "" || c.CallID == "" || c.OperationID == "" || c.Status == "" {
		return nil, false, errors.New("invalid managed tool call")
	}
	h := sha256.Sum256(canonicalArguments)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var old ManagedToolCall
	var oldError sql.NullString
	err = tx.QueryRow(`SELECT argument_hash,operation_id,status,result,error FROM managed_tool_calls WHERE execution_id=? AND epoch=? AND inference_id=? AND generation=? AND call_id=?`, c.ExecutionID, c.Epoch, c.InferenceID, c.Generation, c.CallID).Scan(&old.ArgumentHash, &old.OperationID, &old.Status, &old.Result, &oldError)
	old.Error = oldError.String
	if err == nil {
		if subtle.ConstantTimeCompare(old.ArgumentHash, h[:]) != 1 {
			return nil, false, ErrToolCallConflict
		}
		old.ExecutionID = c.ExecutionID
		old.Epoch = c.Epoch
		old.InferenceID = c.InferenceID
		old.Generation = c.Generation
		old.CallID = c.CallID
		return &old, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	_, err = tx.Exec(`INSERT INTO managed_tool_calls(execution_id,epoch,inference_id,generation,call_id,argument_hash,operation_id,status) VALUES(?,?,?,?,?,?,?,?)`, c.ExecutionID, c.Epoch, c.InferenceID, c.Generation, c.CallID, h[:], c.OperationID, c.Status)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	c.ArgumentHash = h[:]
	return &c, true, nil
}

func (s *Store) CompleteManagedToolCall(executionID, inferenceID, callID string, epoch int64, generation int, status string, result []byte, callErr string) error {
	res, err := s.db.Exec(`UPDATE managed_tool_calls SET status=?,result=?,error=? WHERE execution_id=? AND epoch=? AND inference_id=? AND generation=? AND call_id=?`, status, result, callErr, executionID, epoch, inferenceID, generation, callID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
