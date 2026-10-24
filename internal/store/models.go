package store

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

type CapabilityEvidence struct {
	Source     string     `json:"source"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	APIFormat  string     `json:"api_format"`
}

// Nil fields are unknown; pointers preserve explicit zero, false, and empty lists.
type ModelConfig struct {
	OutputTokenField *string         `json:"output_token_field,omitempty"`
	NativeThinking   *NativeThinking `json:"native_thinking,omitempty"`
	MaxOutputTokens  *int64          `json:"max_output_tokens,omitempty"`
	ContextWindow    *int64          `json:"context_window,omitempty"`
	MaxInputTokens   *int64          `json:"max_input_tokens,omitempty"`
	OutputLimit      *int64          `json:"output_limit,omitempty"`
	Streaming        *bool           `json:"streaming,omitempty"`
	InputModalities  *[]string       `json:"input_modalities,omitempty"`
	OutputModalities *[]string       `json:"output_modalities,omitempty"`
	ThinkingEfforts  *[]string       `json:"thinking_efforts,omitempty"`
}

type Binding struct {
	ChannelID int64  `json:"channel_id"`
	Model     string `json:"model"`
	Priority  int    `json:"priority"`
	Enabled   bool   `json:"enabled"`
}

type DownstreamModel struct {
	DisplayName     string           `json:"display_name"`
	ID              string           `json:"id"`
	Enabled         bool             `json:"enabled"`
	Fallback        bool             `json:"fallback"`
	SortOrder       int              `json:"sort_order"`
	CatalogModelRef *CatalogModelRef `json:"catalog_model_ref,omitempty"`
	Bindings        []Binding        `json:"bindings"`
	Tags            []string         `json:"tags,omitempty"`
	EffectiveTags   []string         `json:"effective_tags,omitempty"`
}

type EffectiveConfig struct {
	Config  ModelConfig       `json:"config"`
	Sources map[string]string `json:"sources"`
}

// ResolveConfig reports the concrete upstream model's known specifications.
func ResolveConfig(config ModelConfig) EffectiveConfig {
	sources := map[string]string{}
	for field := range configFields(config) {
		sources[field] = "上游模型"
	}
	return EffectiveConfig{Config: config, Sources: sources}
}

func ValidateModelConfig(c ModelConfig) error {
	if c.OutputTokenField != nil && *c.OutputTokenField != "max_tokens" && *c.OutputTokenField != "max_completion_tokens" && *c.OutputTokenField != "max_output_tokens" && *c.OutputTokenField != "maxOutputTokens" {
		return NewConfigurationError("invalid output token field")
	}
	if c.NativeThinking != nil {
		seen := map[string]bool{}
		for _, mode := range c.NativeThinking.Modes {
			if mode.ID == "" || len(mode.ID) > 64 || seen[mode.ID+"/"+mode.APIFormat] {
				return NewConfigurationError("native thinking modes require unique identities")
			}
			seen[mode.ID+"/"+mode.APIFormat] = true
			if mode.APIFormat != FormatOpenAIChatCompletions && mode.APIFormat != FormatOpenAIResponses && mode.APIFormat != FormatAnthropicMessages && mode.APIFormat != FormatGeminiGenerateContent {
				return NewConfigurationError("invalid native thinking format")
			}
			if mode.Control != "effort" && mode.Control != "budget" && mode.Control != "enabled" && mode.Control != "adaptive" {
				return NewConfigurationError("invalid native thinking control")
			}
			if mode.BudgetMin != nil && (*mode.BudgetMin < 0 || *mode.BudgetMin > 1000000000) || mode.BudgetMax != nil && (*mode.BudgetMax < 0 || *mode.BudgetMax > 1000000000) || mode.BudgetMin != nil && mode.BudgetMax != nil && *mode.BudgetMin > *mode.BudgetMax {
				return NewConfigurationError("invalid native thinking budget bounds")
			}
			for _, list := range [][]string{mode.Efforts, mode.DisabledEfforts} {
				if len(list) > 30 {
					return NewConfigurationError("too many native thinking efforts")
				}
				seenEfforts := map[string]bool{}
				for _, effort := range list {
					if effort == "" || len(effort) > 64 || seenEfforts[effort] {
						return NewConfigurationError("native thinking efforts require unique 1..64 character entries")
					}
					seenEfforts[effort] = true
				}
			}
			if mode.DefaultEffort != nil && (len(*mode.DefaultEffort) == 0 || len(*mode.DefaultEffort) > 64 || !slices.Contains(mode.Efforts, *mode.DefaultEffort)) {
				return NewConfigurationError("native default effort must belong to declared efforts")
			}
			for _, effort := range mode.DisabledEfforts {
				if !slices.Contains(mode.Efforts, effort) {
					return NewConfigurationError("native disabled efforts must belong to declared efforts")
				}
			}
		}
	}
	for _, n := range []*int64{c.MaxOutputTokens, c.ContextWindow, c.MaxInputTokens, c.OutputLimit} {
		if n != nil && (*n < 0 || *n > 1000000000) {
			return NewConfigurationError("token declarations must be 0..1000000000")
		}
	}
	for _, list := range []*[]string{c.InputModalities, c.OutputModalities} {
		if list == nil {
			continue
		}
		seen := map[string]bool{}
		for _, v := range *list {
			if v != "text" && v != "image" && v != "audio" && v != "video" && v != "file" {
				return NewConfigurationError("invalid modality")
			}
			if seen[v] {
				return NewConfigurationError("duplicate modality")
			}
			seen[v] = true
		}
	}
	if c.ThinkingEfforts != nil {
		if len(*c.ThinkingEfforts) > 30 {
			return NewConfigurationError("too many thinking efforts")
		}
		seen := map[string]bool{}
		for _, v := range *c.ThinkingEfforts {
			if len(v) == 0 || len(v) > 64 {
				return NewConfigurationError("thinking effort entry must be 1..64 characters")
			}
			if seen[v] {
				return NewConfigurationError("duplicate thinking effort")
			}
			seen[v] = true
		}
	}
	return nil
}

func (s *Store) initModels() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS downstream_models(id TEXT PRIMARY KEY, data TEXT NOT NULL)`)
	return err
}

