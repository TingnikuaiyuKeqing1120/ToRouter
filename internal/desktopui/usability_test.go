package desktopui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/wailsapp/wails/v2/pkg/options"
)

func TestGUIAndPetHaveIndependentSingletons(t *testing.T) {
	dir := t.TempDir()
	gui := newAppOptions(launchConfig{dataDir: dir, port: 17878}, nil)
	pet := newAppOptions(launchConfig{dataDir: dir, port: 17878, pet: true}, nil)
	if gui.SingleInstanceLock == nil || pet.SingleInstanceLock == nil {
		t.Fatal("both GUI and pet must be single instance")
	}
	if gui.SingleInstanceLock.UniqueId == pet.SingleInstanceLock.UniqueId {
		t.Fatal("GUI and pet cannot share singleton")
	}
	if gui.SingleInstanceLock.UniqueId != newAppOptions(launchConfig{dataDir: dir, port: 17879}, nil).SingleInstanceLock.UniqueId {
		t.Fatal("port change must recall existing GUI")
	}
	if !pet.StartHidden {
		t.Fatal("pet must receive toolwindow style before becoming visible")
	}
	if gui.StartHidden {
		t.Fatal("GUI should retain normal window visibility")
	}
}

func TestDuplicateLaunchRecoversExpiredSessionAndRejectsOtherMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_console/ready" {
			u, _ := url.Parse("http://" + r.Host)
			port, _ := strconv.Atoi(u.Port())
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "to-router", "installation_id": "A", "instance_id": "restarted", "port": port})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	for _, pet := range []bool{false, true} {
		cfg := launchConfig{dataDir: t.TempDir(), port: port, pet: pet}
		old := &backendConnection{origin: server.URL, port: port, installationID: "A", cookie: &http.Cookie{Name: "to_router_session", Value: "expired"}}
		updated := &backendConnection{origin: server.URL, port: port, installationID: "A", cookie: &http.Cookie{Name: "to_router_session", Value: "new"}}
		c := newNativeClient(cfg, old)
		close(c.ready)
		connects := 0
		c.connect = func(launchConfig) (*backendConnection, error) { connects++; return updated, nil }
		reloads := 0
		c.activate = func(_ string, reload bool) {
			if reload {
				reloads++
			}
		}
		if err := c.secondLaunch(options.SecondInstanceData{Args: cfg.args()}); err != nil {
			t.Fatal(err)
		}
		if connects != 1 || reloads != 1 || c.snapshot() != updated {
			t.Fatal("expired session was not replaced and restored")
		}
		other := cfg
		other.pet = !pet
		if err := c.secondLaunch(options.SecondInstanceData{Args: other.args()}); err == nil {
			t.Fatal("GUI and pet callback modes mixed")
		}
	}
}

func TestDuplicateDoesNotSendExistingSessionToOtherInstallation(t *testing.T) {
	var sessionRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_console/ready" {
			u, _ := url.Parse("http://" + r.Host)
			port, _ := strconv.Atoi(u.Port())
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "to-router", "installation_id": "B", "instance_id": "foreign", "port": port})
			return
		}
		sessionRequests.Add(1)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	cfg := launchConfig{dataDir: t.TempDir(), port: port}
	old := &backendConnection{origin: server.URL, port: port, installationID: "A", cookie: &http.Cookie{Name: "to_router_session", Value: "private"}}
	c := newNativeClient(cfg, old)
	c.connect = func(launchConfig) (*backendConnection, error) { return nil, errors.New("other installation") }
	c.activate = func(string, bool) { t.Fatal("foreign installation activated") }
	if err := c.secondLaunch(options.SecondInstanceData{Args: cfg.args()}); err == nil {
		t.Fatal("other installation accepted")
	}
	if sessionRequests.Load() != 0 {
		t.Fatal("old session disclosed to other installation")
	}
}

func TestDuplicateLaunchPreservesViewWithLiveSession(t *testing.T) {
	for _, pet := range []bool{true, false} {
		t.Run(strconv.FormatBool(pet), func(t *testing.T) {
			var sessionChecks atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/_console/ready":
					if r.Header.Get("Cookie") != "" {
						t.Error("readiness disclosed session")
					}
					address, _ := url.Parse("http://" + r.Host)
					port, _ := strconv.Atoi(address.Port())
					_ = json.NewEncoder(w).Encode(map[string]any{"service": "to-router", "installation_id": "A", "instance_id": "running", "port": port})
				case "/api/auth/session":
					cookie, err := r.Cookie("to_router_session")
					if err != nil || cookie.Value != "existing" {
						t.Error("did not validate existing session")
					}
					sessionChecks.Add(1)
					w.WriteHeader(200)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			port, _ := strconv.Atoi(target.Port())
			cfg := launchConfig{dataDir: t.TempDir(), port: port, pet: pet}
			original := &backendConnection{origin: server.URL, port: port, installationID: "A", cookie: &http.Cookie{Name: "to_router_session", Value: "existing"}}
			c := newNativeClient(cfg, original)
			close(c.ready)
			connected := 0
			c.connect = func(launchConfig) (*backendConnection, error) { connected++; return original, nil }
			recalled := false
			c.activate = func(title string, reload bool) {
				recalled = true
				if reload {
					t.Error("duplicate launch would discard the current view and unsaved edits")
				}
				if !pet && title != "to-router 控制台" {
					t.Errorf("GUI got pet title %q", title)
				}
			}
			if err := c.secondLaunch(options.SecondInstanceData{Args: cfg.args()}); err != nil {
				t.Fatal(err)
			}
			if !recalled || connected != 0 || sessionChecks.Load() != 1 || c.snapshot() != original {
				t.Fatalf("duplicate did not reuse live session: recalled=%v connects=%d checks=%d", recalled, connected, sessionChecks.Load())
			}
		})
	}
}
