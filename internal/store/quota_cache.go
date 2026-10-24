package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// QuotaCache contains only display data and attempt metadata, never credentials.
// The identity digest keeps a replaced pool member from adopting another
// account's snapshot. Generation fences in-flight writes, not display reads:
// changing invocation order must not erase an unchanged account's quota.
type QuotaCache struct {
	Generation    int64           `json:"generation"`
	Identity      string          `json:"identity"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	LastAttemptAt time.Time       `json:"last_attempt_at"`
	Detail        string          `json:"detail,omitempty"`
}

func quotaIdentity(c Channel, credentialID string) string {
	oauth, err := quotaCredential(c, credentialID)
	if err != nil {
		return ""
	}
	identity := []string{c.CredentialType, oauth.Provider, oauth.ClientID, oauth.Account, oauth.Subject}
	// Blank account/subject values cannot establish identity. A digest of the
	// renewable credential is the fallback, never the credential itself.
	if oauth.Account == "" && oauth.Subject == "" {
		identity = append(identity, oauth.RefreshToken)
	}
	data, _ := json.Marshal(identity)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func quotaCredential(c Channel, id string) (*OAuthCredential, error) {
	if c.CredentialType == CredentialCodexOAuth && id != "" {
		for _, credential := range c.Credentials {
			if credential.ID == id && credential.OAuth != nil {
				return credential.OAuth, nil
			}
		}
	}
	return nil, errors.New("Codex credential is no longer available")
}

func quotaCacheKey(id int64, credentialID string) string {
	return "codex_quota_v2:" + strconv.FormatInt(id, 10) + ":" + credentialID
}

// Old channel-level display data cannot be assigned to a specific pool member.
func (s *Store) retireChannelQuotaCache() error {
	_, err := s.db.Exec("DELETE FROM meta WHERE key GLOB 'codex_quota_v1:*'")
	return err
}

// LoadQuotaCache always validates the live channel, including after restart.
func (s *Store) LoadQuotaCache(id int64, credentialID string) (QuotaCache, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return QuotaCache{}, err
	}
	if _, err := quotaCredential(c, credentialID); err != nil {
		return QuotaCache{}, err
	}
	var raw string
	err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", quotaCacheKey(id, credentialID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return QuotaCache{}, nil
	}
	if err != nil {
		return QuotaCache{}, err
	}
	var record QuotaCache
	if json.Unmarshal([]byte(raw), &record) != nil || record.Identity != quotaIdentity(c, credentialID) {
		return QuotaCache{}, nil
	}
	return record, nil
}

// SaveQuotaCache rejects a snapshot if configuration/identity changed during
// its request. It shares the channel write lock, without a schema migration.
func (s *Store) SaveQuotaCache(expected Channel, credentialID string, record QuotaCache) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(expected.ID)
	if err != nil {
		return err
	}
	identity := quotaIdentity(c, credentialID)
	if identity == "" || c.Generation != expected.Generation || identity != quotaIdentity(expected, credentialID) {
		return errors.New("Codex account changed during quota read")
	}
	if len(record.Snapshot) > 1<<20 {
		return errors.New("quota snapshot exceeds size limit")
	}
	record.Generation, record.Identity = c.Generation, identity
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", quotaCacheKey(c.ID, credentialID), string(raw))
	return err
}
