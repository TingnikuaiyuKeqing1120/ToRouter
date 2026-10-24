package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type ClientKey struct {
	Scope      ClientKeyScope `json:"scope"`
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	Prefix     string         `json:"prefix"`
	CreatedAt  time.Time      `json:"created_at"`
	LastUsedAt *time.Time     `json:"last_used_at"`
	Generation uint64         `json:"-"`
	Hash       []byte         `json:"-"`
}

func tokenHash(token string) []byte { h := sha256.Sum256([]byte(token)); return h[:] }
func randomToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}
func (s *Store) initCredentials() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS client_keys(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,prefix TEXT NOT NULL,hash BLOB NOT NULL UNIQUE,created_at INTEGER NOT NULL,last_used_at INTEGER);
 CREATE TABLE IF NOT EXISTS admin_sessions(hash BLOB PRIMARY KEY,admin_hash BLOB NOT NULL,expires_at INTEGER NOT NULL);`)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec(`ALTER TABLE client_keys ADD COLUMN sealed TEXT DEFAULT ''`)
	_, err = s.db.Exec(`ALTER TABLE client_keys ADD COLUMN scope TEXT NOT NULL DEFAULT '{"model_mode":"all","model_ids":[],"tag_rules":{}}'`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
}
func (s *Store) CreateClientKey(name string) (ClientKey, string, error) {
	return s.CreateClientKeyWithScope(name, DefaultClientKeyScope())
}
func (s *Store) CreateClientKeyWithScope(name string, scope ClientKeyScope) (ClientKey, string, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	scope, err := s.validateScope(scope)
	if err != nil {
		return ClientKey{}, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return ClientKey{}, "", errors.New("name must contain 1..100 bytes")
	}
	token, err := randomToken("tr_client_")
	if err != nil {
		return ClientKey{}, "", err
	}
	sealed, err := s.seal(token)
	if err != nil {
		return ClientKey{}, "", err
	}
	k := ClientKey{Scope: scope, Name: name, Prefix: token[:18], CreatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(scope)
	result, err := s.db.Exec("INSERT INTO client_keys(name,prefix,hash,created_at,sealed,scope) VALUES(?,?,?,?,?,?)", k.Name, k.Prefix, tokenHash(token), k.CreatedAt.Unix(), sealed, string(raw))
	if err != nil {
		return ClientKey{}, "", err
	}
	k.ID, err = result.LastInsertId()
	return k, token, err
}
func (s *Store) ListClientKeys() ([]ClientKey, error) {
	rows, err := s.db.Query("SELECT id,name,prefix,created_at,COALESCE(last_used_at,0),scope FROM client_keys ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []ClientKey{}
	for rows.Next() {
		var k ClientKey
		var created, last int64
		var raw string
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &created, &last, &raw); err != nil {
			return nil, err
		}
		k.Scope, err = decodeClientScope(raw)
		if err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(created, 0).UTC()
		if last > 0 {
			v := time.Unix(last, 0).UTC()
			k.LastUsedAt = &v
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
func (s *Store) AuthenticateClientKey(token string) (ClientKey, error) {
	var k ClientKey
	var created int64
	var raw string
	err := s.db.QueryRow("SELECT id,name,prefix,created_at,scope FROM client_keys WHERE hash=?", tokenHash(token)).Scan(&k.ID, &k.Name, &k.Prefix, &created, &raw)
	if err != nil {
		return k, err
	}
	k.Scope, err = decodeClientScope(raw)
	if err != nil {
		return ClientKey{}, err
	}
	k.CreatedAt = time.Unix(created, 0).UTC()
	k.Generation = s.generation.Load()
	k.Hash = tokenHash(token)
	now := time.Now().UTC()
	k.LastUsedAt = &now
	res, err := s.db.Exec("UPDATE client_keys SET last_used_at=? WHERE id=?", now.Unix(), k.ID)
	if err != nil {
		return ClientKey{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ClientKey{}, err
	}
	if n == 0 {
		return ClientKey{}, ErrNotFound
	}
	return k, nil
}
func (s *Store) DeleteClientKey(id int64) error {
	return s.DeleteClientKeyAtVersion(id, "")
}
func (s *Store) DeleteClientKeyAtVersion(id int64, expected string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if expected != "" {
		rows, err := s.ListClientKeys()
		if err != nil {
			return err
		}
		found := false
		for _, key := range rows {
			if key.ID == id {
				found = true
				if ClientKeyVersion(key) != expected {
					return ErrEditConflict
				}
			}
		}
		if !found {
			return ErrNotFound
		}
	}
	res, err := s.db.Exec("DELETE FROM client_keys WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.generation.Add(1)
	return nil
}
func (s *Store) RevealClientKey(id int64) (string, error) {
	var sealed string
	err := s.db.QueryRow("SELECT COALESCE(sealed,'') FROM client_keys WHERE id=?", id).Scan(&sealed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if strings.TrimSpace(sealed) == "" {
		return "", ErrNotFound
	}
	return s.open(sealed)
}
func (s *Store) CreateAdminSession(adminKey string, lifetime time.Duration) (string, error) {
	token, err := randomToken("tr_session_")
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec("DELETE FROM admin_sessions WHERE expires_at<=?", time.Now().Unix())
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec("INSERT INTO admin_sessions(hash,admin_hash,expires_at) VALUES(?,?,?)", tokenHash(token), tokenHash(adminKey), time.Now().Add(lifetime).Unix())
	return token, err
}
func (s *Store) ValidAdminSession(token string) bool {
	settings, err := s.GetSettings()
	if err != nil {
		return false
	}
	var exists int
	err = s.db.QueryRow("SELECT 1 FROM admin_sessions WHERE hash=? AND admin_hash=? AND expires_at>?", tokenHash(token), tokenHash(settings.GatewayKey), time.Now().Unix()).Scan(&exists)
	return err == nil && exists == 1
}
func (s *Store) DeleteAdminSession(token string) error {
	_, err := s.db.Exec("DELETE FROM admin_sessions WHERE hash=?", tokenHash(token))
	return err
}
