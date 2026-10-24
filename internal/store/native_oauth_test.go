package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func nativeTestChannel(credentialType, account string) Channel {
	oauth := &OAuthCredential{Account: account, AccessToken: "synthetic-access-" + account, RefreshToken: "synthetic-refresh-" + account,
		ClientID: "synthetic-client", DeviceID: "synthetic-device", OrganizationID: "synthetic-organization", ProjectID: "synthetic-project", ExpiresAt: time.Now().Add(time.Hour)}
	format, base := FormatAnthropicMessages, "https://api.anthropic.com"
	switch credentialType {
	case CredentialClaudeOAuth:
		oauth.Provider = OAuthProviderClaude
	case CredentialAntigravityOAuth:
		oauth.Provider, format, base = OAuthProviderAntigravity, FormatGeminiGenerateContent, "https://daily-cloudcode-pa.googleapis.com"
	case CredentialDevinOAuth:
		oauth.Provider, format, base = OAuthProviderDevin, FormatOpenAIResponses, "https://server.codeium.com"
		oauth.RefreshToken, oauth.ExpiresAt = "", time.Time{}
	}
	return Channel{Name: "Synthetic native", CredentialType: credentialType, Enabled: true,
		Endpoints: []Endpoint{{Format: format, BaseURL: base}}, Credentials: []CredentialItem{{ID: "one", Enabled: true, OAuth: oauth}}}
}