func (s *Store) ListDownstreamModels() ([]DownstreamModel, error) {
	rows, err := s.db.Query("SELECT data FROM downstream_models ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DownstreamModel{}
	for rows.Next() {
		var raw string
		var m DownstreamModel
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(result, func(a, b DownstreamModel) int {
		if a.SortOrder != b.SortOrder {
			return cmp.Compare(a.SortOrder, b.SortOrder)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return result, nil
}

// FilterDownstreamModelTags permits only existing tags from user-defined groups.
// Tag IDs are opaque: their prefixes do not establish their group's type.
// Built-in IDs retain their meaning even if their stored group was reassigned.
func FilterDownstreamModelTags(ids []string, tags []Tag, groups []TagGroup) []string {
	userGroups := map[string]bool{}
	for _, group := range groups {
		userGroups[group.ID] = group.Type == TagGroupTypeUser
	}
	allowed := map[string]bool{}
	for _, tag := range tags {
		allowed[tag.ID] = userGroups[tag.GroupID] && !isBuiltinTag(tag.ID)
	}
	return normalizeDownstreamModelTags(ids, allowed)
}

func normalizeDownstreamModelTags(ids []string, allowed map[string]bool) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && allowed[id] && !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	return result
}

func downstreamManualTagIDs(tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.Query("SELECT tags.data, tag_groups.data FROM tags JOIN tag_groups ON tags.group_id = tag_groups.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allowed := map[string]bool{}
	for rows.Next() {
		var tagRaw, groupRaw string
		if err := rows.Scan(&tagRaw, &groupRaw); err != nil {
			return nil, err
		}
		var tag Tag
		var group TagGroup
		if err := json.Unmarshal([]byte(tagRaw), &tag); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(groupRaw), &group); err != nil {
			return nil, err
		}
		allowed[tag.ID] = tag.GroupID == group.ID && group.Type == TagGroupTypeUser && !isBuiltinTag(tag.ID)
	}
	return allowed, rows.Err()
}

func validateAndSaveDownstreamModelInTx(tx *sql.Tx, m DownstreamModel) error {
	if strings.TrimSpace(m.ID) != m.ID || m.ID == "" || len(m.ID) > 200 || strings.ContainsAny(m.ID, "/\\?# \t\r\n") {
		return NewConfigurationError("invalid downstream model ID")
	}
	if m.CatalogModelRef != nil {
		if err := ValidateCatalogModelRef(*m.CatalogModelRef); err != nil {
			return err
		}
	}
	if len(m.Bindings) == 0 || len(m.Bindings) > 1000 {
		return NewConfigurationError("model requires 1..1000 bindings")
	}
	seen := map[string]bool{}
	for _, b := range m.Bindings {
		if b.ChannelID <= 0 || b.Model == "" {
			return NewConfigurationError("invalid model binding")
		}
		key := fmt.Sprintf("%d:%s", b.ChannelID, b.Model)
		if seen[key] {
			return NewConfigurationError("duplicate model binding")
		}
		seen[key] = true
		var raw string
		if err := tx.QueryRow("SELECT data FROM channels WHERE id=?", b.ChannelID).Scan(&raw); err != nil {
			return NewConfigurationError("binding provider does not exist")
		}
		var c Channel
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return err
		}
		found := false
		for _, id := range append(append([]string{}, c.Models...), c.AvailableModels...) {
			if id == b.Model {
				found = true
				break
			}
		}
		if !found {
			return NewConfigurationError("binding upstream model does not exist")
		}
	}
	if m.Bindings == nil {
		m.Bindings = []Binding{}
	}
	allowedTags, err := downstreamManualTagIDs(tx)
	if err != nil {
		return err
	}
	m.Tags = normalizeDownstreamModelTags(m.Tags, allowedTags)
	if err := validateObjectTagsInTx(tx, m.Tags); err != nil {
		return err
	}
	m.EffectiveTags = nil
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO downstream_models(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", m.ID, string(raw))
	return err
}

func (s *Store) SaveDownstreamModel(m DownstreamModel) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateAndSaveDownstreamModelInTx(tx, m); err != nil {
		return err
	}
	return tx.Commit()
}

var ErrModelExists = NewConfigurationError("target public model ID already exists")
var ErrModelPriceConflict = NewConfigurationError("target model has historical prices; rename would overwrite them")

func (s *Store) CreateDownstreamModel(m DownstreamModel) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow("SELECT count(*) FROM downstream_models WHERE id=?", m.ID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrModelExists
	}
	if err = validateAndSaveDownstreamModelInTx(tx, m); err != nil {
		return err
	}
	return tx.Commit()
}

// RenameDownstreamModel saves the complete edit and its identity change in one
// transaction. Old price rows and usage remain available for historical billing.
func (s *Store) RenameDownstreamModel(oldID string, m DownstreamModel) error {
	return s.RenameDownstreamModelAtVersion(oldID, m, "")
}

func (s *Store) RenameDownstreamModelAtVersion(oldID string, m DownstreamModel, expected string) error {
	if oldID == m.ID {
		return NewConfigurationError("rename requires a different public model ID")
	}
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow("SELECT count(*) FROM downstream_models WHERE id=?", oldID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	if expected != "" {
		var before string
		var old DownstreamModel
		if err = tx.QueryRow("SELECT data FROM downstream_models WHERE id=?", oldID).Scan(&before); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(before), &old); err != nil {
			return err
		}
		if EntityVersion(old) != expected {
			return ErrEditConflict
		}
	}
	if err = tx.QueryRow("SELECT count(*) FROM downstream_models WHERE id=?", m.ID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrModelExists
	}
	var raw string
	if err = tx.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&raw); err != nil {
		return err
	}
	var settings Settings
	if err = json.Unmarshal([]byte(raw), &settings); err != nil {
		return err
	}
	var copied []Price
	for _, p := range defaultPrices(settings.Prices) {
		if p.ChannelID == 0 && p.Model == oldID {
			p.Model = m.ID
			copied = append(copied, p)
		}
	}
	for _, p := range defaultPrices(settings.Prices) {
		if p.ChannelID == 0 && p.Model == m.ID && len(copied) > 0 {
			return ErrModelPriceConflict
		}
	}
	if err = validateAndSaveDownstreamModelInTx(tx, m); err != nil {
		return err
	}
	if err = replacePricesInTx(tx, copied, func(Price) bool { return false }); err != nil {
		return err
	}
	if err = renameClientModelReferences(tx, oldID, m.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM downstream_models WHERE id=?", oldID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReorderDownstreamModels(order []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := make(map[string]bool, len(order))
	for idx, id := range order {
		if seen[id] {
			continue
		}
		seen[id] = true
		var raw string
		err := tx.QueryRow("SELECT data FROM downstream_models WHERE id=?", id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var m DownstreamModel
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		m.SortOrder = idx + 1
		updatedRaw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE downstream_models SET data=? WHERE id=?", string(updatedRaw), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) BatchUpdateDownstreamModelsStatus(ids []string, enabled bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var raw string
		err := tx.QueryRow("SELECT data FROM downstream_models WHERE id=?", id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var m DownstreamModel
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		m.Enabled = enabled
		updatedRaw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE downstream_models SET data=? WHERE id=?", string(updatedRaw), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) BatchDeleteDownstreamModels(ids []string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err := renameClientModelReferences(tx, id, ""); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM downstream_models WHERE id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteDownstreamModel(id string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = renameClientModelReferences(tx, id, ""); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM downstream_models WHERE id=?", id)
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
	return tx.Commit()
}

// ReplaceCatalogue adopts the models just pulled from an upstream as the
// channel's catalogue. The fetched list is the whole auto part: entries the
// upstream no longer returns disappear, which is what stops the catalogue from
// growing monotonically while providers shuffle their model ids. What survives
// is the fetched set plus the owner's manual entries — a pull never touches a
// hand-added model. The channel's enabled models are not touched either: a pull
// cannot take a working route away, so a model the fetch no longer lists stays
// enabled and simply stops being a catalogue entry (the console's re-enable
// notice is where the owner hears about that).
//
// A catalogue entry that goes takes its per-model config and capability
// evidence with it: the row has to stay savable, and those maps only accept
// models the channel still lists somewhere.
func (s *Store) ReplaceCatalogue(id int64, fetched []string) (Channel, error) {
	return s.replaceCatalogueAtVersion(id, fetched, "")
}

// ReplaceCatalogueAtVersion refuses stale discovery results after connection,
// credential or model configuration changed while the upstream was answering.
// expected may be EntityVersion (full row) or CatalogueSourceVersion (excludes
// only normal OAuth rotation material, retaining identity and all configuration).
func (s *Store) ReplaceCatalogueAtVersion(id int64, fetched []string, expected string, beforeCommit ...func() error) (Channel, error) {
	if expected == "" {
		return Channel{}, ErrEditConflict
	}
	return s.replaceCatalogueAtVersion(id, fetched, expected, beforeCommit...)
}
func (s *Store) replaceCatalogueAtVersion(id int64, fetched []string, expected string, beforeCommit ...func() error) (Channel, error) {
	return s.replaceProviderCatalogue(id, fetched, nil, false, expected, beforeCommit...)
}

// ReplaceProviderCatalogueAtVersion commits a managed model list and its wire
// protocols together. It cannot be called with an absent optimistic version.
func (s *Store) ReplaceProviderCatalogueAtVersion(id int64, formats map[string][]string, expected string, beforeCommit ...func() error) (Channel, error) {
	if expected == "" || formats == nil {
		return Channel{}, ErrEditConflict
	}
	fetched := make([]string, 0, len(formats))
	for model := range formats {
		fetched = append(fetched, model)
	}
	sort.Strings(fetched)
	return s.replaceProviderCatalogue(id, fetched, formats, true, expected, beforeCommit...)
}

func (s *Store) replaceProviderCatalogue(id int64, fetched []string, formats map[string][]string, managed bool, expected string, beforeCommit ...func() error) (Channel, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return Channel{}, err
	}
	actual := EntityVersion(c)
	if strings.HasPrefix(expected, "catalogue:") {
		actual = CatalogueSourceVersion(c)
	}
	if expected != "" && actual != expected {
		return Channel{}, ErrEditConflict
	}
	// No SQL transaction is open here: a caller gate may read the same Store's
	// single connection. Recheck after acquiring the write lifecycle lock.
	for _, check := range beforeCommit {
		if check != nil {
			if err := check(); err != nil {
				return Channel{}, err
			}
		}
	}
	seen := map[string]bool{}
	catalogue := make([]string, 0, len(fetched)+len(c.ManualModels))
	for _, list := range [][]string{fetched, c.ManualModels} {
		for _, m := range list {
			if m != "" && !seen[m] {
				seen[m] = true
				catalogue = append(catalogue, m)
			}
		}
	}
	c.AvailableModels = catalogue
	if managed {
		before := c
		c.ModelFormats = make(map[string][]string, len(formats))
		for model, format := range formats {
			c.ModelFormats[model] = append([]string{}, format...)
		}
		if continuationDomainChanged(before, c) {
			c.Generation++
		}
	}
	pruneModelEntries(&c)
	return c, s.writeChannelInPlace(&c)
}

// CatalogueSourceVersion permits an in-flight OAuth catalogue pull to refresh
// its own tokens without accepting another credential/account/configuration.
// Generation is retained: explicit credential replacement still conflicts.
func CatalogueSourceVersion(c Channel) string {
	withoutRotation := func(o *OAuthCredential) *OAuthCredential {
		if o == nil {
			return nil
		}
		copy := *o
		copy.AccessToken = ""
		copy.RefreshToken = ""
		copy.IDToken = ""
		// Compatible state contains rotating tokens; its stable owner is
		// represented by Provider/Account/APIBase, which remain in this hash.
		copy.PluginState = ""
		copy.ExpiresAt = time.Time{}
		copy.UpdatedAt = time.Time{}
		return &copy
	}
	c.OAuth = withoutRotation(c.OAuth)
	c.Credentials = append([]CredentialItem(nil), c.Credentials...)
	for i := range c.Credentials {
		c.Credentials[i].OAuth = withoutRotation(c.Credentials[i].OAuth)
	}
	return "catalogue:" + EntityVersion(c)
}

// DeleteModel removes one model from a channel: the catalogue entry, the manual
// marker, the enabled list, and the per-model config and evidence. Both a
// catalogue-only entry and an enabled one may be deleted — the delete is the
// operator's decision about their own upstream, and the channel is the record
// of it. ErrNotFound means the channel does not exist, or does list no such
// model.
func (s *Store) DeleteModel(id int64, model string) (Channel, error) {
	return s.DeleteModelAtVersion(id, model, "")
}

func (s *Store) DeleteModelAtVersion(id int64, model, expected string) (Channel, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(id)
	if err != nil {
		return Channel{}, err
	}
	if expected != "" && EntityVersion(c) != expected {
		return Channel{}, ErrEditConflict
	}
	without := func(list []string) []string {
		out := make([]string, 0, len(list))
		for _, v := range list {
			if v != model {
				out = append(out, v)
			}
		}
		return out
	}
	found := containsString(c.Models, model) || containsString(c.AvailableModels, model) || containsString(c.ManualModels, model)
	if !found {
		return Channel{}, ErrNotFound
	}
	c.Models = without(c.Models)
	c.AvailableModels = without(c.AvailableModels)
	c.ManualModels = without(c.ManualModels)
	pruneModelEntries(&c)
	return c, s.writeChannelInPlace(&c)
}

// pruneModelEntries drops the per-model config and capability evidence of models
// the channel no longer lists anywhere. Both maps may only name models the
// channel still has (ValidateChannel), so an entry that leaves the row takes its
// config with it instead of making the row unsavable.
func pruneModelEntries(c *Channel) {
	for model := range c.ModelFormats {
		if !containsString(c.Models, model) && !containsString(c.AvailableModels, model) {
			delete(c.ModelFormats, model)
		}
	}
	for id := range c.ModelTags {
		if !containsString(c.Models, id) && !containsString(c.AvailableModels, id) {
			delete(c.ModelTags, id)
		}
	}
	for id := range c.ModelConfigs {
		if !containsString(c.Models, id) && !containsString(c.AvailableModels, id) {
			delete(c.ModelConfigs, id)
		}
	}
	for id := range c.ModelCatalogRefs {
		if !containsString(c.Models, id) && !containsString(c.AvailableModels, id) {
			delete(c.ModelCatalogRefs, id)
		}
	}
	for id := range c.ModelEvidence {
		if !containsString(c.Models, id) && !containsString(c.AvailableModels, id) {
			delete(c.ModelEvidence, id)
		}
	}
}

// writeChannelInPlace persists an in-place model change to an already-read
// channel. It sits inside the callers' channelSaveMu critical section, so it
// writes through the same validation a console save uses (the row must stay
// savable after the change) without re-taking the lock SaveChannel takes.
func (s *Store) writeChannelInPlace(c *Channel) error {
	if err := ValidateChannel(*c); err != nil {
		return err
	}
	return s.persistRow(c)
}
