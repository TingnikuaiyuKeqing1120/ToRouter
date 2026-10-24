package pluginhost

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"torouter/internal/marketprofile"
)

const marketFixture = `
export default [async ({client}) => ({
 config(c){c.provider.fixture={name:'Fixture',npm:'@ai-sdk/openai-compatible',api:'https://example.invalid/v1',models:{alias:{id:'upstream',provider:{api:BASE,npm:'@ai-sdk/openai-compatible'}},message:{provider:{api:BASE,npm:'@ai-sdk/anthropic'}}}}},
 auth:{provider:'fixture',methods:[
  {type:'api',label:'API token',prompts:[{type:'password',key:'token',message:'Token',validate:v=>!v?'required':undefined}],authorize:async inputs=>({type:'success',key:inputs.token,metadata:{tenant:'synthetic-tenant'}})},
  {type:'oauth',label:'OAuth',prompts:[{type:'select',key:'region',message:'Region',options:[{value:'test',label:'Test'}]}],authorize:async inputs=>({url:'https://example.invalid/login',method:'code',instructions:inputs.region,callback:async code=>({type:'success',access:code,refresh:'synthetic-refresh',expires:9999999999999,account:'synthetic-account'})})}
 ],refresh:async a=>({access:'synthetic-rotated',expires:9999999999999}),
 loader:async getAuth=>({fetch:async(url,init)=>{
  const body=JSON.parse(init.body);
  if(body.error)throw new Error('failure '+(await getAuth()).access);
  if(body.oldError)throw new Error('failure synthetic-private-access');
  if(body.rotate){
   const next=await(await fetch(body.rotate)).json();
   await client.auth.set({path:{id:'fixture'},body:{...await getAuth(),...next}});
  }
  const a=(await client.auth.get({path:{id:'fixture'}})).data;
  init.headers.set('x-fixture-token',a.access??a.key);
  return fetch(url,init)
 }})},
 provider:{id:'fixture',models:async p=>p.models},
 tool:{shouldNeverRun:()=>{throw new Error('unrelated hook was executed')}},
 event:()=>{throw new Error('unrelated hook was executed')}
})];
`

func marketRuntime(t *testing.T) string {
	t.Helper()
	if runtime := os.Getenv("TOROUTER_TEST_BUN"); runtime != "" {
		return runtime
	}
	if runtime, err := exec.LookPath("bun"); err == nil {
		return runtime
	}
	t.Skip("Bun unavailable; market runtime behavior not verified")
	return ""
}

