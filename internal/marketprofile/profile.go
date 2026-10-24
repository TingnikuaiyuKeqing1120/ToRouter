// Package marketprofile defines the data exchanged with a Magpie/OpenCode
// supplier. It has no runtime, registry or credential-store dependencies.
package marketprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

type Provider struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	API     string       `json:"api"`
	NPM     string       `json:"npm"`
	Methods []AuthMethod `json:"methods"`
	Models  []Model      `json:"models,omitempty"`
}

type Model struct {
	ID     string `json:"id"`
	APIID  string `json:"api_id,omitempty"`
	Name   string `json:"name"`
	API    string `json:"api"`
	NPM    string `json:"npm"`
	Format string `json:"format"`
}

type AuthMethod struct {
	Index   int      `json:"index"`
	Type    string   `json:"type"`
	Label   string   `json:"label"`
	Prompts []Prompt `json:"prompts,omitempty"`
}

type Prompt struct {
	Type        string     `json:"type"`
	Key         string     `json:"key"`
	Label       string     `json:"label"`
	Placeholder string     `json:"placeholder,omitempty"`
	Options     []Option   `json:"options,omitempty"`
	Condition   *Condition `json:"condition,omitempty"`
}
type Option struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Hint  string `json:"hint,omitempty"`
}
type Condition struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

type Authorization struct {
	URL          string          `json:"url,omitempty"`
	Instructions string          `json:"instructions,omitempty"`
	Method       string          `json:"method,omitempty"`
	State        json.RawMessage `json:"state,omitempty"`
}

var providerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
var packageID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func PackageID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:16])
}
func CredentialType(pkg, provider string) string { return "market:" + PackageID(pkg) + ":" + provider }
func ParseCredentialType(t string) (string, string, bool) {
	parts := strings.Split(t, ":")
	if len(parts) != 3 || parts[0] != "market" || !packageID.MatchString(parts[1]) || !providerID.MatchString(parts[2]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}
func ValidProviderID(id string) bool { return providerID.MatchString(id) }

// Full plugin-owned auth is opaque to the gateway. The common type is the only
// semantic requirement; identifiers and rotation fields differ by supplier.
func State(raw string) (string, error) {
	if len(raw) > 1<<20 {
		return "", errors.New("供应商凭证过大")
	}
	var v map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &v) != nil || v == nil {
		return "", errors.New("供应商凭证不是有效JSON对象")
	}
	var kind string
	if json.Unmarshal(v["type"], &kind) != nil || (kind != "api" && kind != "oauth") {
		return "", errors.New("供应商凭证缺少api或oauth类型")
	}
	b, _ := json.Marshal(v)
	return string(b), nil
}
