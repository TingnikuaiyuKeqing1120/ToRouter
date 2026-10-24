package pluginhost

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

	"torouter/internal/compatprofile"
	"torouter/internal/nativeoauth"
	"torouter/internal/store"
)

func serviceFixture(t *testing.T, handler http.HandlerFunc, crash bool) (*Service, store.Channel, *store.Store) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node not available")
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	state := `{"type":"oauth","uid":"synthetic-account","access":"synthetic-first","refresh":"synthetic-refresh","expires":1900000000000,"private":{"value":"synthetic-private"}}`
	o, _ := store.CompatibleOAuthCredential(compatprofile.QoderCredential, state)
	c, _, err := st.CommitPluginConsent(compatprofile.QoderCredential, o, store.PluginConsentTarget{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := compatprofile.Lookup(c.CredentialType)
	p.API, p.OpenAPI = server.URL, server.URL
	dir := t.TempDir()
	bridge, module := filepath.Join(dir, "host.mjs"), filepath.Join(dir, "provider.mjs")
	if err = os.WriteFile(bridge, Bridge, 0600); err != nil {
		t.Fatal(err)
	}
	base, _ := json.Marshal(server.URL)
	code := `
const base=` + string(base) + `;
export const _torouter={validate(c){if(c.temperature!=null)throw new Error("unsupported temperature")}};
export async function Provider({client}){
  const fresh=async(getAuth)=>{
    const a=await getAuth();
    if(a.access!=="synthetic-first")return a;
    const next={...a,...await (await fetch(base+"/api/v1/jobToken/refresh",{method:"POST",headers:{"x-synthetic-account":a.uid}})).json()};
    ` + map[bool]string{true: "process.exit(19);", false: ""}[crash] + `
    await client.auth.set({path:{id:"qoder"},body:next});return next;
  };
  return {
    config(c){c.provider.qoder={npm:"@ai-sdk/openai-compatible",api:base}},
    auth:{provider:"qoder",methods:[{type:"oauth",authorize:()=>({url:"https://qoder.com/device/selectAccounts",method:"auto",callback:async()=>({type:"success"})})}],
      loader:async getAuth=>({fetch:async(url,init)=>{await fresh(getAuth);return fetch(base+"/algo/api/v2/service/pro/sse/agent_chat_generation",init)}})},
    provider:{id:"qoder",models:async(p,{auth})=>{const cred=await fresh(async()=>auth);return (await fetch(base+"/algo/api/v2/model/list",{headers:{"x-synthetic-account":cred.uid}})).json()}}
  }
}`
	if err = os.WriteFile(module, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: st, Profile: p}
	s.launch = func(ctx context.Context, p compatprofile.Profile, state string) (*Worker, error) {
		w, err := Start(node, bridge)
		if err != nil {
			return nil, err
		}
		_, err = w.Invoke(ctx, "init", map[string]any{"protocol": 1, "module_path": module, "provider_id": "qoder", "export_name": "Provider", "methods": []int{0}, "auth": json.RawMessage(state)}, nil)
		if err != nil {
			w.Close()
			return nil, err
		}
		return w, nil
	}
	return s, c, st
}

func syntheticRequest(s *Service, ctx context.Context, extra string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, "POST", s.Profile.API+"/v1/chat/completions", strings.NewReader(`{"model":"synthetic","messages":[{"role":"user","content":"hello"}]`+extra+`}`))
	return req
}

func TestServiceDurableRotationStreamAndPreflight(t *testing.T) {
	var rotations, inference atomic.Int32
	s, c, st := serviceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/refresh":
			rotations.Add(1)
			io.WriteString(w, `{"access":"synthetic-new","refresh":"synthetic-refresh-new"}`)
		case "/algo/api/v2/model/list":
			io.WriteString(w, `{"synthetic":{"id":"synthetic"}}`)
		default:
			inference.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: synthetic-result\n\ndata: [DONE]\n\n")
		}
	}, false)
	if _, err := s.Execute(nil, syntheticRequest(s, context.Background(), `,"temperature":0.1`), c); !errors.Is(err, nativeoauth.ErrUnsupportedRequest) || rotations.Load() != 0 {
		t.Fatalf("preflight sent network: %v", err)
	}
	models, err := s.Models(context.Background(), c)
	if err != nil || len(models) != 1 {
		t.Fatalf("models: %v", err)
	}
	res, err := s.Execute(nil, syntheticRequest(s, context.Background(), ""), c)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || string(body) != "data: synthetic-result\n\ndata: [DONE]\n\n" || rotations.Load() != 1 || inference.Load() != 1 {
		t.Fatalf("stream/rotation mismatch: %v", err)
	}
	live, _ := st.Channel(c.ID)
	if !strings.Contains(live.OAuth.PluginState, "synthetic-refresh-new") || !strings.Contains(live.OAuth.PluginState, "synthetic-private") {
		t.Fatal("full state not committed")
	}
}

