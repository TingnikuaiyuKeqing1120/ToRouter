package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

func isolatedServer(t *testing.T) *Server {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	s, err := Start(t.TempDir(), port, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Shutdown(); s.Close() })
	return s
}

func TestRestartKeepsStoreUntilHandlerPersists(t *testing.T) {
	s := isolatedServer(t)
	relaunched := false
	s.relaunch = func(dir string, port int) error {
		relaunched = true
		if port != s.Port {
			t.Error("restart changed an unsaved -port to the default")
		}
		st, err := store.Open(dir)
		if err != nil {
			return err
		}
		defer st.Close()
		rows, err := st.ListUsage(store.UsageFilter{})
		if err != nil || len(rows) != 1 || rows[0].Model != "in-flight" {
			t.Error("relaunch did not observe the in-flight usage in reopened SQLite")
		}
		return nil
	}
	entered, release := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	base := s.http.Handler
	s.http.Handler = s.trackRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/test-write" {
			base.ServeHTTP(w, r)
			return
		}
		close(entered)
		<-release
		writeDone <- s.Store.AddUsage(store.Usage{Model: "in-flight", Status: 200})
		w.Write([]byte("complete"))
	}))
	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()
	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Get(s.Origin() + "/test-write")
		if resp != nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	s.Restart()
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatalf("restart closed Store before the in-flight handler persisted: %v", err)
	}
	<-requestDone
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("restart never completed")
	}
	if !relaunched {
		t.Fatal("restart did not hand over after persisting")
	}
}

