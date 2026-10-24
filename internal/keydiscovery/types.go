// Package keydiscovery reads selected local credential configurations.
// Scanning never logs in, refreshes OAuth, executes tools or probes upstreams.
package keydiscovery

import (
	"context"
	"errors"
	"time"

	"torouter/internal/store"
)

type Source struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Icon  string   `json:"icon"`
	Files []string `json:"-"`
}

func Sources() []Source {
	return []Source{
		{"source-codex", "Codex", "codex", []string{"config.toml", "auth.json"}},
		{"source-claude", "Claude Code", "claudecode", []string{"settings.json", ".credentials.json"}},
		{"source-copilot", "GitHub Copilot（编辑器文件）", "githubcopilot", []string{"apps.json", "hosts.json"}},
		{"source-opencode", "OpenCode", "opencode", []string{"opencode.jsonc", "opencode.json", "config.json", "auth.json"}},
		{"source-zcode", "ZCode", "zai", []string{"provider_config.json", "setting.json", "config.json"}},
		{"source-ccswitch", "CC Switch", "ccswitch", []string{"cc-switch.db", "codex_oauth_auth.json", "copilot_auth.json"}},
		{"source-codex-manager", "Codex-Manager", "codex", []string{"codexmanager.db"}},
		{"source-magpie", "Magpie", "magpie", []string{"providers.json", "settings.json", "logins.json"}},
	}
}

func Lookup(id string) (Source, bool) {
	for _, s := range Sources() {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

var (
	ErrMissing  = errors.New("source file was not found")
	ErrInvalid  = errors.New("invalid API key discovery request")
	ErrConflict = errors.New("discovery source or target changed")
	ErrExpired  = errors.New("discovery session expired or inaccessible")
)

// Resources only serves the selected source's declared file names under one
// discovered default root. Query works on a private consistent SQLite snapshot, never the
// live file. Query text is compiled into trusted readers, not accepted over RPC.
type Resources interface {
	Read(context.Context, string) ([]byte, error)
	Query(context.Context, string, string) ([][]string, error)
}

// Candidate is private to the service. The public preview has only a mask and
// an opaque random reference. Endpoints remain per-key, not per visual URL group.
type Candidate struct {
	Key       string
	Endpoints []store.Endpoint
	Disabled  bool
	OAuth     *store.OAuthTransferItem
}

type Diagnostic struct {
	SourceID string `json:"source_id"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type SourceStatus struct {
	Source
	Enabled bool `json:"enabled"`
}

type SourceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

type Target struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Row struct {
	CredentialType string      `json:"credential_type"`
	ID             string      `json:"id"`
	MaskedKey      string      `json:"masked_key"`
	Sources        []SourceRef `json:"sources"`
	Status         string      `json:"status"`
	Disabled       bool        `json:"disabled"`
	Formats        []string    `json:"formats"`
	Targets        []Target    `json:"targets"`
	Reason         string      `json:"reason,omitempty"`
}

type Group struct {
	Existing    bool     `json:"existing"`
	TargetCount int      `json:"target_count"`
	CopyOnly    bool     `json:"copy_only"`
	Targets     []Target `json:"targets"`
	DisplayIcon string   `json:"display_icon"`
	ID          string   `json:"id"`
	URLDisplay  string   `json:"url_display"`
	Formats     []string `json:"formats"`
	Rows        []Row    `json:"rows"`
}

type ScanStatus struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Status      string       `json:"status"` // pending, scanning, complete, error
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type Preview struct {
	Target         *Target      `json:"target,omitempty"`
	CredentialType string       `json:"credential_type,omitempty"`
	ID             string       `json:"id"`
	ExpiresAt      time.Time    `json:"expires_at"`
	Sources        []ScanStatus `json:"sources"`
	Groups         []Group      `json:"groups"`
}

type BeginOptions struct {
	TargetChannelID int64  `json:"target_channel_id,omitempty"`
	CredentialType  string `json:"credential_type,omitempty"`
}

type Selection struct {
	Enabled         *bool  `json:"enabled,omitempty"`
	DisplayIcon     string `json:"display_icon,omitempty"`
	CandidateID     string `json:"candidate_id"`
	TargetChannelID int64  `json:"target_channel_id"`
	ExpectedVersion string `json:"expected_version"`
	Name            string `json:"name,omitempty"`
}

type ImportResult struct {
	CandidateID  string   `json:"candidate_id"`
	CandidateIDs []string `json:"candidate_ids"`
	ChannelID    int64    `json:"channel_id,omitempty"`
	Added        int      `json:"added"`
	Skipped      int      `json:"skipped"`
	Status       string   `json:"status"`
	Message      string   `json:"message,omitempty"`
}

type ImportResponse struct {
	Results []ImportResult `json:"results"`
	Preview Preview        `json:"preview"`
}

func (r ImportResponse) OperationStatus() string {
	failed, finished := false, false
	for _, result := range r.Results {
		if result.Status == "failed" {
			failed = true
		} else {
			finished = true
		}
	}
	if failed && finished {
		return "partial"
	}
	if failed {
		return "failed"
	}
	return "completed"
}

// Reader diagnostics must use controlled text, never parser/SQL errors that
// might contain key values, account names or private filesystem paths.
func diagnostic(code string) Diagnostic {
	messages := map[string]string{
		"not_found":         "未找到配置。",
		"invalid_format":    "配置格式无法解析，请核对来源版本。",
		"oauth_skipped":     "登录材料不完整或尚不支持此格式。",
		"reference_skipped": "凭证是环境变量、文件引用或加密内容，未自动展开。",
		"unsupported":       "此配置的协议、认证或额外请求条件暂不能等价导入。",
		"incomplete":        "配置缺少必要的地址、协议或 API Key。",
		"proxy_skipped":     "已跳过来源网关的入口或代理占位凭证。",
		"redirect_scope":    "配置目录无法读取。",
		"limit":             "此来源超出单次预览限额，请缩小配置后重试。",
	}
	message := messages[code]
	if message == "" {
		message = "读取来源未完成，请核对配置。"
	}
	return Diagnostic{Code: code, Message: message}
}

// CopyResponse is returned only by the explicit sensitive copy operation.
type CopyItem struct {
	CredentialType string      `json:"credential_type"`
	URL            string      `json:"url"`
	Formats        []string    `json:"formats"`
	Sources        []SourceRef `json:"sources"`
	APIKey         string      `json:"api_key,omitempty"`
	Filename       string      `json:"filename,omitempty"`
	Content        string      `json:"content,omitempty"`
}
type CopyResponse struct {
	Items []CopyItem `json:"items"`
}