func TestServiceCancellationCommitsRotationAndSendsNoInference(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var sends atomic.Int32
	s, c, st := serviceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobToken/refresh" {
			sends.Add(1)
			return
		}
		close(entered)
		<-release
		io.WriteString(w, `{"access":"synthetic-after-cancel","refresh":"synthetic-new-refresh"}`)
	}, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Models(ctx, c); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not start")
	}
	cancel()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	live, _ := st.Channel(c.ID)
	if !strings.Contains(live.OAuth.PluginState, "synthetic-after-cancel") || live.OAuth.Status != store.OAuthStatusActive || sends.Load() != 0 {
		t.Fatal("cancel lost returned rotation or sent another request")
	}
}

func TestServiceCrashAfterRotationQuarantinesOnlyThatState(t *testing.T) {
	var calls atomic.Int32
	s, c, st := serviceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"access":"synthetic-lost","refresh":"synthetic-spent"}`)
	}, true)
	if _, err := s.Models(context.Background(), c); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("unknown outcome: %v", err)
	}
	if _, err := s.Models(context.Background(), c); err == nil || calls.Load() != 1 {
		t.Fatal("spent token replayed")
	}
	live, _ := st.Channel(c.ID)
	if live.OAuth.Status != store.OAuthStatusReauthRequired {
		t.Fatal("unknown rotation was not durably marked")
	}
}

func TestServiceIndependentStreamsDoNotHoldTheRotationGate(t *testing.T) {
	var rotations, sends atomic.Int32
	s, c, _ := serviceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/jobToken/refresh" {
			rotations.Add(1)
			io.WriteString(w, `{"access":"synthetic-concurrent","refresh":"synthetic-next"}`)
			return
		}
		sends.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: pending\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := s.Execute(nil, syntheticRequest(s, ctx, ""), c)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	// Deliberately do not consume the first response: backpressure holds its
	// child while the second request must still reach the upstream.
	second, err := s.Execute(nil, syntheticRequest(s, ctx, ""), c)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if sends.Load() != 2 || rotations.Load() != 1 {
		t.Fatal("stream lock serialized inference or duplicated rotation")
	}
}

func TestServiceUsesExactPoolMemberAcrossTwoAccounts(t *testing.T) {
	s, first, st := serviceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-synthetic-account") != "synthetic-second-account" {
			t.Error("selected credential fell back to the first account")
		}
		if r.URL.Path == "/api/v1/jobToken/refresh" {
			io.WriteString(w, `{"access":"synthetic-second-rotated","refresh":"synthetic-second-refresh"}`)
		} else {
			io.WriteString(w, `{"second-model":{"id":"second-model"}}`)
		}
	}, false)
	second, err := store.CompatibleOAuthCredential(compatprofile.QoderCredential, `{"type":"oauth","uid":"synthetic-second-account","access":"synthetic-first","refresh":"synthetic-second-refresh-before","expires":1900000000000}`)
	if err != nil {
		t.Fatal(err)
	}
	pool, _, err := st.CommitPluginConsent(first.CredentialType, second, store.PluginConsentTarget{ChannelID: first.ID, Version: store.CatalogueSourceVersion(first)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	selected := pool
	selected.Credentials = []store.CredentialItem{pool.Credentials[1]}
	selected.OAuth = selected.Credentials[0].OAuth
	models, err := s.Models(context.Background(), selected)
	if err != nil || len(models) != 1 || models[0].ID != "second-model" {
		t.Fatalf("exact model query: %v", err)
	}
	live, _ := st.Channel(first.ID)
	if live.Credentials[0].OAuth.PluginState != first.OAuth.PluginState || !strings.Contains(live.Credentials[1].OAuth.PluginState, "synthetic-second-rotated") {
		t.Fatal("rotation changed the wrong pool member")
	}
}

func TestInstallationVerifiesBundledPackageAndRuntime(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node not available")
	}
	i := NewInstallation(t.TempDir())
	if i.Status().Installed {
		t.Fatal("fresh install marked present")
	}
	if err := i.Install(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if !i.Status().Installed {
		t.Fatal("install did not activate")
	}
	for _, p := range compatprofile.Profiles() {
		w, err := i.Launch(context.Background(), p, "")
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
	}
	if err := os.WriteFile(filepath.Join(i.root, "qoder", "contract.mjs"), []byte("throw new Error('tampered')"), 0600); err != nil {
		t.Fatal(err)
	}
	if i.Status().Installed {
		t.Fatal("tampered package accepted")
	}
	p, _ := compatprofile.Lookup(compatprofile.QoderCredential)
	if _, err := i.Launch(context.Background(), p, ""); !errors.Is(err, ErrNotInstalled) {
		t.Fatal("tampered code executed")
	}
}
