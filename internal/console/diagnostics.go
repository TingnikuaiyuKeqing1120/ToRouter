package console

import (
	"encoding/json"
	"regexp"
	"strings"
)

type diagnostic struct {
	Source         string `json:"source"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	Suggestion     string `json:"suggestion"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	Raw            string `json:"raw,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
}

var credentialPattern = regexp.MustCompile(`(?i)(bearer\s+|(?:api[_-]?key|token|secret|authorization)\s*[=:]\s*)[^\s,"<>]+`)

// tokenPattern catches the bare credential shapes that reach logs and upstream
// error bodies: the gateway's own tr_* tokens, GitHub's token prefixes, and
// JWTs (the Copilot bearer is one). Substring matching, not parsing — a redaction
// that misses a shape leaks it.
// Bare administrator keys have tr_ plus 43 RawURL base64 characters (32 bytes).
// Consume a longer credential-shaped suffix too, rather than exposing its tail.
var tokenPattern = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]+|tr_(?:client|session|mcp)_[A-Za-z0-9_-]+|tr_[A-Za-z0-9_-]{43,}|gh[opusrex]_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]{8,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+)`)

// Match home prefixes independently of the running OS, including backslashes
// escaped in JSON. Leave the remainder of the path useful for diagnostics.
var homePathPattern = regexp.MustCompile(`(?i)(^|[\s"'=(\[\-])(?:[a-z]:[\\/]+(?:users|documents and settings)[\\/]+[^\\/\r\n"<>]+|[\\/]+(?:home|Users)[\\/]+[^/\\\s"'<>]+)`)

const truncationMarker = "\n…（已截断）"

func safeRaw(raw []byte, secret string) (string, bool) {
	return redactCapped(string(raw), secret)
}

// Redact masks credentials and home paths for logs and diagnostics. It is not an
// absolute privacy guarantee: check the result before sharing it.
func Redact(text string, secrets ...string) string {
	text, _ = redactCapped(text, secrets...)
	return text
}

// redactCapped applies the masking and reports whether the 4096-rune cap cut
// the text.
func redactCapped(text string, secrets ...string) (string, bool) {
	text = strings.ToValidUTF8(text, "�")
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
		var clean func(any) any
		clean = func(v any) any {
			switch x := v.(type) {
			case map[string]any:
				for k, val := range x {
					lower := strings.ToLower(k)
					if strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") || lower == "prompt" || lower == "messages" || lower == "input" || lower == "thinking" || lower == "reasoning" {
						x[k] = "[REDACTED]"
					} else {
						x[k] = clean(val)
					}
				}
			case []any:
				for i := range x {
					x[i] = clean(x[i])
				}
			case string:
				for _, secret := range secrets {
					if secret != "" {
						x = strings.ReplaceAll(x, secret, "[REDACTED]")
					}
				}
				return x
			}
			return v
		}
		if b, err := json.MarshalIndent(clean(value), "", "  "); err == nil {
			text = string(b)
		}
	}
	text = credentialPattern.ReplaceAllString(text, "[REDACTED]")
	text = tokenPattern.ReplaceAllString(text, "[REDACTED]")
	text = homePathPattern.ReplaceAllString(text, "${1}[HOME]")
	chars := []rune(text)
	truncated := len(chars) > 4096
	if truncated {
		text = string(chars[:4096]) + truncationMarker
	}
	return text, truncated
}
func upstreamDiagnostic(status int, raw []byte, key string) *diagnostic {
	d := &diagnostic{Source: "upstream", Code: "upstream_http", UpstreamStatus: status, Message: "上游服务返回错误。", Suggestion: "查看原始错误并核对上游服务状态。"}
	switch {
	case status >= 300 && status < 400:
		d.Code = "redirect"
		d.Message = "上游返回重定向，网关没有跟随。"
		d.Suggestion = "请填写最终 API 地址，避免凭证被转发到其他地址。"
	case status == 401:
		d.Message = "上游拒绝了 API 密钥。"
		d.Suggestion = "检查该提供商的密钥是否正确、过期或已撤销。"
	case status == 403:
		d.Message = "上游拒绝访问。"
		d.Suggestion = "核对账户权限、模型授权及上游的网络访问限制。"
	case status == 404:
		d.Message = "上游接口或模型不存在。"
		d.Suggestion = "检查 Base URL、协议、模型 ID；该上游可能不支持模型列表接口，可手动添加。"
	case status == 429:
		d.Message = "上游限流或额度不足。"
		d.Suggestion = "查看原始错误区分限流与余额问题；稍后重试或检查额度。"
	case status == 400 || status == 422:
		d.Message = "上游不接受本次请求参数。"
		d.Suggestion = "检查模型与协议；固定测试请求不覆盖所有模型的特殊参数要求。"
	case status >= 500:
		d.Message = "上游服务暂时异常。"
		d.Suggestion = "稍后重试或检查提供商状态；不代表客户端密钥错误。"
	}
	d.Raw, d.Truncated = safeRaw(raw, key)
	return d
}
