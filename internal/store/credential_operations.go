package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// These are options of an existing credential, not another account registry.
// The credential owns their lifetime; normal channel edits preserve them.
type CredentialOperationsConfig struct {
	Warmup    CredentialWarmupConfig    `json:"warmup"`
	AutoReset CredentialAutoResetConfig `json:"auto_reset"`
}

type CredentialWarmupConfig struct {
	Enabled    bool     `json:"enabled"`
	Mode       string   `json:"mode"` // reset or daily
	Model      string   `json:"model"`
	DailyTimes []string `json:"daily_times"` // HH:MM in the gateway's local timezone
}

type CredentialAutoResetConfig struct {
	Enabled bool                  `json:"enabled"`
	Rules   []CredentialResetRule `json:"rules"`
}

type CredentialResetRule struct {
	WindowSeconds     int64    `json:"window_seconds"`
	RemainingBelow    *float64 `json:"remaining_below"`
	ExpiresWithinDays *float64 `json:"expires_within_days"`
	Join              string   `json:"join"` // and or or, between present conditions
}

// Each editor owns one part of the credential's settings. Merge under the
// channel lock so another editor's newer settings cannot be overwritten.
type CredentialOperationsPatch struct {
	Warmup      *CredentialWarmupConfig    `json:"warmup,omitempty"`
	AutoReset   *CredentialAutoResetConfig `json:"auto_reset,omitempty"`
	WarmupModel *string                    `json:"warmup_model,omitempty"`
}

var ErrInvalidCredentialOperation = errors.New("invalid credential operation")
var ErrCredentialIdentityChanged = errors.New("credential identity changed; refresh the channel")
var ErrCredentialActionBusy = errors.New("a credential action is already pending")

func DefaultCredentialOperationsConfig() CredentialOperationsConfig {
	return CredentialOperationsConfig{
		Warmup:    CredentialWarmupConfig{Mode: "reset", DailyTimes: []string{}},
		AutoReset: CredentialAutoResetConfig{Rules: []CredentialResetRule{}},
	}
}

func (v CredentialOperationsConfig) Validate() error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", ErrInvalidCredentialOperation, message) }
	if v.Warmup.Mode != "reset" && v.Warmup.Mode != "daily" {
		return invalid("预热方式必须是跟随重置或每日定时")
	}
	if len(v.Warmup.DailyTimes) > 32 || v.Warmup.Enabled && v.Warmup.Mode == "daily" && len(v.Warmup.DailyTimes) == 0 {
		return invalid("每日预热需要1至32个时间点")
	}
	seenTimes := map[string]bool{}
	for _, at := range v.Warmup.DailyTimes {
		if _, err := time.Parse("15:04", at); err != nil || len(at) != 5 || seenTimes[at] {
			return invalid("每日预热时间须为HH:MM，且不能重复")
		}
		seenTimes[at] = true
	}
	if len(v.Warmup.Model) > 256 || v.Warmup.Model != strings.TrimSpace(v.Warmup.Model) || v.Warmup.Enabled && v.Warmup.Model == "" {
		return invalid("请选择预热模型")
	}
	if len(v.AutoReset.Rules) > 32 || v.AutoReset.Enabled && len(v.AutoReset.Rules) == 0 {
		return invalid("自动重置需要1至32组规则")
	}
	for _, rule := range v.AutoReset.Rules {
		if rule.Join != "and" && rule.Join != "or" {
			return invalid("规则组合必须为且或或")
		}
		if rule.RemainingBelow == nil && rule.ExpiresWithinDays == nil {
			return invalid("每组规则至少需要一个条件")
		}
		if n := rule.RemainingBelow; n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 || *n > 100 || rule.WindowSeconds <= 0) {
			return invalid("额度条件需要指定窗口，阈值为0至100%")
		}
		if n := rule.ExpiresWithinDays; n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 || *n > 3650) {
			return invalid("重置卡到期天数必须在0至3650之间")
		}
	}
	return nil
}

func CredentialOperationsIdentity(c Channel, credentialID string) (string, error) {
	oauth, err := quotaCredential(c, credentialID)
	if err != nil {
		return "", fmt.Errorf("%w: 当前凭证不支持Codex操作", ErrInvalidCredentialOperation)
	}
	// A rotating refresh token is not a durable account identity for an
	// operation that can consume a credit. Imported anonymous tokens need a
	// fresh authorization before enabling these account actions.
	if oauth.Account == "" && oauth.Subject == "" {
		return "", fmt.Errorf("%w: 缺少稳定账号标识，请重新授权", ErrInvalidCredentialOperation)
	}
	return quotaIdentity(c, credentialID), nil
}

