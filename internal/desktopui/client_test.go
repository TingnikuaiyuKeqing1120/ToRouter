package desktopui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options"
)

func TestResizeValidationAndWindowErrors(t *testing.T) {
	for _, tc := range []struct {
		name, method, body, header string
		pet, ready, fail           bool
		want                       int
	}{
		{"lower boundary", "POST", `{"height":200}`, "1", true, true, false, 200},
		{"upper boundary", "POST", `{"height":700}`, "1", true, true, false, 200},
		{"below minimum", "POST", `{"height":199}`, "1", true, true, false, 400},
		{"above maximum", "POST", `{"height":701}`, "1", true, true, false, 400},
		{"fractional", "POST", `{"height":220.5}`, "1", true, true, false, 400},
		{"missing", "POST", `{}`, "1", true, true, false, 400},
		{"extra body", "POST", `{"height":220} {}`, "1", true, true, false, 400},
		{"extra field", "POST", `{"height":220,"width":500}`, "1", true, true, false, 400},
		{"malformed", "POST", `bad`, "1", true, true, false, 400},
		{"missing header", "POST", `{"height":220}`, "", true, true, false, 403},
		{"console", "POST", `{"height":220}`, "1", false, true, false, 409},
		{"not ready", "POST", `{"height":220}`, "1", true, false, false, 503},
		{"native error", "POST", `{"height":220}`, "1", true, true, true, 500},
		{"get", "GET", ``, "1", true, true, false, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newNativeClient(launchConfig{pet: tc.pet}, nil)
			if tc.ready {
				close(c.ready)
			}
			calls := 0
			c.resize = func(height int) error {
				calls++
				if tc.fail {
					return errors.New("window gone")
				}
				return nil
			}
			req := httptest.NewRequest(tc.method, "/api/desktop/resize-pet", strings.NewReader(tc.body))
			req.Header.Set("X-To-Router", tc.header)
			rec := httptest.NewRecorder()
			c.serveAPI(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			wantCalls := 0
			if tc.want == 200 || tc.fail {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("window called %d times, want %d", calls, wantCalls)
			}
		})
	}
}

func TestPetAnchorClampRestorationAndDragging(t *testing.T) {
	work := windowRect{-1920, -1080, 0, 0}
	initial := windowRect{-1000, -1050, -740, -850}
	var anchor petAnchor
	expanded := anchor.resize(initial, work, 260, 350)
	if expanded.top != -1080 {
		t.Fatalf("negative monitor top lost: %+v", expanded)
	}
	shrunk := anchor.resize(expanded, work, 260, 200)
	if shrunk != initial {
		t.Fatalf("clamp caused drift: got %+v, want %+v", shrunk, initial)
	}
	expanded = anchor.resize(shrunk, work, 260, 500)
	expanded.top += 80
	expanded.bottom += 80
	expanded.left += 30
	expanded.right += 30
	shrunk = anchor.resize(expanded, work, 260, 200)
	if shrunk.top != initial.top+80 || shrunk.left != initial.left+30 {
		t.Fatalf("expanded drag lost: %+v", shrunk)
	}
}

func TestPetAnchorKeepsBottomForContentSizes(t *testing.T) {
	current := windowRect{300, 600, 560, 800}
	work := windowRect{0, 0, 1920, 1040}
	var anchor petAnchor
	for _, height := range []int{220, 350, 540, 700, 200, 200, 350, 200} {
		current = anchor.resize(current, work, 260, height)
		if current.bottom != 800 || current.bottom-current.top != height {
			t.Fatalf("height %d: %+v", height, current)
		}
	}
}

