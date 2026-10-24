package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/auth"
	"torouter/internal/gateway"
	"torouter/internal/marketprofile"
	"torouter/internal/pluginhost"
	"torouter/internal/pluginmarket"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

const consoleMarketModule = `export const Supplier=async({client})=>({
config(c){c.provider.fixture={name:'Market fixture',npm:'@ai-sdk/openai-compatible',api:BASE,models:{alias:{id:'actual',name:'Fixture model'}}}},
auth:{provider:'fixture',methods:[{type:'api',label:'API Key'},{type:'oauth',label:'OAuth',authorize:async()=>({url:'https://example.invalid/login',method:'code',callback:async code=>({type:'success',access:code,refresh:'synthetic-refresh',expires:9999999999999})})}],
loader:async get=>{let a=await get();if(a.expires===1){a={...a,...await(await fetch(BASE+'/refresh')).json()};await client.auth.set({path:{id:'fixture'},body:a})};return {fetch:async(url,init)=>{init.headers.set('x-fixture-auth',a.key??a.access);return fetch(url,init)}}}},
provider:{id:'fixture',models:async p=>p.models}
});`

func marketFixtureRuntime(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TOROUTER_TEST_BUN"); p != "" {
		return p
	}
	if p, err := exec.LookPath("bun"); err == nil {
		return p
	}
	t.Skip("Bun unavailable; market integration not exercised")
	return ""
}

func consoleMarketFixture(t *testing.T, base string) (http.Handler, *store.Store, *plugins.Manager, string, string) {
	t.Helper()
	runtime := marketFixtureRuntime(t)
	root := t.TempDir()
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pkg := "@fixture/market-supplier"
	id := marketprofile.PackageID(pkg)
	typ := marketprofile.CredentialType(pkg, "fixture")
	dir := filepath.Join(root, "plugins", "market", "packages", id, "versions", "fixture")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(dir, "index.mjs")
	b, _ := json.Marshal(base)
	if err = os.WriteFile(module, []byte("const BASE="+string(b)+";\n"+consoleMarketModule), 0600); err != nil {
		t.Fatal(err)
	}
	providers, err := pluginhost.InspectMarket(context.Background(), runtime, module)
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"id": id, "package": pkg, "name": "Market fixture", "description": "Synthetic integration supplier", "version": "1.0.0", "providers": providers, "module": module, "runtime": runtime}
	b, _ = json.Marshal([]any{record})
	if err = os.WriteFile(filepath.Join(root, "plugins", "market", "active.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := plugins.New(st)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.AttachMarket(pluginmarket.New(root)); err != nil {
		t.Fatal(err)
	}
	h := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, m)
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	return h, st, m, id, typ
}

