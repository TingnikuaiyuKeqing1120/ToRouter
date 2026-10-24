package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const UsageManagementBatchLimit = 1000

var ErrAmountCorrectionConflict = errors.New("amount correction conflict: preview again")

// InitUsageManagement creates only the small preview/audit tables in the same
// database. Authorization belongs to the management adapter, not this store.
func (s *Store) InitUsageManagement() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS amount_correction_previews(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS amount_correction_changes(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS amount_correction_versions(target TEXT NOT NULL, metric TEXT NOT NULL, revision TEXT NOT NULL, PRIMARY KEY(target,metric));`)
	return err
}

// CleanupUsageManagement follows the existing log retention policy. Preview
// IDs expire after ten minutes; applied receipts remain available for the full
// configured retention period and can still answer idempotent retries.
func (s *Store) CleanupUsageManagement(ctx context.Context, now time.Time) error {
	settings, err := s.GetSettings()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM amount_correction_previews WHERE julianday(json_extract(data,'$.expires_at'))<=julianday(?)", now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM amount_correction_changes WHERE julianday(json_extract(data,'$.applied_at'))<julianday(?)", now.AddDate(0, 0, -settings.RetentionDays).UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM amount_correction_versions WHERE target NOT IN
 (SELECT 'usage:'||id||':' FROM usage_logs UNION ALL SELECT 'plan_charge:'||channel_id||':'||month FROM plan_charges)`); err != nil {
		return err
	}
	return tx.Commit()
}

type UsagePageRequest struct {
	Filter UsageFilter `json:"filter"`
	Cursor string      `json:"cursor,omitempty"`
	Limit  int         `json:"limit,omitempty"`
}

type UsagePage struct {
	Items         []Usage     `json:"items"`
	HasMore       bool        `json:"has_more"`
	NextCursor    string      `json:"next_cursor,omitempty"`
	SnapshotMaxID int64       `json:"snapshot_max_id"`
	Filter        UsageFilter `json:"filter"`
}

type usageCursor struct {
	MaxID      int64  `json:"max_id"`
	BeforeID   int64  `json:"before_id"`
	FilterHash string `json:"filter_hash"`
}

func usageFilterHash(f UsageFilter) string {
	f.Limit = 0
	// Match the SQL boundary precision and normalize equivalent instants.
	if !f.From.IsZero() {
		f.From = f.From.UTC().Truncate(1000000)
	}
	if !f.To.IsZero() {
		f.To = f.To.UTC().Truncate(1000000)
	}
	raw, _ := json.Marshal(f)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateManagementFilter(f UsageFilter) error {
	if f.ChannelID < 0 || (!f.From.IsZero() && !f.To.IsZero() && f.From.After(f.To)) {
		return errors.New("invalid usage filter")
	}
	return nil
}

// ListUsagePage traverses all matching logs without the legacy ListUsage cap.
// The ID cutoff freezes membership; amounts can still reflect later corrections.
func (s *Store) ListUsagePage(ctx context.Context, req UsagePageRequest) (UsagePage, error) {
	out := UsagePage{Items: []Usage{}, Filter: req.Filter}
	out.Filter.Limit = 0
	if err := validateManagementFilter(req.Filter); err != nil {
		return out, err
	}
	if req.Limit == 0 {
		req.Limit = 100
	}
	if req.Limit < 1 || req.Limit > UsageManagementBatchLimit {
		return out, errors.New("page limit must be between 1 and 1000")
	}
	cur := usageCursor{FilterHash: usageFilterHash(req.Filter)}
	if req.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		if err != nil {
			return out, errors.New("invalid usage cursor")
		}
		if err = json.Unmarshal(raw, &cur); err != nil || cur.MaxID < 0 || cur.BeforeID <= 0 || cur.BeforeID > cur.MaxID || cur.FilterHash != usageFilterHash(req.Filter) {
			return out, errors.New("usage cursor does not match filter or snapshot")
		}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if req.Cursor == "" {
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM usage_logs").Scan(&cur.MaxID); err != nil {
			return out, err
		}
	}
	out.SnapshotMaxID = cur.MaxID
	where, args := usageWhere(req.Filter)
	where += " AND id<=?"
	args = append(args, cur.MaxID)
	if cur.BeforeID > 0 {
		where += " AND id<?"
		args = append(args, cur.BeforeID)
	}
	args = append(args, req.Limit+1)
	rows, err := tx.QueryContext(ctx, "SELECT id,data FROM usage_logs"+where+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id int64
		var raw string
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var u Usage
		if err = json.Unmarshal([]byte(raw), &u); err != nil {
			break
		}
		u.ID = id
		out.Items = append(out.Items, u)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Items) > req.Limit {
		out.HasMore = true
		out.Items = out.Items[:req.Limit]
		cur.BeforeID = out.Items[len(out.Items)-1].ID
		raw, _ := json.Marshal(cur)
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, tx.Commit()
}

func usageByIDInTx(ctx context.Context, tx *sql.Tx, id int64) (Usage, string, error) {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT data FROM usage_logs WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Usage{}, "", ErrNotFound
	}
	if err != nil {
		return Usage{}, "", err
	}
	var u Usage
	if err = json.Unmarshal([]byte(raw), &u); err != nil {
		return u, raw, err
	}
	u.ID = id
	return u, raw, nil
}

func (s *Store) GetUsageByID(ctx context.Context, id int64) (Usage, error) {
	if id <= 0 {
		return Usage{}, errors.New("usage id must be positive")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Usage{}, err
	}
	defer tx.Rollback()
	u, _, err := usageByIDInTx(ctx, tx, id)
	if err != nil {
		return u, err
	}
	return u, tx.Commit()
}

func correctionTargetKey(kind string, id int64, month string) string {
	return fmt.Sprintf("%s:%d:%s", kind, id, month)
}

func correctionRevision(ctx context.Context, tx *sql.Tx, target, metric string) (string, error) {
	var revision string
	err := tx.QueryRowContext(ctx, "SELECT revision FROM amount_correction_versions WHERE target=? AND metric=?", target, metric).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return revision, err
}

func correctionText(value, field string) error {
	if strings.TrimSpace(value) == "" || len(value) > 2000 {
		return fmt.Errorf("%s must be nonempty and at most 2000 bytes", field)
	}
	return nil
}
