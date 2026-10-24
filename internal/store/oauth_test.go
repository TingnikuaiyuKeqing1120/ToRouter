package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func copilotOAuthCredential(github, bearer string) *OAuthCredential {
	return &OAuthCredential{
		Provider:     "copilot",
		GitHubToken:  github,
		AccessToken:  bearer,
		ExpiresAt:    time.Now().Add(30 * time.Minute).UTC(),
		AccountLabel: "octocat",
		Status:       OAuthStatusActive,
		APIBase:      "https://api.githubcopilot.com",
	}
}

func copilotChannelAt(base string, cred *OAuthCredential) Channel {
	return Channel{Name: "copilot", CredentialType: CredentialCopilotOAuth, Enabled: true,
		Models:    []string{"gpt-5"},
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: base}, {Format: FormatAnthropicMessages, BaseURL: base}},
		OAuth:     cred}
}

// openRawDB opens a second connection to the same database file, so a test can
// inspect or rewrite a stored row the way a direct database edit would.
func openRawDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOAuthCredentialIsSealedAtRestAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	github := "gho_" + strings.Repeat("a", 26)
	bearer := "eyJ" + strings.Repeat("b", 32) + ".cGF5bG9hZA.c2ln"
	c := copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential(github, bearer))
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), github) || strings.Contains(string(data), bearer) {
		t.Fatal("plaintext oauth token in SQLite")
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 {
		t.Fatal("round trip failed", err)
	}
	if rows[0].CredentialType != CredentialCopilotOAuth {
		t.Fatalf("credential type lost: %q", rows[0].CredentialType)
	}
	if rows[0].OAuth == nil || rows[0].OAuth.GitHubToken != github || rows[0].OAuth.AccessToken != bearer {
		t.Fatal("oauth tokens did not round trip", rows[0].OAuth)
	}
	if !rows[0].OAuth.ExpiresAt.Equal(c.OAuth.ExpiresAt) || rows[0].OAuth.AccountLabel != "octocat" {
		t.Fatal("oauth metadata did not round trip", rows[0].OAuth)
	}
}