func TestNativeOAuthPrivateFieldsAreSealedAndConfigProtected(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	channel := nativeTestChannel(CredentialClaudeOAuth, "synthetic-account")
	if err = st.SaveChannel(&channel); err != nil {
		t.Fatal(err)
	}
	channel, err = st.Channel(channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = st.db.QueryRow("SELECT data FROM channels WHERE id=?", channel.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	private := []string{channel.OAuth.OrganizationID, channel.OAuth.DeviceID, channel.OAuth.ProjectID, channel.OAuth.AccessToken, channel.OAuth.RefreshToken}
	public, _ := json.Marshal(channel.OAuth.Public())
	for _, secret := range private {
		if strings.Contains(raw, secret) || strings.Contains(string(public), secret) {
			t.Fatal("private credential field leaked to storage or public projection")
		}
	}
	stale, err := st.Channel(channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := *channel.OAuth
	next.AccessToken, next.RefreshToken = "synthetic-rotated-access", "synthetic-rotated-refresh"
	if _, err = st.ReplaceNativeOAuthCredential(channel.ID, "one", NativeOAuthIdentity(channel.CredentialType, channel.OAuth), next, false); err != nil {
		t.Fatal(err)
	}
	stale.Name, stale.OAuth.OrganizationID, stale.OAuth.DeviceID, stale.OAuth.ProjectID = "Edited name", "stale-org", "stale-device", "stale-project"
	if err = st.SaveChannelConfig(&stale); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := st.Channel(channel.ID)
	if err != nil || !reflect.DeepEqual(*fresh.OAuth, nextWithTimestamp(next, fresh.OAuth.UpdatedAt)) || fresh.Name != "Edited name" {
		t.Fatalf("private material/configuration round trip failed: %v", err)
	}
}

func nextWithTimestamp(oauth OAuthCredential, at time.Time) OAuthCredential {
	oauth.UpdatedAt = at
	return oauth
}

func TestNativeOAuthExactAtomicUpdateAndGeneration(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	channel := nativeTestChannel(CredentialClaudeOAuth, "account-one")
	second := nativeTestChannel(CredentialClaudeOAuth, "account-two").Credentials[0]
	second.ID = "two"
	channel.Credentials = append(channel.Credentials, second)
	if err = st.SaveChannel(&channel); err != nil {
		t.Fatal(err)
	}
	channel, err = st.Channel(channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	second = channel.Credentials[1]
	first := *channel.Credentials[0].OAuth
	identity := NativeOAuthIdentity(channel.CredentialType, second.OAuth)
	next := *second.OAuth
	next.AccessToken, next.RefreshToken = "synthetic-new-access", "synthetic-new-refresh"
	updated, err := st.ReplaceNativeOAuthCredential(channel.ID, "two", identity, next, false)
	if err != nil || updated.Generation != channel.Generation || !reflect.DeepEqual(*updated.Credentials[0].OAuth, first) || updated.Credentials[1].OAuth.AccessToken != next.AccessToken {
		t.Fatalf("rotation modified identity or wrong pool member: %v", err)
	}
	if NativeOAuthIdentity(updated.CredentialType, updated.Credentials[1].OAuth) != identity || CatalogueSourceVersion(channel) != CatalogueSourceVersion(updated) {
		t.Fatal("normal token rotation changed stable identity/version")
	}
	next.DeviceID = "synthetic-new-device"
	if _, err := st.ReplaceNativeOAuthCredential(channel.ID, "two", identity, next, false); !errors.Is(err, ErrNativeOAuthIdentity) {
		t.Fatalf("refresh changed stable private identity: %v", err)
	}
	updated, err = st.ReplaceNativeOAuthCredential(channel.ID, "two", identity, next, true)
	if err != nil || updated.Generation != channel.Generation+1 || updated.Credentials[1].OAuth.DeviceID != next.DeviceID || !reflect.DeepEqual(*updated.Credentials[0].OAuth, first) {
		t.Fatalf("reauthorization did not update exact domain/generation: %v", err)
	}
	if _, err := st.ReplaceNativeOAuthCredential(channel.ID, "two", identity, next, true); !errors.Is(err, ErrNativeOAuthIdentity) {
		t.Fatalf("stale identity accepted: %v", err)
	}
	wrong := next
	wrong.Account = "other-account"
	if _, err := st.ReplaceNativeOAuthCredential(channel.ID, "two", NativeOAuthIdentity(updated.CredentialType, updated.Credentials[1].OAuth), wrong, true); !errors.Is(err, ErrNativeOAuthIdentity) {
		t.Fatalf("reauthorization replaced another account: %v", err)
	}
}

func TestNativeOAuthValidationAndKnownHostProfiles(t *testing.T) {
	for _, credentialType := range []string{CredentialClaudeOAuth, CredentialAntigravityOAuth, CredentialDevinOAuth} {
		channel := nativeTestChannel(credentialType, "synthetic-account")
		if err := ValidateChannel(channel); err != nil {
			t.Fatalf("valid profile %s: %v", credentialType, err)
		}
		wrongHost := channel
		wrongHost.Endpoints = []Endpoint{{Format: channel.Endpoints[0].Format, BaseURL: "https://unknown.example.invalid"}}
		if ValidateChannel(wrongHost) == nil {
			t.Fatalf("unknown host accepted: %s", credentialType)
		}
		wrongFormat := channel
		wrongFormat.Endpoints = []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: channel.Endpoints[0].BaseURL}}
		if ValidateChannel(wrongFormat) == nil {
			t.Fatalf("unknown protocol accepted: %s", credentialType)
		}
	}
	for _, mutate := range []func(*OAuthCredential){func(o *OAuthCredential) { o.DeviceID = "" }, func(o *OAuthCredential) { o.Account, o.Subject = "", "" }, func(o *OAuthCredential) { o.RefreshToken = "" }} {
		c := nativeTestChannel(CredentialClaudeOAuth, "synthetic-account")
		mutate(c.Credentials[0].OAuth)
		if ValidateChannel(c) == nil {
			t.Fatal("incomplete Claude material accepted")
		}
	}
	ag := nativeTestChannel(CredentialAntigravityOAuth, "synthetic-account")
	ag.Credentials[0].OAuth.ProjectID = ""
	if ValidateChannel(ag) == nil {
		t.Fatal("Antigravity without project accepted")
	}
	devin := nativeTestChannel(CredentialDevinOAuth, "")
	first := NativeOAuthAccountIdentity(devin.CredentialType, *devin.Credentials[0].OAuth)
	devin.Credentials[0].OAuth.AccountLabel = "same display name"
	if NativeOAuthAccountIdentity(devin.CredentialType, *devin.Credentials[0].OAuth) != first {
		t.Fatal("display name treated as identity")
	}
	devin.Credentials[0].OAuth.AccessToken = "another-session"
	if NativeOAuthAccountIdentity(devin.CredentialType, *devin.Credentials[0].OAuth) == first {
		t.Fatal("different anonymous Devin sessions merged")
	}
	zen := Channel{Name: "Zen Free", CredentialType: CredentialZenFree, Enabled: true, APIKey: "public", ModelFormats: map[string][]string{}, Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://opencode.ai/inference/v1"}}}
	if err := ValidateChannel(zen); err != nil {
		t.Fatal(err)
	}
	zen.APIKey = "personal-key"
	if ValidateChannel(zen) == nil {
		t.Fatal("Zen Free accepted personal credentials")
	}
}

func TestNativeOAuthUpdateRetriesAndReturnsWriteFailure(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := nativeTestChannel(CredentialClaudeOAuth, "synthetic-account")
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	write := st.persistRow
	writes := 0
	st.persistRow = func(c *Channel) error {
		writes++
		if writes == 1 {
			return errors.New("synthetic transient failure")
		}
		return write(c)
	}
	next := *c.OAuth
	next.AccessToken = "synthetic-new-access"
	if _, err := st.ReplaceNativeOAuthCredential(c.ID, "one", NativeOAuthIdentity(c.CredentialType, c.OAuth), next, false); err != nil || writes != 2 {
		t.Fatalf("rotation write was not retried: %v", err)
	}
	failure := errors.New("synthetic permanent failure")
	st.persistRow = func(*Channel) error { return failure }
	next.AccessToken = "synthetic-discarded-access"
	if _, err := st.ReplaceNativeOAuthCredential(c.ID, "one", NativeOAuthIdentity(c.CredentialType, c.OAuth), next, false); !errors.Is(err, failure) {
		t.Fatalf("permanent write error swallowed: %v", err)
	}
	fresh, err := st.Channel(c.ID)
	if err != nil || fresh.OAuth.AccessToken != "synthetic-new-access" {
		t.Fatal("failed write modified stored credential")
	}
}
