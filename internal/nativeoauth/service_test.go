package nativeoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

type lifecycleDriver struct {
	definition                       Definition
	consent                          store.OAuthCredential
	refresh                          func(context.Context, store.OAuthCredential) (store.OAuthCredential, error)
	exchange                         func()
	refreshes, exchanges, modelCalls atomic.Int32
}

func (d *lifecycleDriver) Definition() Definition { return d.definition }
func (d *lifecycleDriver) Begin(_ context.Context, redirect string) (Authorization, error) {
	return Authorization{URL: "https://example.invalid/authorize", State: "synthetic-state", Verifier: "synthetic-verifier", RedirectURI: redirect}, nil
}
func (d *lifecycleDriver) Exchange(context.Context, Authorization, oauthbase.CallbackResult) (store.OAuthCredential, error) {
	d.exchanges.Add(1)
	if d.exchange != nil {
		d.exchange()
	}
	return d.consent, nil
}

func TestConsentCannotPersistAfterCallerRevocation(t *testing.T) {
	for _, target := range []string{"new", "existing", "reauth", "append"} {
		t.Run(target, func(t *testing.T) {
			st, driver, service := lifecycleFixture(t)
			var channel store.Channel
			if target != "new" {
				channel = lifecycleChannel(t, st, driver)
			}
			driver.consent = *lifecycleOAuth("account-one")
			if target == "append" {
				driver.consent = *lifecycleOAuth("new-pool-account")
			}
			driver.consent.AccessToken = "synthetic-new-consent"
			driver.consent.ExpiresAt = time.Now().Add(time.Hour)
			allowed := true
			driver.exchange = func() { allowed = false }
			ctx := accessguard.With(context.Background(), func() error {
				if !allowed {
					return errors.New("synthetic permission revoked")
				}
				return nil
			})
			var id int64
			identity := ""
			if target == "reauth" {
				id = channel.ID
				identity = store.NativeOAuthIdentity(channel.CredentialType, channel.Credentials[0].OAuth)
			}
			var err error
			if target == "append" {
				_, _, err = service.CompleteIntoPool(ctx, Authorization{State: "fixture"}, oauthbase.CallbackResult{State: "fixture", Code: "fixture-code"}, channel.ID, store.CatalogueSourceVersion(channel))
			} else {
				_, _, err = service.Complete(ctx, Authorization{State: "fixture"}, oauthbase.CallbackResult{State: "fixture", Code: "fixture-code"}, ConsentTarget{ChannelID: id, CredentialID: channel.PrimaryCredential().ID, Identity: identity, Generation: channel.Generation})
			}
			if !accessguard.Denied(err) {
				t.Fatalf("consent persisted after caller revocation: %v", err)
			}
			rows, err := st.ListChannels()
			if err != nil || target == "new" && len(rows) != 0 {
				t.Fatalf("revoked consent created account: %v", err)
			}
			if target != "new" && rows[0].Credentials[0].OAuth.AccessToken == "synthetic-new-consent" {
				t.Fatal("revoked consent replaced existing credential")
			}
			if target == "append" && len(rows[0].Credentials) != len(channel.Credentials) {
				t.Fatal("revoked consent appended a credential")
			}
		})
	}
}

