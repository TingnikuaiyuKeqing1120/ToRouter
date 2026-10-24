package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrManagementConflict = errors.New("management request_id already used with different arguments")

type ManagementReceipt struct {
	RequestID   string          `json:"request_id"`
	Operation   string          `json:"operation"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	Result      json.RawMessage `json:"result,omitempty"`
	Permissions []string        `json:"required_permissions"`
}

// BeginManagementReceipt reserves an operation before effects can occur. An
// existing unfinished reservation is unknown, including after process restart:
// it must never cause automatic re-execution of an external request.
func (s *Store) BeginManagementReceipt(connectionID int64, requestID, operation, argumentsHash string, permissions []string) (ManagementReceipt, bool, error) {
	now := time.Now().UTC()
	rawPermissions, _ := json.Marshal(permissions)
	res, err := s.db.Exec(`INSERT INTO management_receipts(connection_id,request_id,operation,arguments_hash,status,permissions,created_at) VALUES(?,?,?,?,'unknown',?,?) ON CONFLICT(connection_id,request_id) DO NOTHING`, connectionID, requestID, operation, argumentsHash, string(rawPermissions), now.Unix())
	if err != nil {
		return ManagementReceipt{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ManagementReceipt{}, false, err
	}
	var op, hash string
	err = s.db.QueryRow(`SELECT operation,arguments_hash FROM management_receipts WHERE connection_id=? AND request_id=?`, connectionID, requestID).Scan(&op, &hash)
	if err != nil {
		return ManagementReceipt{}, false, err
	}
	if hash != argumentsHash || op != operation {
		return ManagementReceipt{}, false, ErrManagementConflict
	}
	r, err := s.ManagementReceipt(connectionID, requestID)
	return r, n > 0, err
}

func (s *Store) CompleteManagementReceipt(connectionID int64, requestID, status string, result json.RawMessage) error {
	sealed, err := s.seal(string(result))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE management_receipts SET status=?,sealed=? WHERE connection_id=? AND request_id=?`, status, sealed, connectionID, requestID)
	return err
}

func (s *Store) ManagementReceipt(connectionID int64, requestID string) (ManagementReceipt, error) {
	var r ManagementReceipt
	var sealed, permissions string
	var created int64
	err := s.db.QueryRow(`SELECT request_id,operation,status,created_at,sealed,permissions FROM management_receipts WHERE connection_id=? AND request_id=?`, connectionID, requestID).Scan(&r.RequestID, &r.Operation, &r.Status, &created, &sealed, &permissions)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.CreatedAt = time.Unix(created, 0).UTC()
	if err = json.Unmarshal([]byte(permissions), &r.Permissions); err != nil {
		return r, err
	}
	if sealed != "" {
		plain, err := s.open(sealed)
		if err != nil {
			return r, err
		}
		r.Result = json.RawMessage(plain)
	}
	return r, nil
}

func (s *Store) CreateManagementPreview(connectionID int64, operation string, payload json.RawMessage) (string, error) {
	id, err := randomToken("preview_")
	if err != nil {
		return "", err
	}
	sealed, err := s.seal(string(payload))
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM management_previews WHERE expires_at<=?`, time.Now().Unix()); err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO management_previews(id,connection_id,operation,sealed,expires_at) VALUES(?,?,?,?,?)`, id, connectionID, operation, sealed, time.Now().Add(10*time.Minute).Unix())
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// ConsumeManagementPreview returns only the payload bound to this exact
// principal and action, once. The domain must compare its target snapshots
// immediately before committing; this layer cannot invent a safe preview.
func (s *Store) ConsumeManagementPreview(connectionID int64, operation, id string) (json.RawMessage, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var sealed string
	err = tx.QueryRow(`SELECT sealed FROM management_previews WHERE id=? AND connection_id=? AND operation=? AND expires_at>?`, id, connectionID, operation, time.Now().Unix()).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.open(sealed)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`DELETE FROM management_previews WHERE id=?`, id); err != nil {
		return nil, err
	}
	return json.RawMessage(plain), tx.Commit()
}
