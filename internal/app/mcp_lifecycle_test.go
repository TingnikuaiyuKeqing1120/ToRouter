package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func mcpLifecycleFixture(t *testing.T, event DaemonLifecycle) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	entered := make(chan struct{})
	var once sync.Once
	server := httptest.NewUnstartedServer(nil)
	port := server.Listener.Addr().(*net.TCPAddr).Port
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != consoleReadyPath {
			t.Error("monitor used a non-readiness path")
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("monitor sent credentials")
		}
		if r.URL.Query().Get("watch") == "1" {
			once.Do(func() { close(entered) })
			value := event
			value.Service, value.Port = "to-router", port
			if value.State == "restarting" || value.State == "updating" {
				value.TargetPort = port
			}
			if value.State == "disconnect" {
				<-r.Context().Done()
				return
			}
			_ = json.NewEncoder(w).Encode(value)
			return
		}
		_ = json.NewEncoder(w).Encode(daemonReady{Service: "to-router", Installation: "expected", Instance: "original", Port: port})
	})
	server.Start()
	t.Cleanup(server.Close)
	return server, entered
}

func TestMCPDaemonContextCancelsForPinnedLifecycleEvents(t *testing.T) {
	for _, state := range []string{"updating", "stopped", "restarting"} {
		t.Run(state, func(t *testing.T) {
			server, _ := mcpLifecycleFixture(t, DaemonLifecycle{State: state, InstallationID: "expected", InstanceID: "original"})
			ctx, cancel := MCPDaemonContext(context.Background(), server.URL+"/mcp")
			defer cancel()
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("bridge context did not cancel for " + state)
			}
		})
	}
}

func TestMCPDaemonContextRejectsForeignLifecycleEvents(t *testing.T) {
	for _, event := range []DaemonLifecycle{
		{State: "updating", InstallationID: "foreign", InstanceID: "original"},
		{State: "stopped", InstallationID: "expected", InstanceID: "other-instance"},
	} {
		server, entered := mcpLifecycleFixture(t, event)
		ctx, cancel := MCPDaemonContext(context.Background(), server.URL+"/mcp")
		select {
		case <-entered:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("lifecycle monitor did not start")
		}
		select {
		case <-ctx.Done():
			cancel()
			t.Fatal("foreign event stopped a healthy pinned instance")
		case <-time.After(250 * time.Millisecond):
		}
		cancel()
	}
}

func TestMCPDaemonContextCancelsWhenPinnedDaemonDisconnects(t *testing.T) {
	server, entered := mcpLifecycleFixture(t, DaemonLifecycle{State: "disconnect", InstallationID: "expected", InstanceID: "original"})
	ctx, cancel := MCPDaemonContext(context.Background(), server.URL+"/mcp")
	defer cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("lifecycle monitor did not start")
	}
	server.CloseClientConnections()
	server.Close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("bridge held context after selected daemon disappeared")
	}
}

func TestMCPDaemonContextCancelsWhenInitialReadinessIsUnavailable(t *testing.T) {
	for _, closed := range []bool{false, true} {
		server := httptest.NewServer(http.NotFoundHandler())
		if closed {
			server.Close()
		} else {
			defer server.Close()
		}
		ctx, cancel := MCPDaemonContext(context.Background(), server.URL+"/mcp")
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			cancel()
			t.Fatal("HTTP bridge remained unmonitored after initial readiness failure")
		}
		cancel()
	}
}