func credentialIndex(c Channel, id string) int {
	for i, credential := range c.Credentials {
		if credential.ID == id {
			return i
		}
	}
	return -1
}

func (s *Store) SetCredentialOrder(id int64, ids []string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return err
	}
	if len(ids) == 0 || len(ids) != len(c.Credentials) {
		return fmt.Errorf("%w: 凭证列表已变化，请刷新后重试", ErrInvalidCredentialOperation)
	}
	ordered := make([]CredentialItem, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, credID := range ids {
		i := credentialIndex(c, credID)
		if i < 0 || seen[credID] {
			return fmt.Errorf("%w: 顺序必须包含每个现有凭证且仅一次", ErrInvalidCredentialOperation)
		}
		seen[credID] = true
		ordered = append(ordered, c.Credentials[i])
	}
	c.Credentials = ordered
	return s.saveChannelLocked(&c, false)
}

func (s *Store) SetCredentialEnabled(id int64, credentialID string, enabled bool) error {
	_, err := s.SetCredentialEnabledAtGeneration(id, credentialID, enabled, nil)
	return err
}

func (s *Store) SetCredentialEnabledAtGeneration(id int64, credentialID string, enabled bool, expectedGeneration *int64) (int64, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return 0, err
	}
	if expectedGeneration != nil && c.Generation != *expectedGeneration {
		return 0, ErrCredentialIdentityChanged
	}
	i := credentialIndex(c, credentialID)
	if i < 0 {
		return 0, ErrNotFound
	}
	if c.Credentials[i].Enabled == enabled {
		return c.Generation, nil
	}
	if !enabled && c.Enabled {
		remaining := false
		for j, credential := range c.Credentials {
			if j != i && credential.Enabled {
				remaining = true
				break
			}
		}
		if !remaining {
			return 0, fmt.Errorf("%w: 请至少保留一个启用凭证，或先停用渠道", ErrInvalidCredentialOperation)
		}
	}
	c.Credentials[i].Enabled = enabled
	if err := s.saveChannelLocked(&c, false); err != nil {
		return 0, err
	}
	return c.Generation, nil
}

func (s *Store) PatchCredentialOperationsConfig(id int64, credentialID, expectedIdentity string, patch CredentialOperationsPatch) error {
	count := 0
	if patch.Warmup != nil {
		count++
	}
	if patch.AutoReset != nil {
		count++
	}
	if patch.WarmupModel != nil {
		count++
	}
	if count != 1 {
		return fmt.Errorf("%w: 每次只能修改一类操作配置", ErrInvalidCredentialOperation)
	}
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return err
	}
	identity, err := CredentialOperationsIdentity(c, credentialID)

	if err != nil {
		return err
	}
	if expectedIdentity == "" || identity != expectedIdentity {
		return ErrCredentialIdentityChanged
	}
	i := credentialIndex(c, credentialID)
	config := DefaultCredentialOperationsConfig()
	if c.Credentials[i].Operations != nil {
		config = *c.Credentials[i].Operations
	}
	if patch.Warmup != nil {
		config.Warmup = *patch.Warmup
	}
	if patch.AutoReset != nil {
		config.AutoReset = *patch.AutoReset
	}
	if patch.WarmupModel != nil {
		config.Warmup.Model = *patch.WarmupModel
	}
	checkModel := patch.Warmup != nil || patch.WarmupModel != nil
	if err := config.Validate(); err != nil {
		return err
	}
	if checkModel && config.Warmup.Model != "" && !containsString(c.Models, config.Warmup.Model) {
		return fmt.Errorf("%w: 预热模型必须在当前渠道启用", ErrInvalidCredentialOperation)
	}
	if config.AutoReset.Rules == nil {
		config.AutoReset.Rules = []CredentialResetRule{}
	}
	config.Warmup.DailyTimes = append([]string{}, config.Warmup.DailyTimes...)
	sort.Strings(config.Warmup.DailyTimes)
	c.Credentials[i].Operations = &config
	return s.saveChannelLocked(&c, false)
}

type credentialOperationRecord struct {
	Identity string          `json:"identity"`
	Data     json.RawMessage `json:"data"`
}

