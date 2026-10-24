package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// API format names. "OpenAI-compatible" is not a format: a channel that
// speaks Chat Completions says nothing about whether it serves Responses, and
// the two carry completely different state semantics.
const (
	FormatOpenAIChatCompletions = "openai_chat_completions"
	FormatOpenAIResponses       = "openai_responses"
	FormatAnthropicMessages     = "anthropic_messages"
	FormatGeminiGenerateContent = "gemini_generate_content"
)

// Endpoint is one API format a channel actually serves, at its own base URL. A
// provider can serve several: DeepSeek exposes an OpenAI facade and an
// Anthropic facade at different paths, and GLM serves Chat Completions and
// Responses at different paths with different state behaviour.
//
// State records whether this endpoint keeps server-side conversation state.
// Empty means unset and is treated as unknown; "server_state" is the dangerous
// one for routing, because a chain built there is worthless anywhere else.
type Endpoint struct {
	Format  string `json:"format"`
	BaseURL string `json:"base_url"`
	State   string `json:"state"`
}

const (
	DispatchModeFailover    = "failover"
	DispatchModeRoundRobin  = "round_robin"
	ChainTypeUnidirectional = "unidirectional"
	ChainTypeCyclic         = "cyclic"
)

type CredentialItem struct {
	ID                 string                      `json:"id"`
	Label              string                      `json:"label,omitempty"`
	APIKey             string                      `json:"api_key,omitempty"`
	OAuth              *OAuthCredential            `json:"oauth,omitempty"`
	OAuthInfo          *OAuthInfo                  `json:"oauth_info,omitempty"`
	Enabled            bool                        `json:"enabled"`
	HasKey             bool                        `json:"has_key,omitempty"`
	HasOAuth           bool                        `json:"has_oauth,omitempty"`
	MaskedKey          string                      `json:"masked_key,omitempty"`
	SourceChannelID    int64                       `json:"source_channel_id,omitempty"`
	Operations         *CredentialOperationsConfig `json:"operations,omitempty"`
	OperationsRevision int64                       `json:"operations_revision,omitempty"`
}

func MaskKey(k string) string {
	if len(k) <= 8 {
		return "••••••••"
	}
	return k[:3] + "••••" + k[len(k)-4:]
}

func (c CredentialItem) Masked() string {
	if c.MaskedKey != "" {
		return c.MaskedKey
	}
	if c.APIKey != "" {
		return MaskKey(c.APIKey)
	}
	if c.OAuth != nil {
		if c.OAuth.Account != "" {
			return c.OAuth.Account
		}
		return "OAuth"
	}
	return ""
}

