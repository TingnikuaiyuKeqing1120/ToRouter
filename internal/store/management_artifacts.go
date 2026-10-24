package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

const ManagementArtifactMaxBytes = 16 * 1024 * 1024
const ManagementArtifactMaxCount = 20

var ErrManagementArtifactLimit = errors.New("management artifact size or count limit exceeded")

type ManagementArtifact struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	MIMEType    string    `json:"mime_type"`
	Size        int       `json:"size"`
	Permissions []string  `json:"required_permissions"`
	ExpiresAt   time.Time `json:"expires_at"`
	Content     []byte    `json:"-"`
}

func (s *Store) PutManagementArtifact(ctx context.Context, connectionID int64, name, mime string, content []byte, permissions []string) (ManagementArtifact, error) {
	var a ManagementArtifact
	if len(content) > ManagementArtifactMaxBytes {
		return a, ErrManagementArtifactLimit
	}
	id, err := randomToken("export_")
	if err != nil {
		return a, err
	}
	sealed, err := s.seal(base64.StdEncoding.EncodeToString(content))
	if err != nil {
		return a, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM management_artifacts WHERE expires_at<=?`, time.Now().Unix()); err != nil {
		return a, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM management_artifacts WHERE connection_id=?`, connectionID).Scan(&count); err != nil {
		return a, err
	}
	if count >= ManagementArtifactMaxCount {
		return a, ErrManagementArtifactLimit
	}
	a = ManagementArtifact{ID: id, Name: name, MIMEType: mime, Size: len(content), Permissions: permissions, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	rawPermissions, _ := json.Marshal(permissions)
	_, err = tx.ExecContext(ctx, `INSERT INTO management_artifacts(id,connection_id,name,mime,sealed,size,permissions,expires_at) VALUES(?,?,?,?,?,?,?,?)`, id, connectionID, name, mime, sealed, a.Size, string(rawPermissions), a.ExpiresAt.Unix())
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *Store) ReadManagementArtifact(ctx context.Context, connectionID int64, id string) (ManagementArtifact, error) {
	var a ManagementArtifact
	var sealed, rawPermissions string
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,mime,sealed,size,permissions,expires_at FROM management_artifacts WHERE connection_id=? AND id=? AND expires_at>?`, connectionID, id, time.Now().Unix()).Scan(&a.ID, &a.Name, &a.MIMEType, &sealed, &a.Size, &rawPermissions, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal([]byte(rawPermissions), &a.Permissions); err != nil {
		return a, err
	}
	plain, err := s.open(sealed)
	if err != nil {
		return a, err
	}
	a.Content, err = base64.StdEncoding.DecodeString(plain)
	a.ExpiresAt = time.Unix(expires, 0).UTC()
	return a, err
}

// CleanupManagement keeps deduplication identities even after result expiry:
// an expired request must never silently become a new consuming operation.
// Payloads follow log retention (at least 24h); exported artifacts expire in 1h.
func (s *Store) CleanupManagement(ctx context.Context, now time.Time) error {
	settings, err := s.GetSettings()
	if err != nil {
		return err
	}
	days := settings.RetentionDays
	if days < 1 {
		days = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{`DELETE FROM management_artifacts WHERE expires_at<=?`, `DELETE FROM management_previews WHERE expires_at<=?`} {
		if _, err = tx.ExecContext(ctx, query, now.Unix()); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE management_receipts SET sealed='',status='expired' WHERE created_at<? AND status<>'expired'`, now.AddDate(0, 0, -days).Unix()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM management_audit WHERE time<?`, now.AddDate(0, 0, -days).UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