// A row written before credential types existed has neither field: it must read
// back as an api_key channel and keep saving without losing anything.
func TestLegacyRowWithoutCredentialTypeReadsAsAPIKey(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Channel{Name: "legacy", APIKey: "legacy-key", Enabled: true,
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.test/v1"}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	db := openRawDB(t, dir)
	var raw string
	if err = db.QueryRow("SELECT data FROM channels WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err = json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatal(err)
	}
	delete(row, "credential_type")
	delete(row, "oauth")
	out, _ := json.Marshal(row)
	if _, err = db.Exec("UPDATE channels SET data=?", string(out)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListChannels()
	if err != nil || len(rows) != 1 {
		t.Fatal("legacy row unreadable", err)
	}
	if rows[0].CredentialType != CredentialAPIKey {
		t.Fatalf("legacy row must default to api_key, got %q", rows[0].CredentialType)
	}
	if rows[0].OAuth != nil || rows[0].APIKey != "legacy-key" {
		t.Fatal("legacy row changed shape", rows[0])
	}
	if err = s.SaveChannel(&rows[0]); err != nil {
		t.Fatal("legacy row cannot be saved back", err)
	}
	again, err := s.Channel(1)
	if err != nil || again.APIKey != "legacy-key" || again.CredentialType != CredentialAPIKey {
		t.Fatal("re-save lost data", err, again)
	}
}

func TestSaveChannelEnforcesOAuthHostAllowlist(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	allowed := []string{
		"https://api.githubcopilot.com",
		"https://api.githubcopilot.com/",
		"https://api.github.com",
		"https://api.githubcopilot.com:443",
		"https://API.GitHubCopilot.com",
		// Individual accounts get their own api segment from
		// copilot_internal/user; refusing it would fail every Individual
		// authorization at channel creation.
		"https://api.individual.githubcopilot.com",
	}
	for _, base := range allowed {
		c := copilotChannelAt(base, copilotOAuthCredential("gho_test", "bearer"))
		if err := s.SaveChannel(&c); err != nil {
			t.Fatalf("whitelisted host %s rejected: %v", base, err)
		}
		if err := s.DeleteChannel(c.ID); err != nil {
			t.Fatal(err)
		}
	}
	refused := []struct{ base, want string }{
		{"http://127.0.0.1:8080", "allowlist"},
		{"http://api.githubcopilot.com", "https"},
		{"https://api.githubcopilot.com.evil.test", "allowlist"},
		{"https://evil.test/api.githubcopilot.com", "allowlist"},
		{"https://github.com", "allowlist"},
		// The shared segment is on the list, the individual one is a different
		// host: a near-miss on it is not a member.
		{"https://api.individual.githubcopilot.co", "allowlist"},
		{"https://api.individual.githubcopilot.com.evil.test", "allowlist"},
	}
	for _, entry := range refused {
		c := copilotChannelAt(entry.base, copilotOAuthCredential("gho_test", "bearer"))
		err := s.SaveChannel(&c)
		if err == nil {
			t.Fatalf("non-allowlisted host %s accepted", entry.base)
		}
		if !strings.Contains(err.Error(), entry.want) {
			t.Fatalf("host %s: error does not say %q: %v", entry.base, entry.want, err)
		}
	}
}

// The CredentialType is the hard rule's handle: a copilot channel may only
// declare the two documented facades, and an unknown type is refused rather
// than silently stored.
func TestSaveChannelConstrainsOAuthChannels(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("gho_test", "bearer"))
	c.Endpoints = append(c.Endpoints, Endpoint{Format: FormatGeminiGenerateContent, BaseURL: "https://api.githubcopilot.com"})
	if err := s.SaveChannel(&c); err == nil || !strings.Contains(err.Error(), "facades") {
		t.Fatalf("gemini endpoint accepted on a copilot channel: %v", err)
	}
	c = copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("gho_test", "bearer"))
	c.Endpoints[1] = Endpoint{Format: FormatOpenAIResponses, BaseURL: "https://api.githubcopilot.com"}
	if err := s.SaveChannel(&c); err == nil {
		t.Fatal("responses endpoint accepted on a copilot channel")
	}
	c = copilotChannelAt("https://api.githubcopilot.com", nil)
	if err := s.SaveChannel(&c); err != nil {
		t.Fatalf("empty copilot channel refused: %v", err)
	}
	c = copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("  ", "bearer"))
	if err := s.SaveChannel(&c); err == nil {
		t.Fatal("blank github token accepted")
	}
	c = copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("gho_test", "bearer"))
	c.CredentialType = "pat_oauth"
	if err := s.SaveChannel(&c); err == nil {
		t.Fatal("unknown credential type accepted")
	}
	// An api_key channel still requires its key.
	c = Channel{Name: "plain", CredentialType: CredentialAPIKey, Enabled: true,
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.test/v1"}}}
	if err := s.SaveChannel(&c); err == nil {
		t.Fatal("api_key channel without a key accepted")
	}
	// A copilot channel needs none: the OAuth credential is its key.
	c = copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("gho_test", "bearer"))
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthStatusAndRotationReplaceAtomically(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	github := "gho_" + strings.Repeat("c", 26)
	c := copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential(github, "first-bearer"))
	c.OAuth.ExpiresAt = time.Now().Add(time.Minute).UTC()
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	generation := c.Generation
	// A rotation replaces the bearer token and keeps everything else, without
	// bumping the continuation generation: the credential identity — the GitHub
	// token, the api base — is what a session pin is made of.
	next := time.Now().Add(30 * time.Minute).UTC()
	if err = s.ReplaceCopilotToken(c.ID, "second-bearer", next); err != nil {
		t.Fatal(err)
	}
	row, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Generation != generation {
		t.Fatalf("rotation bumped the generation to %d", row.Generation)
	}
	if row.OAuth.AccessToken != "second-bearer" || !row.OAuth.ExpiresAt.Equal(next) {
		t.Fatal("rotation did not land", row.OAuth)
	}
	if row.OAuth.GitHubToken != github || row.OAuth.AccountLabel != "octocat" || !row.Enabled {
		t.Fatal("rotation dropped credential fields", row.OAuth)
	}
	// The failure marks ride on the same row.
	if err = s.SetOAuthStatus(c.ID, OAuthStatusUnsubscribed, "no active subscription", true); err != nil {
		t.Fatal(err)
	}
	row, err = s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.OAuth.Status != OAuthStatusUnsubscribed || row.OAuth.StatusReason != "no active subscription" || row.Enabled {
		t.Fatalf("status mark did not land: %+v", row)
	}
	if row.Generation != generation {
		t.Fatalf("status mark bumped the generation to %d", row.Generation)
	}
	// A reauth mark records the reason without touching the enabled flag the
	// unsubscribed mark already cleared: re-enabling is a separate operator
	// decision, not a side effect of a status write.
	if err = s.SetOAuthStatus(c.ID, OAuthStatusReauthRequired, "github token revoked", false); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Channel(c.ID)
	if row.OAuth.Status != OAuthStatusReauthRequired || row.Enabled {
		t.Fatalf("reauth mark did not land: %+v", row)
	}
	if err = s.SetOAuthStatus(9999, OAuthStatusActive, "", false); err != ErrNotFound {
		t.Fatalf("missing channel: %v", err)
	}
	if err = s.ReplaceCopilotToken(9999, "x", next); err != ErrNotFound {
		t.Fatalf("missing channel: %v", err)
	}
}

