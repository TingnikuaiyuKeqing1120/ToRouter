package store

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type FullConfig struct {
	Version              int                   `json:"version"`
	ExportedAt           time.Time             `json:"exported_at"`
	Channels             []Channel             `json:"channels"`
	DownstreamModels     []DownstreamModel     `json:"downstream_models"`
	Settings             Settings              `json:"settings"`
	PlanPrices           []PlanPriceConfig     `json:"plan_prices,omitempty"`
	TagGroups            []TagGroup            `json:"tag_groups,omitempty"`
	Tags                 []Tag                 `json:"tags,omitempty"`
	ClientKeys           []ClientKeyRecord     `json:"client_keys,omitempty"`
	PluginPreferences    *PluginPreferences    `json:"plugin_preferences,omitempty"`
	ChannelDisplay       *ChannelDisplay       `json:"channel_display,omitempty"`
	CompanionPreferences *CompanionPreferences `json:"companion_preferences,omitempty"`
	ExchangeSources      []string              `json:"exchange_sources"`
	CatalogSources       []CatalogSourceRecord `json:"catalog_sources,omitempty"`

	unmarshaled     bool
	settingsPresent bool
}

func (c *FullConfig) UnmarshalJSON(data []byte) error {
	type Alias FullConfig
	var alias Alias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*c = FullConfig(alias)
	c.unmarshaled = true

	var probe struct {
		Channels         *json.RawMessage `json:"channels"`
		DownstreamModels *json.RawMessage `json:"downstream_models"`
		Settings         *json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}

	if probe.Channels == nil || string(*probe.Channels) == "null" {
		c.Channels = nil
	} else {
		trimmed := bytes.TrimSpace(*probe.Channels)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			c.Channels = nil
		}
	}

	if probe.DownstreamModels == nil || string(*probe.DownstreamModels) == "null" {
		c.DownstreamModels = nil
	} else {
		trimmed := bytes.TrimSpace(*probe.DownstreamModels)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			c.DownstreamModels = nil
		}
	}

	if probe.Settings != nil && string(*probe.Settings) != "null" {
		trimmed := bytes.TrimSpace(*probe.Settings)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			c.settingsPresent = true
		} else {
			c.settingsPresent = false
		}
	} else {
		c.settingsPresent = false
	}

	return nil
}

