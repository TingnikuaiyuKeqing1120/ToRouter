package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ManagementAudit records authorization checks, not upstream requests or usage.
// Result is authorized/denied, never a raw operation error or returned secret.
type ManagementAudit struct {
	ID           int64          `json:"id"`
	ConnectionID int64          `json:"connection_id"`
	Operation    string         `json:"operation"`
	Time         time.Time      `json:"time"`
	Result       string         `json:"result"`
	Targets      map[string]any `json:"targets"`
	Fields       []string       `json:"fields"`
}
type ManagementAuditPage struct {
	Items      []ManagementAudit `json:"items"`
	HasMore    bool              `json:"has_more"`
	NextCursor int64             `json:"next_cursor,omitempty"`
}

func (s *Store) RecordManagementAudit(ctx context.Context, connectionID int64, operation, result string, targets map[string]any, fields []string) error {
	if result != "authorized" && result != "denied" {
		return errors.New("invalid management audit result")
	}
	if targets == nil {
		targets = map[string]any{}
	}
	if fields == nil {
		fields = []string{}
	}
	rawTargets, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	rawFields, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO management_audit(connection_id,operation,time,result,targets,fields) VALUES(?,?,?,?,?,?)`, connectionID, operation, time.Now().UTC().UnixMilli(), result, string(rawTargets), string(rawFields))
	return err
}

func (s *Store) ListManagementAudit(ctx context.Context, connectionID, beforeID int64, limit int) (ManagementAuditPage, error) {
	out := ManagementAuditPage{Items: []ManagementAudit{}}
	if connectionID < 1 || beforeID < 0 || limit < 0 || limit > 200 {
		return out, errors.New("invalid management audit query")
	}
	if limit == 0 {
		limit = 50
	}
	query := `SELECT id,connection_id,operation,time,result,targets,fields FROM management_audit WHERE connection_id=?`
	args := []any{connectionID}
	if beforeID > 0 {
		query += ` AND id<?`
		args = append(args, beforeID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var a ManagementAudit
		var millis int64
		var targets, fields string
		if err = rows.Scan(&a.ID, &a.ConnectionID, &a.Operation, &millis, &a.Result, &targets, &fields); err != nil {
			return out, err
		}
		a.Time = time.UnixMilli(millis).UTC()
		if err = json.Unmarshal([]byte(targets), &a.Targets); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(fields), &a.Fields); err != nil {
			return out, err
		}
		out.Items = append(out.Items, a)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > limit {
		out.HasMore = true
		out.Items = out.Items[:limit]
		out.NextCursor = out.Items[len(out.Items)-1].ID
	}
	return out, nil
}