func credentialOperationKey(id int64, credentialID string) string {
	return "codex_credential_operations:" + strconv.FormatInt(id, 10) + ":" + credentialID
}

// State is separate from configuration so an in-flight action cannot undo a
// settings edit. The transaction also fences two local processes before a POST.
func (s *Store) UpdateCredentialOperationState(expected Channel, credentialID string, update func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	return s.updateCredentialOperationState(expected, credentialID, false, update)
}

// Claim also fences the user configuration immediately before starting a
// side-effect. Completion deliberately only fences identity: disabling or
// sorting a credential must not discard the receipt of an already-sent call.
func (s *Store) ClaimCredentialOperationState(expected Channel, credentialID string, update func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	return s.updateCredentialOperationState(expected, credentialID, true, update)
}

func (s *Store) updateCredentialOperationState(expected Channel, credentialID string, claim bool, update func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var channelRaw string
	err = tx.QueryRow("SELECT data FROM channels WHERE id=?", expected.ID).Scan(&channelRaw)
	missingSource := errors.Is(err, sql.ErrNoRows)
	if missingSource && claim {
		return nil, ErrNotFound
	}
	if err != nil && !missingSource {
		return nil, err
	}
	var c Channel
	if !missingSource {
		c, err = s.openChannel(expected.ID, channelRaw)
		if err != nil {
			return nil, err
		}
	}
	identity, err := CredentialOperationsIdentity(c, credentialID)
	// A completed external action may return after its member was moved. Only
	// completion follows the exact ID and identity; a new claim stays fenced to
	// the original channel/generation. The durable receipt moves in the same
	// transaction as membership, so there remains one journal to complete.
	if err != nil && !claim {
		wanted, wantErr := CredentialOperationsIdentity(expected, credentialID)
		if wantErr != nil {
			return nil, wantErr
		}
		rows, readErr := tx.Query("SELECT id,data FROM channels ORDER BY id")
		if readErr != nil {
			return nil, readErr
		}
		var matches []Channel
		for rows.Next() {
			var id int64
			var raw string
			if readErr = rows.Scan(&id, &raw); readErr != nil {
				break
			}
			candidate, openErr := s.openChannel(id, raw)
			if openErr != nil {
				readErr = openErr
				break
			}
			if found, e := CredentialOperationsIdentity(candidate, credentialID); e == nil && found == wanted {
				matches = append(matches, candidate)
			}
		}
		if readErr == nil {
			readErr = rows.Err()
		}
		rows.Close()
		if readErr != nil {
			return nil, readErr
		}
		if len(matches) != 1 {
			return nil, ErrCredentialIdentityChanged
		}
		c = matches[0]
		identity, err = CredentialOperationsIdentity(c, credentialID)
	}
	if err != nil {
		return nil, err
	}
	wanted, err := CredentialOperationsIdentity(expected, credentialID)
	if err != nil || wanted != identity {
		return nil, ErrCredentialIdentityChanged
	}
	if claim {
		i, before := credentialIndex(c, credentialID), credentialIndex(expected, credentialID)
		if i < 0 || before < 0 || c.Generation != expected.Generation || c.OperationsRevision != expected.OperationsRevision || !c.Enabled || !c.Credentials[i].Enabled || c.Credentials[i].OperationsRevision != expected.Credentials[before].OperationsRevision || !reflect.DeepEqual(c.Credentials[i].Operations, expected.Credentials[before].Operations) {
			return nil, ErrCredentialIdentityChanged
		}
	}
	var raw string
	key := credentialOperationKey(c.ID, credentialID)
	err = tx.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var record credentialOperationRecord
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, errors.New("cannot read credential operation state")
		}
	}
	if record.Identity != identity {
		record = credentialOperationRecord{Identity: identity}
	}
	next, err := update(record.Data)
	if err != nil {
		return nil, err
	}
	if len(next) > 1<<20 || !json.Valid(next) {
		return nil, errors.New("invalid credential operation state")
	}
	record.Data = next
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(body)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), next...), nil
}

func (s *Store) LoadCredentialOperationState(id int64, credentialID string) (json.RawMessage, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return nil, err
	}
	identity, err := CredentialOperationsIdentity(c, credentialID)
	if err != nil {
		return nil, err
	}
	var raw string
	err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", credentialOperationKey(id, credentialID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record credentialOperationRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, errors.New("cannot read credential operation state")
	}
	if record.Identity != identity {
		return nil, nil
	}
	return record.Data, nil
}
