// Package compatprofile describes the fixed, reviewed external provider
// contracts. It is data/validation only: no runtime, storage or network.
package compatprofile

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const (
	QoderCredential         = "plugin:qoder"
	QoderCNCredential       = "plugin:qoder-cn"
	WorkBuddyCredential     = "plugin:workbuddy"
	WorkBuddyAICredential   = "plugin:workbuddy-ai"
	MiniMaxCredential       = "plugin:minimax-code"
	MiniMaxGlobalCredential = "plugin:minimax-code-global"
	MaxStateBytes           = 1 << 20
)

type Profile struct {
	ID, Name, CredentialType, Export, API, Web, OpenAPI string
	Package, Version, Icon                              string
}

func Profiles() []Profile {
	return []Profile{
		{"qoder", "Qoder", QoderCredential, "QoderAuthPlugin", "https://api3.qoder.sh", "https://qoder.com", "https://openapi.qoder.sh", "qoder", "0.2.19 / to-router 1", "qoder"},
		{"qoder-cn", "Qoder CN", QoderCNCredential, "QoderCNAuthPlugin", "https://gateway.qoder.com.cn", "https://qoder.cn", "https://openapi.qoder.com.cn", "qoder", "0.2.19 / to-router 1", "qoder"},
		{"workbuddy", "WorkBuddy", WorkBuddyCredential, "WorkBuddyAuthPlugin", "https://copilot.tencent.com/v2", "https://www.codebuddy.cn", "https://copilot.tencent.com", "workbuddy", "0.1.25 / to-router 1", "codebuddy"},
		{"workbuddy-ai", "WorkBuddy AI", WorkBuddyAICredential, "WorkBuddyAIAuthPlugin", "https://www.workbuddy.ai/v2", "https://www.workbuddy.ai", "https://www.workbuddy.ai", "workbuddy", "0.1.25 / to-router 1", "codebuddy"},
		{"minimax-code", "MiniMax Code", MiniMaxCredential, "MiniMaxCodeAuthPlugin", "https://agent.minimax.cn/mavis/api/v1/llm/v1", "https://account.minimax.cn", "https://agent.minimaxi.com", "minimax", "0.1.2 / to-router 1", "minimax"},
		{"minimax-code-global", "MiniMax Code 国际版", MiniMaxGlobalCredential, "MiniMaxCodeGlobalAuthPlugin", "https://agent.minimax.io/mavis/api/v1/llm/v1", "https://account.minimax.io", "https://agent.minimax.io", "minimax", "0.1.2 / to-router 1", "minimax"},
	}
}

func (p Profile) Format() string {
	if p.Package == "minimax" {
		return "anthropic_messages"
	}
	return "openai_chat_completions"
}

func (p Profile) NPM() string {
	if p.Package == "minimax" {
		return "@ai-sdk/anthropic"
	}
	return "@ai-sdk/openai-compatible"
}

// Authorization URLs come from the verified provider, but are still limited
// to that region's observed product sites before being offered to the user.
func (p Profile) AuthorizationURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Scheme != "https" || u.Fragment != "" || len(raw) > 8192 || u.Path == "" {
		return false
	}
	origin := u.Scheme + "://" + u.Host
	switch p.Package {
	case "qoder":
		return origin == p.Web && u.Path == "/device/selectAccounts"
	case "minimax":
		return origin == p.Web
	case "workbuddy":
		if p.ID == "workbuddy" {
			return origin == p.Web || origin == p.OpenAPI || origin == "https://www.workbuddy.cn"
		}
		return origin == p.Web || origin == "https://www.codebuddy.ai"
	}
	return false
}

func Lookup(credentialType string) (Profile, bool) {
	for _, p := range Profiles() {
		if p.CredentialType == credentialType {
			return p, true
		}
	}
	return Profile{}, false
}

var ErrState = errors.New("兼容插件授权状态无效，请重新独立授权")

// State validates the supported authorization branch while retaining every
// opaque field. Tokens are never split into another independently writable copy.
// PAT/VPC/native-login imports are not part of the reviewed device flow.
func (p Profile) State(raw string) (canonical, account string, err error) {
	if len(raw) == 0 || len(raw) > MaxStateBytes {
		return "", "", ErrState
	}
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &value) != nil || value == nil {
		return "", "", ErrState
	}
	text := func(key string) string {
		var v string
		_ = json.Unmarshal(value[key], &v)
		return strings.TrimSpace(v)
	}
	for _, key := range []string{"vpc", "pat", "patAuth"} {
		if v, ok := value[key]; ok && string(v) != "null" && string(v) != "false" && string(v) != `""` {
			return "", "", ErrState
		}
	}
	if text("type") != "oauth" || text("uid") == "" || text("access") == "" || text("refresh") == "" {
		return "", "", ErrState
	}
	var expires float64
	if json.Unmarshal(value["expires"], &expires) != nil || expires <= 0 {
		return "", "", ErrState
	}
	var deviceChat bool
	if v, ok := value["deviceChat"]; ok && json.Unmarshal(v, &deviceChat) != nil {
		return "", "", ErrState
	}
	if deviceChat && p.ID != "qoder-cn" {
		return "", "", ErrState
	}
	account = text("uid")
	switch p.Package {
	case "workbuddy":
		if text("source") != "" || !uuid.MatchString(text("sessionID")) {
			return "", "", ErrState
		}
		if domain := text("domain"); domain != "" && (len(domain) > 253 || !dnsName.MatchString(domain)) {
			return "", "", ErrState
		}
		if raw, ok := value["enterpriseId"]; ok && string(raw) != "null" {
			var enterprise string
			if json.Unmarshal(raw, &enterprise) != nil {
				return "", "", ErrState
			}
		}
		identity, _ := json.Marshal([]string{account, text("enterpriseId")})
		account = string(identity)
	case "minimax":
		if text("realUserID") != account || !uuid.MatchString(text("sessionID")) {
			return "", "", ErrState
		}
	case "qoder":
	default:
		return "", "", ErrState
	}
	out, err := json.Marshal(value)
	if err != nil {
		return "", "", ErrState
	}
	return string(out), account, nil
}

var dnsName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)+$`)
var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
