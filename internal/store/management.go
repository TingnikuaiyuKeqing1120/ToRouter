package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ManagementConnection contains no recoverable token. Tokens are returned only
// by creation and rotation and are never stored alongside ordinary settings.
type ManagementConnection struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Enabled     bool       `json:"enabled"`
	Permissions []string   `json:"permissions"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}

type ManagementPatch struct {
	Name        *string   `json:"name,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
	Permissions *[]string `json:"permissions,omitempty"`
}

func (s *Store) initManagement() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS management_connections(
id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,enabled INTEGER NOT NULL,
permissions TEXT NOT NULL,hash BLOB NOT NULL UNIQUE,created_at INTEGER NOT NULL,last_used_at INTEGER);
CREATE TABLE IF NOT EXISTS management_receipts(
connection_id INTEGER NOT NULL,request_id TEXT NOT NULL,operation TEXT NOT NULL,
arguments_hash TEXT NOT NULL,status TEXT NOT NULL,sealed TEXT NOT NULL DEFAULT '',
permissions TEXT NOT NULL,created_at INTEGER NOT NULL,PRIMARY KEY(connection_id,request_id));
CREATE TABLE IF NOT EXISTS management_previews(
id TEXT PRIMARY KEY,connection_id INTEGER NOT NULL,operation TEXT NOT NULL,sealed TEXT NOT NULL,expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS management_artifacts(
id TEXT PRIMARY KEY,connection_id INTEGER NOT NULL,name TEXT NOT NULL,mime TEXT NOT NULL,
sealed TEXT NOT NULL,size INTEGER NOT NULL,permissions TEXT NOT NULL,expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS management_audit(
id INTEGER PRIMARY KEY AUTOINCREMENT,connection_id INTEGER NOT NULL,operation TEXT NOT NULL,
time INTEGER NOT NULL,result TEXT NOT NULL,targets TEXT NOT NULL,fields TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS management_audit_owner ON management_audit(connection_id,id);`)
	return err
}

func ValidateManagementPermissions(values []string) ([]string, error) {
	seen := map[string]bool{}
	for _, p := range values {
		switch p {
		case "read", "manage", "maintain", "sensitive":
			seen[p] = true
		default:
			return nil, errors.New("invalid management permission")
		}
	}
	out := []string{}
	for _, p := range []string{"read", "manage", "maintain", "sensitive"} {
		if seen[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

func managementName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", errors.New("name must contain 1..100 bytes")
	}
	return name, nil
}

func (s *Store) CreateManagementConnection(name string, permissions []string) (ManagementConnection, string, error) {
	var c ManagementConnection
	name, err := managementName(name)
	if err != nil {
		return c, "", err
	}
	if permissions == nil {
		permissions = []string{"read", "manage"}
	}
	permissions, err = ValidateManagementPermissions(permissions)
	if err != nil {
		return c, "", err
	}
	token, err := randomToken("tr_mcp_")
	if err != nil {
		return c, "", err
	}
	c = ManagementConnection{Name: name, Enabled: true, Permissions: permissions, CreatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(permissions)
	res, err := s.db.Exec(`INSERT INTO management_connections(name,enabled,permissions,hash,created_at) VALUES(?,1,?,?,?)`, name, string(raw), tokenHash(token), c.CreatedAt.Unix())
	if err != nil {
		return c, "", err
	}
	c.ID, err = res.LastInsertId()
	return c, token, err
}

type managementScanner interface{ Scan(...any) error }

func scanManagement(row managementScanner) (ManagementConnection, error) {
	var c ManagementConnection
	var raw string
	var created, last int64
	err := row.Scan(&c.ID, &c.Name, &c.Enabled, &raw, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal([]byte(raw), &c.Permissions); err != nil {
		return c, err
	}
	c.CreatedAt = time.Unix(created, 0).UTC()
	if last > 0 {
		v := time.Unix(last, 0).UTC()
		c.LastUsedAt = &v
	}
	return c, nil
}

const managementSelect = `SELECT id,name,enabled,permissions,created_at,COALESCE(last_used_at,0) FROM management_connections`

func (s *Store) ListManagementConnections() ([]ManagementConnection, error) {
	rows, err := s.db.Query(managementSelect + ` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagementConnection{}
	for rows.Next() {
		c, err := scanManagement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AuthenticateManagementConnection(token string) (ManagementConnection, error) {
	if !strings.HasPrefix(token, "tr_mcp_") {
		return ManagementConnection{}, ErrNotFound
	}
	c, err := scanManagement(s.db.QueryRow(managementSelect+` WHERE hash=? AND enabled=1`, tokenHash(token)))
	if err != nil {
		return c, err
	}
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE management_connections SET last_used_at=? WHERE id=? AND hash=? AND enabled=1`, now.Unix(), c.ID, tokenHash(token))
	if err != nil {
		return ManagementConnection{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ManagementConnection{}, err
	}
	if n == 0 {
		return ManagementConnection{}, ErrNotFound
	}
	c.LastUsedAt = &now
	return c, nil
}

func (s *Store) UpdateManagementConnection(id int64, patch ManagementPatch) (ManagementConnection, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ManagementConnection{}, err
	}
	defer tx.Rollback()
	c, err := scanManagement(tx.QueryRow(managementSelect+` WHERE id=?`, id))
	if err != nil {
		return c, err
	}
	if patch.Name != nil {
		c.Name, err = managementName(*patch.Name)
		if err != nil {
			return c, err
		}
	}
	if patch.Enabled != nil {
		c.Enabled = *patch.Enabled
	}
	if patch.Permissions != nil {
		c.Permissions, err = ValidateManagementPermissions(*patch.Permissions)
		if err != nil {
			return c, err
		}
	}
	raw, _ := json.Marshal(c.Permissions)
	_, err = tx.Exec(`UPDATE management_connections SET name=?,enabled=?,permissions=? WHERE id=?`, c.Name, c.Enabled, string(raw), id)
	if err != nil {
		return c, err
	}
	return c, tx.Commit()
}

func (s *Store) DeleteManagementConnection(id int64) error {
	res, err := s.db.Exec(`DELETE FROM management_connections WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) RotateManagementConnection(id int64) (ManagementConnection, string, error) {
	token, err := randomToken("tr_mcp_")
	if err != nil {
		return ManagementConnection{}, "", err
	}
	res, err := s.db.Exec(`UPDATE management_connections SET hash=? WHERE id=?`, tokenHash(token), id)
	if err != nil {
		return ManagementConnection{}, "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ManagementConnection{}, "", err
	}
	if n == 0 {
		return ManagementConnection{}, "", ErrNotFound
	}
	c, err := scanManagement(s.db.QueryRow(managementSelect+` WHERE id=?`, id))
	return c, token, err
}