func waitMarketFlow(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		w := pluginHTTP(t, h, "GET", "/plugins/market/auth/"+id, "", 200)
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if v["status"] != "pending" {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("market flow did not finish")
	return nil
}

func TestMarketConsoleAuthorizationGatewayRotationAndUninstall(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var hits atomic.Int32
	var lastToken atomic.Value
	var st *store.Store
	var channelID int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/refresh" {
			_, _ = io.WriteString(w, `{"access":"synthetic-new","expires":9999999999999}`)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("wrong endpoint %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "actual" {
			t.Error("model alias not translated")
		}
		if r.Header.Get("x-fixture-auth") == "synthetic-new" {
			current, _ := st.Channel(channelID)
			if !strings.Contains(current.PrimaryOAuth().PluginState, "synthetic-new") {
				t.Error("inference preceded durable refresh")
			}
		}
		hits.Add(1)
		lastToken.Store(r.Header.Get("x-fixture-auth"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"synthetic","object":"chat.completion","model":"actual","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer up.Close()
	h, s, m, pkgID, typ := consoleMarketFixture(t, up.URL+"/v1")
	st = s
	start := pluginHTTP(t, h, "POST", "/plugins/market/"+pkgID+"/auth/fixture/start", `{"method":0,"inputs":{"key":"synthetic-key"}}`, 200)
	if strings.Contains(start.Body.String(), "synthetic-key") {
		t.Fatal("auth result leaked input")
	}
	var flow map[string]any
	_ = json.Unmarshal(start.Body.Bytes(), &flow)
	done := waitMarketFlow(t, h, flow["flow_id"].(string))
	if done["status"] != "success" {
		t.Fatalf("auth: %v", done)
	}
	channelID = int64(done["channel_id"].(float64))
	c, err := st.Channel(channelID)
	if err != nil || c.CredentialType != typ || len(c.AvailableModels) != 1 || len(c.Models) != 0 {
		t.Fatalf("created channel wrong: %v", err)
	}
	c.Models = []string{"alias"}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveDownstreamModel(store.DownstreamModel{ID: "public-fixture", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "alias", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	g := gateway.NewWithPlugins(st, m)
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"public-fixture","messages":[{"role":"user","content":"hello"}]}`))
		r = r.WithContext(auth.WithClientContext(r.Context(), store.ClientKey{Scope: store.DefaultClientKeyScope()}))
		g.ServeHTTP(w, r)
		return w
	}
	if w := call(); w.Code != 200 {
		t.Fatalf("gateway: %d %s", w.Code, w.Body.String())
	}
	rotated, err := store.MarketOAuthCredential(typ, `{"type":"oauth","access":"synthetic-old","refresh":"synthetic-refresh","expires":1}`, c.PrimaryOAuth().Account)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ReplacePluginState(c.ID, c.Credentials[0].ID, store.PluginStateVersion(c.PrimaryOAuth()), rotated); err != nil {
		t.Fatal(err)
	}
	if w := call(); w.Code != 200 {
		t.Fatalf("rotated gateway: %d %s", w.Code, w.Body.String())
	}
	pluginHTTP(t, h, "POST", "/channels/"+jsonNumber(c.ID)+"/oauth/models", "", 200)
	// Probing uses the same generic executor, including the exact selected pool
	// member rather than accidentally passing the whole pool or its first token.
	currentForAppend, _ := st.Channel(c.ID)
	appendBody, _ := json.Marshal(map[string]any{"method": 0, "inputs": map[string]string{"key": "synthetic-second"}, "channel_id": c.ID})
	second := pluginHTTP(t, h, "POST", "/plugins/market/"+pkgID+"/auth/fixture/start", string(appendBody), 200)
	_ = json.Unmarshal(second.Body.Bytes(), &flow)
	if r := waitMarketFlow(t, h, flow["flow_id"].(string)); r["status"] != "success" {
		t.Fatalf("append: %v", r)
	}
	pool, _ := st.Channel(c.ID)
	if len(pool.Credentials) != len(currentForAppend.Credentials)+1 {
		t.Fatal("append did not retain pool")
	}
	probeBody, _ := json.Marshal(map[string]any{"model": "alias", "formats": []string{store.FormatOpenAIChatCompletions}, "credential_ids": []string{pool.Credentials[1].ID}, "confirmed": true})
	probe := pluginHTTP(t, h, "POST", "/channels/"+jsonNumber(c.ID)+"/probe", string(probeBody), 200)
	var probeResult map[string]any
	_ = json.Unmarshal(probe.Body.Bytes(), &probeResult)
	if probeResult["ok"] != true || lastToken.Load() != "synthetic-second" {
		t.Fatalf("market second credential probe: %s", probe.Body.String())
	}
	consolePluginToggle(t, h, typ, false)
	before := hits.Load()
	if w := call(); w.Code != 503 || hits.Load() != before {
		t.Fatalf("disabled plugin invoked %d", w.Code)
	}
	consolePluginToggle(t, h, typ, true)
	old, guarded, err := m.Resolve(context.Background(), typ)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.RemoveMarket(context.Background(), pkgID); err != nil {
		t.Fatal(err)
	}
	current, _ := st.Channel(c.ID)
	if current.PrimaryOAuth() == nil {
		t.Fatal("uninstall deleted credentials")
	}
	if _, err = old.Bearer(guarded, current); err == nil {
		t.Fatal("old runtime survived uninstall")
	}
	list, _ := m.Snapshot()
	found := false
	for _, p := range list.Items {
		if p.ID == typ {
			found = true
			if p.Capabilities[0].Available {
				t.Fatal("uninstalled supplier advertised available")
			}
		}
	}
	if !found {
		t.Fatal("retained channel lost management entry")
	}
}

func jsonNumber(id int64) string { b, _ := json.Marshal(id); return string(b) }

func TestMarketCodeConsentRevocationAndOwnership(t *testing.T) {
	h, st, m, pkgID, typ := consoleMarketFixture(t, "https://example.invalid/v1")
	start := pluginHTTP(t, h, "POST", "/plugins/market/"+pkgID+"/auth/fixture/start", `{"method":1}`, 200)
	var flow map[string]any
	_ = json.Unmarshal(start.Body.Bytes(), &flow)
	id := flow["flow_id"].(string)
	complete := pluginHTTP(t, h, "POST", "/plugins/market/auth/"+id+"/complete", `{"code":"synthetic-oauth-token"}`, 200)
	if strings.Contains(complete.Body.String(), "synthetic-oauth-token") {
		t.Fatal("code exposed")
	}
	done := waitMarketFlow(t, h, id)
	if done["status"] != "success" {
		t.Fatalf("code auth failed: %v", done)
	}
	start = pluginHTTP(t, h, "POST", "/plugins/market/"+pkgID+"/auth/fixture/start", `{"method":1}`, 200)
	_ = json.Unmarshal(start.Body.Bytes(), &flow)
	id = flow["flow_id"].(string)
	o := h.(consoleHandler).oauth
	denied := WithOAuthFlowAccess(context.Background(), func(string) error { return errors.New("not owner") })
	if _, err := o.statusMarketCore(denied, id, nil, true); err == nil {
		t.Fatal("foreign flow accepted")
	}
	consolePluginToggle(t, h, typ, false)
	if out, err := o.statusMarketCore(context.Background(), id, nil, false); err != nil || asObject(out)["status"] == "pending" {
		t.Fatal("revoked flow still pending")
	}
	rows, _ := st.ListChannels()
	if len(rows) != 1 {
		t.Fatal("cancelled flow created account")
	}
	_ = m
}
