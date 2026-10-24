package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncryptedPersistenceAndRetention(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := Channel{Name: "local", Protocol: "openai", BaseURL: "http://localhost:9999", APIKey: "must-not-be-plaintext-upstream-secret", Models: []string{"test"}, Enabled: true}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	settings, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	key := settings.GatewayKey
	u := Usage{Time: time.Now().AddDate(0, 0, -31), Model: "test", Status: 200}
	if err = s.AddUsage(u); err != nil {
		t.Fatal(err)
	}
	if err = s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	logs, err := s.ListUsage(UsageFilter{})
	if err != nil || len(logs) != 0 {
		t.Fatal("retention failed", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), c.APIKey) || strings.Contains(string(data), key) {
		t.Fatal("plaintext secret in SQLite")
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 || rows[0].APIKey != c.APIKey {
		t.Fatal("encrypted roundtrip failed", err)
	}
	settings, err = s.GetSettings()
	if err != nil || settings.GatewayKey != key {
		t.Fatal("settings key changed", err)
	}
}

// The initial failure cooldown has a resolved default:
// unset (zero) means DefaultFailureCooldownSeconds, explicit values round-trip,
// out-of-range values are refused, and a row written before the field existed
// reads back as the default.
func TestFailureCooldownRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := func(seconds int) Settings {
		return Settings{GatewayKey: "gateway-key-1234", Prices: []Price{}, RetentionDays: 30, MaxAttempts: 3, RetriesPerChannel: 2, RequestDefaultsEnabled: true, RequestDefaults: ModelDefaults{MaxOutputTokens: ptr(int64(8192))}, FailureCooldownSeconds: seconds}
	}
	if v, err := s.GetSettings(); err != nil || v.FailureCooldownSeconds != DefaultFailureCooldownSeconds {
		t.Fatalf("a fresh store carries %d (%v), want the default %d", v.FailureCooldownSeconds, err, DefaultFailureCooldownSeconds)
	}
	if err = s.SaveSettings(base(0)); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSettings(); err != nil || v.FailureCooldownSeconds != DefaultFailureCooldownSeconds {
		t.Fatalf("zero reads back as %d (%v), want the default %d", v.FailureCooldownSeconds, err, DefaultFailureCooldownSeconds)
	}
	for _, want := range []int{1, 30, 300} {
		if err = s.SaveSettings(base(want)); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetSettings(); err != nil || v.FailureCooldownSeconds != want {
			t.Fatalf("saved %d, read back %d (%v)", want, v.FailureCooldownSeconds, err)
		}
	}
	for _, bad := range []int{-1, 301, 600} {
		if err = s.SaveSettings(base(bad)); err == nil {
			t.Fatalf("accepted %d", bad)
		}
	}
	// A row from before the field existed has no such JSON key; reading it must
	// resolve the default rather than hand back a zero cooldown.
	sealed, err := s.seal("gateway-key-1234")
	if err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf(`{"gateway_key":%q,"prices":[],"retention_days":30,"max_attempts":3,"retries_per_channel":5,"max_tokens_default":8192}`, sealed)
	if _, err = s.db.Exec("INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", legacy); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSettings(); err != nil || v.FailureCooldownSeconds != DefaultFailureCooldownSeconds {
		t.Fatalf("a legacy row reads back as %d (%v), want the default %d", v.FailureCooldownSeconds, err, DefaultFailureCooldownSeconds)
	}
}

