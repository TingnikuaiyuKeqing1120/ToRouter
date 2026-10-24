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
)

const fixture = `
export async function Provider({client}) {
  if(process.env.SYNTHETIC_PRIVATE_VALUE)throw new Error("ambient credential was inherited");
  return {
    config(c){c.provider.fixture={npm:"@ai-sdk/openai-compatible",api:"https://example.invalid/v1",models:{snapshot:{id:"snapshot"}}}},
    auth:{
      provider:"fixture",
      methods:[{type:"oauth",label:"Independent login",authorize:()=>({
        url:"https://example.invalid/login",method:"auto",
        callback:async()=>({type:"success",access:"synthetic-consent",uid:"synthetic-account"})
      })}],
      refresh:async()=>({access:"synthetic-rotated",expires:123456}),
      loader:async getAuth=>({fetch:async (url,init)=>{
        const body=JSON.parse(Buffer.from(init.body).toString());
        if(body.crash)process.exit(23);
        if(body.sdk)return client.unsupported.operation();
        if(body.rotate){
          const next=await (await fetch(body.rotate)).json();
          await client.auth.set({path:{id:"fixture"},body:{...await getAuth(),...next}});
        }
        return fetch(url,init)
      }})
    },
    provider:{id:"fixture",models:async(p,{auth})=>({live:{id:"live",name:auth.access==="synthetic-rotated"?"refreshed":"original",hasRefresh:auth.refresh==="synthetic-refresh",snapshot:Object.keys(p.models).length}})}
  }
}`

