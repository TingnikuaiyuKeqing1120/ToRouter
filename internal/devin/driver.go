// Package devin owns independent Devin PKCE and the subscription's private RPC
// transport. No native CLI, local login discovery or agent configuration.
package devin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

const (
	clientVersion = "3000.11.3"
	exchangePath  = "/exa.seat_management_pb.SeatManagementService/ExchangeDevinCLIPKCECode"
	statusPath    = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
	modelsPath    = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"
	chatPath      = "/exa.api_server_pb.ApiServerService/GetChatMessage"
)

type Endpoints struct{ AuthorizeURL, ServerURL string }
type Driver struct {
	client    *http.Client
	endpoints Endpoints
}

func NewDriver(client *http.Client, ep Endpoints) *Driver {
	if ep.AuthorizeURL == "" {
		ep.AuthorizeURL = "https://app.devin.ai/auth/cli/continue"
	}
	if ep.ServerURL == "" {
		ep.ServerURL = "https://server.codeium.com"
	}
	return &Driver{client: client, endpoints: ep}
}
func (d *Driver) Definition() nativeoauth.Definition {
	return nativeoauth.Definition{ID: "devin", Name: "Devin", Provider: "devin", CredentialType: store.CredentialDevinOAuth, CallbackHost: "127.0.0.1", CallbackPath: "/callback",
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: d.endpoints.ServerURL, State: "stateless"}}}
}
func (d *Driver) Begin(ctx context.Context, redirect string) (nativeoauth.Authorization, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nativeoauth.Authorization{}, err
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/callback" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nativeoauth.Authorization{}, errors.New("invalid Devin loopback callback")
	}
	pkce, err := oauthbase.NewPKCE()
	if err != nil {
		return nativeoauth.Authorization{}, err
	}
	state, err := oauthbase.RandomToken(32)
	if err != nil {
		return nativeoauth.Authorization{}, err
	}
	auth, err := url.Parse(d.endpoints.AuthorizeURL)
	if err != nil {
		return nativeoauth.Authorization{}, errors.New("invalid Devin authorization endpoint")
	}
	q := auth.Query()
	for k, v := range map[string]string{"redirect_uri": redirect, "state": state, "prompt": "select_account", "code_challenge": pkce.Challenge, "code_challenge_method": "S256", "cli_pkce_marker": "1"} {
		q.Set(k, v)
	}
	auth.RawQuery = q.Encode()
	return nativeoauth.Authorization{URL: auth.String(), RedirectURI: redirect, State: state, Verifier: pkce.Verifier}, nil
}
func (d *Driver) Exchange(ctx context.Context, auth nativeoauth.Authorization, cb oauthbase.CallbackResult) (store.OAuthCredential, error) {
	if auth.State == "" || subtle.ConstantTimeCompare([]byte(auth.State), []byte(cb.State)) != 1 || cb.Code == "" || cb.Error != "" {
		return store.OAuthCredential{}, errors.New("Devin authorization callback was not accepted")
	}
	body, _ := json.Marshal(map[string]string{"code": cb.Code, "code_verifier": auth.Verifier, "redirect_uri": auth.RedirectURI})
	raw, err := d.control(ctx, exchangePath, "application/json", body)
	if err != nil {
		return store.OAuthCredential{}, err
	}
	var reply struct {
		SessionToken string `json:"sessionToken"`
		LegacyToken  string `json:"session_token"`
	}
	if json.Unmarshal(raw, &reply) != nil {
		return store.OAuthCredential{}, errors.New("invalid Devin authorization response")
	}
	token := reply.SessionToken
	if token == "" {
		token = reply.LegacyToken
	}
	if token == "" {
		return store.OAuthCredential{}, errors.New("Devin did not return session material")
	}
	raw, err = d.control(ctx, statusPath, "application/proto", message{}.bytes(1, metadata(token)))
	if err != nil {
		return store.OAuthCredential{}, err
	}
	outer, err := fields(raw)
	if err != nil {
		return store.OAuthCredential{}, err
	}
	account := ""
	for _, f := range outer {
		if f.number == 1 && f.kind == 2 {
			inner, e := fields(f.data)
			if e != nil {
				return store.OAuthCredential{}, e
			}
			for _, item := range inner {
				if item.number == 36 && item.kind == 2 {
					account = string(item.data)
				}
			}
		}
	}
	if account == "" {
		return store.OAuthCredential{}, errors.New("Devin did not provide a stable account identity; authorization was not saved")
	}
	return store.OAuthCredential{Provider: "devin", Account: account, Subject: account, AccountLabel: "Devin", AccessToken: token, APIBase: d.endpoints.ServerURL, Status: store.OAuthStatusActive}, nil
}
func (d *Driver) Refresh(context.Context, store.OAuthCredential) (store.OAuthCredential, error) {
	return store.OAuthCredential{}, errors.New("Devin session has no verified refresh endpoint; authorize again if it is rejected")
}
func (d *Driver) Models(ctx context.Context, oauth store.OAuthCredential) ([]nativeoauth.Model, error) {
	raw, err := d.control(ctx, modelsPath, "application/proto", message{}.bytes(1, metadata(oauth.AccessToken)))
	if err != nil {
		return nil, err
	}
	outer, err := fields(raw)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, f := range outer {
		if f.number == 1 && f.kind == 2 {
			config, e := fields(f.data)
			if e != nil {
				return nil, e
			}
			for _, v := range config {
				if v.number == 22 && v.kind == 2 && len(v.data) > 0 && len(v.data) <= 512 {
					found[string(v.data)] = true
				}
			}
		}
	}
	if len(found) == 0 {
		return nil, errors.New("Devin did not return any explicit model IDs")
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]nativeoauth.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, nativeoauth.Model{ID: id, Format: store.FormatOpenAIResponses})
	}
	return out, nil
}
func metadata(token string) message {
	return message{}.text(1, "devin-cli").text(2, clientVersion).text(3, token).text(4, "en").text(5, runtime.GOOS).text(7, clientVersion).text(12, "chisel").text(28, "chisel")
}
func (d *Driver) control(ctx context.Context, path, contentType string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(d.endpoints.ServerURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid Devin endpoint")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := d.do(d.client, req)
	if err != nil {
		return nil, fmt.Errorf("Devin control request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Devin control returned HTTP %d; check account access or authorize again", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWire+1))
	if err != nil || len(raw) > maxWire {
		return nil, errors.New("Devin control response could not be read")
	}
	return raw, nil
}
func (d *Driver) do(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return accessguard.Do(&copy, req)
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