// Same-channel retries carry the same back-compat default: the console validates
// 1..10, so a row written before the field existed (or one saved with a zero)
// must read back as DefaultRetriesPerChannel instead of a 0 the settings form
// then refuses to save.
func TestRetriesPerChannelDefaultOnRead(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := func(retries int) Settings {
		return Settings{GatewayKey: "gateway-key-1234", Prices: []Price{}, RetentionDays: 30, MaxAttempts: 3, RequestDefaultsEnabled: true, RequestDefaults: ModelDefaults{MaxOutputTokens: ptr(int64(8192))}, RetriesPerChannel: retries, FailureCooldownSeconds: DefaultFailureCooldownSeconds}
	}
	if v, err := s.GetSettings(); err != nil || v.RetriesPerChannel != DefaultRetriesPerChannel {
		t.Fatalf("a fresh store carries %d (%v), want the default %d", v.RetriesPerChannel, err, DefaultRetriesPerChannel)
	}
	if err = s.SaveSettings(base(0)); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSettings(); err != nil || v.RetriesPerChannel != DefaultRetriesPerChannel {
		t.Fatalf("zero reads back as %d (%v), want the default %d", v.RetriesPerChannel, err, DefaultRetriesPerChannel)
	}
	for _, want := range []int{1, 5, 10} {
		if err = s.SaveSettings(base(want)); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetSettings(); err != nil || v.RetriesPerChannel != want {
			t.Fatalf("saved %d, read back %d (%v)", want, v.RetriesPerChannel, err)
		}
	}
	for _, bad := range []int{-1, 11} {
		if err = s.SaveSettings(base(bad)); err == nil {
			t.Fatalf("accepted %d", bad)
		}
	}
	// A row from before the field existed has no such JSON key; reading it must
	// resolve the default rather than hand the console a zero its validation
	// refuses.
	sealed, err := s.seal("gateway-key-1234")
	if err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf(`{"gateway_key":%q,"prices":[],"retention_days":30,"max_attempts":3,"failure_cooldown_seconds":30,"max_tokens_default":8192}`, sealed)
	if _, err = s.db.Exec("INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", legacy); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSettings(); err != nil || v.RetriesPerChannel != DefaultRetriesPerChannel {
		t.Fatalf("a legacy row reads back as %d (%v), want the default %d", v.RetriesPerChannel, err, DefaultRetriesPerChannel)
	}
}

// The OAuth auto-open switch defaults to on, and the pointer is what carries
// that default: nil (a row written before the switch existed, or a save that
// omits it) resolves to DefaultOAuthAutoOpen on read and on save, while a plain
// bool's zero value would read an old row back as off.
func TestOAuthAutoOpenDefaultsOn(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := func(open *bool) Settings {
		return Settings{GatewayKey: "gateway-key-1234", Prices: []Price{}, RetentionDays: 30, MaxAttempts: 3, RequestDefaultsEnabled: true, RequestDefaults: ModelDefaults{MaxOutputTokens: ptr(int64(8192))}, RetriesPerChannel: DefaultRetriesPerChannel, FailureCooldownSeconds: DefaultFailureCooldownSeconds, OAuthAutoOpen: open}
	}
	on := func() *bool { enabled := true; return &enabled }
	off := func() *bool { disabled := false; return &disabled }
	if v, err := s.GetSettings(); err != nil || v.OAuthAutoOpen == nil || *v.OAuthAutoOpen != DefaultOAuthAutoOpen {
		t.Fatalf("a fresh store reads back %v (%v), want the default %t", v.OAuthAutoOpen, err, DefaultOAuthAutoOpen)
	}
	if err = s.SaveSettings(base(nil)); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSettings(); err != nil || v.OAuthAutoOpen == nil || *v.OAuthAutoOpen != DefaultOAuthAutoOpen {
		t.Fatalf("nil round-trips as %v (%v), want the default %t", v.OAuthAutoOpen, err, DefaultOAuthAutoOpen)
	}
	if err = s.SaveSettings(base(off())); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSettings(); err != nil || v.OAuthAutoOpen == nil || *v.OAuthAutoOpen {
		t.Fatalf("off reads back as %v (%v), want off", v.OAuthAutoOpen, err)
	}
	if err = s.SaveSettings(base(on())); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSettings(); err != nil || v.OAuthAutoOpen == nil || !*v.OAuthAutoOpen {
		t.Fatalf("on reads back as %v (%v), want on", v.OAuthAutoOpen, err)
	}
	// A row from before the switch existed has no such key; reading it must
	// resolve the default rather than hand back a false "off".
	sealed, err := s.seal("gateway-key-1234")
	if err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf(`{"gateway_key":%q,"prices":[],"retention_days":30,"max_attempts":3,"retries_per_channel":5,"failure_cooldown_seconds":30,"max_tokens_default":8192}`, sealed)
	if _, err = s.db.Exec("INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", legacy); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSettings(); err != nil || v.OAuthAutoOpen == nil || *v.OAuthAutoOpen != DefaultOAuthAutoOpen {
		t.Fatalf("a legacy row reads back as %v (%v), want the default %t", v.OAuthAutoOpen, err, DefaultOAuthAutoOpen)
	}
}
func TestValidation(t *testing.T) {
	c := Channel{Name: "test", Protocol: "openai", BaseURL: "https://user:pass@example.test", APIKey: "test", Models: []string{"test"}}
	if ValidateChannel(c) == nil {
		t.Fatal("accepted URL credentials")
	}
	c.BaseURL = "https://example.test?key=x"
	if ValidateChannel(c) == nil {
		t.Fatal("accepted URL query")
	}
	c.BaseURL = "https://example.test/v1"
	if err := ValidateChannel(c); err != nil {
		t.Fatal(err)
	}
}

