package console

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/auth"
	"torouter/internal/management"
	"torouter/internal/store"
	"torouter/internal/updater"
)

func TestAppUpdatesCheckCloseConnection(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := New(st, "http://127.0.0.1:19000", Hooks{CheckUpdate: func(context.Context) (updater.Status, error) {
		return updater.Status{}, errors.New("invalid release signature")
	}})
	defer ReleaseOAuthListeners(h)
	server := httptest.NewServer(h)
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/app-updates/check", strings.NewReader(`{}`))
	req.Close = true
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "invalid release signature") {
		t.Fatalf("check response incomplete: status=%d body=%q err=%v", response.StatusCode, body, err)
	}
}

func TestAppUpdatesHTTPPreparationAndActivation(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var activated bool
	var response *httptest.ResponseRecorder
	h := New(s, "http://127.0.0.1:19000", Hooks{
		UpdateStatus: func() updater.Status {
			return updater.Status{Enabled: true, CurrentVersion: "1.0.0", State: "available", Available: &updater.Release{Version: "1.1.0", SHA256: strings.Repeat("a", 64)}}
		},
		PrepareUpdate: func(ctx context.Context, version, hash, mode string) (UpdateAccepted, func(), error) {
			if version == "1.2.0" {
				return UpdateAccepted{}, nil, errors.New("download cancelled")
			}
			if version != "1.1.0" || mode != "desktop" {
				t.Fatal("selected version/view lost")
			}
			return UpdateAccepted{Requested: true, Port: 19000, Version: version}, func() {
				if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"requested":true`) {
					t.Fatal("activated before writing acknowledgment")
				}
				activated = true
			}, nil
		},
	})
	defer ReleaseOAuthListeners(h)
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"version":"1.1.0","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","open_mode":"all"}`, 400},
		{`{"version":"1.2.0","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","open_mode":"desktop"}`, 409},
		{`{"version":"1.1.0","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","open_mode":"desktop"}`, 202},
	} {
		activated = false
		response = httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest("POST", "/app-updates/install", strings.NewReader(tc.body)))
		if response.Code != tc.code || activated != (tc.code == 202) {
			t.Fatalf("response=%d activated=%v", response.Code, activated)
		}
	}
}

func TestAppUpdatesMCPMaintenanceAndPreview(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	var calls atomic.Int32
	activated := make(chan struct{}, 2)
	ui := New(st, origin, Hooks{
		UpdateStatus: func() updater.Status {
			return updater.Status{Enabled: true, CurrentVersion: "1.0.0", State: "available", Available: &updater.Release{Version: "1.1.0", SHA256: strings.Repeat("a", 64), Notes: "Synthetic release"}}
		},
		PrepareUpdate: func(_ context.Context, version, hash, mode string) (UpdateAccepted, func(), error) {
			return UpdateAccepted{Requested: true, Version: version, Port: 19000}, func() { calls.Add(1); activated <- struct{}{} }, nil
		},
	})
	defer ReleaseOAuthListeners(ui)
	mux := http.NewServeMux()
	mux.Handle("/mcp", ManagementHandler(ui))
	mux.Handle("/api/", auth.New(st).RequireAdmin(http.StripPrefix("/api", ui)))
	server.Config.Handler = mux
	server.Start()
	defer server.Close()
	h := &dataHarness{store: st, http: server}
	reader, _ := h.client(t, management.Read)
	manager, _ := h.client(t, management.Read, management.Manage)
	maintainer, _ := h.client(t, management.Read, management.Maintain)
	dataCall(t, reader, "get_app_updates", map[string]any{})
	dataDenied(t, manager, "install_app_update", dataWrite(map[string]any{"version": "1.1.0", "sha256": strings.Repeat("a", 64)}))
	preview := dataSuccess(t, maintainer, "install_app_update", dataWrite(map[string]any{"version": "1.1.0", "sha256": strings.Repeat("a", 64)}))
	if calls.Load() != 0 {
		t.Fatal("preview installed executable")
	}
	args := dataWrite(map[string]any{"version": "1.1.0", "sha256": strings.Repeat("a", 64), "preview_id": preview["preview_id"]})
	dataSuccess(t, maintainer, "install_app_update", args)
	select {
	case <-activated:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted maintenance did not activate")
	}
	if calls.Load() != 1 {
		t.Fatalf("activation count=%d", calls.Load())
	}
	dataSuccess(t, maintainer, "install_app_update", args)
	if calls.Load() != 1 {
		t.Fatal("receipt replay activated a second update")
	}
}

func TestLaunchPreferenceMCPUsesManageAndVersion(t *testing.T) {
	h, _ := fullManagementHarness(t)
	reader, _ := h.client(t, management.Read)
	manager, _ := h.client(t, management.Read, management.Manage)
	before := dataCall(t, reader, "get_settings", map[string]any{})
	args := dataWrite(map[string]any{"expected_version": before["version"], "patch": map[string]any{"default_launch_mode": "desktop"}})
	dataDenied(t, reader, "update_settings", args)
	got := dataSuccess(t, manager, "update_settings", args)
	if got["settings"].(map[string]any)["default_launch_mode"] != "desktop" {
		t.Fatal("launch preference not reflected in settings")
	}
}