// A re-authorization — a new GitHub token, or a moved api base — is a new
// continuation domain; a rotated bearer token is not.
func TestOAuthCredentialIdentityDrivesGeneration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("gho_first", "bearer"))
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("new channel starts at generation 1, got %d", c.Generation)
	}
	c.Name = "renamed"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("cosmetic edit bumped the generation to %d", c.Generation)
	}
	c.OAuth.GitHubToken = "gho_second"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 2 {
		t.Fatalf("a re-authorization must bump the generation, got %d", c.Generation)
	}
	c.OAuth.AccessToken = "rotated-bearer"
	c.OAuth.ExpiresAt = time.Now().Add(time.Hour).UTC()
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 2 {
		t.Fatalf("a rotated bearer bumped the generation to %d", c.Generation)
	}
}

// One GitHub account is one channel: a second authorization of the same token
// must resolve to the channel that already owns it, so two channels never share
// a rotation (G017, across channels).
func TestChannelByGitHubToken(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.ChannelByGitHubToken(""); err != ErrNotFound {
		t.Fatalf("empty token: %v", err)
	}
	if _, err = s.ChannelByGitHubToken("gho_absent"); err != ErrNotFound {
		t.Fatalf("absent token: %v", err)
	}
	c := copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("gho_shared", "bearer"))
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	plain := Channel{Name: "plain", APIKey: "sk-plain", Enabled: true,
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.test/v1"}}}
	if err = s.SaveChannel(&plain); err != nil {
		t.Fatal(err)
	}
	found, err := s.ChannelByGitHubToken("gho_shared")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != c.ID {
		t.Fatalf("found channel %d, want %d", found.ID, c.ID)
	}
	// A rotated bearer must not change the answer: the identity is the GitHub
	// token, not the short-lived one.
	if err = s.ReplaceCopilotToken(c.ID, "rotated", time.Now().Add(time.Hour).UTC()); err != nil {
		t.Fatal(err)
	}
	found, err = s.ChannelByGitHubToken("gho_shared")
	if err != nil || found.ID != c.ID {
		t.Fatalf("rotation changed the identity lookup: %v", err)
	}
	// A second channel with a different token is a different identity.
	other := copilotChannelAt("https://api.individual.githubcopilot.com", copilotOAuthCredential("gho_other", "bearer"))
	if err = s.SaveChannel(&other); err != nil {
		t.Fatal(err)
	}
	if found, err = s.ChannelByGitHubToken("gho_other"); err != nil || found.ID != other.ID {
		t.Fatalf("second identity not found: %v", err)
	}
	if _, err = s.ChannelByGitHubToken("gho_missing"); err != ErrNotFound {
		t.Fatalf("unknown token: %v", err)
	}
}