// The channel base URL is the default the endpoints inherit when they declare
// no address of their own. The fallback is resolved once, on read and on save,
// and stored expanded: everything downstream (the gateway, the OAuth host
// allowlist, the console tools) sees the address an endpoint is reached at and
// never has to know the rule exists.
func TestChannelBaseURLIsTheEndpointDefault(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Channel{Name: "shared-base", APIKey: "k", Enabled: true,
		BaseURL: "https://shared.example.com/v1",
		Endpoints: []Endpoint{
			{Format: FormatOpenAIChatCompletions},
			{Format: FormatAnthropicMessages, BaseURL: "https://other.example.com"},
		}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	// The default is stored expanded, so no consumer resolves a fallback.
	if got := rows[0].Endpoints[0].BaseURL; got != "https://shared.example.com/v1" {
		t.Fatalf("inherited endpoint address = %q, want the channel default", got)
	}
	// An endpoint with its own address keeps it.
	if got := rows[0].Endpoints[1].BaseURL; got != "https://other.example.com" {
		t.Fatalf("own endpoint address = %q, want it preserved", got)
	}
	// The fallback rule itself: own address wins, otherwise the channel default,
	// and empty on both sides is empty rather than a guess.
	byFormat := map[string]Endpoint{}
	for _, e := range rows[0].Endpoints {
		byFormat[e.Format] = e
	}
	if got := rows[0].EndpointBaseURL(byFormat[FormatAnthropicMessages]); got != "https://other.example.com" {
		t.Fatalf("EndpointBaseURL with own address = %q", got)
	}
	if got := rows[0].EndpointBaseURL(Endpoint{Format: FormatGeminiGenerateContent}); got != "https://shared.example.com/v1" {
		t.Fatalf("EndpointBaseURL without own address = %q, want the channel default", got)
	}
	// Empty on both sides is empty rather than a guess.
	noDefault := rows[0]
	noDefault.BaseURL = ""
	if got := noDefault.EndpointBaseURL(Endpoint{Format: FormatGeminiGenerateContent}); got != "" {
		t.Fatalf("EndpointBaseURL with neither = %q, want empty", got)
	}
	// A draft that only names formats and one channel default validates, and its
	// endpoints resolve to the default: that is the shape the console's checkbox
	// editor sends.
	draft := Channel{Name: "draft", APIKey: "k", BaseURL: "https://shared.example.com",
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions}, {Format: FormatGeminiGenerateContent}}}
	if err := ValidateChannel(draft); err != nil {
		t.Fatalf("a channel default that covers its endpoints: %v", err)
	}
	// An unusable default is refused rather than inherited by an endpoint that
	// would then be unreachable with no visible cause.
	draft.BaseURL = "not-a-url"
	if err := ValidateChannel(draft); err == nil {
		t.Fatal("accepted an unusable channel base_url")
	}
	draft.BaseURL = "https://shared.example.com?token=x"
	if err := ValidateChannel(draft); err == nil {
		t.Fatal("accepted a channel base_url carrying a query")
	}
	// An endpoint with neither its own address nor a default has nowhere to go.
	if err := ValidateChannel(Channel{Name: "nowhere", APIKey: "k", Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions}}}); err == nil {
		t.Fatal("accepted an endpoint with no address anywhere")
	}
}