func TestForcedShutdownWaitsForFinalWriter(t *testing.T) {
	s := isolatedServer(t)
	s.shutdownGrace = 20 * time.Millisecond
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	written := make(chan error, 1)
	s.http.Handler = s.trackRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
		written <- s.Store.AddUsage(store.Usage{Model: "canceled", Status: 499})
	}))
	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()
	clientDone := make(chan struct{})
	go func() {
		resp, _ := http.Get(s.Origin())
		if resp != nil {
			resp.Body.Close()
		}
		close(clientDone)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	s.Shutdown()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel the request")
	}
	select {
	case err := <-served:
		t.Fatalf("Serve returned before final writer exited: %v", err)
	default:
	}
	if _, err := s.Store.GetSettings(); err != nil {
		t.Fatal("Store closed while canceled handler still needed it")
	}
	finish()
	if err := <-written; err != nil {
		t.Fatalf("canceled handler lost its final usage: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	<-clientDone
}

func TestNonExitingHandlerAbortsRestartWithoutClosingStore(t *testing.T) {
	s := isolatedServer(t)
	s.shutdownGrace, s.cancelWait = 20*time.Millisecond, 30*time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	s.http.Handler = s.trackRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	launched := false
	s.relaunch = func(string, int) error { launched = true; return nil }
	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()
	go func() {
		resp, _ := http.Get(s.Origin())
		if resp != nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	s.Restart()
	if err := <-served; err == nil || !strings.Contains(err.Error(), "Store remains open") {
		t.Fatalf("non-exiting handler was not reported: %v", err)
	}
	if launched {
		t.Fatal("restart launched while old persistence handler remained active")
	}
	if err := s.Close(); err == nil {
		t.Fatal("Close silently closed an active writer's Store")
	}
	if _, err := s.Store.GetSettings(); err != nil {
		t.Fatal("Store closed while a handler was still running")
	}
	finish()
	select {
	case <-s.handlersDone:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not exit after release")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestartUsesSavedPortAndReportsLauncherFailure(t *testing.T) {
	s := isolatedServer(t)
	target := s.Port + 1
	if target > 65535 {
		target = s.Port - 1
	}
	if err := s.Store.SavePort(target); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("deliberate child launch failure")
	s.relaunch = func(dir string, port int) error {
		if port != target {
			t.Error("restart did not use the saved target port")
		}
		if _, err := s.Store.GetSettings(); err == nil {
			t.Error("child launch preceded Store close")
		}
		return failed
	}
	s.Restart()
	if err := s.Serve(context.Background()); !errors.Is(err, failed) {
		t.Fatalf("launcher failure was hidden: %v", err)
	}
	log, err := os.ReadFile(LogPath(s.DataDir))
	if err != nil || !strings.Contains(string(log), "restart failed") {
		t.Fatal("launcher failure left no diagnostic")
	}
}

func TestRestartPersistsReceivedRefreshAndFinalUsage(t *testing.T) {
	for _, name := range []string{"normal_drain", "cancel_after_token_received", "cancel_during_exchange"} {
		t.Run(name, func(t *testing.T) {
			forceCancel, cancelDuring := name != "normal_drain", name == "cancel_during_exchange"
			s := isolatedServer(t)
			if forceCancel {
				s.shutdownGrace = 20 * time.Millisecond
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "old-refresh" {
					t.Error("mock issuer did not receive the old stored refresh token")
					w.WriteHeader(400)
					return
				}
				close(entered)
				<-release
				json.NewEncoder(w).Encode(codex.TokenResponse{AccessToken: "received-access", RefreshToken: "received-refresh", ExpiresIn: 3600})
			}))
			defer issuer.Close()
			defer finish()
			eps := codex.DefaultEndpoints()
			eps.Token = issuer.URL
			svc := codex.NewService(s.Store, issuer.Client(), eps)
			c := codex.ChannelFor(codex.LoginInput{Tokens: codex.TokenResponse{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresIn: 3600}, Account: "mock-account", Models: []string{"m"}})
			c.OAuth.ExpiresAt = time.Now().Add(-time.Minute)
			if err := s.Store.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			tokenReceived := make(chan struct{})
			handlerDone := make(chan error, 1)
			s.http.Handler = s.trackRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token, err := svc.Bearer(r.Context(), c)
				if cancelDuring {
					if !errors.Is(err, context.Canceled) {
						handlerDone <- errors.New("in-flight exchange was not canceled")
						return
					}
					handlerDone <- s.Store.AddUsage(store.Usage{Model: "refreshed", Status: 499})
					return
				}
				if err != nil {
					handlerDone <- err
					return
				}
				if token != "received-access" {
					handlerDone <- errors.New("wrong refreshed Bearer")
					return
				}
				close(tokenReceived)
				if forceCancel {
					<-r.Context().Done()
				}
				handlerDone <- s.Store.AddUsage(store.Usage{Model: "refreshed", Status: 200})
				w.Write([]byte("complete"))
			}))
			s.relaunch = func(dir string, port int) error {
				st, err := store.Open(dir)
				if err != nil {
					return err
				}
				defer st.Close()
				stored, err := st.Channel(c.ID)
				access, refresh := "received-access", "received-refresh"
				if cancelDuring {
					access, refresh = "old-access", "old-refresh"
				}
				if err != nil || stored.OAuth.AccessToken != access || stored.OAuth.RefreshToken != refresh {
					t.Error("received OAuth token was not available after restart")
				}
				rows, err := st.ListUsage(store.UsageFilter{})
				if err != nil || len(rows) != 1 || rows[0].Model != "refreshed" {
					t.Error("final usage was not available after restart")
				}
				return nil
			}
			served := make(chan error, 1)
			go func() { served <- s.Serve(context.Background()) }()
			go func() {
				resp, _ := http.Get(s.Origin())
				if resp != nil {
					resp.Body.Close()
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("rotation did not reach mock issuer")
			}
			if forceCancel && !cancelDuring {
				finish()
				select {
				case <-tokenReceived:
				case <-time.After(3 * time.Second):
					t.Fatal("token was not received")
				}
			}
			s.Restart()
			if !forceCancel {
				finish()
			}
			if err := <-handlerDone; err != nil {
				t.Fatalf("in-flight refresh or final usage failed: %v", err)
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
		})
	}
}