type ClientKeyRecord struct {
	Name      string         `json:"name"`
	Prefix    string         `json:"prefix"`
	Token     string         `json:"token,omitempty"`
	Hash      string         `json:"hash,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Scope     ClientKeyScope `json:"scope"`
}

type CatalogSourceRecord struct {
	Source         string `json:"source"`
	Name           string `json:"name"`
	Enabled        int    `json:"enabled"`
	PrefillEnabled int    `json:"prefill_enabled"`
	Priority       int    `json:"priority"`
}

type FullConfigSummary struct {
	ChannelsCount         int       `json:"channels_count"`
	DownstreamModelsCount int       `json:"downstream_models_count"`
	ClientKeysCount       int       `json:"client_keys_count"`
	PlanPricesCount       int       `json:"plan_prices_count"`
	TagsCount             int       `json:"tags_count"`
	ExportedAt            time.Time `json:"exported_at"`
	Version               int       `json:"version"`
}

func (c *FullConfig) Summary() FullConfigSummary {
	return FullConfigSummary{
		ChannelsCount:         len(c.Channels),
		DownstreamModelsCount: len(c.DownstreamModels),
		ClientKeysCount:       len(c.ClientKeys),
		PlanPricesCount:       len(c.PlanPrices),
		TagsCount:             len(c.Tags),
		ExportedAt:            c.ExportedAt,
		Version:               c.Version,
	}
}

// Validate verifies structural integrity, valid endpoints, and all internal references.
func (c *FullConfig) Validate() error {
	if c == nil {
		return errors.New("configuration is nil")
	}
	if c.Version != 1 {
		return fmt.Errorf("unsupported configuration version: %d", c.Version)
	}
	if c.ExportedAt.IsZero() {
		return errors.New("configuration missing exported_at timestamp")
	}
	if c.Channels == nil {
		return errors.New("configuration missing channels collection")
	}
	if c.DownstreamModels == nil {
		return errors.New("configuration missing downstream_models collection")
	}
	if c.unmarshaled && !c.settingsPresent {
		return errors.New("configuration missing settings object")
	}
	if !c.unmarshaled && c.Settings.MaxAttempts == 0 && c.Settings.RetentionDays == 0 {
		return errors.New("configuration missing settings object")
	}
	if err := validateSettingsDomain(&c.Settings); err != nil {
		return fmt.Errorf("invalid settings: %w", err)
	}

	validChannelIDs := make(map[int64]bool, len(c.Channels))

	for _, ch := range c.Channels {
		if ch.ID <= 0 {
			return fmt.Errorf("channel has invalid ID: %d", ch.ID)
		}
		if err := ValidateChannel(ch); err != nil {
			return err
		}
		validChannelIDs[ch.ID] = true
	}

	for _, ch := range c.Channels {
		for _, cred := range ch.Credentials {
			if cred.SourceChannelID > 0 && !validChannelIDs[cred.SourceChannelID] {
				return fmt.Errorf("channel %d credential %q references non-existent source channel ID %d", ch.ID, cred.ID, cred.SourceChannelID)
			}
		}
	}

	validModelIDs := make(map[string]bool, len(c.DownstreamModels))
	for _, m := range c.DownstreamModels {
		if strings.TrimSpace(m.ID) == "" {
			return errors.New("downstream model has empty ID")
		}
		validModelIDs[m.ID] = true
		for _, b := range m.Bindings {
			if !validChannelIDs[b.ChannelID] {
				return fmt.Errorf("downstream model %q references non-existent channel ID %d", m.ID, b.ChannelID)
			}
			if strings.TrimSpace(b.Model) == "" {
				return fmt.Errorf("downstream model %q has empty binding model name", m.ID)
			}
		}
	}

	for _, p := range c.PlanPrices {
		if !validChannelIDs[p.ChannelID] {
			return fmt.Errorf("plan price references non-existent channel ID %d", p.ChannelID)
		}
		if p.DefaultAmount != nil && *p.DefaultAmount < 0 {
			return fmt.Errorf("plan price for channel %d has negative amount: %f", p.ChannelID, *p.DefaultAmount)
		}
		if strings.TrimSpace(p.Currency) == "" {
			return fmt.Errorf("plan price for channel %d has empty currency", p.ChannelID)
		}
	}

	for _, pr := range c.Settings.Prices {
		if pr.ChannelID > 0 && !validChannelIDs[pr.ChannelID] {
			return fmt.Errorf("custom price references non-existent channel ID %d", pr.ChannelID)
		}
	}

	validGroupIDs := map[string]bool{
		"modality": true,
		"privacy":  true,
		"billing":  true,
	}
	for _, g := range c.TagGroups {
		if strings.TrimSpace(g.ID) == "" {
			return errors.New("tag group has empty ID")
		}
		validGroupIDs[g.ID] = true
	}
	validTagIDs := make(map[string]map[string]bool)
	for _, t := range c.Tags {
		if strings.TrimSpace(t.ID) == "" {
			return errors.New("tag has empty ID")
		}
		if !validGroupIDs[t.GroupID] {
			return fmt.Errorf("tag %q references non-existent tag group %q", t.ID, t.GroupID)
		}
		if validTagIDs[t.GroupID] == nil {
			validTagIDs[t.GroupID] = make(map[string]bool)
		}
		validTagIDs[t.GroupID][t.ID] = true
	}

	for i := range c.ClientKeys {
		k := &c.ClientKeys[i]
		if strings.TrimSpace(k.Name) == "" {
			return errors.New("client key has empty name")
		}
		if strings.TrimSpace(k.Token) == "" && strings.TrimSpace(k.Hash) == "" {
			return fmt.Errorf("client key %q has empty token and hash", k.Name)
		}
		canonical, err := canonicalScope(k.Scope)
		if err != nil {
			return err
		}
		k.Scope = canonical
		if canonical.ModelMode == "selected" {
			for _, id := range canonical.ModelIDs {
				if !validModelIDs[id] {
					return fmt.Errorf("scope model %q does not exist", id)
				}
			}
		}
		for group, ids := range canonical.TagRules {
			if !validGroupIDs[group] {
				return fmt.Errorf("scope group %q does not exist", group)
			}
			for _, id := range ids {
				if !validTagIDs[group][id] {
					return fmt.Errorf("scope tag %q does not belong to group %q", id, group)
				}
			}
		}
	}

	if c.PluginPreferences != nil {
		if c.PluginPreferences.Plugins == nil {
			return errors.New("invalid plugin preferences: plugins must be an object")
		}
		for id, capabilities := range c.PluginPreferences.Plugins {
			if err := validatePluginPreferenceName(id); err != nil || capabilities == nil {
				return fmt.Errorf("invalid plugin entry: %s", id)
			}
			for cap := range capabilities {
				if err := validatePluginPreferenceName(cap); err != nil {
					return fmt.Errorf("invalid capability entry: %s", cap)
				}
			}
		}
	}

	if c.ChannelDisplay != nil {
		for _, id := range c.ChannelDisplay.Order {
			if !validChannelIDs[id] {
				return fmt.Errorf("channel display order references non-existent channel ID %d", id)
			}
		}
		for k := range c.ChannelDisplay.Icons {
			if id, err := strconv.ParseInt(k, 10, 64); err == nil {
				if !validChannelIDs[id] {
					return fmt.Errorf("channel display icon references non-existent channel ID %d", id)
				}
			}
		}
	}

	if c.CompanionPreferences != nil {
		for _, target := range c.CompanionPreferences.QuotaTargets {
			if target.ChannelID > 0 && !validChannelIDs[target.ChannelID] {
				return fmt.Errorf("companion preference references non-existent channel ID %d", target.ChannelID)
			}
		}
	}

	return nil
}

func (s *Store) SavePreRestoreSnapshot() (string, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	return s.savePreRestoreSnapshotLocked()
}

func (s *Store) savePreRestoreSnapshotLocked() (string, error) {
	if s.dir == "" {
		return "", nil
	}
	cfg, err := s.exportFullConfigLocked()
	if err != nil {
		return "", fmt.Errorf("export pre-restore snapshot: %w", err)
	}

	snapshot := *cfg
	snapshot.Channels = make([]Channel, len(cfg.Channels))
	for i, ch := range cfg.Channels {
		ch.Normalize()
		sealedCh, err := s.sealChannel(ch)
		if err != nil {
			return "", fmt.Errorf("seal channel %d for snapshot: %w", ch.ID, err)
		}
		snapshot.Channels[i] = sealedCh
	}
	snapshot.ClientKeys = make([]ClientKeyRecord, len(cfg.ClientKeys))
	for i, k := range cfg.ClientKeys {
		rec := k
		if rec.Token != "" {
			sealedToken, err := s.seal(rec.Token)
			if err != nil {
				return "", fmt.Errorf("seal client key %q token for snapshot: %w", rec.Name, err)
			}
			rec.Token = sealedToken
		}
		snapshot.ClientKeys[i] = rec
	}

	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal pre-restore snapshot: %w", err)
	}

	randBytes := make([]byte, 4)
	if _, err := rand.Read(randBytes); err != nil {
		return "", fmt.Errorf("generate snapshot id: %w", err)
	}
	filename := fmt.Sprintf("pre-restore-snapshot-%s-%s.json",
		time.Now().UTC().Format("20060102-150405.000000000"),
		hex.EncodeToString(randBytes))

	tmpFilename := fmt.Sprintf("tmp-%s", filename)
	tmpPath := filepath.Join(s.dir, tmpFilename)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", fmt.Errorf("create temporary snapshot file: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write temporary snapshot file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close temporary snapshot file: %w", err)
	}

	path := filepath.Join(s.dir, filename)
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("commit snapshot file: %w", err)
	}

	verified, err := s.ReadPreRestoreSnapshot(filename)
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("verify pre-restore snapshot readability: %w", err)
	}
	if err := verified.Validate(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("verify pre-restore snapshot validity: %w", err)
	}

	s.lastPreRestoreSnapshot = filename
	if s.db != nil {
		_, _ = s.db.Exec("INSERT INTO meta(key, value) VALUES('last_pre_restore_snapshot', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", filename)
	}
	return filename, nil
}

func (s *Store) ReadPreRestoreSnapshot(filename string) (*FullConfig, error) {
	if s.dir == "" {
		return nil, errors.New("store directory not configured")
	}
	clean := filepath.Base(filename)
	if clean != filename || strings.Contains(filename, "..") || !strings.HasPrefix(clean, "pre-restore-snapshot-") || !strings.HasSuffix(clean, ".json") {
		return nil, errors.New("invalid snapshot filename")
	}
	path := filepath.Join(s.dir, clean)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read snapshot file: %w", err)
	}
	var cfg FullConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot: %w", err)
	}

	for i := range cfg.Channels {
		rawCh, err := json.Marshal(cfg.Channels[i])
		if err != nil {
			return nil, fmt.Errorf("marshal channel for unsealing: %w", err)
		}
		unsealedCh, err := s.openChannel(cfg.Channels[i].ID, string(rawCh))
		if err != nil {
			return nil, fmt.Errorf("decrypt channel %d: %w", cfg.Channels[i].ID, err)
		}
		cfg.Channels[i] = unsealedCh
	}

	for i := range cfg.ClientKeys {
		k := &cfg.ClientKeys[i]
		if k.Token != "" {
			plainToken, err := s.open(k.Token)
			if err != nil {
				return nil, fmt.Errorf("decrypt client key %q token: %w", k.Name, err)
			}
			k.Token = plainToken
		}
	}

	return &cfg, nil
}

func (s *Store) RestoreSnapshot(filename string) error {
	cfg, err := s.ReadPreRestoreSnapshot(filename)
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	return s.restoreFullConfigInternal(cfg, true)
}

func (s *Store) LastPreRestoreSnapshot() string {
	if s == nil {
		return ""
	}
	s.restoreHooksMu.Lock()
	snap := s.lastPreRestoreSnapshot
	s.restoreHooksMu.Unlock()
	if snap != "" && s.snapshotExists(snap) {
		return snap
	}
	if s.db != nil {
		var val string
		if err := s.db.QueryRow("SELECT value FROM meta WHERE key='last_pre_restore_snapshot'").Scan(&val); err == nil && val != "" {
			if s.snapshotExists(val) {
				s.restoreHooksMu.Lock()
				s.lastPreRestoreSnapshot = val
				s.restoreHooksMu.Unlock()
				return val
			}
		}
	}
	if s.dir != "" {
		entries, err := os.ReadDir(s.dir)
		if err == nil {
			var latest string
			var latestMod time.Time
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if strings.HasPrefix(name, "pre-restore-snapshot-") && strings.HasSuffix(name, ".json") {
					info, err := e.Info()
					if err == nil {
						if latest == "" || info.ModTime().After(latestMod) || (info.ModTime().Equal(latestMod) && name > latest) {
							latest = name
							latestMod = info.ModTime()
						}
					}
				}
			}
			if latest != "" {
				s.restoreHooksMu.Lock()
				s.lastPreRestoreSnapshot = latest
				s.restoreHooksMu.Unlock()
				return latest
			}
		}
	}
	return ""
}

func (s *Store) snapshotExists(filename string) bool {
	if s.dir == "" || filename == "" {
		return false
	}
	clean := filepath.Base(filename)
	if clean != filename || strings.Contains(filename, "..") || !strings.HasPrefix(clean, "pre-restore-snapshot-") || !strings.HasSuffix(clean, ".json") {
		return false
	}
	_, err := os.Stat(filepath.Join(s.dir, clean))
	return err == nil
}

func (s *Store) OnConfigRestore(fn func()) {
	s.restoreHooksMu.Lock()
	defer s.restoreHooksMu.Unlock()
	s.restoreHooks = append(s.restoreHooks, fn)
}

// ExportFullConfig serializes the complete instance configuration.
// Machine-bound secrets (master.key, admin login key, MCP tokens) and
// ephemeral history (logs, usage, charges) are excluded.
func (s *Store) ExportFullConfig() (*FullConfig, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	return s.exportFullConfigLocked()
}

func (s *Store) exportFullConfigLocked() (*FullConfig, error) {
	cfg := &FullConfig{
		Version:    1,
		ExportedAt: time.Now().UTC(),
	}

	// 1. Channels (decrypted)
	channels, err := s.ListChannels()
	if err != nil {
		return nil, fmt.Errorf("export channels: %w", err)
	}
	if channels == nil {
		channels = []Channel{}
	}
	cfg.Channels = channels

	// 2. Downstream Models
	models, err := s.ListDownstreamModels()
	if err != nil {
		return nil, fmt.Errorf("export models: %w", err)
	}
	if models == nil {
		models = []DownstreamModel{}
	}
	cfg.DownstreamModels = models

	// 3. Settings (clear admin GatewayKey)
	settings, err := s.GetSettings()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("export settings: %w", err)
	}
	settings.GatewayKey = ""
	if settings.Prices == nil {
		settings.Prices = []Price{}
	}
	cfg.Settings = settings

	// 4. Plan Prices
	planRows, err := s.db.Query("SELECT channel_id, default_amount, currency, first_payment_at, timezone FROM plan_prices ORDER BY channel_id")
	if err != nil {
		return nil, fmt.Errorf("export plan prices: %w", err)
	}
	defer planRows.Close()
	for planRows.Next() {
		var p PlanPriceConfig
		var first string
		if err := planRows.Scan(&p.ChannelID, &p.DefaultAmount, &p.Currency, &first, &p.Timezone); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, first); err == nil {
			p.FirstPaymentAt = t
		} else if t, err := time.Parse(time.RFC3339, first); err == nil {
			p.FirstPaymentAt = t
		}
		cfg.PlanPrices = append(cfg.PlanPrices, p)
	}
	if err := planRows.Err(); err != nil {
		return nil, err
	}

	// 5. Tag Groups & Tags
	tagGroups, err := s.ListTagGroups()
	if err != nil {
		return nil, fmt.Errorf("export tag groups: %w", err)
	}
	cfg.TagGroups = tagGroups

	tags, err := s.ListTags()
	if err != nil {
		return nil, fmt.Errorf("export tags: %w", err)
	}
	cfg.Tags = tags

	// 6. Client Keys (with decrypted tokens if sealed)
	keyRows, err := s.db.Query("SELECT id, name, prefix, created_at, COALESCE(sealed, ''), scope, hash FROM client_keys ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("export client keys: %w", err)
	}
	defer keyRows.Close()
	for keyRows.Next() {
		var id int64
		var name, prefix, sealed, rawScope string
		var created int64
		var hash []byte
		if err := keyRows.Scan(&id, &name, &prefix, &created, &sealed, &rawScope, &hash); err != nil {
			return nil, err
		}
		scope, err := decodeClientScope(rawScope)
		if err != nil {
			var rawScopeObj ClientKeyScope
			if json.Unmarshal([]byte(rawScope), &rawScopeObj) == nil {
				if canon, canonErr := canonicalScope(rawScopeObj); canonErr == nil {
					scope = canon
					err = nil
				}
			}
			if err != nil {
				return nil, err
			}
		}
		rec := ClientKeyRecord{
			Name:      name,
			Prefix:    prefix,
			CreatedAt: time.Unix(created, 0).UTC(),
			Scope:     scope,
		}
		if sealed != "" {
			if token, err := s.open(sealed); err == nil && token != "" {
				rec.Token = token
			}
		}
		if rec.Token == "" && len(hash) > 0 {
			rec.Hash = base64.RawURLEncoding.EncodeToString(hash)
		}
		cfg.ClientKeys = append(cfg.ClientKeys, rec)
	}
	if err := keyRows.Err(); err != nil {
		return nil, err
	}

	// 7. Plugin Preferences
	var rawPlugin string
	err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", pluginPreferencesKey).Scan(&rawPlugin)
	if err == nil {
		var prefs PluginPreferences
		if json.Unmarshal([]byte(rawPlugin), &prefs) == nil {
			cfg.PluginPreferences = &prefs
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("export plugin preferences: %w", err)
	}

	// 8. Channel Display
	var rawDisplay string
	err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", channelDisplayKey).Scan(&rawDisplay)
	if err == nil {
		var display ChannelDisplay
		if json.Unmarshal([]byte(rawDisplay), &display) == nil {
			cfg.ChannelDisplay = &display
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("export channel display: %w", err)
	}

	// 9. Companion Preferences
	compPrefs, err := s.CompanionPreferences()
	if err == nil {
		cfg.CompanionPreferences = &compPrefs
	}

	// 10. Exchange Sources
	sources, err := s.GetExchangeSources()
	if err == nil {
		cfg.ExchangeSources = sources
	}

	// 12. Catalog Sources
	catRows, err := s.db.Query("SELECT source, name, enabled, prefill_enabled, priority FROM catalog_sources")
	if err == nil {
		defer catRows.Close()
		for catRows.Next() {
			var rec CatalogSourceRecord
			if err := catRows.Scan(&rec.Source, &rec.Name, &rec.Enabled, &rec.PrefillEnabled, &rec.Priority); err == nil {
				cfg.CatalogSources = append(cfg.CatalogSources, rec)
			}
		}
	}

	return cfg, nil
}

// RestoreFullConfig restores the provided configuration atomically inside a transaction.
// It maps channel integer IDs past any existing IDs in channels and usage history
// to preserve target log integrity, re-encrypts all secrets with the target's master.key,
// and preserves target admin credentials, listening port, and MCP connections.
func (s *Store) RestoreFullConfig(cfg *FullConfig) error {
	return s.restoreFullConfigInternal(cfg, false)
}

func (s *Store) restoreFullConfigInternal(cfg *FullConfig, isRollback bool) error {
	if cfg == nil {
		return errors.New("configuration is nil")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()

	snapName, snapErr := s.savePreRestoreSnapshotLocked()
	if snapErr != nil {
		if isRollback {
			if s.dir != "" {
				failName := fmt.Sprintf("corrupt-pre-rollback-%s.log", time.Now().UTC().Format("20060102-150405"))
				_ = os.WriteFile(filepath.Join(s.dir, failName), []byte(snapErr.Error()), 0600)
			}
		} else {
			return fmt.Errorf("create pre-restore snapshot: %w", snapErr)
		}
	} else if snapName != "" {
		s.lastPreRestoreSnapshot = snapName
		if s.db != nil {
			_, _ = s.db.Exec("INSERT INTO meta(key, value) VALUES('last_pre_restore_snapshot', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", snapName)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Find the maximum channel ID ever seen in target DB (channels, usage_logs, plan_charges, sqlite_sequence)
	// so restored channels never collide with target history.
	var maxChanID int64
	_ = tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM channels").Scan(&maxChanID)
	var maxUsageChanID int64
	_ = tx.QueryRow("SELECT COALESCE(MAX(channel_id), 0) FROM usage_logs").Scan(&maxUsageChanID)
	if maxUsageChanID > maxChanID {
		maxChanID = maxUsageChanID
	}
	var maxChargeChanID int64
	_ = tx.QueryRow("SELECT COALESCE(MAX(channel_id), 0) FROM plan_charges").Scan(&maxChargeChanID)
	if maxChargeChanID > maxChanID {
		maxChanID = maxChargeChanID
	}
	var maxSeqChanID int64
	_ = tx.QueryRow("SELECT COALESCE(seq, 0) FROM sqlite_sequence WHERE name='channels'").Scan(&maxSeqChanID)
	if maxSeqChanID > maxChanID {
		maxChanID = maxSeqChanID
	}

	// Find the maximum client key ID ever seen in target DB so restored client keys never collide
	var maxClientID int64
	_ = tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM client_keys").Scan(&maxClientID)
	var maxSeqClientID int64
	_ = tx.QueryRow("SELECT COALESCE(seq, 0) FROM sqlite_sequence WHERE name='client_keys'").Scan(&maxSeqClientID)
	if maxSeqClientID > maxClientID {
		maxClientID = maxSeqClientID
	}

	// Map old channel IDs to new non-colliding channel IDs
	channelIDMap := make(map[int64]int64, len(cfg.Channels))
	for i, c := range cfg.Channels {
		newID := maxChanID + int64(i+1)
		channelIDMap[c.ID] = newID
	}

	// Read existing target Settings to preserve GatewayKey and RetentionDays
	var curSettingsRaw string
	var curSettings Settings
	err = tx.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&curSettingsRaw)
	if err == nil {
		_ = json.Unmarshal([]byte(curSettingsRaw), &curSettings)
	}

	// 1. Clear replaceable tables
	if _, err := tx.Exec("DELETE FROM channels"); err != nil {
		return fmt.Errorf("clear channels: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM downstream_models"); err != nil {
		return fmt.Errorf("clear models: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM plan_prices"); err != nil {
		return fmt.Errorf("clear plan prices: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM client_keys"); err != nil {
		return fmt.Errorf("clear client keys: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM tags"); err != nil {
		return fmt.Errorf("clear tags: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM tag_groups WHERE type='user'"); err != nil {
		return fmt.Errorf("clear user tag groups: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM meta WHERE key IN ('channel_display', 'plugin_preferences', 'companion_preferences')"); err != nil {
		return fmt.Errorf("clear meta: %w", err)
	}

	// 2. Insert restored Channels (re-encrypted with target's master.key)
	for _, c := range cfg.Channels {
		newID := channelIDMap[c.ID]
		c.ID = newID
		for j := range c.Credentials {
			if c.Credentials[j].SourceChannelID > 0 {
				if remapped, ok := channelIDMap[c.Credentials[j].SourceChannelID]; ok {
					c.Credentials[j].SourceChannelID = remapped
				}
			}
		}
		sealedChan, err := s.sealChannel(c)
		if err != nil {
			return fmt.Errorf("seal channel %d: %w", newID, err)
		}
		raw, err := json.Marshal(sealedChan)
		if err != nil {
			return fmt.Errorf("marshal channel %d: %w", newID, err)
		}
		if _, err := tx.Exec("INSERT INTO channels(id, data) VALUES(?, ?)", newID, string(raw)); err != nil {
			return fmt.Errorf("insert channel %d: %w", newID, err)
		}
	}

	// 3. Insert Downstream Models (remap binding ChannelIDs)
	for _, m := range cfg.DownstreamModels {
		for bi := range m.Bindings {
			remapped, ok := channelIDMap[m.Bindings[bi].ChannelID]
			if !ok {
				return fmt.Errorf("downstream model %q references unknown channel ID %d", m.ID, m.Bindings[bi].ChannelID)
			}
			m.Bindings[bi].ChannelID = remapped
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal downstream model %s: %w", m.ID, err)
		}
		if _, err := tx.Exec("INSERT INTO downstream_models(id, data) VALUES(?, ?)", m.ID, string(raw)); err != nil {
			return fmt.Errorf("insert downstream model %s: %w", m.ID, err)
		}
	}

	// 4. Update Settings (preserve target's sealed GatewayKey and local RetentionDays, remap channel prices)
	settingsToSave := cfg.Settings
	settingsToSave.GatewayKey = curSettings.GatewayKey
	if curSettings.RetentionDays >= 1 && curSettings.RetentionDays <= 3650 {
		settingsToSave.RetentionDays = curSettings.RetentionDays
	} else if settingsToSave.RetentionDays < 1 || settingsToSave.RetentionDays > 3650 {
		settingsToSave.RetentionDays = 30
	}
	if settingsToSave.Prices == nil {
		settingsToSave.Prices = []Price{}
	}
	for i := range settingsToSave.Prices {
		if settingsToSave.Prices[i].ChannelID > 0 {
			if remapped, ok := channelIDMap[settingsToSave.Prices[i].ChannelID]; ok {
				settingsToSave.Prices[i].ChannelID = remapped
			}
		}
	}
	if err := validateSettingsDomain(&settingsToSave); err != nil {
		return fmt.Errorf("validate settings before save: %w", err)
	}
	rawSettings, err := json.Marshal(settingsToSave)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if _, err := tx.Exec("UPDATE settings SET data=? WHERE id=1", string(rawSettings)); err != nil {
		return fmt.Errorf("update settings: %w", err)
	}

	// 5. Insert Plan Prices (remap channel_id, set active_from to now in milliseconds)
	restoreTime := time.Now().UnixMilli()
	for _, p := range cfg.PlanPrices {
		newChanID, ok := channelIDMap[p.ChannelID]
		if !ok {
			continue
		}
		firstStr := p.FirstPaymentAt.Format(time.RFC3339Nano)
		if _, err := tx.Exec("INSERT INTO plan_prices(channel_id, default_amount, currency, first_payment_at, timezone, active_from) VALUES(?, ?, ?, ?, ?, ?)",
			newChanID, p.DefaultAmount, p.Currency, firstStr, p.Timezone, restoreTime); err != nil {
			return fmt.Errorf("insert plan price for channel %d: %w", newChanID, err)
		}
	}

	// 6. Insert Tag Groups & Tags
	for _, g := range cfg.TagGroups {
		raw, err := json.Marshal(g)
		if err != nil {
			return err
		}
		if g.Type == TagGroupTypeUser {
			if _, err := tx.Exec("INSERT INTO tag_groups(id, type, data) VALUES(?, ?, ?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", g.ID, string(g.Type), string(raw)); err != nil {
				return fmt.Errorf("insert tag group %s: %w", g.ID, err)
			}
		} else {
			if _, err := tx.Exec("UPDATE tag_groups SET data=? WHERE id=?", string(raw), g.ID); err != nil {
				return fmt.Errorf("update builtin tag group %s: %w", g.ID, err)
			}
		}
	}
	for _, t := range cfg.Tags {
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO tags(id, group_id, data) VALUES(?, ?, ?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", t.ID, t.GroupID, string(raw)); err != nil {
			return fmt.Errorf("insert tag %s: %w", t.ID, err)
		}
	}

	// 7. Insert Client Keys (re-seal tokens with target AEAD, assign non-colliding IDs)
	for i, k := range cfg.ClientKeys {
		var sealed string
		var hash []byte
		var prefix string
		if k.Token != "" {
			sToken, err := s.seal(k.Token)
			if err != nil {
				return fmt.Errorf("seal client key: %w", err)
			}
			sealed = sToken
			hash = tokenHash(k.Token)
			if len(k.Token) >= 18 {
				prefix = k.Token[:18]
			} else {
				prefix = k.Prefix
			}
		} else if k.Hash != "" {
			decoded, err := base64.RawURLEncoding.DecodeString(k.Hash)
			if err != nil {
				return fmt.Errorf("decode client key hash: %w", err)
			}
			hash = decoded
			prefix = k.Prefix
		} else {
			continue
		}
		canonical, err := canonicalScope(k.Scope)
		if err != nil {
			return fmt.Errorf("canonicalize client key scope %s: %w", k.Name, err)
		}
		rawScope, err := json.Marshal(canonical)
		if err != nil {
			return fmt.Errorf("marshal client key scope: %w", err)
		}
		newKeyID := maxClientID + int64(i+1)
		if _, err := tx.Exec("INSERT INTO client_keys(id, name, prefix, hash, created_at, sealed, scope) VALUES(?, ?, ?, ?, ?, ?, ?)",
			newKeyID, k.Name, prefix, hash, k.CreatedAt.Unix(), sealed, string(rawScope)); err != nil {
			return fmt.Errorf("insert client key %s: %w", k.Name, err)
		}
	}

	// 8. Insert Channel Display (remap channel order and icon keys)
	if cfg.ChannelDisplay != nil {
		display := *cfg.ChannelDisplay
		var newOrder []int64
		for _, oldID := range display.Order {
			if remapped, ok := channelIDMap[oldID]; ok {
				newOrder = append(newOrder, remapped)
			}
		}
		display.Order = newOrder
		newIcons := make(map[string]string, len(display.Icons))
		for k, v := range display.Icons {
			if oldID, err := strconv.ParseInt(k, 10, 64); err == nil {
				if remapped, ok := channelIDMap[oldID]; ok {
					newIcons[strconv.FormatInt(remapped, 10)] = v
				}
			} else {
				newIcons[k] = v
			}
		}
		display.Icons = newIcons
		rawDisplay, err := json.Marshal(display)
		if err == nil {
			if _, err := tx.Exec("INSERT INTO meta(key, value) VALUES(?, ?)", channelDisplayKey, string(rawDisplay)); err != nil {
				return fmt.Errorf("insert channel display: %w", err)
			}
		}
	}

	// 9. Insert Plugin Preferences
	if cfg.PluginPreferences != nil {
		rawPlugin, err := json.Marshal(cfg.PluginPreferences)
		if err == nil {
			if _, err := tx.Exec("INSERT INTO meta(key, value) VALUES(?, ?)", pluginPreferencesKey, string(rawPlugin)); err != nil {
				return fmt.Errorf("insert plugin preferences: %w", err)
			}
		}
	}

	// 10. Insert Companion Preferences (remap QuotaTargets)
	if cfg.CompanionPreferences != nil {
		comp := *cfg.CompanionPreferences
		for i := range comp.QuotaTargets {
			if remapped, ok := channelIDMap[comp.QuotaTargets[i].ChannelID]; ok {
				comp.QuotaTargets[i].ChannelID = remapped
			}
		}
		rawComp, err := json.Marshal(comp)
		if err == nil {
			if _, err := tx.Exec("INSERT INTO meta(key, value) VALUES('companion_preferences', ?)", string(rawComp)); err != nil {
				return fmt.Errorf("insert companion preferences: %w", err)
			}
		}
	}

	// 11. Insert Exchange Sources
	if cfg.ExchangeSources != nil {
		rawSources, err := json.Marshal(cfg.ExchangeSources)
		if err == nil {
			if _, err := tx.Exec("INSERT INTO meta(key, value) VALUES('exchange_sources', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(rawSources)); err != nil {
				return fmt.Errorf("insert exchange sources: %w", err)
			}
		}
	}

	// 12. Update Catalog Sources
	for _, cat := range cfg.CatalogSources {
		_, _ = tx.Exec("UPDATE catalog_sources SET enabled=?, prefill_enabled=?, priority=? WHERE source=?",
			cat.Enabled, cat.PrefillEnabled, cat.Priority, cat.Source)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	s.generation.Add(1)

	s.restoreHooksMu.Lock()
	hooks := make([]func(), len(s.restoreHooks))
	copy(hooks, s.restoreHooks)
	s.restoreHooksMu.Unlock()
	for _, h := range hooks {
		if h != nil {
			h()
		}
	}

	return nil
}

// VerifyClientKey verifies whether an authenticated client key is still valid
// and belongs to the current configuration generation (has not crossed a restore boundary).
func (s *Store) VerifyClientKey(k ClientKey) bool {
	if s == nil || k.ID <= 0 {
		return true
	}
	if k.Generation == 0 && len(k.Hash) == 0 {
		return true
	}
	if k.Generation != s.generation.Load() {
		return false
	}
	return true
}