func marketModule(t *testing.T, base string) string {
	t.Helper()
	b, _ := json.Marshal(base)
	file := filepath.Join(t.TempDir(), "provider.mjs")
	if err := os.WriteFile(file, []byte("const BASE="+string(b)+";\n"+marketFixture), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}
func marketWorker(t *testing.T, module string, auth string) *Worker {
	t.Helper()
	w, err := StartMarket(context.Background(), marketRuntime(t), module, "fixture", json.RawMessage(auth))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.Close()
		select {
		case <-w.done:
		case <-time.After(3 * time.Second):
			t.Error("market worker did not exit")
		}
	})
	return w
}
func marketHandler(t *testing.T, saved *atomic.Int32, output *bytes.Buffer) Handler {
	t.Helper()
	return func(ctx context.Context, e Event) (json.RawMessage, error) {
		switch e.Name {
		case "network":
			return json.RawMessage(`true`), ctx.Err()
		case "auth":
			if ctx.Err() != nil {
				t.Error("rotation got cancelled persistence context")
			}
			var p struct {
				Provider string          `json:"provider_id"`
				Value    json.RawMessage `json:"value"`
			}
			if json.Unmarshal(e.Payload, &p) != nil || p.Provider != "fixture" {
				t.Error("wrong auth event")
			}
			saved.Add(1)
			return p.Value, nil
		case "head":
			var p struct {
				Status  int               `json:"status"`
				Headers map[string]string `json:"headers"`
			}
			if json.Unmarshal(e.Payload, &p) != nil || p.Status != 200 || p.Headers["x-fixture"] != "ok" {
				t.Error("lost response head")
			}
			return nil, nil
		case "chunk":
			var p struct {
				Data string `json:"data"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			b, err := base64.StdEncoding.DecodeString(p.Data)
			if err == nil {
				_, err = output.Write(b)
			}
			return nil, err
		}
		return nil, errors.New("unexpected event")
	}
}

func TestMarketDescribeAndAuthorization(t *testing.T) {
	runtime := marketRuntime(t)
	module := marketModule(t, "https://example.invalid/v1")
	providers, err := InspectMarket(context.Background(), runtime, module)
	if err != nil || len(providers) != 1 || len(providers[0].Methods) != 2 || len(providers[0].Methods[0].Prompts) != 1 {
		t.Fatalf("describe: %#v %v", providers, err)
	}
	if providers[0].Models[0].Format == providers[0].Models[1].Format {
		t.Fatal("model protocols collapsed")
	}
	w := marketWorker(t, module, `null`)
	_, err = w.Invoke(context.Background(), "authorize", map[string]any{"method": 0, "inputs": map[string]string{"token": ""}}, nil)
	if err == nil {
		t.Fatal("missing prompt validation")
	}
	raw, err := w.Invoke(context.Background(), "authorize", map[string]any{"method": 0, "inputs": map[string]string{"token": "synthetic-api-token"}}, nil)
	var a marketprofile.Authorization
	if err != nil || json.Unmarshal(raw, &a) != nil || a.Method != "api" || !bytes.Contains(a.State, []byte(`"type":"api"`)) {
		t.Fatalf("API consent: %s %v", raw, err)
	}
	if _, err = w.Invoke(context.Background(), "models", nil, nil); err != nil {
		t.Fatalf("models after API consent: %v", err)
	}
	raw, err = w.Invoke(context.Background(), "authorize", map[string]any{"method": 1, "inputs": map[string]string{"region": "test"}}, nil)
	if err != nil || json.Unmarshal(raw, &a) != nil || a.Method != "code" || a.Instructions != "test" {
		t.Fatalf("OAuth start: %s %v", raw, err)
	}
	raw, err = w.Invoke(context.Background(), "complete", map[string]any{"code": "synthetic-consent"}, nil)
	if err != nil || !bytes.Contains(raw, []byte(`"type":"oauth"`)) || !bytes.Contains(raw, []byte(`"account":"synthetic-account"`)) {
		t.Fatalf("OAuth complete: %s %v", raw, err)
	}
	if _, err = w.Invoke(context.Background(), "models", nil, nil); err != nil {
		t.Fatalf("models after OAuth consent: %v", err)
	}
}

func TestMarketRejectsAuthOnlyTool(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tool.mjs")
	code := `export default async()=>({config(c){c.provider.search={name:'Search sign-in',models:{}}},auth:{provider:'search',methods:[{type:'api',label:'Token'}],loader:async()=>({})},tool:{search:{execute:async()=>({})}});`
	if err := os.WriteFile(file, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectMarket(context.Background(), marketRuntime(t), file); err == nil {
		t.Fatal("auth-only tool accepted as model supplier")
	}
}

func TestMarketModelEndpointRefreshAndStream(t *testing.T) {
	_ = marketRuntime(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/actual/chat/completions" {
			t.Errorf("wrong model URL: %s", r.URL.Path)
		}
		if r.Header.Get("x-fixture-token") != "synthetic-rotated" {
			t.Error("request started before auth ACK")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "upstream" {
			t.Error("model API ID was not mapped")
		}
		hits.Add(1)
		w.Header().Set("x-fixture", "ok")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: synthetic\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	w := marketWorker(t, marketModule(t, server.URL+"/actual"), `{"type":"oauth","access":"synthetic-old","refresh":"synthetic-refresh","expires":1}`)
	var saved atomic.Int32
	var output bytes.Buffer
	handler := marketHandler(t, &saved, &output)
	raw, err := w.Invoke(context.Background(), "models", nil, handler)
	if err != nil || !bytes.Contains(raw, []byte(`"api_id":"upstream"`)) {
		t.Fatalf("models: %s %v", raw, err)
	}
	body := base64.StdEncoding.EncodeToString([]byte(`{"model":"alias","stream":true}`))
	_, err = w.Invoke(context.Background(), "fetch", map[string]any{"url": "https://placeholder.invalid/v1/chat/completions", "body": body, "model_id": "alias", "format": "openai_chat_completions"}, handler)
	if err != nil || saved.Load() != 1 || hits.Load() != 1 || output.String() != "data: synthetic\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream: saved=%d hits=%d out=%q err=%v", saved.Load(), hits.Load(), output.String(), err)
	}
}

func TestMarketCancellationDrainsRotation(t *testing.T) {
	_ = marketRuntime(t)
	started, release := make(chan struct{}), make(chan struct{})
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"access":"synthetic-late-rotation"}`)
	}))
	defer refresh.Close()
	w := marketWorker(t, marketModule(t, "https://example.invalid/v1"), `{"type":"oauth","access":"synthetic-before","refresh":"synthetic-refresh","expires":9999999999999}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var saved atomic.Int32
	var output bytes.Buffer
	body, _ := json.Marshal(map[string]any{"model": "alias", "rotate": refresh.URL})
	done := make(chan error, 1)
	go func() {
		_, err := w.Invoke(ctx, "fetch", map[string]any{"url": "https://placeholder.invalid/v1/chat/completions", "body": base64.StdEncoding.EncodeToString(body), "model_id": "alias", "format": "openai_chat_completions"}, marketHandler(t, &saved, &output))
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("refresh did not start")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || saved.Load() != 1 {
			t.Fatalf("cancel lost rotation: %v saved=%d", err, saved.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not drain")
	}
}

func TestMarketSanitizesErrors(t *testing.T) {
	w := marketWorker(t, marketModule(t, "https://example.invalid/v1"), `{"type":"oauth","access":"synthetic-private-access","refresh":"synthetic-private-refresh"}`)
	body := base64.StdEncoding.EncodeToString([]byte(`{"model":"alias","error":true}`))
	_, err := w.Invoke(context.Background(), "fetch", map[string]any{"body": body, "model_id": "alias", "format": "openai_chat_completions"}, nil)
	if err == nil || strings.Contains(err.Error(), "synthetic-private") || !strings.Contains(err.Error(), "failure") {
		t.Fatalf("unsafe/unhelpful error: %v", err)
	}
	var saved atomic.Int32
	var output bytes.Buffer
	if _, err = w.Invoke(context.Background(), "refresh", nil, marketHandler(t, &saved, &output)); err != nil {
		t.Fatal(err)
	}
	body = base64.StdEncoding.EncodeToString([]byte(`{"model":"alias","oldError":true}`))
	_, err = w.Invoke(context.Background(), "fetch", map[string]any{"body": body, "model_id": "alias", "format": "openai_chat_completions"}, nil)
	if err == nil || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatalf("rotated-away token leaked: %v", err)
	}
}

// Opt-in local source evidence. This uses unchanged files from the fixed public
// commit already recorded by T126; it does not download or contact a vendor.
func TestMarketUnmodifiedCommunityPackages(t *testing.T) {
	runtime := marketRuntime(t)
	repo := filepath.Join("..", "..", ".local", "research", "magpie-plugins-t120")
	const sha = "a1689b1b6feb37f9de4c213fee276bc9be4676cb"
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Skip("fixed community research checkout unavailable")
	}
	for _, name := range []string{"cloudflare-workers-ai", "zen-free", "qoder", "nous"} {
		t.Run(name, func(t *testing.T) {
			prefix := "packages/" + name + "/"
			files, err := exec.Command("git", "-C", repo, "ls-tree", "-r", "--name-only", sha, "--", prefix).Output()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			for _, file := range strings.Fields(string(files)) {
				raw, err := exec.Command("git", "-C", repo, "show", sha+":"+file).Output()
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(file, prefix)))
				if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(target, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			worker, providers, err := startMarket(context.Background(), runtime, dir, "", nil, func(context.Context, Event) (json.RawMessage, error) {
				return nil, errors.New("offline original-package probe")
			})
			if worker != nil {
				defer worker.Close()
			}
			if err != nil || len(providers) == 0 {
				t.Fatalf("original package probe: %v %#v", err, providers)
			}
			if len(providers[0].Methods) == 0 {
				t.Fatal("original package methods absent")
			}
			if name == "qoder" && len(providers) != 2 {
				t.Fatal("original package lost a region provider")
			}
		})
	}
}

func TestMarketStreamCancellationAndBackpressure(t *testing.T) {
	_ = marketRuntime(t)
	var ended atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer ended.Store(true)
		w.Header().Set("x-fixture", "ok")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 128<<10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	w := marketWorker(t, marketModule(t, server.URL+"/actual"), `{"type":"api","key":"synthetic-key"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var saved atomic.Int32
	var output bytes.Buffer
	base := marketHandler(t, &saved, &output)
	chunks := make(chan struct{}, 4)
	handler := func(ctx context.Context, e Event) (json.RawMessage, error) {
		if e.Name == "chunk" {
			chunks <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return base(ctx, e)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.Invoke(ctx, "fetch", map[string]any{"body": base64.StdEncoding.EncodeToString([]byte(`{"model":"alias"}`)), "model_id": "alias", "format": "openai_chat_completions"}, handler)
		done <- err
	}()
	select {
	case <-chunks:
	case <-time.After(5 * time.Second):
		t.Fatal("first stream chunk absent")
	}
	select {
	case <-chunks:
		t.Fatal("worker emitted chunk before previous consumption ACK")
	case <-time.After(80 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled stream did not finish")
	}
	deadline := time.After(2 * time.Second)
	for !ended.Load() {
		select {
		case <-deadline:
			t.Fatal("cancel did not close upstream body")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestMarketStartHonorsDeadline(t *testing.T) {
	runtime := marketRuntime(t)
	module := filepath.Join(t.TempDir(), "never.mjs")
	if err := os.WriteFile(module, []byte(`export default async()=>{await new Promise(()=>{});return {}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := StartMarket(ctx, runtime, module, "fixture", nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("startup deadline ignored: %v", err)
	}
}

func TestMarketDynamicModelProtocols(t *testing.T) {
	_ = marketRuntime(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anthropic/messages":
			if r.Header.Get("x-api-key") != "synthetic-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
				t.Error("missing Anthropic SDK headers")
			}
		case "/responses/responses":
			if r.Header.Get("Authorization") != "Bearer synthetic-key" {
				t.Error("missing Responses SDK headers")
			}
		case "/google/models/upstream-google:streamGenerateContent":
			if r.Header.Get("x-goog-api-key") != "synthetic-key" || r.URL.Query().Get("alt") != "sse" {
				t.Error("missing Google SDK declaration")
			}
		default:
			t.Errorf("wrong model-specific protocol URL %s", r.URL.Path)
		}
		hits.Add(1)
		w.Header().Set("x-fixture", "ok")
		_, _ = io.WriteString(w, "synthetic-response")
	}))
	defer server.Close()
	base, _ := json.Marshal(server.URL)
	source := `const BASE=` + string(base) + `; export default {id:'fixture',server:async()=>({
	 auth:{provider:'fixture',methods:[{type:'api',label:'Key'}]},
	 provider:{id:'fixture',models:async()=>({
	  claude:{api:{id:'upstream-claude',npm:'@ai-sdk/anthropic',url:BASE+'/anthropic'}},
	  response:{api:{id:'upstream-response',npm:'@ai-sdk/openai',url:BASE+'/responses'}},
	  gemini:{api:{id:'upstream-google',npm:'@ai-sdk/google',url:BASE+'/google'}}
	 })}
	})}`
	module := filepath.Join(t.TempDir(), "dynamic.mjs")
	if err := os.WriteFile(module, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	w := marketWorker(t, module, `{"type":"api","key":"synthetic-key"}`)
	var saved atomic.Int32
	var output bytes.Buffer
	handler := marketHandler(t, &saved, &output)
	raw, err := w.Invoke(context.Background(), "models", nil, handler)
	var models []marketprofile.Model
	if err != nil || json.Unmarshal(raw, &models) != nil || len(models) != 3 {
		t.Fatalf("dynamic models: %s %v", raw, err)
	}
	for _, m := range models {
		body := base64.StdEncoding.EncodeToString([]byte(`{"model":"placeholder","stream":true}`))
		_, err = w.Invoke(context.Background(), "fetch", map[string]any{"url": "https://placeholder.invalid:443/models/placeholder:streamGenerateContent", "body": body, "model_id": m.ID, "format": m.Format}, handler)
		if err != nil {
			t.Fatalf("dynamic %s fetch: %v", m.ID, err)
		}
	}
	if hits.Load() != 3 {
		t.Fatal("not all protocol requests reached upstream")
	}
}

func TestMarketLoaderOnlyEndpoint(t *testing.T) {
	runtime := marketRuntime(t)
	source := `export const Provider=async()=>({config(c){c.provider.fixture={npm:'@ai-sdk/openai-compatible',models:{test:{}}}},auth:{provider:'fixture',methods:[{type:'api',label:'Key'}],loader:async()=>({baseURL:'https://example.invalid/account/v1'})}})`
	module := filepath.Join(t.TempDir(), "loader.mjs")
	if err := os.WriteFile(module, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	providers, err := InspectMarket(context.Background(), runtime, module)
	if err != nil || len(providers) != 1 || providers[0].API != "" {
		t.Fatalf("loader-only probe: %v %#v", err, providers)
	}
	w := marketWorker(t, module, `{"type":"api","key":"synthetic-key"}`)
	raw, err := w.Invoke(context.Background(), "models", nil, nil)
	var models []marketprofile.Model
	if err != nil || json.Unmarshal(raw, &models) != nil || len(models) != 1 || models[0].API != "https://example.invalid/account/v1" {
		t.Fatalf("loader-only model URL: %s %v", raw, err)
	}
}