// A rotation whose write fails once is retried and kept; one whose write fails
// twice reports the failure and leaves the stored token exactly as it was —
// the atomic replace never lands half of an update.
func TestRotationRetriesTheWriteAndKeepsTheOldTokenOnFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := copilotChannelAt("https://api.githubcopilot.com", copilotOAuthCredential("gho_shared", "first-bearer"))
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	generation := c.Generation

	// One transient failure: the retry lands the token.
	failures := 0
	real := s.persistRow
	s.persistRow = func(row *Channel) error {
		if failures == 0 {
			failures++
			return errors.New("database is locked")
		}
		return real(row)
	}
	next := time.Now().Add(30 * time.Minute).UTC()
	if err = s.ReplaceCopilotToken(c.ID, "second-bearer", next); err != nil {
		t.Fatal(err)
	}
	s.persistRow = real
	row, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.OAuth.AccessToken != "second-bearer" || !row.OAuth.ExpiresAt.Equal(next) {
		t.Fatalf("retried rotation did not land: %+v", row.OAuth)
	}
	if row.Generation != generation {
		t.Fatalf("rotation moved the generation to %d", row.Generation)
	}

	// Both writes fail: the error travels and the stored token is untouched.
	s.persistRow = func(row *Channel) error { return errors.New("database is locked") }
	err = s.ReplaceCopilotToken(c.ID, "third-bearer", next)
	s.persistRow = real
	if err == nil || !strings.Contains(err.Error(), "old token kept") {
		t.Fatalf("failed rotation did not report: %v", err)
	}
	row, err = s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.OAuth.AccessToken != "second-bearer" || row.Generation != generation {
		t.Fatalf("failed rotation damaged the stored credential: %+v gen=%d", row.OAuth, row.Generation)
	}
}

// ScopeCodexAuthorize mirrors the Codex track's authorization scope set, kept
// here as a literal so the store test does not import the track it validates.
const ScopeCodexAuthorize = "openid profile email offline_access api.connectors.read api.connectors.invoke"

// chatgptCredential is a minimally valid SIWC credential.
func chatgptCredential(client, host, subject string) *OAuthCredential {
	return &OAuthCredential{
		Provider:     OAuthProviderChatGPT,
		RefreshToken: "rt-" + client,
		IDToken:      "id-token",
		ClientID:     client,
		HostID:       host,
		Subject:      subject,
		Scopes:       "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
		AccessToken:  "at-live",
		ExpiresAt:    time.Now().Add(time.Hour).UTC(),
		AccountLabel: "user@example.test",
		APIBase:      "https://api.openai.com/v1",
		Status:       OAuthStatusActive,
	}
}

func chatgptChannel(base string, cred *OAuthCredential) Channel {
	return Channel{Name: "chatgpt", CredentialType: CredentialChatGPTOAuth, Enabled: true,
		Models:    []string{"gpt-5"},
		Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: base}},
		OAuth:     cred}
}

