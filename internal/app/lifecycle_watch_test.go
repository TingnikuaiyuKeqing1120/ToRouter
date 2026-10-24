package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestLifecycleWatchStopRestartAndSignalReleaseDrain(t *testing.T) {
	for _, action := range []string{"stop", "restart", "signal"} {
		t.Run(action, func(t *testing.T) {
			s := isolatedServer(t)
			installation := s.activeIdentity.Installation
			target := s.Port + 1
			if target > 65535 {
				target = s.Port - 1
			}
			if err := s.Store.SavePort(target); err != nil {
				t.Fatal(err)
			}
			relaunchPort := 0
			s.relaunch = func(_ string, port int) error { relaunchPort = port; return nil }
			entered := make(chan struct{})
			base := s.http.Handler
			s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("watch") == "1" {
					close(entered)
				}
				base.ServeHTTP(w, r)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			served := make(chan error, 1)
			go func() { served <- s.Serve(ctx) }()
			watchCtx, cancelWatch := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelWatch()
			type result struct {
				value DaemonLifecycle
				err   error
			}
			observed := make(chan result, 1)
			go func() {
				value, err := WaitDaemonLifecycle(watchCtx, s.Origin(), installation)
				observed <- result{value, err}
			}()
			select {
			case <-entered:
			case <-watchCtx.Done():
				t.Fatal("watch did not enter")
			}
			if action == "restart" {
				s.Restart()
			} else if action == "signal" {
				cancel()
			} else {
				s.Shutdown()
			}
			got := <-observed
			if got.err != nil {
				t.Fatal(got.err)
			}
			want := "stopped"
			if action == "restart" {
				want = "restarting"
			}
			if got.value.State != want || got.value.InstallationID != installation || got.value.InstanceID != s.activeIdentity.Instance {
				t.Fatalf("lifecycle event: %+v", got.value)
			}
			select {
			case err := <-served:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("lifecycle watch held graceful shutdown open")
			}
			if action == "restart" && (got.value.TargetPort != target || relaunchPort != target) {
				t.Fatalf("watch/relaunch target mismatch: event=%d launched=%d", got.value.TargetPort, relaunchPort)
			}
			if action != "restart" && (got.value.TargetPort != 0 || relaunchPort != 0) {
				t.Fatal("stop unexpectedly requested a restart")
			}
		})
	}
}

func TestLifecycleWatchContextCancellationDoesNotBlockShutdown(t *testing.T) {
	s := isolatedServer(t)
	entered := make(chan struct{})
	base := s.http.Handler
	s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); base.ServeHTTP(w, r) })
	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := WaitDaemonLifecycle(ctx, s.Origin(), s.activeIdentity.Installation); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("watch did not enter")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("watch cancellation: %v", err)
	}
	s.Shutdown()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled watch blocked shutdown")
	}
}

func TestLifecycleWatchRejectsOtherInstallationAndRedirect(t *testing.T) {
	for _, mode := range []string{"other-installation", "redirect", "bad-state", "bad-target"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, "http://evil.example/", http.StatusFound)
					return
				}
				_, rawPort, _ := net.SplitHostPort(r.Host)
				port, _ := strconv.Atoi(rawPort)
				value := DaemonLifecycle{Service: "to-router", State: "stopped", InstallationID: "expected", InstanceID: "instance", Port: port}
				switch mode {
				case "other-installation":
					value.InstallationID = "other"
				case "bad-state":
					value.State = "anything"
				case "bad-target":
					value.State = "restarting"
					value.TargetPort = 65536
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			if _, err := WaitDaemonLifecycle(context.Background(), server.URL, "expected"); err == nil {
				t.Fatal("invalid lifecycle metadata accepted")
			}
		})
	}
	for _, origin := range []string{"http://example.com:19000", "https://localhost:19000", "http://localhost:19000/path", "http://localhost:19000?x=1", "http://user@localhost:19000"} {
		if _, err := WaitDaemonLifecycle(context.Background(), origin, "expected"); err == nil {
			t.Fatalf("invalid origin %s accepted", origin)
		}
	}
}