func fixtureWorker(t *testing.T, source string) *Worker {
	t.Helper()
	runtime, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node runtime not installed; host process behavior not verified")
	}
	dir := t.TempDir()
	bridge, module := filepath.Join(dir, "host.mjs"), filepath.Join(dir, "provider.mjs")
	if err = os.WriteFile(bridge, Bridge, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(module, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Start(runtime, bridge)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.Close()
		select {
		case <-w.done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not exit")
		}
	})
	result, err := w.Invoke(context.Background(), "init", map[string]any{
		"protocol": 1, "module_path": module, "export_name": "Provider", "provider_id": "fixture", "methods": []int{0},
		"auth": map[string]any{"type": "oauth", "access": "synthetic-access", "refresh": "synthetic-refresh", "uid": "synthetic-account"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result, []byte(`"label":"Independent login"`)) {
		t.Fatal("missing verified authorization method")
	}
	return w
}
func eventHandler(t *testing.T, saved *atomic.Int32, output *bytes.Buffer) Handler {
	t.Helper()
	return func(ctx context.Context, event Event) (json.RawMessage, error) {
		switch event.Name {
		case "network":
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return json.RawMessage(`true`), nil
		case "auth":
			if ctx.Err() != nil {
				t.Error("rotated material got a cancelled storage context")
			}
			var value struct {
				Provider string          `json:"provider_id"`
				Mode     string          `json:"mode"`
				Value    json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal(event.Payload, &value); err != nil {
				return nil, err
			}
			if value.Provider != "fixture" || !bytes.Contains(value.Value, []byte(`"refresh":"synthetic-refresh"`)) {
				t.Error("credential target or preserved refresh lost")
			}
			if saved != nil {
				saved.Add(1)
			}
			return value.Value, nil
		case "head":
			if !bytes.Contains(event.Payload, []byte(`"status":200`)) {
				t.Error("HTTP head lost")
			}
			return json.RawMessage(`true`), nil
		case "chunk":
			var value struct {
				Data string `json:"data"`
			}
			if err := json.Unmarshal(event.Payload, &value); err != nil {
				return nil, err
			}
			b, err := base64.StdEncoding.DecodeString(value.Data)
			if err != nil {
				return nil, err
			}
			if len(b) > 65536 {
				t.Error("unbounded stream chunk")
			}
			if output != nil {
				output.Write(b)
			}
			return json.RawMessage(`true`), nil
		default:
			return nil, errors.New("unexpected event")
		}
	}
}
func TestVerifiedWorkerRefreshMergeAndNoAmbientSecrets(t *testing.T) {
	t.Setenv("SYNTHETIC_PRIVATE_VALUE", "synthetic-private")
	t.Setenv("NODE_OPTIONS", "--require=not-an-approved-module")
	w := fixtureWorker(t, fixture)
	var saved atomic.Int32
	if _, err := w.Invoke(context.Background(), "refresh", nil, eventHandler(t, &saved, nil)); err != nil {
		t.Fatal(err)
	}
	models, err := w.Invoke(context.Background(), "models", nil, nil)
	if err != nil || saved.Load() != 1 || !bytes.Contains(models, []byte(`"name":"refreshed"`)) || !bytes.Contains(models, []byte(`"hasRefresh":true`)) || !bytes.Contains(models, []byte(`"snapshot":0`)) {
		t.Fatalf("state merge or live model contract failed: %v", err)
	}
	if _, err = w.Invoke(context.Background(), "checkin", nil, nil); err == nil {
		t.Fatal("unverified hook executed")
	}
}
func TestWorkerAuthorizationIsPrivateAndExplicit(t *testing.T) {
	w := fixtureWorker(t, fixture)
	raw, err := w.Invoke(context.Background(), "authorize", map[string]int{"method": 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Flow string `json:"flow"`
	}
	json.Unmarshal(raw, &result)
	if result.Flow == "" || bytes.Contains(raw, []byte("synthetic-consent")) {
		t.Fatal("authorization started without separate consent callback")
	}
	raw, err = w.Invoke(context.Background(), "complete", map[string]string{"flow": result.Flow}, nil)
	if err != nil || !bytes.Contains(raw, []byte("synthetic-consent")) {
		t.Fatal("private consent material not returned")
	}
	if _, err = w.Invoke(context.Background(), "complete", map[string]string{"flow": result.Flow}, nil); err == nil {
		t.Fatal("consent callback replayed")
	}
}
func TestWorkerStreamsBytesWithBoundedChunksAndNoTerminalSynthesis(t *testing.T) {
	body := strings.Repeat("data: synthetic fragment\n\n", 12000)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	defer server.Close()
	w := fixtureWorker(t, fixture)
	var out bytes.Buffer
	_, err := w.Invoke(context.Background(), "fetch", map[string]string{"url": server.URL + "/chat/completions", "body": base64.StdEncoding.EncodeToString([]byte(`{}`))}, eventHandler(t, nil, &out))
	if err != nil || requests.Load() != 1 || out.String() != body {
		t.Fatalf("stream changed/replayed: %v, requests=%d bytes=%d", err, requests.Load(), out.Len())
	}
}
func TestWorkerCancellationPersistsReturnedRotationBeforeStopping(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var inference atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rotate" {
			close(started)
			<-release
			io.WriteString(w, `{"access":"synthetic-rotated"}`)
			return
		}
		inference.Add(1)
		io.WriteString(w, "unexpected inference")
	}))
	defer server.Close()
	w := fixtureWorker(t, fixture)
	var saved atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"rotate": server.URL + "/rotate"})
	done := make(chan error, 1)
	go func() {
		_, err := w.Invoke(ctx, "fetch", map[string]string{"url": server.URL + "/chat/completions", "body": base64.StdEncoding.EncodeToString(payload)}, eventHandler(t, &saved, nil))
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("rotation did not start")
	}
	queued, stopWaiting := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer stopWaiting()
	if _, err := w.Invoke(queued, "models", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cancelled operation kept waiting behind an active worker")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || saved.Load() != 1 || inference.Load() != 0 {
			t.Fatalf("rotation lost or inference sent: err=%v saved=%d inference=%d", err, saved.Load(), inference.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel failed to drain")
	}
}
func TestWorkerCrashIsUnknownAndUnknownSDKDoesNotPretendSuccess(t *testing.T) {
	w := fixtureWorker(t, fixture)
	_, err := w.Invoke(context.Background(), "fetch", map[string]string{"url": "https://example.invalid/chat/completions", "body": base64.StdEncoding.EncodeToString([]byte(`{"sdk":true}`))}, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported plugin SDK") {
		t.Fatal("unknown SDK method pretended success")
	}
	_, err = w.Invoke(context.Background(), "fetch", map[string]string{"url": "https://example.invalid/chat/completions", "body": base64.StdEncoding.EncodeToString([]byte(`{"crash":true}`))}, nil)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("crash was not unknown outcome: %v", err)
	}
}