func TestConsentAddsDirectlyToPoolAndNeverDuplicatesAccount(t *testing.T) {
	st, driver, service := lifecycleFixture(t)
	target := lifecycleChannel(t, st, driver)
	auth := Authorization{State: "fixture"}
	callback := oauthbase.CallbackResult{State: "fixture", Code: "fixture-code"}
	driver.consent = *lifecycleOAuth("account-three")
	driver.consent.ExpiresAt = time.Now().Add(time.Hour)
	added, linked, err := service.CompleteIntoPool(context.Background(), auth, callback, target.ID, store.CatalogueSourceVersion(target))
	if err != nil || linked || len(added.Credentials) != 3 {
		t.Fatal("append failed", linked, err)
	}
	rows, _ := st.ListChannels()
	if len(rows) != 1 {
		t.Fatal("created a temporary duplicate account channel")
	}
	if added.Credentials[2].OAuth.Account != "account-three" {
		t.Fatal("wrong appended account")
	}
	driver.consent.AccessToken = "synthetic-new-consent"
	_, linked, err = service.CompleteIntoPool(context.Background(), auth, callback, added.ID, store.CatalogueSourceVersion(added))
	if !errors.Is(err, store.ErrNativeOAuthIdentity) || linked {
		t.Fatal("append silently overwrote an existing member", err)
	}
	driver.consent = *lifecycleOAuth("account-four")
	if _, _, err = service.CompleteIntoPool(context.Background(), auth, callback, target.ID, store.CatalogueSourceVersion(target)); !errors.Is(err, store.ErrEditConflict) {
		t.Fatal("stale pool accepted", err)
	}
	// A second owner does not receive a copy of an identity already in the pool.
	other := store.Channel{Name: "Other native pool", Enabled: true, CredentialType: driver.definition.CredentialType, Endpoints: driver.definition.Endpoints, Credentials: []store.CredentialItem{{ID: "other", Enabled: true, OAuth: lifecycleOAuth("another-account")}}}
	if err = st.SaveChannel(&other); err != nil {
		t.Fatal(err)
	}
	driver.consent = *lifecycleOAuth("account-three")
	_, linked, err = service.CompleteIntoPool(context.Background(), auth, callback, other.ID, store.CatalogueSourceVersion(other))
	if !errors.Is(err, store.ErrNativeOAuthIdentity) || linked {
		t.Fatal("duplicate account changed owner", err)
	}
	other, _ = st.Channel(other.ID)
	if len(other.Credentials) != 1 {
		t.Fatal("copied into second renewal owner")
	}
}
func (d *lifecycleDriver) Refresh(ctx context.Context, oauth store.OAuthCredential) (store.OAuthCredential, error) {
	d.refreshes.Add(1)
	if d.refresh != nil {
		return d.refresh(ctx, oauth)
	}
	return store.OAuthCredential{AccessToken: "new-" + oauth.Account, RefreshToken: "new-refresh-" + oauth.Account, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (d *lifecycleDriver) Models(context.Context, store.OAuthCredential) ([]Model, error) {
	d.modelCalls.Add(1)
	return nil, errors.New("synthetic catalog unavailable")
}
func (d *lifecycleDriver) Execute(client *http.Client, req *http.Request, oauth store.OAuthCredential) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header.Set("Authorization", "Bearer "+oauth.AccessToken)
	return accessguard.Do(client, copy)
}

func lifecycleFixture(t *testing.T) (*store.Store, *lifecycleDriver, *Service) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	driver := &lifecycleDriver{definition: Definition{ID: "claude", Name: "Claude", CredentialType: store.CredentialClaudeOAuth, Provider: store.OAuthProviderClaude,
		CallbackHost: "localhost", CallbackPath: "/callback", CallbackPorts: []int{54545}, Endpoints: []store.Endpoint{{Format: store.FormatAnthropicMessages, BaseURL: "https://api.anthropic.com"}}}}
	return st, driver, NewService(st, driver)
}

func lifecycleOAuth(account string) *store.OAuthCredential {
	return &store.OAuthCredential{Provider: store.OAuthProviderClaude, Account: account, DeviceID: "synthetic-device-" + account, OrganizationID: "synthetic-org-" + account,
		AccessToken: "old-" + account, RefreshToken: "refresh-" + account, ClientID: "synthetic-client", ExpiresAt: time.Now().Add(-time.Hour)}
}

func lifecycleChannel(t *testing.T, st *store.Store, driver *lifecycleDriver) store.Channel {
	t.Helper()
	c := store.Channel{Name: "Synthetic pool", CredentialType: driver.definition.CredentialType, Enabled: true, Endpoints: driver.definition.Endpoints,
		Credentials: []store.CredentialItem{{ID: "one", Enabled: true, OAuth: lifecycleOAuth("account-one")}, {ID: "two", Enabled: true, OAuth: lifecycleOAuth("account-two")}}}
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func selected(c store.Channel, index int) store.Channel {
	c.Credentials = []store.CredentialItem{c.Credentials[index]}
	c.OAuth = c.Credentials[0].OAuth
	return c
}

func waitResult[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
		var zero T
		return zero
	}
}

