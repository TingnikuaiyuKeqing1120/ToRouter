package pluginhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/marketprofile"
)

// MarketBridge runs unmodified OpenCode/Magpie supplier packages under Bun.
// Like the featured host, it is trusted installed code, not an OS sandbox.
//
//go:embed market.mjs
var MarketBridge []byte

func marketBridgePath() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", errors.New("cannot locate market host cache")
	}
	sum := sha256.Sum256(MarketBridge)
	dir := filepath.Join(root, "to-router", "market-host", hex.EncodeToString(sum[:]))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", errors.New("cannot prepare market host")
	}
	path := filepath.Join(dir, "market.mjs")
	if raw, e := os.ReadFile(path); e == nil && bytes.Equal(raw, MarketBridge) {
		return path, nil
	}
	f, err := os.CreateTemp(dir, "bridge-*.mjs")
	if err != nil {
		return "", errors.New("cannot write market host")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(MarketBridge)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return "", errors.New("cannot write market host")
	}
	if err = os.Rename(name, path); err != nil {
		// Another launch may have published exactly the same content.
		if raw, e := os.ReadFile(path); e != nil || !bytes.Equal(raw, MarketBridge) {
			return "", errors.New("cannot publish market host")
		}
	}
	return path, nil
}

func startMarket(ctx context.Context, runtimePath, modulePath, providerID string, auth json.RawMessage, handlers ...Handler) (*Worker, []marketprofile.Provider, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	bridge, err := marketBridgePath()
	if err != nil {
		return nil, nil, err
	}
	w, err := Start(runtimePath, bridge)
	if err != nil {
		return nil, nil, err
	}
	handler := Handler(func(ctx context.Context, event Event) (json.RawMessage, error) {
		if event.Name != "network" {
			return nil, errors.New("supplier initialization cannot persist an unbound account")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return json.RawMessage(`true`), accessguard.Check(ctx)
	})
	if len(handlers) > 0 && handlers[0] != nil {
		handler = handlers[0]
	}
	type initialization struct {
		result json.RawMessage
		err    error
	}
	done := make(chan initialization, 1)
	go func() {
		result, err := w.Invoke(ctx, "init", map[string]any{"module_path": modulePath, "provider_id": providerID, "auth": auth}, handler)
		done <- initialization{result, err}
	}()
	var result json.RawMessage
	select {
	case <-ctx.Done():
		w.Close()
		return nil, nil, ctx.Err()
	case reply := <-done:
		result, err = reply.result, reply.err
	}
	if err != nil {
		w.Close()
		return nil, nil, err
	}
	var providers []marketprofile.Provider
	if json.Unmarshal(result, &providers) != nil || len(providers) == 0 {
		w.Close()
		return nil, nil, errors.New("plugin exposes no supplier")
	}
	return w, providers, nil
}

// StartMarket initializes one worker for one selected supplier/account.
// Consent is supplied privately over stdin. Subsequent SDK auth.set events use
// the same durable auth ACK contract as the featured worker.
func StartMarket(ctx context.Context, runtimePath, modulePath, providerID string, auth json.RawMessage, handlers ...Handler) (*Worker, error) {
	w, _, err := startMarket(ctx, runtimePath, modulePath, providerID, auth, handlers...)
	return w, err
}

// InspectMarket checks actual exported supplier hooks without a signed-in
// account. Package/config hooks may query public catalogues; auth-dependent
// model hooks are not called and unbound auth writes are rejected.
func InspectMarket(ctx context.Context, runtimePath, modulePath string) ([]marketprofile.Provider, error) {
	w, providers, err := startMarket(ctx, runtimePath, modulePath, "", nil)
	if err != nil {
		return nil, err
	}
	defer w.Close()
	return providers, nil
}
