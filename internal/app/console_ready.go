package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
	"torouter/internal/buildinfo"
	"torouter/internal/store"
)

const consoleReadyPath = "/_console/ready"

// This public loopback-only observation contains no account or settings data.
// A different local port may read it so the old page can observe a completed
// restart. It never grants cross-origin access to the management API.
func localConsoleProbe(r *http.Request) bool {
	if r.URL.Path != consoleReadyPath || r.Method != http.MethodGet {
		return false
	}
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"
}

func consoleReadyHandler(st *store.Store, port int) (http.Handler, error) {
	handler, _, err := consoleReadyHandlerWithIdentity(st, port)
	return handler, err
}

func consoleReadyHandlerWithIdentity(st *store.Store, port int) (http.Handler, daemonReady, error) {
	h, identity, _, err := consoleReadyHandlerWithLifecycle(st, port)
	return h, identity, err
}

func consoleReadyHandlerWithLifecycle(st *store.Store, port int) (http.Handler, daemonReady, *daemonLifecycle, error) {
	installation, err := st.ConsoleIdentity()
	if err != nil {
		return nil, daemonReady{}, nil, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, daemonReady{}, nil, err
	}
	instance := hex.EncodeToString(random[:])
	identity := daemonReady{Service: "to-router", Installation: installation, Instance: instance, Port: port, Version: buildinfo.Version}
	lifecycle := newDaemonLifecycle(identity)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if localConsoleProbe(r) {
			w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "1" {
			timer := time.NewTimer(25 * time.Second)
			defer timer.Stop()
			select {
			case <-lifecycle.done:
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(lifecycle.snapshot())
			return
		}
		_ = json.NewEncoder(w).Encode(identity)
	}), identity, lifecycle, nil
}