// The SIWC profile is one endpoint on one host: the Responses facade on
// api.openai.com, with the registered client id and this host's id. Everything
// else is a facade the issuer does not serve for this credential.
func TestSaveChannelConstrainsChatGPTChannels(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host, err := s.HostID()
	if err != nil {
		t.Fatal(err)
	}
	good := chatgptChannel("https://api.openai.com/v1", chatgptCredential("oaiapp_local", host, "sub-1"))
	if err := s.SaveChannel(&good); err != nil {
		t.Fatalf("a valid chatgpt channel refused: %v", err)
	}
	// Any other format is refused.
	for _, format := range []string{FormatOpenAIChatCompletions, FormatAnthropicMessages, FormatGeminiGenerateContent} {
		c := chatgptChannel("https://api.openai.com/v1", chatgptCredential("oaiapp_local", host, "sub-1"))
		c.Endpoints[0].Format = format
		if err := s.SaveChannel(&c); err == nil || !strings.Contains(err.Error(), "responses endpoint") {
			t.Fatalf("format %s accepted: %v", format, err)
		}
	}
	// A second endpoint is not part of the profile either.
	c := chatgptChannel("https://api.openai.com/v1", chatgptCredential("oaiapp_local", host, "sub-1"))
	c.Endpoints = append(c.Endpoints, Endpoint{Format: FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"})
	if err := s.SaveChannel(&c); err == nil {
		t.Fatal("duplicate endpoint format accepted")
	}
	// Host allowlist: only api.openai.com (and the issuer) may hold the token.
	for _, base := range []string{"http://127.0.0.1:8080/v1", "https://api.openai.com.evil.test/v1", "https://evil.test/v1"} {
		c := chatgptChannel(base, chatgptCredential("oaiapp_local", host, "sub-1"))
		if err := s.SaveChannel(&c); err == nil || !strings.Contains(err.Error(), "allowlist") {
			t.Fatalf("host %s accepted: %v", base, err)
		}
	}
	c = chatgptChannel("http://api.openai.com/v1", chatgptCredential("oaiapp_local", host, "sub-1"))
	if err := s.SaveChannel(&c); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("cleartext endpoint accepted: %v", err)
	}
	// Credential shape.
	for name, mutate := range map[string]func(*OAuthCredential){
		"no token material": func(o *OAuthCredential) { o.RefreshToken = " "; o.AccessToken = " " },
		"wrong provider":    func(o *OAuthCredential) { o.Provider = OAuthProviderCopilot },
		"no client id":      func(o *OAuthCredential) { o.ClientID = "" },
		"registration id":   func(o *OAuthCredential) { o.ClientID = "dynamic_agent_client" },
		"no host id":        func(o *OAuthCredential) { o.HostID = "" },
		"bad status":        func(o *OAuthCredential) { o.Status = OAuthStatusUnsubscribed },
	} {
		cred := chatgptCredential("oaiapp_local", host, "sub-1")
		mutate(cred)
		c := chatgptChannel("https://api.openai.com/v1", cred)
		if err := s.SaveChannel(&c); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// The SIWC secret set is sealed at rest and round-trips.
func TestChatGPTCredentialIsSealedAtRestAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	host, err := s.HostID()
	if err != nil {
		t.Fatal(err)
	}
	cred := chatgptCredential("oaiapp_sealed", host, "sub-sealed")
	cred.RefreshToken = "rt-super-secret"
	cred.IDToken = "eyJhbGciOiJSUzI1NiJ9.payload.sig"
	cred.AccessToken = "at-super-secret"
	c := chatgptChannel("https://api.openai.com/v1", cred)
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"rt-super-secret", "at-super-secret", "eyJhbGciOiJSUzI1NiJ9.payload.sig"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("plaintext %q in SQLite", secret)
		}
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	row, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.OAuth.RefreshToken != "rt-super-secret" || row.OAuth.IDToken != "eyJhbGciOiJSUzI1NiJ9.payload.sig" || row.OAuth.AccessToken != "at-super-secret" {
		t.Fatalf("secrets did not round trip: %+v", row.OAuth)
	}
	if row.OAuth.ClientID != "oaiapp_sealed" || row.OAuth.HostID != host || row.OAuth.Subject != "sub-sealed" {
		t.Fatalf("identity fields did not round trip: %+v", row.OAuth)
	}
	if row.CredentialType != CredentialChatGPTOAuth {
		t.Fatalf("credential type %q", row.CredentialType)
	}
}

// ext_agent_host_id is generated once and reused: every call returns the same
// value, and the value survives a restart. A host that changed it would look
// like a new device on every boot.
func TestHostIDIsGeneratedOnceAndPersisted(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.HostID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "urn:uuid:") {
		t.Fatalf("host id %q", first)
	}
	if len(first) != len("urn:uuid:")+36 {
		t.Fatalf("host id is not a uuid: %q", first)
	}
	for i := 0; i < 3; i++ {
		again, err := s.HostID()
		if err != nil || again != first {
			t.Fatalf("host id changed: %q vs %q (%v)", again, first, err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.HostID()
	if err != nil || after != first {
		t.Fatalf("host id did not survive a restart: %q vs %q (%v)", after, first, err)
	}
}

// A rotation replaces the access and refresh tokens and leaves the identity —
// client, host, account — alone, so a session pin survives it.
func TestChatGPTRotationKeepsIdentityAndGeneration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host, _ := s.HostID()
	c := chatgptChannel("https://api.openai.com/v1", chatgptCredential("oaiapp_rot", host, "sub-rot"))
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	generation := c.Generation
	next := time.Now().Add(time.Hour).UTC()
	if err = s.ReplaceOAuthTokens(c.ID, OAuthTokens{AccessToken: "at-2", RefreshToken: "rt-2", IDToken: "id-2", ExpiresAt: next}); err != nil {
		t.Fatal(err)
	}
	row, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.OAuth.AccessToken != "at-2" || row.OAuth.RefreshToken != "rt-2" || row.OAuth.IDToken != "id-2" || !row.OAuth.ExpiresAt.Equal(next) {
		t.Fatalf("rotation did not land: %+v", row.OAuth)
	}
	if row.OAuth.ClientID != "oaiapp_rot" || row.OAuth.Subject != "sub-rot" || row.OAuth.HostID != host {
		t.Fatalf("rotation moved the identity: %+v", row.OAuth)
	}
	if row.Generation != generation {
		t.Fatalf("rotation bumped the generation to %d", row.Generation)
	}
	// An empty refresh token keeps the stored one (the Copilot rotation shape).
	if err = s.ReplaceOAuthTokens(c.ID, OAuthTokens{AccessToken: "at-3", ExpiresAt: next}); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Channel(c.ID)
	if row.OAuth.AccessToken != "at-3" || row.OAuth.RefreshToken != "rt-2" {
		t.Fatalf("empty refresh token replaced it: %+v", row.OAuth)
	}
}

// The continuation generation moves when the SIWC identity moves — a different
// registered client, host or account — and stays put for a re-authorization of
// the same account.
func TestChatGPTCredentialIdentityDrivesGeneration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host, _ := s.HostID()
	c := chatgptChannel("https://api.openai.com/v1", chatgptCredential("oaiapp_a", host, "sub-a"))
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("new channel starts at generation 1, got %d", c.Generation)
	}
	c.OAuth.RefreshToken, c.OAuth.AccessToken = "rt-new", "at-new"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("a re-authorization of the same account bumped the generation to %d", c.Generation)
	}
	c.OAuth.ClientID = "oaiapp_b"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 2 {
		t.Fatalf("a different registered client must bump the generation, got %d", c.Generation)
	}
	c.OAuth.Subject = "sub-b"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 3 {
		t.Fatalf("a different account must bump the generation, got %d", c.Generation)
	}
	c.OAuth.HostID = "urn:uuid:00000000-0000-4000-8000-000000000000"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 4 {
		t.Fatalf("a different host must bump the generation, got %d", c.Generation)
	}
}

