package desktopui

import (
	"context"
	"net"
	"testing"
	"time"

	"torouter/internal/app"
)

func TestNativeFollowersQuitWithRealBackendStop(t *testing.T) {
	for _, pet := range []bool{false, true} {
		t.Run(map[bool]string{false: "GUI", true: "pet"}[pet], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			dir := t.TempDir()
			server, err := app.Start(dir, port, 0)
			if err != nil {
				t.Fatal(err)
			}
			serverCtx, cancelServer := context.WithCancel(context.Background())
			defer cancelServer()
			defer server.Close()
			doneServer := make(chan error, 1)
			go func() { doneServer <- server.Serve(serverCtx) }()
			identity, err := app.InstallationID(dir)
			if err != nil {
				t.Fatal(err)
			}
			c := newNativeClient(launchConfig{pet: pet, dataDir: dir, port: port}, &backendConnection{origin: server.Origin(), port: port, installationID: identity})
			closed := make(chan struct{}, 1)
			c.quit = func() { closed <- struct{}{} }
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			defer cancelWatch()
			watchReady := make(chan struct{})
			wait := c.waitLifecycle
			c.waitLifecycle = func(ctx context.Context, origin, id string) (app.DaemonLifecycle, error) {
				close(watchReady)
				return wait(ctx, origin, id)
			}
			go c.followBackend(watchCtx)
			<-watchReady
			// Let the real long-poll request enter the handler before stopping.
			time.Sleep(50 * time.Millisecond)
			server.Shutdown()
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("backend stop did not close native follower")
			}
			select {
			case err := <-doneServer:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("follower kept backend drain blocked")
			}
		})
	}
}

func TestNativeFollowersExitForExecutableUpdate(t *testing.T) {
	for _, pet := range []bool{false, true} {
		c := newNativeClient(launchConfig{pet: pet}, &backendConnection{origin: "http://127.0.0.1:17878", installationID: "test-installation"})
		c.waitLifecycle = func(context.Context, string, string) (app.DaemonLifecycle, error) {
			return app.DaemonLifecycle{State: "updating"}, nil
		}
		closed := false
		c.quit = func() { closed = true }
		c.connect = func(launchConfig) (*backendConnection, error) {
			t.Fatal("update must exit without reconnecting the old executable")
			return nil, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c.followBackend(ctx)
		cancel()
		if !closed {
			t.Fatalf("pet=%v: update left executable view alive", pet)
		}
	}
}
