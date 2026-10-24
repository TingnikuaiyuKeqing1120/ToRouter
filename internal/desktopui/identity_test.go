package desktopui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExistingNativeConnectionRejectsOtherInstallationTakingPort(t *testing.T) {
	var identity atomic.Value
	identity.Store("installation-A")
	var instance atomic.Value
	instance.Store("first-process")
	var bRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_console/ready" {
			if r.Header.Get("Cookie") != "" {
				t.Error("readiness check disclosed session")
			}
			_, port, _ := net.SplitHostPort(r.Host)
			number, _ := strconv.Atoi(port)
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "to-router", "installation_id": identity.Load().(string), "instance_id": instance.Load().(string), "port": number})
			return
		}
		if identity.Load().(string) == "installation-B" {
			bRequests.Add(1)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(target.Port())
	c := newNativeClient(launchConfig{pet: true}, &backendConnection{origin: server.URL, proxy: installationProxy(port, "installation-A", httputil.NewSingleHostReverseProxy(target)), cookie: &http.Cookie{Name: "to_router_session", Value: "A-session"}})
	before := httptest.NewRecorder()
	c.serveAPI(before, httptest.NewRequest("GET", "/api/connection", nil))
	if before.Code != http.StatusAccepted {
		t.Fatalf("A request rejected: %d", before.Code)
	}
	identity.Store("installation-B")
	for _, path := range []string{"/api/auth/local", "/api/settings"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"setting":"A-only"}`))
		req.Header.Set("X-To-Router", "1")
		rec := httptest.NewRecorder()
		c.serveAPI(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("old A window followed B on %s: got %d, want 503", path, rec.Code)
		}
	}
	if bRequests.Load() != 0 {
		t.Fatalf("B received %d sensitive requests from A window", bRequests.Load())
	}
	identity.Store("installation-A")
	instance.Store("restarted-process")
	after := httptest.NewRecorder()
	c.serveAPI(after, httptest.NewRequest("POST", "/api/auth/local", nil))
	if after.Code != http.StatusAccepted {
		t.Fatalf("same installation could not recover: %d", after.Code)
	}
}

func TestExistingNativeConnectionDoesNotFollowReadinessRedirect(t *testing.T) {
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationRequests.Add(1); w.WriteHeader(http.StatusOK) }))
	defer destination.Close()
	var forwarded atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_console/ready" {
			http.Redirect(w, r, destination.URL, http.StatusFound)
			return
		}
		forwarded.Add(1)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(target.Port())
	c := newNativeClient(launchConfig{pet: true}, &backendConnection{origin: server.URL, proxy: installationProxy(port, "installation-A", httputil.NewSingleHostReverseProxy(target)), cookie: &http.Cookie{Name: "to_router_session", Value: "A-session"}})
	rec := httptest.NewRecorder()
	c.serveAPI(rec, httptest.NewRequest("POST", "/api/auth/local", nil))
	if rec.Code != http.StatusServiceUnavailable || destinationRequests.Load() != 0 || forwarded.Load() != 0 {
		t.Fatalf("redirect accepted or request leaked: status=%d destination=%d forwarded=%d", rec.Code, destinationRequests.Load(), forwarded.Load())
	}
}