func TestLaunchContextAndSingletonIdentity(t *testing.T) {
	root := t.TempDir()
	cfg, err := parseLaunch([]string{"--pet", "-data", "one/../profile", "-port", "3823"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dataDir != filepath.Join(root, "profile") || !cfg.pet || cfg.port != 3823 {
		t.Fatalf("context lost: %+v", cfg)
	}
	again, err := parseLaunch(cfg.args(), t.TempDir())
	if err != nil || again != cfg {
		t.Fatalf("forwarded args lost context: %+v %v", again, err)
	}
	first := newAppOptions(cfg, nil)
	cfg.port = 3824
	second := newAppOptions(cfg, nil)
	if first.SingleInstanceLock == nil || first.SingleInstanceLock.UniqueId != second.SingleInstanceLock.UniqueId {
		t.Fatal("port change created new singleton")
	}
	cfg.pet = false
	if gui := newAppOptions(cfg, nil).SingleInstanceLock; gui == nil || gui.UniqueId == first.SingleInstanceLock.UniqueId {
		t.Fatal("console must have its own independent singleton")
	}
	for _, args := range [][]string{{"-port", "-1"}, {"-port", "65536"}, {"--pet", "surprise"}, {"--unknown"}} {
		if _, err := parseLaunch(args, root); err == nil {
			t.Fatalf("accepted malformed args %v", args)
		}
	}
	if singletonID(root) == singletonID(filepath.Join(root, "other")) {
		t.Fatal("different installations share pet")
	}
}

func TestSecondLaunchRetargetsProxyCookieAndBootstrap(t *testing.T) {
	var requestsMu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("to_router_session")
		if err != nil || cookie.Value != "new-session" {
			t.Errorf("missing retarget session: %v", err)
		}
		requestsMu.Lock()
		seen = append(seen, r.URL.Path)
		requestsMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	cfg := launchConfig{dataDir: t.TempDir(), port: 3823, pet: true}
	c := newNativeClient(cfg, &backendConnection{origin: cfg.origin(), proxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("old proxy used after retarget") })})
	close(c.ready)
	c.connect = func(next launchConfig) (*backendConnection, error) {
		if next.dataDir != cfg.dataDir || next.port != 3824 {
			t.Errorf("incorrect second launch: %+v", next)
		}
		return &backendConnection{origin: next.origin(), proxy: httputil.NewSingleHostReverseProxy(target), cookie: &http.Cookie{Name: "to_router_session", Value: "new-session", Path: "/api"}}, nil
	}
	activated := make(chan struct{}, 1)
	c.activate = func(title string, reload bool) {
		if !reload || !strings.Contains(title, "3824") {
			t.Errorf("wrong activation %q %v", title, reload)
		}
		activated <- struct{}{}
	}
	cfg.port = 3824
	// Exercise the actual callback installed in Wails, not just a helper.
	c.options().SingleInstanceLock.OnSecondInstanceLaunch(options.SecondInstanceData{Args: cfg.args()})
	select {
	case <-activated:
	case <-time.After(2 * time.Second):
		t.Fatal("pet not reactivated")
	}
	req := httptest.NewRequest("POST", "/api/desktop/launch-gui", nil)
	req.Header.Set("Origin", "http://wails.localhost")
	req.Header.Set("X-To-Router", "1")
	rec := httptest.NewRecorder()
	c.serveAPI(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("new backend response not forwarded: %d", rec.Code)
	}
	requestsMu.Lock()
	count := len(seen)
	requestsMu.Unlock()
	if count != 1 {
		t.Fatalf("requests=%d", count)
	}
	page := httptest.NewRecorder()
	c.middleware(http.NotFoundHandler()).ServeHTTP(page, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(page.Body.String(), `"origin":"http://127.0.0.1:3824"`) || !strings.Contains(page.Body.String(), "writable:false,configurable:false") || !strings.Contains(page.Body.String(), `"pet":true`) {
		t.Fatal("bootstrap marker not updated/readonly")
	}
	found := false
	for _, cookie := range page.Result().Cookies() {
		if cookie.Value == "new-session" {
			found = true
		}
	}
	if !found {
		t.Fatal("reloaded page retained old session")
	}
}

func TestSecondLaunchFailureKeepsCurrentBackend(t *testing.T) {
	cfg := launchConfig{dataDir: t.TempDir(), port: 3823, pet: true}
	original := &backendConnection{origin: cfg.origin()}
	c := newNativeClient(cfg, original)
	c.connect = func(launchConfig) (*backendConnection, error) { return nil, errors.New("foreign service") }
	c.activate = func(string, bool) { t.Error("failed launch reactivated with invalid backend") }
	if err := c.secondLaunch(options.SecondInstanceData{Args: cfg.args()}); err == nil {
		t.Fatal("failure ignored")
	}
	if c.snapshot() != original {
		t.Fatal("failed retarget replaced valid backend")
	}
	other := cfg
	other.dataDir = t.TempDir()
	if err := c.secondLaunch(options.SecondInstanceData{Args: other.args()}); err == nil {
		t.Fatal("other installation accepted")
	}
}

func TestNativeCommandsPreserveBackendRejection(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusServiceUnavailable} {
		c := newNativeClient(launchConfig{pet: true}, &backendConnection{origin: "http://127.0.0.1:3823", proxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-To-Router") != "" {
				t.Error("native handler manufactured CSRF authorization")
			}
			w.WriteHeader(status)
		})})
		for _, path := range []string{"launch-pet", "launch-gui"} {
			rec := httptest.NewRecorder()
			c.serveAPI(rec, httptest.NewRequest("POST", "/api/desktop/"+path, nil))
			if rec.Code != status {
				t.Fatalf("%s bypassed backend rejection: got %d, want %d", path, rec.Code, status)
			}
		}
	}
}