// The OAuth host allowlist is checked against the address each endpoint is
// reached at, so a channel-level default on the allowlist covers the endpoints
// that inherit it and one off it refuses them.
func TestOAuthHostAllowlistAppliesToExpandedEndpoint(t *testing.T) {
	copilot := func(base string, endpoints ...Endpoint) Channel {
		return Channel{Name: "copilot", Enabled: true, CredentialType: CredentialCopilotOAuth,
			BaseURL:   base,
			Endpoints: endpoints,
			OAuth:     &OAuthCredential{Provider: OAuthProviderCopilot, GitHubToken: "gho_x", AccountLabel: "octocat", Status: OAuthStatusActive}}
	}
	// Both documented facades, addresses inherited from the allowlisted default.
	if err := ValidateChannel(copilot("https://api.githubcopilot.com",
		Endpoint{Format: FormatOpenAIChatCompletions}, Endpoint{Format: FormatAnthropicMessages})); err != nil {
		t.Fatalf("allowlisted default refused: %v", err)
	}
	// A default off the allowlist is refused for the endpoint inheriting it.
	err := ValidateChannel(copilot("https://evil.example.com", Endpoint{Format: FormatOpenAIChatCompletions}))
	if err == nil || !strings.Contains(err.Error(), "host allowlist") {
		t.Fatalf("default off the allowlist = %v, want a host allowlist refusal", err)
	}
	// One endpoint's own address off the allowlist is refused next to a default
	// that is on it.
	err = ValidateChannel(copilot("https://api.githubcopilot.com",
		Endpoint{Format: FormatOpenAIChatCompletions}, Endpoint{Format: FormatAnthropicMessages, BaseURL: "https://evil.example.com"}))
	if err == nil || !strings.Contains(err.Error(), "host allowlist") {
		t.Fatalf("own address off the allowlist = %v, want a host allowlist refusal", err)
	}
}

// The preferred endpoint is a routing hint, not state: empty means the fixed
// default order, and a row written before the field existed reads back empty.
func TestPreferredEndpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Channel{Name: "p", APIKey: "k", Models: []string{"m"}, Enabled: true,
		Endpoints:         []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "http://localhost:9999"}, {Format: FormatAnthropicMessages, BaseURL: "http://localhost:9998"}},
		PreferredEndpoint: FormatAnthropicMessages}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 {
		t.Fatal("channel round trip failed", err)
	}
	if rows[0].PreferredEndpoint != FormatAnthropicMessages {
		t.Fatalf("preferred endpoint lost: %q", rows[0].PreferredEndpoint)
	}
	c.PreferredEndpoint = ""
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if rows, err = s.ListChannels(); err != nil || rows[0].PreferredEndpoint != "" {
		t.Fatalf("clearing the preference failed: %q %v", rows[0].PreferredEndpoint, err)
	}
	c.PreferredEndpoint = FormatGeminiGenerateContent
	if err := ValidateChannel(c); err == nil {
		t.Fatal("accepted a preference the channel declares no endpoint for")
	}
	c.Endpoints = append(c.Endpoints, Endpoint{Format: FormatOpenAIResponses, BaseURL: "http://localhost:9997"})
	c.PreferredEndpoint = FormatOpenAIResponses
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal("Responses preference should be valid for a declared endpoint", err)
	}
	if rows, err = s.ListChannels(); err != nil || rows[0].PreferredEndpoint != FormatOpenAIResponses {
		t.Fatalf("Responses preference did not persist: %v", err)
	}
}

func TestUsageAttemptsPersistenceAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	attempts := []AttemptRecord{
		{
			AttemptIndex:      1,
			ChannelID:         101,
			ChannelName:       "primary-chan",
			CredentialID:      "cred-1",
			CredentialMaskKey: "sk-••••abcd",
			UpstreamModel:     "gpt-4o",
			StatusCode:        429,
			DurationMS:        120,
			ErrorReason:       "upstream returned HTTP 429",
			ActionTaken:       ActionTakenCoolingSwitched,
		},
		{
			AttemptIndex:      2,
			ChannelID:         101,
			ChannelName:       "primary-chan",
			CredentialID:      "cred-2",
			CredentialMaskKey: "sk-••••ef01",
			UpstreamModel:     "gpt-4o",
			StatusCode:        502,
			DurationMS:        300,
			ErrorReason:       "upstream connection failed",
			ActionTaken:       ActionTakenExhaustedFallback,
		},
		{
			AttemptIndex:      3,
			ChannelID:         102,
			ChannelName:       "backup-chan",
			CredentialID:      "cred-3",
			CredentialMaskKey: "sk-••••9999",
			UpstreamModel:     "claude-3-5-sonnet",
			StatusCode:        200,
			DurationMS:        450,
			ErrorReason:       "",
			ActionTaken:       ActionTakenSuccess,
		},
	}

	u := Usage{
		Time:        time.Now(),
		ChannelID:   102,
		ChannelName: "backup-chan",
		Model:       "claude-3-5-sonnet",
		Status:      200,
		Attempts:    attempts,
	}

	if err := s.AddUsage(u); err != nil {
		t.Fatalf("AddUsage failed: %v", err)
	}

	rows, err := s.ListUsage(UsageFilter{})
	if err != nil {
		t.Fatalf("ListUsage failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	got := rows[0].Attempts
	if len(got) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(got))
	}

	for i, want := range attempts {
		g := got[i]
		if g.AttemptIndex != want.AttemptIndex ||
			g.ChannelID != want.ChannelID ||
			g.ChannelName != want.ChannelName ||
			g.CredentialID != want.CredentialID ||
			g.CredentialMaskKey != want.CredentialMaskKey ||
			g.UpstreamModel != want.UpstreamModel ||
			g.StatusCode != want.StatusCode ||
			g.DurationMS != want.DurationMS ||
			g.ErrorReason != want.ErrorReason ||
			g.ActionTaken != want.ActionTaken {
			t.Fatalf("attempt[%d] mismatch: got %+v, want %+v", i, g, want)
		}
	}

	// Backwards compatibility: empty/nil attempts
	legacy := Usage{
		Time:      time.Now(),
		ChannelID: 101,
		Model:     "gpt-4",
		Status:    200,
	}
	if err := s.AddUsage(legacy); err != nil {
		t.Fatalf("AddUsage legacy failed: %v", err)
	}
	rows, err = s.ListUsage(UsageFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d (err: %v)", len(rows), err)
	}
	if len(rows[0].Attempts) != 0 {
		t.Fatalf("expected empty attempts for legacy row, got %+v", rows[0].Attempts)
	}
}

func TestMaskKeyAndCredentialItemMasked(t *testing.T) {
	if got := MaskKey("short"); got != "••••••••" {
		t.Fatalf("MaskKey short = %q, want ••••••••", got)
	}
	if got := MaskKey("sk-1234567890abcdef"); got != "sk-••••cdef" {
		t.Fatalf("MaskKey long = %q, want sk-••••cdef", got)
	}

	ciKey := CredentialItem{APIKey: "sk-abcdefghijklmnop"}
	if got := ciKey.Masked(); got != "sk-••••mnop" {
		t.Fatalf("ciKey.Masked() = %q, want sk-••••mnop", got)
	}

	ciMasked := CredentialItem{MaskedKey: "custom-masked"}
	if got := ciMasked.Masked(); got != "custom-masked" {
		t.Fatalf("ciMasked.Masked() = %q, want custom-masked", got)
	}

	ciOAuth := CredentialItem{OAuth: &OAuthCredential{Account: "dev@company.com"}}
	if got := ciOAuth.Masked(); got != "dev@company.com" {
		t.Fatalf("ciOAuth.Masked() = %q, want dev@company.com", got)
	}

	ciEmpty := CredentialItem{}
	if got := ciEmpty.Masked(); got != "" {
		t.Fatalf("ciEmpty.Masked() = %q, want empty", got)
	}
}