type Channel struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	APIKey  string   `json:"api_key"`
	Models  []string `json:"models"`
	Enabled bool     `json:"enabled"`

	Endpoints        []Endpoint                    `json:"endpoints"`
	ModelConfigs     map[string]ModelConfig        `json:"model_configs"`
	ModelCatalogRefs map[string]CatalogModelRef    `json:"model_catalog_refs,omitempty"`
	ModelEvidence    map[string]CapabilityEvidence `json:"model_evidence"`
	AvailableModels  []string                      `json:"available_models"`
	// ModelFormats holds model outbound protocol sets. API Key entries override
	// channel defaults; Zen/Copilot catalogues are complete, including when nil.
	// Explicit empty sets never route. Other fixed-protocol channels retain defaults.
	ModelFormats map[string][]string `json:"model_formats"`
	Tags         []string            `json:"tags,omitempty"`
	ModelTags    map[string][]string `json:"model_tags,omitempty"`

	// ManualModels are the catalogue entries the owner typed by hand instead of
	// pulling them from the upstream. They are what separates "I added this
	// myself" from "the last fetch said so": a catalogue replacement rebuilds
	// the fetched part and keeps these (ReplaceCatalogue), so a hand-added model
	// survives a provider reshuffling its ids. Rows written before the field
	// existed read back empty, which is the truth for them — everything they
	// list came from a fetch. normalize() keeps the markers consistent with
	// AvailableModels: a marker describes a catalogue entry and lives or dies
	// with it.
	ManualModels []string `json:"manual_models,omitempty"`

	// CredentialType says which kind of credential authenticates this channel:
	// api_key (the default, and what every row written before OAuth existed
	// reads back as) or copilot_oauth. It is also the handle for the host
	// allowlist an OAuth credential may be sent to (see OAuthHostAllowed).
	CredentialType string `json:"credential_type,omitempty"`

	// OAuth is the OAuth half of the credential, nil for api_key channels. It
	// is sealed at rest field by field, exactly like APIKey.
	OAuth *OAuthCredential `json:"oauth,omitempty"`

	// DispatchMode controls credential selection within this channel:
	// "failover" (default) or "round_robin".
	DispatchMode string `json:"dispatch_mode,omitempty"`

	// ChainType controls topology and recovery behavior for this channel's pool:
	// "unidirectional" (default, resets to head on recovery) or "cyclic" (continuous ring).
	ChainType string `json:"chain_type,omitempty"`

	// Credentials holds the credential pool for this channel.
	Credentials []CredentialItem `json:"credentials,omitempty"`

	// PreferredEndpoint names the endpoint format this channel prefers when no
	// endpoint matches the inbound format exactly. Empty means the fixed
	// portable order (Chat Completions, Messages, Gemini). Rows written before
	// it existed unmarshal to empty, so they keep the default behaviour.
	PreferredEndpoint string `json:"preferred_endpoint,omitempty"`

	// Generation counts how often this channel's continuation-domain inputs
	// changed while its id stayed the same: the credential, a base URL, or the
	// endpoint declaration set. It is the fourth dimension of a session pin
	// (internal/gateway/affinity.go): state carried by a session was produced
	// by one configuration, and a channel re-pointed at another account or
	// endpoint cannot continue it. Rows written before it existed unmarshal to
	// zero, which compares equal to itself, so existing pins keep working.
	Generation int64 `json:"generation"`
	// OperationsRevision cancels queued automatic work when its controls
	// change without invalidating model conversation continuity.
	OperationsRevision int64 `json:"operations_revision,omitempty"`

	// Legacy fields: rows written before endpoints existed. normalize() turns
	// them into Endpoints on read and they are never written back.
	//
	// BaseURL has a second role: it is the channel-level default every endpoint
	// inherits when it declares no address of its own (EndpointBaseURL). The two
	// roles never collide — a legacy row's endpoints are created carrying it,
	// and a row saved through the checkbox editor stores it expanded into the
	// endpoints that use it.
	Protocol string `json:"protocol,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
}

// EndpointBaseURL is where one endpoint lives: its own address, or the
// channel-level default when it declares none. This is the only place the
// fallback rule lives, so the storage expansion (normalize), the OAuth host
// allowlist, the console's saved-key destination check, and the discover and
// probe URL building cannot drift apart on what "no address of its own" means.
// Empty on both sides means the endpoint has nowhere to go, which
// ValidateChannel rejects.
func (c Channel) EndpointBaseURL(e Endpoint) string {
	if base := strings.TrimSpace(e.BaseURL); base != "" {
		return base
	}
	return strings.TrimSpace(c.BaseURL)
}

// PrimaryCredential returns the first credential from the credentials pool,
// or falls back to top-level APIKey/OAuth.
func (c Channel) PrimaryCredential() CredentialItem {
	if len(c.Credentials) > 0 {
		return c.Credentials[0]
	}
	return CredentialItem{
		ID:      "cred_default",
		APIKey:  c.APIKey,
		OAuth:   c.OAuth,
		Enabled: c.Enabled,
	}
}

// PrimaryKey returns the effective API key of the channel.
func (c Channel) PrimaryKey() string {
	if len(c.Credentials) > 0 {
		return c.Credentials[0].APIKey
	}
	return c.APIKey
}

// PrimaryOAuth returns the effective OAuth credential of the channel.
func (c Channel) PrimaryOAuth() *OAuthCredential {
	if len(c.Credentials) > 0 {
		return c.Credentials[0].OAuth
	}
	return c.OAuth
}

type Settings struct {
	GatewayKey        string  `json:"gateway_key"`
	Prices            []Price `json:"prices"`
	RetentionDays     int     `json:"retention_days"`
	MaxAttempts       int     `json:"max_attempts"`
	RetriesPerChannel int     `json:"retries_per_channel"`
	ReasoningPolicy   string  `json:"reasoning_policy"`
	DefaultLaunchMode string  `json:"default_launch_mode"`
	// FailureCooldownSeconds is the initial local delay after an unavailable
	// upstream attempt. A provider's Retry-After takes precedence over it.
	FailureCooldownSeconds int           `json:"failure_cooldown_seconds"`
	RequestDefaultsEnabled bool          `json:"request_defaults_enabled"`
	RequestDefaults        ModelDefaults `json:"request_defaults"`
	// OAuthAutoOpen decides whether a browser authorization flow opens the
	// consent page on its own when it starts. It is a pointer because the
	// default is on and a plain bool cannot tell "unset" from "off": its zero
	// value is false, so a row written before the switch existed would read
	// back as off. Nil resolves to the default on read and on save.
	OAuthAutoOpen       *bool `json:"oauth_auto_open"`
	QuotaRefreshSeconds *int  `json:"quota_refresh_seconds"`
}

const (
	ActionTakenSuccess           = "success"
	ActionTakenCoolingSwitched   = "cooling_switched"
	ActionTakenExhaustedFallback = "exhausted_fallback"
	ActionTakenClientCancelled   = "client_cancelled"
	ActionTakenRetry             = "retry"
)

type AttemptRecord struct {
	AttemptIndex      int    `json:"attempt_index"`
	ChannelID         int64  `json:"channel_id"`
	ChannelName       string `json:"channel_name"`
	CredentialID      string `json:"credential_id,omitempty"`
	CredentialMaskKey string `json:"credential_mask_key,omitempty"`
	UpstreamModel     string `json:"upstream_model"`
	StatusCode        int    `json:"status_code"`
	DurationMS        int64  `json:"duration_ms"`
	ErrorReason       string `json:"error_reason,omitempty"`
	ActionTaken       string `json:"action_taken,omitempty"` // "success", "cooling_switched", "exhausted_fallback", "client_cancelled", "retry"
}

type Usage struct {
	ID               int64              `json:"id"`
	RequestID        string             `json:"request_id,omitempty"`
	Diagnostic       json.RawMessage    `json:"diagnostic,omitempty"`
	Time             time.Time          `json:"time"`
	ChannelID        int64              `json:"channel_id"`
	ChannelName      string             `json:"channel_name"`
	ClientKeyID      int64              `json:"client_key_id"`
	ClientKeyName    string             `json:"client_key_name"`
	Model            string             `json:"model"`
	UpstreamModel    string             `json:"upstream_model"`
	Inbound          string             `json:"inbound"`
	Outbound         string             `json:"outbound"`
	InputTokens      int64              `json:"input_tokens"`
	OutputTokens     int64              `json:"output_tokens"`
	CacheReadTokens  int64              `json:"cache_read_tokens"`
	CacheWriteTokens int64              `json:"cache_write_tokens"`
	Estimated        bool               `json:"estimated"`
	Currency         string             `json:"currency"`
	OfficialValue    *float64           `json:"official_value"`
	OfficialCurrency string             `json:"official_currency"`
	ActualCost       *float64           `json:"actual_cost"`
	ActualPricing    *UsagePricingBasis `json:"actual_pricing,omitempty"`
	OfficialPricing  *UsagePricingBasis `json:"official_pricing,omitempty"`
	DurationMS       int64              `json:"duration_ms"`
	TTFTMS           int64              `json:"ttft_ms"`
	Status           int                `json:"status"`
	Error            string             `json:"error"`
	// NotSent records a gateway rejection without inventing an upstream call.
	NotSent bool `json:"not_sent,omitempty"`
	// Stateful marks a request that carried provider-native state (a thinking
	// signature, previous_response_id, ...). Degraded names reasoning carriers
	// stripped from the response because the client's format cannot express
	// them; empty means nothing was dropped.
	Stateful bool            `json:"stateful"`
	Degraded string          `json:"degraded"`
	Attempts []AttemptRecord `json:"attempts,omitempty"`
}
type UsageFilter struct {
	From, To       time.Time
	Timezone       string
	Model          string
	RequestID      string
	ChannelID      int64
	Status, Limit  int
	ExcludeNotSent bool
}
type Store struct {
	db   *sql.DB
	aead cipher.AEAD
	// channelSaveMu makes one SaveChannel one critical section. The
	// read→compare→increment→write of the continuation generation is several
	// statements, and the single database connection only serializes each
	// statement on its own: two concurrent edits would otherwise both read
	// generation N and both write N+1, re-using a continuation domain. A
	// personal gateway edits channels from one console, so a process-local lock
	// is enough.
	credentialLifecycle sync.Map
	channelSaveMu       sync.Mutex
	settingsSaveMu      sync.Mutex
	exchangeMu          sync.Mutex
	// A caller shares an attempt only if one completes while it waits for the lock.
	exchangeGeneration atomic.Uint64
	catalogMu          sync.RWMutex

	// persistRow lands one already-sealed channel row. It is a field only so a
	// test can substitute a write that fails on demand and prove the rotation
	// retry (ReplaceCopilotToken); production uses writeRow, set in Open.
	persistRow func(*Channel) error

	dir                    string
	generation             atomic.Uint64
	restoreHooksMu         sync.Mutex
	restoreHooks           []func()
	lastPreRestoreSnapshot string
}

var ErrNotFound = errors.New("not found")

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// defaultRetriesPerChannel is how many times one channel is retried before the
// gateway moves to the next candidate.
const DefaultRetriesPerChannel = 5

// DefaultFailureCooldownSeconds starts local exponential cooldown. It does not
// cap provider Retry-After hints. Local growth is capped by the health manager.
const DefaultFailureCooldownSeconds = 10

// DefaultOAuthAutoOpen is the default of the OAuth auto-open switch: a settings
// row from before the switch existed has no key for it, and nil (the zero value
// of the pointer) means this default, not the zero value of the underlying
// bool (off).
const DefaultOAuthAutoOpen = true

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "master.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(key)
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("master.key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, aead: aead, dir: dir}
	s.generation.Store(1)
	s.persistRow = s.writeRow
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS channels(id INTEGER PRIMARY KEY AUTOINCREMENT, data TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS settings(id INTEGER PRIMARY KEY CHECK(id=1),data TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS usage_logs(id INTEGER PRIMARY KEY AUTOINCREMENT,time INTEGER NOT NULL,channel_id INTEGER NOT NULL,model TEXT NOT NULL,status INTEGER NOT NULL,data TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS runtime(id INTEGER PRIMARY KEY CHECK(id=1),data TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS plan_prices(channel_id INTEGER PRIMARY KEY,default_amount REAL,currency TEXT NOT NULL,first_payment_at TEXT NOT NULL,timezone TEXT NOT NULL,active_from INTEGER);
 CREATE TABLE IF NOT EXISTS plan_charges(channel_id INTEGER NOT NULL,month TEXT NOT NULL,channel_name TEXT NOT NULL,amount REAL,currency TEXT NOT NULL,paid_at INTEGER NOT NULL,PRIMARY KEY(channel_id,month));
 CREATE INDEX IF NOT EXISTS plan_charge_paid ON plan_charges(paid_at);
 CREATE INDEX IF NOT EXISTS usage_time ON usage_logs(time);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = s.upgradeModelFormats(); err != nil {
		db.Close()
		return nil, err
	}
	// Discovery uses the selected application's normal locations. Retire the
	// former per-directory consent records at the loading boundary.
	if _, err = db.Exec("DELETE FROM meta WHERE substr(key,1,20)='key_discovery_scope:'"); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.upgradeCredentialDailyTimes(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initModels(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initCredentials(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initManagement(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.InitUsageManagement(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initCatalog(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initTags(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.upgradeTagScopes(dir); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initChannelDisplay(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.retireChannelQuotaCache(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.upgradeCatalogContracts(dir); err != nil {
		db.Close()
		return nil, err
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM settings").Scan(&count); err != nil {
		db.Close()
		return nil, err
	}
	if count == 0 {
		token := make([]byte, 32)
		if _, err = rand.Read(token); err == nil {
			err = s.SaveSettings(Settings{GatewayKey: "tr_" + base64.RawURLEncoding.EncodeToString(token), Prices: []Price{}, RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: DefaultRetriesPerChannel, FailureCooldownSeconds: DefaultFailureCooldownSeconds})
		}
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = s.upgradeFailureCooldown(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = s.ListChannels(); err != nil {
		db.Close()
		return nil, fmt.Errorf("decrypt channels (preserve master.key): %w", err)
	}
	if err = s.Cleanup(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

// Port returns the port a previous run bound successfully, or 0 when none was
// remembered. Kept out of Settings so a settings save cannot reset it.
func (s *Store) Port() (int, error) {
	var raw string
	if err := s.db.QueryRow("SELECT data FROM runtime WHERE id=1").Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	var v struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return 0, err
	}
	return v.Port, nil
}
func (s *Store) SavePort(port int) error { return s.SavePortIfCurrent(port, nil) }

func (s *Store) SavePortIfCurrent(port int, expected *int) error {
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	if expected != nil {
		current, err := s.Port()
		if err != nil {
			return err
		}
		if current != *expected {
			return ErrEditConflict
		}
	}
	if port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	raw, err := json.Marshal(struct {
		Port int `json:"port"`
	}{port})
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO runtime(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(raw))
	return err
}
func (s *Store) seal(text string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(text), nil)), nil
}
func (s *Store) open(text string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", err
	}
	n := s.aead.NonceSize()
	if len(b) < n {
		return "", errors.New("invalid encrypted key")
	}
	plain, err := s.aead.Open(nil, b[:n], b[n:], nil)
	return string(plain), err
}
func (s *Store) ListChannels() ([]Channel, error) {
	rows, err := s.db.Query("SELECT id,data FROM channels ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		var id int64
		var raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		c, err := s.openChannel(id, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// legacyFormats maps the old three-value protocol to the endpoint formats it
// implied. An "openai" channel was assumed to serve both OpenAI facades, which
// is exactly the assumption this split exists to correct; migration keeps it so
// existing channels behave as before until their owner tightens them.
var legacyFormats = map[string][]string{
	"openai":    {FormatOpenAIChatCompletions, FormatOpenAIResponses},
	"anthropic": {FormatAnthropicMessages},
	"gemini":    {FormatGeminiGenerateContent},
}

var legacyEvidenceFormat = map[string]string{
	"chat_completions": FormatOpenAIChatCompletions,
	"responses":        FormatOpenAIResponses,
	"messages":         FormatAnthropicMessages,
	"generate_content": FormatGeminiGenerateContent,
}

// normalize upgrades a row written before channels had endpoints, folds the
// short evidence format names into the endpoint names, defaults the credential
// type for rows written before OAuth existed, and reconciles the manual-model
// markers with the catalogue. It never drops data — the one thing it discards
// is a manual marker whose catalogue entry is gone, which is what "gone" means.
func (c *Channel) Normalize() {
	if c.CredentialType == "" {
		c.CredentialType = CredentialAPIKey
	}
	if len(c.Endpoints) == 0 && c.Protocol != "" {
		for _, f := range legacyFormats[c.Protocol] {
			c.Endpoints = append(c.Endpoints, Endpoint{Format: f, BaseURL: c.BaseURL})
		}
		c.Protocol = ""
	}
	// An endpoint that declares no address of its own rides on the channel
	// default. Expanding it here — on every read and every save, before
	// anything looks at the endpoints — is what keeps the downstream code
	// (gateway, OAuth allowlist, console tools) unaware of the fallback: they
	// only ever see the address an endpoint is reached at. The price is stored
	// redundancy, since a saved row carries the default in each endpoint that
	// uses it; the alternative, resolving the fallback at every read site, puts
	// the rule in five places instead of one.
	for i, e := range c.Endpoints {
		if strings.TrimSpace(e.BaseURL) == "" {
			c.Endpoints[i].BaseURL = c.EndpointBaseURL(e)
		}
	}
	for id, e := range c.ModelEvidence {
		if full, ok := legacyEvidenceFormat[e.APIFormat]; ok {
			e.APIFormat = full
			c.ModelEvidence[id] = e
		}
	}
	// A manual marker describes a catalogue entry, so it goes the way that entry
	// does: when the channel no longer lists the model (the owner removed it, or
	// a catalogue replacement dropped it), the marker is meaningless and dies
	// with it, and repeated markers collapse into one.
	manual := make([]string, 0, len(c.ManualModels))
	for _, m := range c.ManualModels {
		if containsString(c.AvailableModels, m) && !containsString(manual, m) {
			manual = append(manual, m)
		}
	}
	if len(manual) == 0 {
		c.ManualModels = nil
	} else {
		c.ManualModels = manual
	}

	if c.DispatchMode == "" {
		c.DispatchMode = DispatchModeFailover
	}
	if c.ChainType == "" {
		c.ChainType = ChainTypeUnidirectional
	}

	if len(c.Credentials) == 0 {
		if c.APIKey != "" || c.OAuth != nil {
			label := "默认凭据"
			if c.CredentialType == CredentialCopilotOAuth {
				label = "Copilot 账号"
			} else if c.CredentialType == CredentialChatGPTOAuth {
				label = "ChatGPT 账号"
			} else if c.CredentialType == CredentialCodexOAuth {
				label = "Codex 账号"
			}
			c.Credentials = []CredentialItem{{
				ID:      "cred_default",
				Label:   label,
				APIKey:  c.APIKey,
				OAuth:   c.OAuth,
				Enabled: true,
			}}
		}
	} else {
		for i := range c.Credentials {
			if strings.TrimSpace(c.Credentials[i].ID) == "" {
				c.Credentials[i].ID = fmt.Sprintf("cred_%d", i+1)
			}
			if strings.TrimSpace(c.Credentials[i].Label) == "" {
				c.Credentials[i].Label = fmt.Sprintf("凭据 %d", i+1)
			}
		}
		// When there is exactly 1 credential and caller updated c.APIKey / c.OAuth directly (e.g. tests or external callers)
		if len(c.Credentials) == 1 && c.APIKey != "" && c.Credentials[0].APIKey != c.APIKey {
			c.Credentials[0].APIKey = c.APIKey
		}
		if len(c.Credentials) == 1 && c.OAuth != nil && c.Credentials[0].OAuth == nil {
			c.Credentials[0].OAuth = c.OAuth
		}
	}
	// In-memory read projection for backward-compatibility with callers/tests reading c.APIKey / c.OAuth
	if len(c.Credentials) > 0 {
		c.APIKey = c.Credentials[0].APIKey
		if c.Credentials[0].OAuth != nil {
			c.OAuth = c.Credentials[0].OAuth
		}
	}
}

func (c *Channel) normalize() {
	c.Normalize()
}

// Formats lists the API formats this channel serves. Like Family and Base it
// reports what a row written before endpoints existed means, so a legacy
// declaration answers with the formats it implied.
func (c Channel) Formats() []string {
	if len(c.Endpoints) == 0 && c.Protocol != "" {
		c.normalize()
	}
	out := make([]string, 0, len(c.Endpoints))
	for _, e := range c.Endpoints {
		out = append(out, e.Format)
	}
	return out
}

// Family is the wire-format family of the channel's first endpoint. Model
// discovery and single-model tests work against one endpoint, and the first is
// the one the channel was configured with.
func (c Channel) Family() string {
	if len(c.Endpoints) == 0 && c.Protocol != "" {
		c.normalize()
	}
	if len(c.Endpoints) == 0 {
		return ""
	}
	switch c.Endpoints[0].Format {
	case FormatOpenAIChatCompletions, FormatOpenAIResponses:
		return "openai"
	case FormatAnthropicMessages:
		return "anthropic"
	case FormatGeminiGenerateContent:
		return "gemini"
	}
	return ""
}

// Base is the base URL of the channel's first endpoint.
func (c Channel) Base() string {
	if len(c.Endpoints) == 0 && c.Protocol != "" {
		c.normalize()
	}
	if len(c.Endpoints) == 0 {
		return ""
	}
	return c.Endpoints[0].BaseURL
}

// EndpointFor returns the endpoint serving a format, if any.
func (c Channel) EndpointFor(format string) (Endpoint, bool) {
	for _, e := range c.Endpoints {
		if e.Format == format {
			return e, true
		}
	}
	return Endpoint{}, false
}

// validBaseURL is the shape a base URL must have: HTTP(S), no credentials, no
// query, no fragment. The path is the provider's business — what the router
// appends to it is not.
func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func ValidateChannel(c Channel) error {
	c.normalize()
	if c.DispatchMode != "" && c.DispatchMode != DispatchModeFailover && c.DispatchMode != DispatchModeRoundRobin {
		return errors.New("dispatch mode must be failover or round_robin")
	}
	if c.ChainType != "" && c.ChainType != ChainTypeUnidirectional && c.ChainType != ChainTypeCyclic {
		return errors.New("chain type must be unidirectional or cyclic")
	}
	credIDs := make(map[string]bool)
	hasEnabled := false
	for _, cred := range c.Credentials {
		if strings.TrimSpace(cred.ID) == "" {
			return errors.New("credential id cannot be empty")
		}
		if credIDs[cred.ID] {
			return fmt.Errorf("duplicate credential id: %s", cred.ID)
		}
		credIDs[cred.ID] = true
		if cred.Operations != nil {
			if c.CredentialType != CredentialCodexOAuth {
				return errors.New("credential operations require Codex OAuth")
			}
			if err := cred.Operations.Validate(); err != nil {
				return err
			}
		}
		if cred.Enabled {
			hasEnabled = true
		}
	}
	if c.Enabled && len(c.Credentials) > 0 && !hasEnabled {
		return errors.New("enabled channel must have at least one enabled credential")
	}
	inventory := map[string]bool{}
	for _, id := range append(append([]string{}, c.Models...), c.AvailableModels...) {
		inventory[id] = true
	}
	for model, formats := range c.ModelFormats {
		if !inventory[model] {
			return errors.New("model protocol must name a listed model")
		}
		seen := map[string]bool{}
		for _, format := range formats {
			if seen[format] || !containsString(c.Formats(), format) {
				return errors.New("model protocol must name unique declared endpoints")
			}
			seen[format] = true
		}
	}
	for id, ref := range c.ModelCatalogRefs {
		if !containsString(c.Models, id) && !containsString(c.AvailableModels, id) {
			return errors.New("catalog reference names an unlisted model")
		}
		if err := ValidateCatalogModelRef(ref); err != nil {
			return err
		}
	}
	for id, config := range c.ModelConfigs {
		if !inventory[id] {
			return errors.New("model config references unknown model")
		}
		if err := ValidateModelConfig(config); err != nil {
			return err
		}
	}
	for id, evidence := range c.ModelEvidence {
		if !inventory[id] {
			return errors.New("evidence references unknown model")
		}
		if evidence.Source != "manual" && evidence.Source != "official" && evidence.Source != "discovery" && evidence.Source != "test" {
			return errors.New("invalid evidence source")
		}
		if !containsString(c.Formats(), evidence.APIFormat) {
			return errors.New("evidence format must match one of the channel's endpoints")
		}
	}
	if c.PreferredEndpoint != "" && !containsString(c.Formats(), c.PreferredEndpoint) {
		return errors.New("preferred endpoint must match one of the channel's endpoints")
	}
	// The channel default is an address like any other, and an unusable one is a
	// trap: an endpoint that inherited it would be unreachable with no visible
	// cause. Checking it here catches the default itself and, through the
	// expansion above, every endpoint that just inherited it.
	if base := strings.TrimSpace(c.BaseURL); base != "" && !validBaseURL(base) {
		return errors.New("channel base_url must be HTTP(S), without credentials, query or fragment")
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required")
	}
	if len(c.Endpoints) == 0 {
		return errors.New("channel must declare at least one endpoint")
	}
	seen := map[string]bool{}
	for _, e := range c.Endpoints {
		switch e.Format {
		case FormatOpenAIChatCompletions, FormatOpenAIResponses, FormatAnthropicMessages, FormatGeminiGenerateContent:
		default:
			return errors.New("invalid endpoint format")
		}
		if seen[e.Format] {
			return errors.New("duplicate endpoint format")
		}
		seen[e.Format] = true
		switch e.State {
		case "", "stateless", "server_state":
		default:
			return errors.New("endpoint state must be stateless, server_state or empty")
		}
		if !validBaseURL(e.BaseURL) {
			return errors.New("endpoint base_url must be HTTP(S), without credentials, query or fragment")
		}
	}
	if len(c.Models)+len(c.AvailableModels) > 20000 {
		return errors.New("too many models")
	}
	for _, m := range append(append([]string{}, c.Models...), c.AvailableModels...) {
		if strings.TrimSpace(m) == "" {
			return errors.New("model cannot be empty")
		}
	}
	switch c.CredentialType {
	case CredentialClaudeOAuth, CredentialAntigravityOAuth, CredentialDevinOAuth, CredentialZenFree:
		return validateNativeChannel(c)
	case CredentialAPIKey:
		hasSecret := c.PrimaryKey() != ""
		if !hasSecret {
			for _, cred := range c.Credentials {
				if cred.SourceChannelID > 0 {
					hasSecret = true
					break
				}
			}
		}
		if !hasSecret {
			return errors.New("api_key is required")
		}
	case CredentialCopilotOAuth:
		if len(c.Credentials) > 0 {
			oauthCred := c.PrimaryOAuth()
			if oauthCred == nil || strings.TrimSpace(oauthCred.GitHubToken) == "" {
				return errors.New("copilot_oauth channel requires an oauth credential with a github token")
			}
			if oauthCred.Provider != OAuthProviderCopilot {
				return errors.New("oauth provider must be " + OAuthProviderCopilot)
			}
			switch oauthCred.Status {
			case "", OAuthStatusActive, OAuthStatusUnsubscribed, OAuthStatusReauthRequired:
			default:
				return errors.New("oauth status must be active, unsubscribed, reauth_required or empty")
			}
		}

		// Copilot serves exactly the two documented facades
		// (docs/protocol-refs/oauth-github-copilot.md), and only off the hosts
		// its issuer documents. Both constraints are checked here, and the
		// second one again per request in the gateway.
		for _, e := range c.Endpoints {
			if e.Format != FormatOpenAIChatCompletions && e.Format != FormatAnthropicMessages {
				return errors.New("copilot_oauth channels serve only the chat completions and messages facades")
			}
			if !OAuthHostAllowed(c.CredentialType, e.BaseURL) {
				return fmt.Errorf("oauth credentials may only be sent to the %s host allowlist; %s is not on it", c.CredentialType, e.BaseURL)
			}
			u, err := url.Parse(e.BaseURL)
			if err != nil || (u.Scheme != "https" && !EndpointRelaxedForDiagnostics(e.BaseURL)) {
				return errors.New("copilot_oauth endpoints must use https")
			}
		}
	case CredentialChatGPTOAuth:
		if len(c.Credentials) > 0 {
			oauthCred := c.PrimaryOAuth()
			if oauthCred == nil || strings.TrimSpace(oauthCred.RefreshToken) == "" {
				return errors.New("chatgpt_oauth channel requires an oauth credential with a refresh token")
			}
			if oauthCred.Provider != OAuthProviderChatGPT {
				return errors.New("oauth provider must be " + OAuthProviderChatGPT)
			}
			if strings.TrimSpace(oauthCred.ClientID) == "" || !strings.HasPrefix(oauthCred.ClientID, "oaiapp_") {
				return errors.New("chatgpt_oauth credential must carry the registered oaiapp_ client id")
			}
			if strings.TrimSpace(oauthCred.HostID) == "" {
				return errors.New("chatgpt_oauth credential must carry this host's ext_agent_host_id")
			}
			switch oauthCred.Status {
			case "", OAuthStatusActive, OAuthStatusReauthRequired:
			default:
				return errors.New("oauth status must be active, reauth_required or empty")
			}
		}

		// The SIWC profile is one endpoint: POST /v1/responses on api.openai.com,
		// with no server-side state and a fixed omit list the gateway enforces
		// (docs/protocol-refs/oauth-openai-siwc.md). Anything else declared here
		// would be a facade the issuer does not serve for this credential.
		for _, e := range c.Endpoints {
			if e.Format != FormatOpenAIResponses {
				return errors.New("chatgpt_oauth channels serve only the responses endpoint")
			}
			if !OAuthHostAllowed(c.CredentialType, e.BaseURL) {
				return fmt.Errorf("oauth credentials may only be sent to the %s host allowlist; %s is not on it", c.CredentialType, e.BaseURL)
			}
			u, err := url.Parse(e.BaseURL)
			if err != nil || (u.Scheme != "https" && !EndpointRelaxedForDiagnostics(e.BaseURL)) {
				return errors.New("chatgpt_oauth endpoints must use https")
			}
		}
	case CredentialCodexOAuth:
		if len(c.Credentials) > 0 {
			oauthCred := c.PrimaryOAuth()
			if oauthCred == nil || strings.TrimSpace(oauthCred.AccessToken) == "" && strings.TrimSpace(oauthCred.RefreshToken) == "" {
				return errors.New("codex_oauth channel requires an access or refresh token")
			}
			if oauthCred.Provider != OAuthProviderCodex {
				return errors.New("oauth provider must be " + OAuthProviderCodex)
			}
			if strings.TrimSpace(oauthCred.ClientID) == "" {
				return errors.New("codex_oauth credential must carry the client id the flow used")
			}
			switch oauthCred.Status {
			case "", OAuthStatusActive, OAuthStatusReauthRequired:
			default:
				return errors.New("oauth status must be active, reauth_required or empty")
			}
		}

		// The Codex backend is a Responses-shaped API behind chatgpt.com; the
		// capability surface is unverified (docs/protocol-refs/
		// oauth-openai-codex.md), so the gateway passes requests through and the
		// declaration is limited to the one documented facade on the one host.
		for _, e := range c.Endpoints {
			if e.Format != FormatOpenAIResponses {
				return errors.New("codex_oauth channels serve only the responses endpoint")
			}
			if !OAuthHostAllowed(c.CredentialType, e.BaseURL) {
				return fmt.Errorf("oauth credentials may only be sent to the %s host allowlist; %s is not on it", c.CredentialType, e.BaseURL)
			}
			u, err := url.Parse(e.BaseURL)
			if err != nil || (u.Scheme != "https" && !EndpointRelaxedForDiagnostics(e.BaseURL)) {
				return errors.New("codex_oauth endpoints must use https")
			}
		}
	default:
		if IsCompatibleOAuth(c.CredentialType) {
			return validateCompatibleChannel(c)
		}
		return errors.New("invalid credential type")
	}
	return nil
}
func (s *Store) SaveChannel(c *Channel) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	return s.saveChannelLocked(c, false)
}

// SaveChannelConfig is the ordinary management edit entry point. OAuth is
// owned by the credential lifecycle, so preserve the current stored credential
// inside the same lock as the edit, even when the caller's snapshot is stale.
// SaveChannel remains the full-row creation/authorization entry point; ordinary
// OAuth edits must use this method instead.
func (s *Store) SaveChannelConfig(c *Channel) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	return s.saveChannelLocked(c, true)
}

func (s *Store) saveChannelLocked(c *Channel, preserveOAuth bool) error {
	c.normalize()
	var old Channel
	if c.ID != 0 {
		var err error
		var raw string
		err = s.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err == nil {
			old, err = s.openChannel(c.ID, raw)
		}
		if err != nil {
			return err
		}
		if preserveOAuth {
			if RequiresIndependentOAuth(old.CredentialType) && c.Generation != old.Generation {
				return ErrEditConflict
			}
			c.ModelConfigs = old.ModelConfigs
			c.ModelCatalogRefs = old.ModelCatalogRefs
			if old.CredentialType != CredentialAPIKey {
				c.ModelFormats = old.ModelFormats
			}
			pruneModelEntries(c)
			// Credential actions have a narrow settings endpoint. An ordinary
			// channel draft must not overwrite settings saved since it was opened.
			for i := range c.Credentials {
				c.Credentials[i].Operations = nil
				for _, existing := range old.Credentials {
					if existing.ID == c.Credentials[i].ID {
						c.Credentials[i].Operations = existing.Operations
						break
					}
				}
			}
		}
		if preserveOAuth && c.CredentialType == old.CredentialType && old.OAuth != nil && len(c.Credentials) > 0 {
			c.OAuth = nil
			for i := range c.Credentials {
				for _, oldCred := range old.Credentials {
					if c.Credentials[i].ID == oldCred.ID && oldCred.OAuth != nil {
						c.Credentials[i].OAuth = oldCred.OAuth
					}
				}
			}
			c.OAuth = c.Credentials[0].OAuth
		}
		for i, cred := range c.Credentials {
			if cred.Operations != nil && (c.CredentialType != CredentialCodexOAuth || quotaIdentity(old, cred.ID) != quotaIdentity(*c, cred.ID)) {
				// New consent for another account must not inherit automatic
				// consuming actions configured for the replaced identity.
				c.Credentials[i].Operations = nil
			}
			c.Credentials[i].OperationsRevision = 0
			if previous := credentialIndex(old, cred.ID); previous >= 0 {
				c.Credentials[i].OperationsRevision = old.Credentials[previous].OperationsRevision
				if !reflect.DeepEqual(c.Credentials[i].Operations, old.Credentials[previous].Operations) {
					c.Credentials[i].OperationsRevision++
				}
			}
		}
	}
	if err := s.validateChannelTags(*c); err != nil {
		return err
	}
	if err := ValidateChannel(*c); err != nil {
		return err
	}
	if c.ID == 0 {
		c.Generation = 1
		c.OperationsRevision = 1
	} else {
		c.OperationsRevision = old.OperationsRevision
		if old.Enabled != c.Enabled {
			c.OperationsRevision++
		}
		// The generation belongs to what is already stored, not to what the
		// caller believed about it: the stored row is the only record of the
		// previous configuration to compare against.
		if continuationDomainChanged(old, *c) {
			c.Generation = old.Generation + 1
		} else {
			c.Generation = old.Generation
		}
	}
	row, err := s.sealChannel(*c)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if c.ID == 0 {
		res, err := s.db.Exec("INSERT INTO channels(data) VALUES(?)", string(raw))
		if err != nil {
			return err
		}
		c.ID, err = res.LastInsertId()
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = transitionPlanChannelInTx(tx, old, *c, time.Now()); err != nil {
		return err
	}
	res, err := tx.Exec("UPDATE channels SET data=? WHERE id=?", string(raw), c.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// channel reads one stored channel the way ListChannels returns it: normalized,
// with its credential decrypted.
func (s *Store) channel(id int64) (Channel, error) {
	var raw string
	if err := s.db.QueryRow("SELECT data FROM channels WHERE id=?", id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Channel{}, ErrNotFound
		}
		return Channel{}, err
	}
	return s.openChannel(id, raw)
}

// continuationDomainChanged reports whether a re-save changes what a channel's
// continuation domain is made of: the credential, a base URL, or the endpoint
// declaration set. Everything else — the name, models, the enabled
// flag — leaves the state namespace alone and must not invalidate the sessions
// pinned to the channel. Both sides are normalized first, so a legacy row
// re-saved with its endpoints spelled out for the first time is not a change.
func continuationDomainChanged(old, updated Channel) bool {
	if len(old.ModelFormats) != len(updated.ModelFormats) || (old.ModelFormats == nil) != (updated.ModelFormats == nil) {
		return true
	}
	for model, format := range old.ModelFormats {
		if !reflect.DeepEqual(updated.ModelFormats[model], format) {
			return true
		}
	}
	if old.CredentialType != updated.CredentialType {
		return true
	}
	if old.CredentialType == CredentialCopilotOAuth {
		switch {
		case old.OAuth == nil || updated.OAuth == nil:
			return (old.OAuth == nil) != (updated.OAuth == nil)
		case old.OAuth.Provider != updated.OAuth.Provider:
			return true
		case old.OAuth.GitHubToken != updated.OAuth.GitHubToken:
			return true
		case old.OAuth.APIBase != updated.OAuth.APIBase:
			return true
		}
		// A rotated bearer token is deliberately not here: it changes every
		// few minutes and a session pin must survive it.
	}
	if old.CredentialType == CredentialChatGPTOAuth {
		switch {
		case old.OAuth == nil || updated.OAuth == nil:
			return (old.OAuth == nil) != (updated.OAuth == nil)
		case old.OAuth.ClientID != updated.OAuth.ClientID:
			return true
		case old.OAuth.HostID != updated.OAuth.HostID:
			return true
		case old.OAuth.Subject != updated.OAuth.Subject:
			return true
		case old.OAuth.APIBase != updated.OAuth.APIBase:
			return true
		}
		// The identity is (client, host, account): a rotated refresh token or a
		// re-issued id_token is the same account on the same installation, so a
		// session pin must survive it.
	}
	if old.CredentialType == CredentialCodexOAuth {
		switch {
		case old.OAuth == nil || updated.OAuth == nil:
			return (old.OAuth == nil) != (updated.OAuth == nil)
		case old.OAuth.ClientID != updated.OAuth.ClientID:
			return true
		case old.OAuth.Account != updated.OAuth.Account:
			return true
		case old.OAuth.APIBase != updated.OAuth.APIBase:
			return true
		}
		// A rotated refresh token is the same account and must not invalidate a
		// pin; there is no host identity on this track, so the account id is
		// what a pin is made of.
	}
	if len(old.Credentials) != len(updated.Credentials) {
		return true
	}
	for i := range old.Credentials {
		if old.Credentials[i].ID != updated.Credentials[i].ID ||
			old.Credentials[i].APIKey != updated.Credentials[i].APIKey ||
			old.Credentials[i].Enabled != updated.Credentials[i].Enabled {
			return true
		}
		oldO := old.Credentials[i].OAuth
		newO := updated.Credentials[i].OAuth
		if (oldO == nil) != (newO == nil) {
			return true
		}
		if oldO != nil && newO != nil {
			if IsCompatibleOAuth(old.CredentialType) {
				if CompatibleOAuthIdentity(old.CredentialType, oldO) != CompatibleOAuthIdentity(updated.CredentialType, newO) {
					return true
				}
				continue
			}
			if IsNativeOAuth(old.CredentialType) {
				if nativeOAuthDomainIdentity(old.CredentialType, *oldO) != nativeOAuthDomainIdentity(updated.CredentialType, *newO) {
					return true
				}
				continue
			}
			if oldO.Subject != newO.Subject || oldO.Account != newO.Account || oldO.GitHubToken != newO.GitHubToken || oldO.ClientID != newO.ClientID || oldO.HostID != newO.HostID {
				return true
			}
		}
	}
	declared := map[string]Endpoint{}
	for _, e := range old.Endpoints {
		declared[e.Format] = e
	}
	for _, e := range updated.Endpoints {
		if have, ok := declared[e.Format]; !ok || have != e {
			return true
		}
		delete(declared, e.Format)
	}
	// A format the old row declared and the new one dropped.
	return len(declared) > 0
}

// DeleteChannel removes the channel and, with it, its credential. There is no
// revoke step to add: the Copilot bearer token is short-lived, and the GitHub
// token that authorizes new ones has no revoke endpoint the gateway could call
// (GitHub's own "revoke token" UI is the way, and it is optional — deleting the
// channel is what stops to-router from using it). Deleting the row is therefore
// the whole revocation semantics, and it is the existing path for both
// credential types.
func (s *Store) DeleteChannel(id int64) error {
	return s.DeleteChannelAtVersion(id, "")
}

func (s *Store) DeleteChannelAtVersion(id int64, expected string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	if expected != "" {
		c, err := s.channel(id)
		if err != nil {
			return err
		}
		if EntityVersion(c) != expected {
			return ErrEditConflict
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	if err = accruePlanChargesInTx(tx, now, id); err != nil {
		return err
	}
	if err = cancelFuturePlanChargesInTx(tx, id, now); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM plan_prices WHERE channel_id=?", id); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM channels WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	display, _, err := readChannelDisplay(tx)
	if err != nil {
		return err
	}
	if err = writeChannelDisplay(tx, display); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM meta WHERE key LIKE ?", fmt.Sprintf("codex_credential_operations:%d:%%", id)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) GetSettings() (Settings, error) {
	var raw string
	var v Settings
	err := s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&raw)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(raw), &v)
	if err == nil {
		if err = normalizeDefaultLaunchMode(&v); err != nil {
			return v, err
		}
		if err = normalizeReasoningPolicy(&v); err != nil {
			return v, err
		}
		// Resolve omitted local cooldown and retry values to their defaults.
		if v.FailureCooldownSeconds == 0 {
			v.FailureCooldownSeconds = DefaultFailureCooldownSeconds
		}
		if v.RetriesPerChannel == 0 {
			v.RetriesPerChannel = DefaultRetriesPerChannel
		}
		// The auto-open switch defaults to on, and a row from before the
		// switch existed has no key at all: nil resolves to the default.
		if v.OAuthAutoOpen == nil {
			enabled := DefaultOAuthAutoOpen
			v.OAuthAutoOpen = &enabled
		}
		if err = normalizeQuotaRefresh(&v); err != nil {
			return v, err
		}
		v.Prices = defaultPrices(v.Prices)
		v.GatewayKey, err = s.open(v.GatewayKey)
	}
	return v, err
}
func (s *Store) SaveSettings(v Settings) error {
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	validatePrices := v.Prices != nil
	if v.Prices == nil || v.DefaultLaunchMode == "" {
		current, err := s.GetSettings()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if v.Prices == nil {
			v.Prices = current.Prices
		}
		if v.DefaultLaunchMode == "" {
			v.DefaultLaunchMode = current.DefaultLaunchMode
		}
	}
	return s.saveSettingsLocked(v, validatePrices)
}

func validateSettingsDomain(v *Settings) error {
	if err := normalizeDefaultLaunchMode(v); err != nil {
		return err
	}
	if err := normalizeReasoningPolicy(v); err != nil {
		return err
	}
	if err := normalizeQuotaRefresh(v); err != nil {
		return err
	}
	// Rows written before same-channel retries existed unmarshal to zero.
	// Treat zero as "unset" and apply the default so an old database keeps
	// round-tripping through the console instead of failing validation.
	if v.RetriesPerChannel == 0 {
		v.RetriesPerChannel = DefaultRetriesPerChannel
	}
	// An omitted initial cooldown uses the current default.
	if v.FailureCooldownSeconds == 0 {
		v.FailureCooldownSeconds = DefaultFailureCooldownSeconds
	}
	// The auto-open switch resolves its default the same way: nil means the row
	// predates the switch, not "off", so the stored row always names an
	// explicit choice.
	if v.OAuthAutoOpen == nil {
		enabled := DefaultOAuthAutoOpen
		v.OAuthAutoOpen = &enabled
	}
	if v.RetentionDays < 1 || v.RetentionDays > 3650 {
		return errors.New("retention_days must be 1..3650")
	}
	if v.MaxAttempts < 1 || v.MaxAttempts > 10 {
		return errors.New("max_attempts must be 1..10")
	}
	if v.RetriesPerChannel < 1 || v.RetriesPerChannel > 10 {
		return errors.New("retries_per_channel must be 1..10")
	}
	if v.FailureCooldownSeconds < 1 || v.FailureCooldownSeconds > 300 {
		return errors.New("failure_cooldown_seconds must be 1..300")
	}
	if err := ValidateModelDefaults(&v.RequestDefaults); err != nil {
		return err
	}
	return nil
}

func (s *Store) saveSettingsLocked(v Settings, validatePrices bool) error {
	var err error
	if validatePrices {
		v.Prices, err = normalizePrices(v.Prices)
		if err != nil {
			return err
		}
	} else {
		v.Prices = defaultPrices(v.Prices)
	}
	if err := validateSettingsDomain(&v); err != nil {
		return err
	}
	if len(v.GatewayKey) < 16 || strings.TrimSpace(v.GatewayKey) != v.GatewayKey {
		return errors.New("gateway key must be at least 16 characters without surrounding whitespace")
	}
	v.GatewayKey, err = s.seal(v.GatewayKey)
	if err != nil {
		return err
	}
	if v.Prices == nil {
		v.Prices = []Price{}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(raw))
	return err
}
func (s *Store) AddUsage(u Usage) error {
	if u.NotSent {
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens = 0, 0, 0, 0
		u.ActualCost, u.OfficialValue, u.ActualPricing, u.OfficialPricing = nil, nil, nil, nil
		u.Currency, u.OfficialCurrency = "", ""
		u.Estimated = true
	}
	if err := normalizeUsagePricing(&u); err != nil {
		return err
	}
	if u.Time.IsZero() {
		u.Time = time.Now()
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,?,?,?,?)", u.Time.UnixMilli(), u.ChannelID, u.Model, u.Status, string(raw))
	return err
}
func usageWhere(f UsageFilter) (string, []any) {
	query := " WHERE 1=1"
	args := []any{}
	if f.RequestID != "" {
		query += " AND json_extract(data,'$.request_id')=?"
		args = append(args, f.RequestID)
	}
	if f.ExcludeNotSent {
		query += " AND COALESCE(json_extract(data,'$.not_sent'),0)=0"
	}
	if !f.From.IsZero() {
		query += " AND time>=?"
		args = append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		query += " AND time<=?"
		args = append(args, f.To.UnixMilli())
	}
	if f.Model != "" {
		query += " AND model=?"
		args = append(args, f.Model)
	}
	if f.ChannelID != 0 {
		query += " AND channel_id=?"
		args = append(args, f.ChannelID)
	}
	if f.Status != 0 {
		query += " AND status=?"
		args = append(args, f.Status)
	}
	return query, args
}

func (s *Store) ListUsage(f UsageFilter) ([]Usage, error) {
	where, args := usageWhere(f)
	query := "SELECT id,data FROM usage_logs" + where
	query += " ORDER BY id DESC"
	if f.Limit <= 0 || f.Limit > 10000 {
		f.Limit = 10000
	}
	query += " LIMIT ?"
	args = append(args, f.Limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Usage{}
	for rows.Next() {
		var id int64
		var raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var u Usage
		if err = json.Unmarshal([]byte(raw), &u); err != nil {
			return nil, err
		}
		u.ID = id
		out = append(out, u)
	}
	return out, rows.Err()
}
func (s *Store) Cleanup() error {
	settings, err := s.GetSettings()
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = s.db.Exec("DELETE FROM usage_logs WHERE time<?", now.AddDate(0, 0, -settings.RetentionDays).UnixMilli())
	if err != nil {
		return err
	}
	if err = s.CleanupUsageManagement(context.Background(), now); err != nil {
		return err
	}
	return s.CleanupManagement(context.Background(), now)
}