func TestConcurrentRetargetAndRequestsUseWholeConnection(t *testing.T) {
	cfg := launchConfig{dataDir: t.TempDir(), port: 3823, pet: true}
	makeBackend := func(next launchConfig) *backendConnection {
		origin := next.origin()
		return &backendConnection{origin: origin, cookie: &http.Cookie{Name: "to_router_session", Value: origin}, proxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie("to_router_session")
			if err != nil || cookie.Value != origin || "http://"+r.Host != origin {
				t.Errorf("mixed backend target/session: host=%q err=%v", r.Host, err)
			}
		})}
	}
	c := newNativeClient(cfg, makeBackend(cfg))
	close(c.ready)
	c.connect = func(next launchConfig) (*backendConnection, error) { return makeBackend(next), nil }
	c.activate = func(string, bool) {}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			c.serveAPI(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/connection", nil))
		}
	}()
	for i := 0; i < 20; i++ {
		next := cfg
		next.port += i % 2
		if err := c.secondLaunch(options.SecondInstanceData{Args: next.args()}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestSecondInstanceCallbackDoesNotBlockWailsMessageLoop(t *testing.T) {
	cfg := launchConfig{dataDir: t.TempDir(), port: 3823, pet: true}
	c := newNativeClient(cfg, nil)
	close(c.ready)
	connecting := make(chan struct{}, 1)
	resume := make(chan struct{})
	activated := make(chan string, 4)
	c.connect = func(next launchConfig) (*backendConnection, error) {
		if next.port == 3823 {
			connecting <- struct{}{}
			<-resume
		}
		return &backendConnection{origin: next.origin()}, nil
	}
	c.activate = func(title string, _ bool) { activated <- title }
	callback := c.options().SingleInstanceLock.OnSecondInstanceLaunch
	callback(options.SecondInstanceData{Args: cfg.args()})
	select {
	case <-connecting:
	case <-time.After(2 * time.Second):
		t.Fatal("first launch did not start")
	}
	dispatched := make(chan struct{})
	go func() {
		for port := 3824; port <= 3826; port++ {
			next := cfg
			next.port = port
			callback(options.SecondInstanceData{Args: next.args()})
		}
		close(dispatched)
	}()
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		close(resume)
		t.Fatal("notification callback blocked behind backend/UI work")
	}
	close(resume)
	for _, port := range []string{"3823", "3824", "3825", "3826"} {
		select {
		case title := <-activated:
			if !strings.Contains(title, ":"+port) {
				t.Fatalf("launch order changed: wanted %s, got %s", port, title)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("launch queue stopped")
		}
	}
}