// An SIWC channel carries no GitHub token, so the Copilot identity lookup never
// answers for it: SIWC's own identity is the (client, host, account) triple.
func TestChatGPTChannelIsNotFoundByGitHubToken(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host, _ := s.HostID()
	c := chatgptChannel("https://api.openai.com/v1", chatgptCredential("oaiapp_x", host, "sub-x"))
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ChannelByGitHubToken(c.OAuth.RefreshToken); err != ErrNotFound {
		t.Fatalf("an SIWC channel answered the GitHub-token lookup: %v", err)
	}
}

// Two concurrent first calls must agree on one host id: the insert is
// DO NOTHING and the value is read back, so the loser adopts the winner's row.
func TestHostIDConcurrentFirstCallAgrees(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const goroutines = 8
	ids := make([]string, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = s.HostID()
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("goroutine %d got %q, want %q", i, ids[i], ids[0])
		}
	}
	if !strings.HasPrefix(ids[0], "urn:uuid:") {
		t.Fatalf("host id %q", ids[0])
	}
	var stored string
	if err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", metaOAuthHostID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != ids[0] {
		t.Fatalf("stored %q, callers agreed on %q", stored, ids[0])
	}
}

// The Codex track: one host, one facade, and the identity a session pin is made
// of.
func TestSaveChannelConstrainsCodexChannels(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cred := func() *OAuthCredential {
		return &OAuthCredential{
			Provider: OAuthProviderCodex, RefreshToken: "rt-x", IDToken: "id-x",
			ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Account: "acct-1", Scopes: ScopeCodexAuthorize,
			AccessToken: "at-x", ExpiresAt: time.Now().Add(time.Hour).UTC(), Status: OAuthStatusActive,
		}
	}
	channel := func(base string) Channel {
		return Channel{Name: "codex", CredentialType: CredentialCodexOAuth, Enabled: true,
			Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: base}}, OAuth: cred()}
	}
	good := channel("https://chatgpt.com/backend-api/codex")
	if err := s.SaveChannel(&good); err != nil {
		t.Fatalf("a valid codex channel refused: %v", err)
	}
	// The authorization surface is the auth issuer: a channel may point there
	// too, because the credential travels to it for the authorize and device
	// endpoints (the two hosts are one decision, not two).
	issuer := channel("https://auth.openai.com/oauth/authorize")
	if err := s.SaveChannel(&issuer); err != nil {
		t.Fatalf("the auth issuer host rejected: %v", err)
	}
	if err := s.DeleteChannel(issuer.ID); err != nil {
		t.Fatal(err)
	}
	// Any other format is refused.
	for _, format := range []string{FormatOpenAIChatCompletions, FormatAnthropicMessages, FormatGeminiGenerateContent} {
		c := channel("https://chatgpt.com/backend-api/codex")
		c.Endpoints[0].Format = format
		if err := s.SaveChannel(&c); err == nil || !strings.Contains(err.Error(), "responses endpoint") {
			t.Fatalf("format %s accepted: %v", format, err)
		}
	}
	// Host allowlist and https.
	for _, base := range []string{"http://127.0.0.1:8080/backend-api/codex", "https://chatgpt.com.evil.test/backend-api/codex", "https://evil.test/v1"} {
		c := channel(base)
		if err := s.SaveChannel(&c); err == nil || !strings.Contains(err.Error(), "allowlist") {
			t.Fatalf("host %s accepted: %v", base, err)
		}
	}
	c := channel("http://chatgpt.com/backend-api/codex")
	if err := s.SaveChannel(&c); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("cleartext endpoint accepted: %v", err)
	}
	// Credential shape.
	for name, mutate := range map[string]func(*OAuthCredential){
		"no token material": func(o *OAuthCredential) { o.RefreshToken = " "; o.AccessToken = " " },
		"wrong provider":    func(o *OAuthCredential) { o.Provider = OAuthProviderChatGPT },
		"no client id":      func(o *OAuthCredential) { o.ClientID = "" },
		"bad status":        func(o *OAuthCredential) { o.Status = OAuthStatusUnsubscribed },
	} {
		c := channel("https://chatgpt.com/backend-api/codex")
		mutate(c.OAuth)
		if err := s.SaveChannel(&c); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// The Codex secrets are sealed at rest, and the identity fields round-trip.
func TestCodexCredentialIsSealedAtRestAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := Channel{Name: "codex", CredentialType: CredentialCodexOAuth, Enabled: true,
		Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}},
		OAuth: &OAuthCredential{
			Provider: OAuthProviderCodex, RefreshToken: "rt-super-secret", IDToken: "eyJhbGciOiJub25lIn0.payload.",
			ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Account: "acct-sealed",
			AccessToken: "at-super-secret", ExpiresAt: time.Now().Add(time.Hour).UTC(), Status: OAuthStatusActive,
		}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"rt-super-secret", "at-super-secret", "eyJhbGciOiJub25lIn0.payload."} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("plaintext %q in SQLite", secret)
		}
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	row, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.OAuth.RefreshToken != "rt-super-secret" || row.OAuth.AccessToken != "at-super-secret" || row.OAuth.IDToken != "eyJhbGciOiJub25lIn0.payload." {
		t.Fatalf("secrets did not round trip: %+v", row.OAuth)
	}
	if row.OAuth.Account != "acct-sealed" || row.OAuth.ClientID != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Fatalf("identity did not round trip: %+v", row.OAuth)
	}
}

