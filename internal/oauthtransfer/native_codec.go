package oauthtransfer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"torouter/internal/accessguard"
	"torouter/internal/antigravity"
	"torouter/internal/claude"
	"torouter/internal/compatprofile"
	"torouter/internal/store"
)

// NativeFile contains one tool's credential object, never a transfer envelope.
type NativeFile struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

func nativeString(m map[string]any, names ...string) string {
	for _, k := range names {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
func nativeTime(v any) time.Time {
	switch x := v.(type) {
	case string:
		t, _ := time.Parse(time.RFC3339Nano, x)
		return t
	case float64:
		if x > 1e11 {
			return time.UnixMilli(int64(x))
		}
		if x > 0 {
			return time.Unix(int64(x), 0)
		}
	}
	return time.Time{}
}
func nativeScopes(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	a, _ := v.([]any)
	var out []string
	for _, v := range a {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}
func nativeItem(kind, provider, format, base string, o store.OAuthCredential) store.OAuthTransferItem {
	o.Provider = provider
	o.APIBase = base
	o.Status = store.OAuthStatusActive
	return store.OAuthTransferItem{CredentialType: kind, Enabled: true, Endpoints: []store.Endpoint{{Format: format, BaseURL: base, State: "stateless"}}, OAuth: &o}
}

// ParseNativeCredentialsForType is offline. An empty kind only recognizes
// unambiguous tool shapes; opaque plugin auth requires an explicit type.
func ParseNativeCredentialsForType(ctx context.Context, kind string, raw []byte) ([]store.OAuthTransferItem, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > MaxFileBytes {
		return nil, ErrLimit
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, ErrFile
	}
	var objects []any
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, r := range x {
				walk(r)
			}
		case map[string]any:
			if a, ok := x["accounts"].([]any); ok {
				walk(a)
			} else {
				isHosts := kind == store.CredentialCopilotOAuth || kind == ""
				found := false
				if isHosts && x["oauth_token"] == nil {
					for key, v := range x {
						if child, ok := v.(map[string]any); ok && child["oauth_token"] != nil {
							objects = append(objects, map[string]any{key: child})
							found = true
						}
					}
				}
				if !found {
					objects = append(objects, x)
				}
			}
		default:
			objects = append(objects, v)
		}
	}
	walk(value)
	if len(objects) == 0 || len(objects) > 100 {
		return nil, ErrLimit
	}
	out := make([]store.OAuthTransferItem, 0, len(objects))
	for i, v := range objects {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: 第 %d 项凭证格式无效", ErrFile, i+1)
		}
		k := kind
		if k == "" {
			switch {
			case m["claudeAiOauth"] != nil:
				k = store.CredentialClaudeOAuth
			case m["tokens"] != nil || m["meta"] != nil || m["id_token"] != nil:
				k = store.CredentialCodexOAuth
			case m["clientId"] != nil && m["hostId"] != nil:
				k = store.CredentialChatGPTOAuth
			case m["type"] == "antigravity" || m["project_id"] != nil || m["token"] != nil:
				k = store.CredentialAntigravityOAuth
			case m["oauth_token"] != nil:
				k = store.CredentialCopilotOAuth
			default:
				for _, v := range m {
					if child, ok := v.(map[string]any); ok && child["oauth_token"] != nil {
						k = store.CredentialCopilotOAuth
						break
					}
				}
			}
		}
		item, err := parseNativeObject(k, m)
		if err != nil {
			return nil, fmt.Errorf("%w: 第 %d 项凭证无法解析为所选类型", ErrFile, i+1)
		}
		if store.ValidateOAuthTransferMaterial(item) != nil {
			return nil, fmt.Errorf("%w: 第 %d 项凭证材料不完整", ErrFile, i+1)
		}
		if !store.IsCompatibleOAuth(k) {
			item.OAuth.PluginState = string(mustNativeJSON(m))
		}
		out = append(out, item)
	}
	return out, nil
}
func parseNativeObject(kind string, m map[string]any) (store.OAuthTransferItem, error) {
	raw, _ := json.Marshal(m)
	switch kind {
	case store.CredentialCodexOAuth:
		if typ := nativeString(m, "type"); typ != "" && typ != "codex" && typ != "oauth" {
			return store.OAuthTransferItem{}, ErrFile
		}
		if item, ok := parseSingleNative(raw); ok {
			return item, nil
		}
	case store.CredentialClaudeOAuth:
		if typ := nativeString(m, "type"); typ != "" && typ != "claude" && typ != "oauth" {
			return store.OAuthTransferItem{}, ErrFile
		}
		tok := m
		if child, ok := m["claudeAiOauth"].(map[string]any); ok {
			tok = child
		}
		o := store.OAuthCredential{AccessToken: nativeString(tok, "accessToken", "access_token"), RefreshToken: nativeString(tok, "refreshToken", "refresh_token"), ClientID: claude.ClientID, Account: nativeString(tok, "account_uuid", "accountUuid"), Subject: nativeString(tok, "account_uuid", "accountUuid"), OrganizationID: nativeString(tok, "organization_uuid"), DeviceID: nativeString(tok, "device_id"), ExpiresAt: nativeTime(tok["expiresAt"]), Scopes: nativeScopes(tok["scopes"])}
		if o.ExpiresAt.IsZero() {
			o.ExpiresAt = nativeTime(tok["expires_at"])
		}
		return nativeItem(kind, store.OAuthProviderClaude, store.FormatAnthropicMessages, "https://api.anthropic.com", o), nil
	case store.CredentialAntigravityOAuth:
		if typ := nativeString(m, "type"); typ != "" && typ != "antigravity" {
			return store.OAuthTransferItem{}, ErrFile
		}
		tok := m
		if child, ok := m["token"].(map[string]any); ok {
			tok = child
		}
		o := store.OAuthCredential{AccessToken: nativeString(tok, "access_token", "accessToken"), RefreshToken: nativeString(tok, "refresh_token", "refreshToken"), ClientID: antigravity.DefaultEndpoints.ClientID, Account: nativeString(m, "email"), Subject: nativeString(tok, "sub", "id"), ProjectID: nativeString(tok, "project_id", "projectId", "project"), ExpiresAt: nativeTime(tok["expiry_timestamp"]), Scopes: nativeScopes(tok["scopes"])}
		if o.ProjectID == "" {
			o.ProjectID = nativeString(m, "project_id", "projectId", "project")
		}
		if o.Account == "" {
			o.Account = nativeString(tok, "email")
		}
		if o.ExpiresAt.IsZero() {
			o.ExpiresAt = nativeTime(tok["expires_at"])
		}
		return nativeItem(kind, store.OAuthProviderAntigravity, store.FormatGeminiGenerateContent, antigravity.DefaultEndpoints.APIBase, o), nil
	case store.CredentialChatGPTOAuth:
		o := store.OAuthCredential{ClientID: nativeString(m, "clientId"), HostID: nativeString(m, "hostId"), Subject: nativeString(m, "sub"), IDToken: nativeString(m, "idToken"), AccessToken: nativeString(m, "access"), RefreshToken: nativeString(m, "refresh"), ExpiresAt: nativeTime(m["expires"]), Scopes: nativeScopes(m["scopes"])}
		return nativeItem(kind, store.OAuthProviderChatGPT, store.FormatOpenAIResponses, "https://api.openai.com/v1", o), nil
	case store.CredentialCopilotOAuth:
		if nativeString(m, "oauth_token") == "" {
			for key, v := range m {
				if tok, ok := v.(map[string]any); ok && nativeString(tok, "oauth_token") != "" {
					if key != "github.com" && !strings.HasPrefix(key, "github.com:") {
						return store.OAuthTransferItem{}, ErrFile
					}
					m = tok
					break
				}
			}
		}
		o := store.OAuthCredential{GitHubToken: nativeString(m, "oauth_token")}
		return nativeItem(kind, store.OAuthProviderCopilot, store.FormatOpenAIChatCompletions, "https://api.githubcopilot.com", o), nil
	case store.CredentialDevinOAuth:
		// Magpie's saved API auth shape contains its existing session token.
		// This parser does not claim arbitrary Devin CLI keys are interoperable.
		if nativeString(m, "type") != "api" {
			return store.OAuthTransferItem{}, ErrFile
		}
		o := store.OAuthCredential{AccessToken: nativeString(m, "key")}
		return nativeItem(kind, store.OAuthProviderDevin, store.FormatOpenAIResponses, "https://server.codeium.com", o), nil
	default:
		if p, ok := compatprofile.Lookup(kind); ok {
			if p.Package == "workbuddy" || p.Package == "minimax" {
				if nativeString(m, "sessionID") == "" {
					m["sessionID"] = uuid.NewString()
				}
			}
			raw, _ = json.Marshal(m)
			o, err := store.CompatibleOAuthCredential(kind, string(raw))
			if err != nil {
				return store.OAuthTransferItem{}, err
			}
			return nativeItem(kind, p.ID, p.Format(), p.API, o), nil
		}
	}
	return store.OAuthTransferItem{}, ErrFile
}
func EncodeNativeCredential(item store.OAuthTransferItem) (NativeFile, error) {
	if store.ValidateOAuthTransferMaterial(item) != nil {
		return NativeFile{}, ErrInvalid
	}
	o := item.OAuth
	var value any
	name := "auth.json"
	switch item.CredentialType {
	case store.CredentialCodexOAuth:
		value = map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]any{"access_token": o.AccessToken, "refresh_token": o.RefreshToken, "id_token": o.IDToken, "account_id": o.Account}}
	case store.CredentialClaudeOAuth:
		name = ".credentials.json"
		value = map[string]any{"claudeAiOauth": map[string]any{"accessToken": o.AccessToken, "refreshToken": o.RefreshToken, "expiresAt": nativeMillis(o.ExpiresAt), "scopes": strings.Fields(o.Scopes), "account_uuid": o.Account, "organization_uuid": o.OrganizationID, "device_id": o.DeviceID}}
	case store.CredentialAntigravityOAuth:
		name = "antigravity.json"
		value = map[string]any{"type": "antigravity", "email": o.Account, "access_token": o.AccessToken, "refresh_token": o.RefreshToken, "project_id": o.ProjectID, "expires_at": o.ExpiresAt, "sub": o.Subject, "scopes": strings.Fields(o.Scopes)}
	case store.CredentialChatGPTOAuth:
		name = "chatgpt.json"
		value = map[string]any{"clientId": o.ClientID, "hostId": o.HostID, "sub": o.Subject, "idToken": o.IDToken, "access": o.AccessToken, "refresh": o.RefreshToken, "expires": o.ExpiresAt, "scopes": strings.Fields(o.Scopes)}
	case store.CredentialCopilotOAuth:
		name = "hosts.json"
		value = map[string]any{"github.com": map[string]any{"oauth_token": o.GitHubToken}}
	case store.CredentialDevinOAuth:
		name = "devin.json"
		value = map[string]any{"type": "api", "key": o.AccessToken}
	default:
		if p, ok := compatprofile.Lookup(item.CredentialType); ok {
			name = p.ID + ".json"
			return NativeFile{name, o.PluginState}, nil
		}
		return NativeFile{}, ErrInvalid
	}
	if o.PluginState != "" {
		var original map[string]any
		if json.Unmarshal([]byte(o.PluginState), &original) == nil {
			updates, _ := value.(map[string]any)
			// A source file may carry both flat and nested aliases. Refreshes
			// must replace every present token/expiry slot, not leave a stale
			// alternative for another tool to consume.
			switch item.CredentialType {
			case store.CredentialCodexOAuth:
				for key, v := range map[string]any{"access_token": o.AccessToken, "refresh_token": o.RefreshToken, "id_token": o.IDToken, "account_id": o.Account} {
					if _, ok := original[key]; ok {
						original[key] = v
					}
				}
			case store.CredentialClaudeOAuth:
				for key, v := range map[string]any{"accessToken": o.AccessToken, "refreshToken": o.RefreshToken, "access_token": o.AccessToken, "refresh_token": o.RefreshToken, "expiresAt": nativeMillis(o.ExpiresAt), "expires_at": o.ExpiresAt} {
					if _, ok := original[key]; ok {
						original[key] = v
					}
				}
			case store.CredentialAntigravityOAuth:
				refreshNativeGoogleSlots(original, o)
				if tok, ok := original["token"].(map[string]any); ok {
					refreshNativeGoogleSlots(tok, o)
				}
			}

			if item.CredentialType == store.CredentialCopilotOAuth {
				if _, ok := original["oauth_token"]; ok {
					original["oauth_token"] = o.GitHubToken
					updates = map[string]any{}
				}
				for key, v := range original {
					if child, ok := v.(map[string]any); ok && child["oauth_token"] != nil {
						child["oauth_token"] = o.GitHubToken
						updates = map[string]any{}
						if strings.Contains(key, ":") {
							name = "apps.json"
						}
					}
				}
			}
			if item.CredentialType == store.CredentialAntigravityOAuth {
				if tok, ok := original["token"].(map[string]any); ok {
					for k, v := range updates {
						if k != "type" {
							tok[k] = v
						}
					}
				}
			}
			for k, v := range updates {
				if nested, ok := v.(map[string]any); ok {
					if old, ok := original[k].(map[string]any); ok {
						for nk, nv := range nested {
							old[nk] = nv
						}
						v = old
					}
				}
				original[k] = v
			}
			value = original
		}
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(raw) > MaxFileBytes {
		return NativeFile{}, ErrLimit
	}
	return NativeFile{name, string(raw)}, nil
}
func nativeMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func mustNativeJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func refreshNativeGoogleSlots(m map[string]any, o *store.OAuthCredential) {
	for key, v := range map[string]any{"accessToken": o.AccessToken, "refreshToken": o.RefreshToken, "access_token": o.AccessToken, "refresh_token": o.RefreshToken, "projectId": o.ProjectID, "project_id": o.ProjectID, "expires_at": o.ExpiresAt} {
		if _, ok := m[key]; ok {
			m[key] = v
		}
	}
	if old, ok := m["expiry_timestamp"].(float64); ok {
		if old > 1e11 {
			m["expiry_timestamp"] = nativeMillis(o.ExpiresAt)
		} else if o.ExpiresAt.IsZero() {
			m["expiry_timestamp"] = 0
		} else {
			m["expiry_timestamp"] = o.ExpiresAt.Unix()
		}
	}
}