func TestNativeServiceExactCredentialRefreshesOnce(t *testing.T) {
	st, driver, svc := lifecycleFixture(t)
	c := lifecycleChannel(t, st, driver)
	second := selected(c, 1)
	results := make(chan error, 24)
	for i := 0; i < cap(results); i++ {
		go func() {
			token, err := svc.Bearer(context.Background(), second)
			if err == nil && token != "new-account-two" {
				err = errors.New("refreshed wrong pool member")
			}
			results <- err
		}()
	}
	for i := 0; i < cap(results); i++ {
		if err := waitResult(t, results); err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := st.Channel(c.ID)
	if err != nil || driver.refreshes.Load() != 1 || !reflect.DeepEqual(c.Credentials[0], fresh.Credentials[0]) || fresh.Generation != c.Generation || fresh.Credentials[1].OAuth.RefreshToken != "new-refresh-account-two" {
		t.Fatalf("refresh isolation/single-flight failed: %v", err)
	}
	if _, err := svc.Bearer(context.Background(), c); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("ambiguous pool defaulted to first credential: %v", err)
	}
	wrong := selected(c, 1)
	oauth := *wrong.Credentials[0].OAuth
	oauth.Account = "another-account"
	wrong.Credentials[0].OAuth = &oauth
	if _, err := svc.Bearer(context.Background(), wrong); !errors.Is(err, store.ErrNativeOAuthIdentity) {
		t.Fatalf("snapshot identity drift accepted: %v", err)
	}
}