// The continuation generation moves when the account or the base moves, and
// stays put for a rotation.
func TestCodexCredentialIdentityDrivesGeneration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := "https://chatgpt.com/backend-api/codex"
	c := Channel{Name: "codex", CredentialType: CredentialCodexOAuth, Enabled: true,
		Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: base}},
		OAuth: &OAuthCredential{
			Provider: OAuthProviderCodex, RefreshToken: "rt-1", ClientID: "app_x", Account: "acct-a",
			AccessToken: "at-1", ExpiresAt: time.Now().Add(time.Hour).UTC(), Status: OAuthStatusActive,
		}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("new channel starts at generation 1, got %d", c.Generation)
	}
	c.OAuth.RefreshToken, c.OAuth.AccessToken = "rt-2", "at-2"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 {
		t.Fatalf("a rotation bumped the generation to %d", c.Generation)
	}
	c.OAuth.Account = "acct-b"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 2 {
		t.Fatalf("a different account must bump the generation, got %d", c.Generation)
	}
	c.Endpoints[0].BaseURL = "https://chatgpt.com/backend-api/codex-alt"
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if c.Generation != 3 {
		t.Fatalf("a moved base must bump the generation, got %d", c.Generation)
	}
}

// A re-authorization writes the new credential and clears the failure marks in
// one statement: the channel becomes usable again, which is the difference
// between it and a rotation.
func TestReplaceChatGPTAuthorizationClearsReauth(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host, _ := s.HostID()
	c := Channel{Name: "chatgpt", CredentialType: CredentialChatGPTOAuth, Enabled: true,
		Endpoints: []Endpoint{{Format: FormatOpenAIResponses, BaseURL: "https://api.openai.com/v1"}},
		OAuth: &OAuthCredential{
			Provider: OAuthProviderChatGPT, RefreshToken: "rt-old", IDToken: "id-old",
			ClientID: "oaiapp_reauth", HostID: host, Subject: "sub-1",
			AccessToken: "at-old", ExpiresAt: time.Now().Add(-time.Minute).UTC(),
			Status: OAuthStatusReauthRequired, StatusReason: "invalid_grant",
		}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	generation := c.Generation
	next := time.Now().Add(time.Hour).UTC()
	if err = s.ReplaceChatGPTAuthorization(c.ID, AuthorizationResult{
		AccessToken: "at-new", RefreshToken: "rt-new", IDToken: "id-new",
		ExpiresAt: next, Scopes: "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
	}); err != nil {
		t.Fatal(err)
	}
	row, err := s.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.OAuth.Status != OAuthStatusActive || row.OAuth.StatusReason != "" {
		t.Fatalf("the reauthorization did not clear the failure marks: %+v", row.OAuth)
	}
	if row.OAuth.AccessToken != "at-new" || row.OAuth.RefreshToken != "rt-new" || row.OAuth.IDToken != "id-new" || !row.OAuth.ExpiresAt.Equal(next) {
		t.Fatalf("tokens not written: %+v", row.OAuth)
	}
	if !strings.Contains(row.OAuth.Scopes, "chatgpt.tokens.use.direct") {
		t.Fatalf("scopes not written: %q", row.OAuth.Scopes)
	}
	// The identity and the pin's generation survive: a re-authorization of the
	// same account is not a new domain.
	if row.OAuth.ClientID != "oaiapp_reauth" || row.OAuth.Subject != "sub-1" || row.Generation != generation {
		t.Fatalf("identity or generation moved: %+v gen=%d", row.OAuth, row.Generation)
	}
	if err = s.ReplaceChatGPTAuthorization(9999, AuthorizationResult{}); err != ErrNotFound {
		t.Fatalf("missing channel: %v", err)
	}
}
