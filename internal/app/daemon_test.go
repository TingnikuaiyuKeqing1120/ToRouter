package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"torouter/internal/store"
)

func testServerPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	u, _ := url.Parse(server.URL)
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoveryRejectsOrdinaryHTTPAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(strconv.FormatBool(redirect), func(t *testing.T) {
			var destinationHits atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationHits.Add(1); w.WriteHeader(200) }))
			defer destination.Close()
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, destination.URL, 302)
				} else {
					w.WriteHeader(200)
				}
			}))
			defer fake.Close()
			if IsInstanceRunning(testServerPort(t, fake)) {
				t.Error("ordinary HTTP service accepted as gateway")
			}
			if destinationHits.Load() != 0 {
				t.Error("discovery followed redirect")
			}
		})
	}
}

func TestAutomaticSessionNeverPostsToUnverifiedService(t *testing.T) {
	var posts atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		w.WriteHeader(200)
	}))
	defer fake.Close()
	if cookie := ObtainAdminSessionCookie(t.TempDir(), testServerPort(t, fake)); cookie != nil {
		t.Error("unexpected session")
	}
	if posts.Load() != 0 {
		t.Fatal("automatic authentication posted to an unverified service")
	}
}

func TestEnsureMissingDaemonFails(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	port := testServerPort(t, server)
	server.Close()
	if err := EnsureDaemonRunning(t.TempDir(), port); err == nil {
		t.Fatal("missing daemon reported success")
	}
}

func TestResolvePortPrecedenceAndConcurrentFirstUse(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, err := ResolvePort(dir, 0)
			if err != nil || port != DefaultPort {
				t.Errorf("first use: %d %v", port, err)
			}
		}()
	}
	wg.Wait()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.SavePort(19197); err != nil {
		t.Fatal(err)
	}
	st.Close()
	for _, tc := range []struct{ flag, want int }{{0, 19197}, {19198, 19198}} {
		port, err := ResolvePort(dir, tc.flag)
		if err != nil || port != tc.want {
			t.Errorf("ResolvePort(%d)=%d,%v", tc.flag, port, err)
		}
	}
}

func TestInstallationMatchAndLocalSession(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var ready http.Handler
	var posts atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == consoleReadyPath {
			ready.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/api/auth/local" || r.Method != http.MethodPost || r.Header.Get("X-To-Router") != "1" {
			t.Errorf("unexpected authentication request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		posts.Add(1)
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Error("automatic local authentication sent a request body")
		}
		http.SetCookie(w, &http.Cookie{Name: "to_router_session", Value: "test-session", Path: "/"})
	}))
	port := server.Listener.Addr().(*net.TCPAddr).Port
	ready, err = consoleReadyHandler(st, port)
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	defer server.Close()
	if !IsInstanceRunning(port) {
		t.Fatal("valid gateway not recognized")
	}
	if ok, err := ProbeInstallation(dir, port); !ok || err != nil {
		t.Fatalf("same installation: %v %v", ok, err)
	}
	if cookie := ObtainAdminSessionCookie(dir, port); cookie == nil {
		t.Fatal("local session missing")
	}
	if ok, err := ProbeInstallation(t.TempDir(), port); ok || err == nil {
		t.Fatalf("other installation accepted: %v %v", ok, err)
	}
	if err := EnsureDaemonRunning(t.TempDir(), port); err == nil {
		t.Error("daemon attach accepted other data directory")
	}
	if cookie := ObtainAdminSessionCookie(t.TempDir(), port); cookie != nil {
		t.Fatal("other installation got session")
	}
	if posts.Load() != 1 {
		t.Fatalf("authentication requests = %d", posts.Load())
	}
}

func TestStartupLockAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("TOROUTER_TEST_LOCK_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "child-started"), []byte("started"), 0600); err != nil {
			t.Fatal(err)
		}
		unlock, err := AcquireDaemonStartupLock(dir, 19199)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := os.WriteFile(filepath.Join(dir, "child-acquired"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	unlock, err := AcquireDaemonStartupLock(dir, 19200)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	command := exec.Command(os.Args[0], "-test.run=^TestStartupLockAcrossProcesses$")
	command.Env = append(os.Environ(), "TOROUTER_TEST_LOCK_DIR="+dir)
	var output strings.Builder
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	startedDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "child-started")); err == nil {
			break
		}
		if time.Now().After(startedDeadline) {
			command.Process.Kill()
			command.Wait()
			t.Fatal("child did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "child-acquired")); err == nil {
		t.Error("second process entered startup critical section")
	}
	unlock()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child failed: %v %s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		command.Process.Kill()
		<-done
		t.Fatal("startup lock not released")
	}
	if _, err := os.Stat(filepath.Join(dir, "child-acquired")); err != nil {
		t.Fatal(fmt.Errorf("child never acquired released lock: %w", err))
	}
}
func TestResolvePortKeepsRunningDaemonAfterSavingNextPort(t *testing.T) {
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	server, err := Start(dir, port, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background()) }()
	defer func() { server.Shutdown(); <-done; server.Close() }()
	deadline := time.Now().Add(3 * time.Second)
	for !IsInstanceRunning(port) {
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	next := port + 1
	if next > 65535 {
		next = port - 1
	}
	if err := server.Store.SavePort(next); err != nil {
		t.Fatal(err)
	}
	actual, err := ResolvePort(dir, 0)
	if err != nil || actual != port {
		t.Fatalf("normal attach moved from live port %d to saved %d: got %d, %v", port, next, actual, err)
	}
	connected, err := ConnectDaemon(dir, 0)
	if err != nil || connected != port {
		t.Fatalf("normal client failed to retain running endpoint: %d %v", connected, err)
	}
	explicit, err := ResolvePort(dir, next)
	if err == nil || explicit != 0 || !strings.Contains(err.Error(), "-data") || !strings.Contains(err.Error(), "设置") {
		t.Fatalf("explicit port conflict must require settings restart or different data directory: %d %v", explicit, err)
	}
}

func TestClosingOldDaemonPreservesNewActiveDescriptor(t *testing.T) {
	dir := t.TempDir()
	start := func() (*Server, chan error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		server, err := Start(dir, port, 0)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- server.Serve(context.Background()) }()
		deadline := time.Now().Add(3 * time.Second)
		for !IsInstanceRunning(port) {
			if time.Now().After(deadline) {
				server.Shutdown()
				<-done
				server.Close()
				t.Fatal("server not ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
		return server, done
	}
	old, oldDone := start()
	newer, newDone := start()
	defer func() { newer.Shutdown(); <-newDone; newer.Close() }()
	old.Shutdown()
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	actual, err := ResolvePort(dir, 0)
	if err != nil || actual != newer.Port {
		t.Fatalf("old close removed new active registration: %d %v", actual, err)
	}
}

func TestActiveDescriptorRejectsReusedPort(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	identity, err := st.ConsoleIdentity()
	if err != nil {
		t.Fatal(err)
	}
	var port int
	fake := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"service":"to-router","installation_id":%q,"instance_id":"replacement-instance","port":%d}`, identity, port)
	}))
	port = fake.Listener.Addr().(*net.TCPAddr).Port
	fake.Start()
	defer fake.Close()
	record := &Server{DataDir: dir, activeIdentity: daemonReady{Service: "to-router", Installation: identity, Instance: "original-instance", Port: port}}
	if err := record.publishActiveDaemon(); err != nil {
		t.Fatal(err)
	}
	next := port + 1
	if next > 65535 {
		next = port - 1
	}
	if err := st.SavePort(next); err != nil {
		t.Fatal(err)
	}
	actual, err := ResolvePort(dir, 0)
	if err != nil || actual != next {
		t.Fatalf("stale instance descriptor was reused: %d %v", actual, err)
	}
}
