package store

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"strconv"
	"strings"
)

// ChannelDisplay contains console-only preferences, independent of routing
// configuration, credential order and downstream binding priorities.
type ChannelDisplay struct {
	SortMode string            `json:"sort_mode"`
	Order    []int64           `json:"order"`
	Icons    map[string]string `json:"icons"`
}

type ChannelDisplayIcon struct {
	ChannelID int64  `json:"channel_id"`
	Value     string `json:"value"`
}

type ChannelDisplayPatch struct {
	SortMode *string
	Order    *[]int64
	Icon     *ChannelDisplayIcon
}

var ErrInvalidChannelDisplay = errors.New("invalid channel display preferences")

const channelDisplayKey = "channel_display"
const maxChannelIconBytes = 64 << 10

// initChannelDisplay fills the mode once at the storage boundary. Existing
// manual sequences keep their meaning; new preferences default to name order.
func (s *Store) initChannelDisplay() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRow("SELECT value FROM meta WHERE key=?", channelDisplayKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var v ChannelDisplay
	if json.Unmarshal([]byte(raw), &v) != nil {
		// A malformed optional UI preference remains a display-read error;
		// it must not prevent the gateway from starting.
		return nil
	}
	if v.SortMode != "" {
		return nil
	}
	v.SortMode = "name"
	if len(v.Order) > 0 {
		v.SortMode = "manual"
	}
	if err = writeChannelDisplay(tx, v); err != nil {
		return err
	}
	return tx.Commit()
}

func invalidChannelDisplay(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidChannelDisplay, message)
}

func validateChannelIcon(value string) error {
	switch value {
	case "", "generic", "openai", "anthropic", "deepseek", "moonshot", "zhipu", "minimax", "stepfun", "nvidia", "openrouter", "xiaomi":
		return nil
	case "gemini", "qwen", "doubao", "hunyuan", "mistral", "xai", "meta", "perplexity", "vertexai", "bedrock", "azure", "siliconflow",
		"codex", "claude", "claudecode", "githubcopilot", "geminicli", "antigravity", "opencode", "qoder", "devin", "cursor", "ollama", "lmstudio", "kimi", "zai", "codebuddy", "ccswitch", "magpie":
		return nil
	case "cloudflare", "cline", "factory", "kiro", "trae", "zed", "commandcode", "mimocode", "workbuddy", "nousresearch", "sensenova", "zcode":
		return nil
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(value, prefix) {
		return invalidChannelDisplay("icon must be a built-in ID or PNG data URL")
	}
	encoded := strings.TrimPrefix(value, prefix)
	if len(encoded) > base64.StdEncoding.EncodedLen(maxChannelIconBytes) {
		return invalidChannelDisplay("icon PNG exceeds 64 KiB")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > maxChannelIconBytes {
		return invalidChannelDisplay("invalid or oversized icon PNG")
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 128 || config.Height > 128 {
		return invalidChannelDisplay("icon must be a PNG of 1..128 pixels per side")
	}
	if _, err = png.Decode(bytes.NewReader(raw)); err != nil {
		return invalidChannelDisplay("invalid icon PNG")
	}
	return nil
}

// readChannelDisplay uses only channel IDs; it never reads or decrypts secrets.
// Deleted identities are omitted even if older metadata still refers to them.
func readChannelDisplay(tx *sql.Tx) (ChannelDisplay, map[int64]bool, error) {
	v := ChannelDisplay{SortMode: "name", Order: []int64{}, Icons: map[string]string{}}
	var raw string
	err := tx.QueryRow("SELECT value FROM meta WHERE key=?", channelDisplayKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return v, nil, err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			return v, nil, err
		}
	}
	ids := map[int64]bool{}
	rows, err := tx.Query("SELECT id FROM channels")
	if err != nil {
		return v, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return v, nil, err
		}
		ids[id] = true
	}
	if err = rows.Err(); err != nil {
		return v, nil, err
	}
	order := []int64{}
	seen := map[int64]bool{}
	for _, id := range v.Order {
		if ids[id] && !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	v.Order = order
	icons := map[string]string{}
	for key, value := range v.Icons {
		id, err := strconv.ParseInt(key, 10, 64)
		if err == nil && ids[id] && value != "" {
			icons[key] = value
		}
	}
	v.Icons = icons
	return v, ids, nil
}

func writeChannelDisplay(tx *sql.Tx, v ChannelDisplay) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", channelDisplayKey, string(raw))
	return err
}

func (s *Store) GetChannelDisplay() (ChannelDisplay, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ChannelDisplay{}, err
	}
	defer tx.Rollback()
	v, _, err := readChannelDisplay(tx)
	if err != nil {
		return ChannelDisplay{}, err
	}
	return v, tx.Commit()
}

// PatchChannelDisplay changes only the supplied preference in one transaction.
// Switching sort mode preserves the manual sequence. An empty icon restores
// automatic matching; an explicit empty order clears only the manual sequence.
func (s *Store) PatchChannelDisplay(patch ChannelDisplayPatch) (ChannelDisplay, error) {
	if patch.SortMode == nil && patch.Order == nil && patch.Icon == nil {
		return ChannelDisplay{}, invalidChannelDisplay("empty patch")
	}
	if patch.SortMode != nil && *patch.SortMode != "name" && *patch.SortMode != "manual" {
		return ChannelDisplay{}, invalidChannelDisplay("sort_mode must be name or manual")
	}
	if patch.Icon != nil {
		if err := validateChannelIcon(patch.Icon.Value); err != nil {
			return ChannelDisplay{}, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ChannelDisplay{}, err
	}
	defer tx.Rollback()
	v, ids, err := readChannelDisplay(tx)
	if err != nil {
		return ChannelDisplay{}, err
	}
	if patch.SortMode != nil {
		v.SortMode = *patch.SortMode
	}
	if patch.Order != nil {
		seen := map[int64]bool{}
		order := []int64{}
		for _, id := range *patch.Order {
			if id <= 0 || !ids[id] || seen[id] {
				return ChannelDisplay{}, invalidChannelDisplay("order requires unique existing positive channel IDs")
			}
			seen[id] = true
			order = append(order, id)
		}
		v.Order = order
	}
	if patch.Icon != nil {
		if patch.Icon.ChannelID <= 0 || !ids[patch.Icon.ChannelID] {
			return ChannelDisplay{}, invalidChannelDisplay("icon requires an existing positive channel ID")
		}
		key := strconv.FormatInt(patch.Icon.ChannelID, 10)
		if patch.Icon.Value == "" {
			delete(v.Icons, key)
		} else {
			v.Icons[key] = patch.Icon.Value
		}
	}
	if err = writeChannelDisplay(tx, v); err != nil {
		return ChannelDisplay{}, err
	}
	return v, tx.Commit()
}
