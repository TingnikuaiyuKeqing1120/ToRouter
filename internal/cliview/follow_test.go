package cliview

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

	"torouter/internal/app"
)

func TestCLIFollowsRestartTargetUntilExplicitStop(t *testing.T) {
	entered, stop := make(chan struct{}), make(chan struct{})
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, rawPort, _ := net.SplitHostPort(r.Host)
		port, _ := strconv.Atoi(rawPort)
		if r.URL.Query().Get("watch") != "1" {
			_ = json.NewEncoder(w).Encode(app.DaemonLifecycle{Service: "to-router", State: "running", InstallationID: "expected", InstanceID: "new", Port: port})
			return
		}
		close(entered)
		select {
		case <-stop:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(app.DaemonLifecycle{Service: "to-router", State: "stopped", InstallationID: "expected", InstanceID: "new", Port: port})
	}))
	defer next.Close()
	_, rawNext, _ := net.SplitHostPort(next.Listener.Addr().String())
	nextPort, _ := strconv.Atoi(rawNext)
	previous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, rawPort, _ := net.SplitHostPort(r.Host)
		port, _ := strconv.Atoi(rawPort)
		_ = json.NewEncoder(w).Encode(app.DaemonLifecycle{Service: "to-router", State: "restarting", InstallationID: "expected", InstanceID: "old", Port: port, TargetPort: nextPort})
	}))
	defer previous.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- followDaemon(ctx, previous.URL, "expected", nil) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("CLI did not follow the new port")
	}
	select {
	case err := <-done:
		t.Fatalf("CLI exited during restart: %v", err)
	default:
	}
	close(stop)
	if err := <-done; !errors.Is(err, errDaemonStopped) {
		t.Fatalf("CLI did not exit on stop: %v", err)
	}
}

func TestCLILifecycleCancelInterruptsWatch(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- followDaemon(ctx, server.URL, "expected", nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("CLI watch did not enter")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CLI cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CLI watch ignored local close")
	}
}

func TestCLIExitsForExecutableUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, rawPort, _ := net.SplitHostPort(r.Host)
		port, _ := strconv.Atoi(rawPort)
		_ = json.NewEncoder(w).Encode(app.DaemonLifecycle{Service: "to-router", State: "updating", InstallationID: "expected", InstanceID: "old", Port: port, TargetPort: port})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := followDaemon(ctx, server.URL, "expected", func() (int, error) { t.Fatal("update must exit without resolving a successor"); return 0, nil })
	if !errors.Is(err, errDaemonStopped) {
		t.Fatalf("CLI update exit: %v", err)
	}
}

func TestCLIRecoversMissedCrossPortRestartWithoutLaunching(t *testing.T) {
	entered, stop := make(chan struct{}), make(chan struct{})
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, rawPort, _ := net.SplitHostPort(r.Host)
		port, _ := strconv.Atoi(rawPort)
		value := app.DaemonLifecycle{Service: "to-router", State: "running", InstallationID: "expected", InstanceID: "new", Port: port}
		if r.URL.Query().Get("watch") == "1" {
			close(entered)
			select {
			case <-stop:
			case <-r.Context().Done():
				return
			}
			value.State = "stopped"
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer next.Close()
	port := next.Listener.Addr().(*net.TCPAddr).Port
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	old.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- followDaemon(ctx, old.URL, "expected", func() (int, error) { return port, nil }) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("CLI did not recover the missed cross-port restart")
	}
	close(stop)
	if err := <-done; !errors.Is(err, errDaemonStopped) {
		t.Fatalf("CLI recovery exit: %v", err)
	}
}
