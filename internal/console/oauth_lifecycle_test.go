package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

func TestCodexReauthorizationClearsFailureAndKeepsDisabledChoice(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	first := finishCodexBrowser(t, h, d, oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start")))
	if first["status"] != "complete" {
		t.Fatal("first authorization failed")
	}
	id := int64(first["channel_id"].(float64))
	if err := s.SetOAuthStatus(id, store.OAuthStatusReauthRequired, "invalid_grant", true); err != nil {
		t.Fatal(err)
	}
	before, err := s.Channel(id)
	if err != nil {
		t.Fatal(err)
	}
	started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=reauth&channel_id="+strconv.FormatInt(id, 10)))
	if result := finishCodexBrowser(t, h, d, started); result["status"] != "complete" {
		t.Fatalf("reauthorization failed: %v", result)
	}
	got, err := s.Channel(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuth.Status != store.OAuthStatusActive || got.OAuth.StatusReason != "" {
		t.Fatal("completed authorization kept the failed credential status")
	}
	if got.Enabled || got.Generation != before.Generation || got.OAuth.Account != before.OAuth.Account {
		t.Fatal("reauthorization reset the disabled choice or changed ownership/generation")
	}
	if bearer, err := d.service(s).Bearer(context.Background(), got); err != nil || bearer != "at-console" {
		t.Fatalf("completed authorization has no usable Bearer: %v", err)
	}
}

func TestConsoleChannelEditPreservesCurrentOAuth(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	d := newCodexDouble(t)
	s, h := codexConsole(t, d)
	result := finishCodexBrowser(t, h, d, oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start")))
	id := int64(result["channel_id"].(float64))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/channels", nil))
	var channels []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &channels); err != nil || len(channels) != 1 {
		t.Fatal("cannot fetch editable public channel")
	}
	edit := channels[0]
	delete(edit, "has_key")
	delete(edit, "has_oauth")
	delete(edit, "associated_prices")
	edit["name"], edit["enabled"] = "edited-name", false
	if err := s.ReplaceOAuthTokens(id, store.OAuthTokens{AccessToken: "current-access", RefreshToken: "current-refresh", IDToken: "current-id", ExpiresAt: time.Now().Add(time.Hour).UTC(), Scopes: "current-scope"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOAuthStatus(id, store.OAuthStatusReauthRequired, "current-status", false); err != nil {
		t.Fatal(err)
	}
	latest, err := s.Channel(id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(edit)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PUT", "/channels/"+strconv.FormatInt(id, 10), bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("management edit refused: %d %s", w.Code, w.Body.String())
	}
	got, err := s.Channel(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.OAuth, latest.OAuth) || got.Name != "edited-name" || got.Enabled {
		t.Fatal("management edit changed current OAuth or lost chosen configuration")
	}
}

func TestCodexReauthorizationSerializesWithInFlightRefresh(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, rejected := range []bool{false, true} {
		name := "late_success"
		if rejected {
			name = "late_rejection"
		}
		t.Run(name, func(t *testing.T) {
			previous := codex.CallbackPorts
			codex.CallbackPorts = []int{0}
			t.Cleanup(func() { codex.CallbackPorts = previous })
			d := newCodexDouble(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseRefresh := func() { once.Do(func() { close(release) }) }
			defer releaseRefresh()
			d.tokenHook = func(w http.ResponseWriter, r *http.Request) bool {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return true
				}
				if r.Form.Get("grant_type") != "refresh_token" {
					return false
				}
				if r.Form.Get("refresh_token") != "old-refresh" {
					t.Error("rotation did not use the old stored refresh token")
				}
				close(entered)
				<-release
				if rejected {
					w.WriteHeader(400)
					json.NewEncoder(w).Encode(codex.TokenResponse{Error: "invalid_grant"})
					return true
				}
				json.NewEncoder(w).Encode(codex.TokenResponse{AccessToken: "old-operation-access", RefreshToken: "old-operation-refresh", ExpiresIn: 3600})
				return true
			}
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			svc := d.service(s)
			h := NewWithOAuth(s, "http://127.0.0.1:17878", Hooks{}, nil, nil, svc)
			t.Cleanup(func() { ReleaseOAuthListeners(h) })
			first := finishCodexBrowser(t, h, d, oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start")))
			id := int64(first["channel_id"].(float64))
			if err := s.ReplaceOAuthTokens(id, store.OAuthTokens{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			before, err := s.Channel(id)
			if err != nil {
				t.Fatal(err)
			}
			refreshDone := make(chan error, 1)
			go func() { _, err := svc.Bearer(context.Background(), before); refreshDone <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("old refresh did not reach token endpoint")
			}
			started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=reauth&channel_id="+strconv.FormatInt(id, 10)))
			parsed, err := url.Parse(started["authorization_url"].(string))
			if err != nil {
				t.Fatal(err)
			}
			callback := started["redirect_uri"].(string) + "?code=reauth&state=" + url.QueryEscape(parsed.Query().Get("state"))
			path := "/oauth/codex/status?flow=" + started["flow"].(string) + "&callback=" + url.QueryEscape(callback)
			loginDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				loginDone <- w
			}()
			var w *httptest.ResponseRecorder
			select {
			case w = <-loginDone:
			case <-time.After(300 * time.Millisecond):
			}
			releaseRefresh()
			if err := <-refreshDone; rejected && !errors.Is(err, codex.ErrReauthRequired) || !rejected && err != nil {
				t.Fatalf("old refresh result: %v", err)
			}
			if w == nil {
				w = <-loginDone
			}
			var result map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["status"] != "complete" {
				t.Fatalf("authorization did not complete: %s", w.Body.String())
			}
			got, err := s.Channel(id)
			if err != nil {
				t.Fatal(err)
			}
			if got.OAuth.AccessToken != "at-console" || got.OAuth.RefreshToken != "rt-console" || got.OAuth.Status != store.OAuthStatusActive || got.OAuth.StatusReason != "" {
				t.Fatal("old operation displaced or invalidated the completed Codex authorization")
			}
			if got.OAuth.Account != before.OAuth.Account || got.Generation != before.Generation {
				t.Fatal("reauthorization changed account ownership or continuation generation")
			}
			if bearer, err := svc.Bearer(context.Background(), got); err != nil || bearer != "at-console" {
				t.Fatalf("reauthorized Bearer unusable: %v", err)
			}
		})
	}
}

func TestPendingReauthorizationDoesNotRecreateDeletedChannel(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	t.Run("siwc", func(t *testing.T) {
		d := newChatgptDouble(t)
		s, h := chatgptConsole(t, d)
		id := finishChatGPT(t, h, d, oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start")))
		started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/chatgpt/start?operation=reauth&channel_id="+strconv.FormatInt(id, 10)))
		parsed, err := url.Parse(started["authorization_url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		d.nonce = func() string { return parsed.Query().Get("nonce") }
		if result := oauthCall(t, h, "DELETE", "/channels/"+strconv.FormatInt(id, 10)); result["ok"] != true {
			t.Fatal("delete failed")
		}
		callback := started["redirect_uri"].(string) + "?code=late&state=" + url.QueryEscape(parsed.Query().Get("state")) + "&client_id=oaiapp_console"
		result := oauthCall(t, h, "GET", authorizationFixturePath(t, h, "/oauth/chatgpt/status?flow="+started["flow"].(string)+"&callback="+url.QueryEscape(callback)))
		if result["status"] != "error" || result["code"] != "channel_missing" {
			t.Fatalf("deleted bound channel was recreated: %v", result)
		}
		if rows, err := s.ListChannels(); err != nil || len(rows) != 0 {
			t.Fatal("deleted channel reappeared")
		}
	})
	t.Run("codex", func(t *testing.T) {
		d := newCodexDouble(t)
		s, h := codexConsole(t, d)
		first := finishCodexBrowser(t, h, d, oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start")))
		id := int64(first["channel_id"].(float64))
		started := oauthCall(t, h, "POST", authorizationFixturePath(t, h, "/oauth/codex/start?operation=reauth&channel_id="+strconv.FormatInt(id, 10)))
		if result := oauthCall(t, h, "DELETE", "/channels/"+strconv.FormatInt(id, 10)); result["ok"] != true {
			t.Fatal("delete failed")
		}
		result := finishCodexBrowser(t, h, d, started)
		if result["status"] != "error" || result["code"] != "channel_missing" {
			t.Fatalf("deleted bound channel was recreated: %v", result)
		}
		if rows, err := s.ListChannels(); err != nil || len(rows) != 0 {
			t.Fatal("deleted channel reappeared")
		}
	})
}
