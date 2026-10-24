package desktopui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v2/pkg/options"
	"torouter/internal/desktop"
)

func TestNewAppOptionsPetMode(t *testing.T) {
	appOpts := newAppOptions(launchConfig{pet: true, port: 17878}, nil)
	if appOpts == nil {
		t.Fatal("expected non-nil options.App")
	}

	if !appOpts.Frameless {
		t.Errorf("expected Frameless = true, got %v", appOpts.Frameless)
	}
	if !appOpts.AlwaysOnTop {
		t.Errorf("expected AlwaysOnTop = true, got %v", appOpts.AlwaysOnTop)
	}
	if !appOpts.DisableResize {
		t.Errorf("expected DisableResize = true, got %v", appOpts.DisableResize)
	}
	if appOpts.Width != 260 || appOpts.Height != 200 {
		t.Errorf("expected 260x200, got %dx%d", appOpts.Width, appOpts.Height)
	}
	if appOpts.BackgroundColour == nil || *appOpts.BackgroundColour != (options.RGBA{R: 0, G: 0, B: 0, A: 0}) {
		t.Errorf("expected transparent BackgroundColour, got %+v", appOpts.BackgroundColour)
	}
	if appOpts.Windows == nil || !appOpts.Windows.WebviewIsTransparent {
		t.Errorf("expected Windows.WebviewIsTransparent = true, got %+v", appOpts.Windows)
	}
	if appOpts.Windows == nil || !appOpts.Windows.DisableFramelessWindowDecorations {
		t.Errorf("expected Windows.DisableFramelessWindowDecorations = true, got %+v", appOpts.Windows)
	}
	if appOpts.Windows == nil || !appOpts.Windows.WindowIsTranslucent {
		t.Errorf("expected Windows.WindowIsTranslucent = true, got %+v", appOpts.Windows)
	}
	if appOpts.Title != desktop.PetWindowTitle("http://127.0.0.1:17878", "") {
		t.Errorf("expected title to-router 桌面伴侣, got %s", appOpts.Title)
	}
}

func TestNewAppOptionsConsoleMode(t *testing.T) {
	appOpts := newAppOptions(launchConfig{port: 17878}, nil)
	if appOpts == nil {
		t.Fatal("expected non-nil options.App")
	}

	if appOpts.Frameless {
		t.Errorf("expected Frameless = false for full console, got %v", appOpts.Frameless)
	}
	if appOpts.AlwaysOnTop {
		t.Errorf("expected AlwaysOnTop = false for full console, got %v", appOpts.AlwaysOnTop)
	}
	if appOpts.DisableResize {
		t.Errorf("expected DisableResize = false for full console, got %v", appOpts.DisableResize)
	}
	if appOpts.Width != 1200 || appOpts.Height != 800 {
		t.Errorf("expected 1200x800, got %dx%d", appOpts.Width, appOpts.Height)
	}
	if appOpts.BackgroundColour == nil || *appOpts.BackgroundColour != (options.RGBA{R: 10, G: 13, B: 20, A: 255}) {
		t.Errorf("expected Obsidian BackgroundColour, got %+v", appOpts.BackgroundColour)
	}
	if appOpts.Title != "to-router 控制台" {
		t.Errorf("expected title to-router 控制台, got %s", appOpts.Title)
	}
}

func TestAssetMiddlewareInjectsRoutePerMode(t *testing.T) {
	sessionCookie := &http.Cookie{
		Name:     "to_router_session",
		Value:    "test_token_123",
		Path:     "/api",
		HttpOnly: true,
	}

	// 1. Pet Mode injects pet redirect and pet-mode class
	petApp := newAppOptions(launchConfig{pet: true, port: 17878}, &backendConnection{cookie: sessionCookie, origin: "http://127.0.0.1:17878"})
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	hPet := petApp.AssetServer.Middleware(dummyHandler)
	req := httptest.NewRequest("GET", "/index.html", nil)
	rec := httptest.NewRecorder()
	hPet.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", res.StatusCode)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "window.location.hash='#/pet'") || !strings.Contains(body, "pet-mode") {
		t.Errorf("expected injected pet hash redirect and pet-mode class, got: %s", body)
	}

	// Verify cookie is set on index.html response
	cookies := res.Cookies()
	var found bool
	for _, c := range cookies {
		if c.Name == "to_router_session" && c.Value == "test_token_123" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected to_router_session cookie on index.html response")
	}

	// 2. Console Mode does not inject pet redirect
	consoleApp := newAppOptions(launchConfig{port: 17878}, &backendConnection{cookie: sessionCookie, origin: "http://127.0.0.1:17878"})
	hConsole := consoleApp.AssetServer.Middleware(dummyHandler)
	rec2 := httptest.NewRecorder()
	hConsole.ServeHTTP(rec2, req)
	body2 := rec2.Body.String()
	if strings.Contains(body2, "window.location.hash='#/pet'") {
		t.Errorf("console mode should not inject pet redirect, got: %s", body2)
	}
}

func TestDesktopCommandEndpoints(t *testing.T) {
	var commands []string
	backend := &backendConnection{origin: "http://127.0.0.1:3823", proxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commands = append(commands, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})}
	client := newNativeClient(launchConfig{pet: true, port: 3823}, backend)
	close(client.ready)
	resized := 0
	client.resize = func(height int) error { resized = height; return nil }
	handler := client.options().AssetServer.Handler
	for _, path := range []string{"launch-pet", "launch-gui"} {
		req := httptest.NewRequest("POST", "/api/desktop/"+path, nil)
		req.Header.Set("X-To-Router", "1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if len(commands) != 2 {
		t.Fatalf("commands bypassed backend: %v", commands)
	}
	req := httptest.NewRequest("POST", "/api/desktop/resize-pet", strings.NewReader(`{"height":350}`))
	req.Header.Set("X-To-Router", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("resize: %d %s", rec.Code, rec.Body.String())
	}
	if resized != 350 {
		t.Fatalf("resize did not reach window: %d", resized)
	}
}
