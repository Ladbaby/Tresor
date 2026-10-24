package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrIntegrationNotFound = errors.New("integration execution not found or inactive")
	ErrInferenceConflict   = errors.New("managed inference identity conflict")
	ErrToolCallConflict    = errors.New("managed tool call conflict")
)

type Execution struct {
	ExecutionID  string
	Service      string
	Subject      string
	AliasID      string
	AssignmentID string
	Epoch        int64
	Active       bool
}

type ExecutionRegistration struct {
	ExecutionID, Service, Subject, AliasID, AssignmentID string
	Epoch                                                int64
}

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

// RegisterExecution stores only a digest of the randomly generated bearer capability.
// The returned capability is available only to the caller of this method.
func (s *Store) RegisterExecution(r ExecutionRegistration) (string, error) {
	if r.ExecutionID == "" || r.Service == "" || r.Subject == "" || r.AliasID == "" || r.AssignmentID == "" || r.Epoch < 0 {
		return "", errors.New("invalid execution registration")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate capability: %w", err)
	}
	token := hex.EncodeToString(raw)
	h := sha256.Sum256([]byte(token))
	_, err := s.db.Exec(`INSERT INTO integration_executions(execution_id,service,subject,alias_id,assignment_id,epoch,token_hash,state) VALUES(?,?,?,?,?,?,?,'active')`, r.ExecutionID, r.Service, r.Subject, r.AliasID, r.AssignmentID, r.Epoch, h[:])
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) ActiveExecution(id string) (*Execution, error) {
	var e Execution
	err := s.db.QueryRow(`SELECT execution_id,service,subject,alias_id,assignment_id,epoch FROM integration_executions WHERE execution_id=? AND state='active'`, id).Scan(&e.ExecutionID, &e.Service, &e.Subject, &e.AliasID, &e.AssignmentID, &e.Epoch)
	if err != nil {
		return nil, ErrIntegrationNotFound
	}
	e.Active = true
	return &e, nil
}

// RevokeServiceExecution only permits the owning service to revoke a scope.
func (s *Store) RevokeServiceExecution(id, service string) error {
	result, err := s.db.Exec(`UPDATE integration_executions SET state='revoked' WHERE execution_id=? AND service=?`, id, service)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrIntegrationNotFound
	}
	return nil
}

// AuthenticateExecution verifies capability with a constant-time digest comparison and fails closed for revoked scopes.
func (s *Store) AuthenticateExecution(token string) (*Execution, error) {
	h := sha256.Sum256([]byte(token))
	var e Execution
	var stored []byte
	var state string
	err := s.db.QueryRow(`SELECT execution_id,service,subject,alias_id,assignment_id,epoch,token_hash,state FROM integration_executions WHERE token_hash=?`, h[:]).Scan(&e.ExecutionID, &e.Service, &e.Subject, &e.AliasID, &e.AssignmentID, &e.Epoch, &stored, &state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrIntegrationNotFound
		}
		return nil, err
	}
	if subtle.ConstantTimeCompare(stored, h[:]) != 1 || state != "active" {
		return nil, ErrIntegrationNotFound
	}
	e.Active = true
	return &e, nil
}

func (s *Store) RevokeExecution(executionID string) error {
	res, err := s.db.Exec(`UPDATE integration_executions SET state='revoked',revoked_at=CURRENT_TIMESTAMP WHERE execution_id=? AND state='active'`, executionID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrIntegrationNotFound
	}
	return nil
}

func (s *Store) PurgeRevokedIntegrationExecution(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var state string
	if err := tx.QueryRow(`SELECT state FROM integration_executions WHERE execution_id=?`, id).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrIntegrationNotFound
		}
		return err
	}
	if state != "revoked" {
		return errors.New("integration execution is not revoked")
	}
	var active, pending int
	if err := tx.QueryRow(`SELECT
		(SELECT COUNT(*) FROM managed_inferences WHERE execution_id=? AND status!='complete') +
		(SELECT COUNT(*) FROM managed_tool_calls WHERE execution_id=? AND status NOT IN ('complete','error'))`, id, id).Scan(&active); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM managed_segments s
		JOIN json_each(s.pending_client_ids) p
		LEFT JOIN managed_client_results r ON r.execution_id=s.execution_id AND r.epoch=s.epoch AND r.call_id=p.value
		WHERE s.execution_id=? AND r.call_id IS NULL`, id).Scan(&pending); err != nil {
		return err
	}
	if active != 0 || pending != 0 {
		return errors.New("integration execution has unresolved ledger state")
	}
	if _, err := tx.Exec(`DELETE FROM managed_tool_calls WHERE execution_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM managed_segments WHERE execution_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM managed_inferences WHERE execution_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM managed_client_results WHERE execution_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM integration_executions WHERE execution_id=? AND state='revoked'`, id); err != nil {
		return err
	}
	return tx.Commit()
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