func TestNativeServiceIssuedRefreshSurvivesDisableAndConfigEdit(t *testing.T) {
	st, driver, svc := lifecycleFixture(t)
	c := lifecycleChannel(t, st, driver)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"access_token":"issued-two","refresh_token":"issued-refresh-two"}`)
	}))
	defer issuer.Close()
	// Ensure a failed test releases the server before Close waits for handlers.
	defer unblock()
	driver.refresh = func(ctx context.Context, oauth store.OAuthCredential) (store.OAuthCredential, error) {
		req, _ := http.NewRequestWithContext(ctx, "POST", issuer.URL, nil)
		resp, err := accessguard.Do(issuer.Client(), req)
		if err != nil {
			return store.OAuthCredential{}, err
		}
		defer resp.Body.Close()
		var token struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&token); err != nil {
			return store.OAuthCredential{}, err
		}
		return store.OAuthCredential{AccessToken: token.Access, RefreshToken: token.Refresh, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	var allowed atomic.Bool
	allowed.Store(true)
	denied := errors.New("synthetic plugin epoch changed")
	ctx := accessguard.With(context.Background(), func() error {
		if !allowed.Load() {
			return denied
		}
		return nil
	})
	results := make(chan error, 1)
	go func() {
		token, err := svc.Bearer(ctx, selected(c, 1))
		if err == nil && token != "issued-two" {
			err = errors.New("wrong issued token")
		}
		results <- err
	}()
	waitResult(t, entered)
	stale, err := st.Channel(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale.Name = "Edited while refreshing"
	stale.Credentials[1].OAuth.DeviceID = "stale-private-device"
	if err = st.SaveChannelConfig(&stale); err != nil {
		t.Fatal(err)
	}
	allowed.Store(false)
	unblock()
	if err := waitResult(t, results); err != nil {
		t.Fatalf("issued rotation was rejected: %v", err)
	}
	fresh, err := st.Channel(c.ID)
	if err != nil || fresh.Name != stale.Name || fresh.Credentials[1].OAuth.AccessToken != "issued-two" || fresh.Credentials[1].OAuth.RefreshToken != "issued-refresh-two" || fresh.Credentials[1].OAuth.DeviceID != c.Credentials[1].OAuth.DeviceID {
		t.Fatalf("rotation/configuration protection failed: %v", err)
	}
	if _, err := svc.Models(ctx, selected(fresh, 1)); !errors.Is(err, denied) || driver.modelCalls.Load() != 0 {
		t.Fatalf("disabled epoch sent another models request: %v", err)
	}
}

func TestNativeServiceDeleteSharesRefreshLock(t *testing.T) {
	st, driver, svc := lifecycleFixture(t)
	c := lifecycleChannel(t, st, driver)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	driver.refresh = func(context.Context, store.OAuthCredential) (store.OAuthCredential, error) {
		close(entered)
		<-release
		return store.OAuthCredential{AccessToken: "issued-before-delete", RefreshToken: "issued-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	refreshDone := make(chan error, 1)
	go func() { _, err := svc.Bearer(context.Background(), selected(c, 1)); refreshDone <- err }()
	waitResult(t, entered)
	observed := make(chan bool, 1)
	ctx := accessguard.With(context.Background(), func() error {
		fresh, err := st.Channel(c.ID)
		observed <- err == nil && fresh.Credentials[1].OAuth.AccessToken == "issued-before-delete"
		return nil
	})
	deleted := make(chan error, 1)
	go func() { deleted <- svc.DisconnectChannelAtVersion(ctx, c.ID, "") }()
	select {
	case <-observed:
		t.Fatal("delete entered before refresh released lifecycle lock")
	default:
	}
	unblock()
	if err := waitResult(t, refreshDone); err != nil {
		t.Fatal(err)
	}
	if !waitResult(t, observed) {
		t.Fatal("delete did not observe the durable rotated token")
	}
	if err := waitResult(t, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Channel(c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("channel not deleted: %v", err)
	}
}

func TestNativeServiceCompleteTargetsExactAccountAndPreservesChannel(t *testing.T) {
	st, driver, svc := lifecycleFixture(t)
	c := lifecycleChannel(t, st, driver)
	c.Enabled = false
	c.Credentials[1].OAuth.Status = store.OAuthStatusReauthRequired
	if err := st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	driver.consent = *c.Credentials[1].OAuth
	driver.consent.AccessToken = "consent-two"
	driver.consent.RefreshToken = "consent-refresh-two"
	driver.consent.DeviceID = "new-consent-device"
	driver.consent.ExpiresAt = time.Now().Add(time.Hour)
	auth := Authorization{State: "synthetic-state"}
	cb := oauthbase.CallbackResult{Code: "synthetic-code", State: auth.State}
	updated, linked, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{ChannelID: c.ID, CredentialID: c.Credentials[1].ID, Identity: store.NativeOAuthIdentity(c.CredentialType, c.Credentials[1].OAuth), Generation: c.Generation})
	if err != nil || !linked || updated.Enabled || updated.Credentials[0].OAuth.AccessToken != c.Credentials[0].OAuth.AccessToken || updated.Credentials[1].OAuth.AccessToken != "consent-two" || updated.Credentials[1].OAuth.Status != store.OAuthStatusActive || updated.Credentials[1].OAuth.DeviceID != c.Credentials[1].OAuth.DeviceID || updated.Generation != c.Generation {
		t.Fatalf("exact reauthorization failed: %v", err)
	}
	driver.consent.Account = "other-account"
	if _, _, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{ChannelID: c.ID, CredentialID: updated.Credentials[1].ID, Identity: store.NativeOAuthIdentity(updated.CredentialType, updated.Credentials[1].OAuth), Generation: updated.Generation}); !errors.Is(err, store.ErrNativeOAuthIdentity) {
		t.Fatalf("foreign account overwrote target: %v", err)
	}
	if err := svc.DisconnectChannelAtVersion(context.Background(), c.ID, ""); err != nil {
		t.Fatal(err)
	}
	before := driver.exchanges.Load()
	if _, _, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{ChannelID: c.ID, CredentialID: updated.Credentials[1].ID, Identity: store.NativeOAuthIdentity(updated.CredentialType, updated.Credentials[1].OAuth), Generation: updated.Generation}); !errors.Is(err, store.ErrNotFound) || driver.exchanges.Load() != before {
		t.Fatalf("deleted reauthorization target was reused: %v", err)
	}
}

func TestNativeServiceConsentPersistsWithoutCatalogAndDeduplicates(t *testing.T) {
	st, driver, svc := lifecycleFixture(t)
	driver.consent = *lifecycleOAuth("new-account")
	driver.consent.ExpiresAt = time.Now().Add(time.Hour)
	driver.consent.AccountLabel = "private-display@example.invalid"
	auth, err := svc.Start(context.Background(), "http://localhost:54545/callback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), "http://127.0.0.1:54545/callback"); err == nil {
		t.Fatal("wrong callback host accepted")
	}
	if _, err := svc.Start(context.Background(), "http://localhost:54546/callback"); err == nil {
		t.Fatal("unregistered callback port accepted")
	}
	cb := oauthbase.CallbackResult{Code: "synthetic-code", State: auth.State}
	channel, linked, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{})
	if err != nil || linked || channel.OAuth.RefreshToken != driver.consent.RefreshToken || channel.OAuth.AccountLabel != "Claude" || len(channel.Models) != 0 || len(channel.AvailableModels) != 0 || driver.modelCalls.Load() != 0 {
		t.Fatalf("consent was lost/implicitly published/catalogued: %v", err)
	}
	again, linked, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{})
	if err != nil || !linked || again.ID != channel.ID {
		t.Fatalf("same account duplicated: %v", err)
	}
	before := driver.exchanges.Load()
	cb.State = "foreign-state"
	if _, _, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{}); err == nil || driver.exchanges.Load() != before {
		t.Fatal("invalid callback contacted provider")
	}
	channels, err := st.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatalf("unexpected channel count: %v", err)
	}
}

func TestNativeServiceAnonymousDevinUsesSessionFingerprint(t *testing.T) {
	st, driver, _ := lifecycleFixture(t)
	driver.definition = Definition{ID: "devin", Name: "Devin", Provider: store.OAuthProviderDevin, CredentialType: store.CredentialDevinOAuth, CallbackHost: "127.0.0.1", CallbackPath: "/callback", Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://server.codeium.com"}}}
	svc := NewService(st, driver)
	driver.consent = store.OAuthCredential{Provider: store.OAuthProviderDevin, AccessToken: "session-one", AccountLabel: "same label"}
	auth := Authorization{State: "synthetic-state"}
	cb := oauthbase.CallbackResult{Code: "code", State: auth.State}
	first, linked, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{})
	if err != nil || linked {
		t.Fatal(err)
	}
	driver.consent.AccessToken = "session-two"
	second, linked, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{})
	if err != nil || linked || second.ID == first.ID {
		t.Fatalf("display name merged anonymous sessions: %v", err)
	}
	again, linked, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{})
	if err != nil || !linked || again.ID != second.ID {
		t.Fatalf("identical session did not deduplicate: %v", err)
	}
	if token, err := svc.Bearer(context.Background(), first); err != nil || token != "session-one" || driver.refreshes.Load() != 0 {
		t.Fatalf("anonymous session claimed refresh/expiry: %v", err)
	}
	if _, _, err := svc.Complete(context.Background(), auth, cb, ConsentTarget{ChannelID: first.ID, CredentialID: first.PrimaryCredential().ID, Identity: store.NativeOAuthIdentity(first.CredentialType, first.OAuth), Generation: first.Generation}); !errors.Is(err, store.ErrNativeOAuthIdentity) {
		t.Fatalf("unproven Devin account identity replaced old session: %v", err)
	}
}

func TestNativeServiceExecuteUsesGuardAndSelectedCredential(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	st, driver, svc := lifecycleFixture(t)
	c := lifecycleChannel(t, st, driver)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer new-account-two" {
			t.Error("wrong selected credential sent")
		}
		fmt.Fprint(w, "OK")
	}))
	defer upstream.Close()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", upstream.URL, nil)
	resp, err := svc.Execute(upstream.Client(), req, selected(c, 1))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	denied := errors.New("synthetic access revoked")
	ctx := accessguard.With(context.Background(), func() error { return denied })
	if _, err := svc.Execute(upstream.Client(), req.WithContext(ctx), selected(c, 1)); !errors.Is(err, denied) || calls.Load() != 1 {
		t.Fatalf("guarded execute contacted provider: %v", err)
	}
}
