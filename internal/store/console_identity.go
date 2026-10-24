package store

import (
	"crypto/rand"
	"encoding/hex"
)

// ConsoleIdentity is a non-secret installation identifier, independent from
// credentials. It lets a browser distinguish a restarted instance from an
// unrelated process that already occupies the requested port.
func (s *Store) ConsoleIdentity() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(key,value) VALUES('console_identity',?)`, hex.EncodeToString(random[:])); err != nil {
		return "", err
	}
	var id string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key='console_identity'`).Scan(&id)
	return id, err
}
