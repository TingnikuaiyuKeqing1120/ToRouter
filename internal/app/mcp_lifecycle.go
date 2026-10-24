package app

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

// MCPDaemonContext lets a stdio host restart the bridge when its selected HTTP
// daemon stops, restarts or updates. It reads public loopback metadata only:
// no Store access, authentication material or protocol output is involved.
// Other bridge destinations retain their existing transport validation.
func MCPDaemonContext(parent context.Context, endpoint string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "/mcp" || u.RawQuery != "" || u.Fragment != "" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		return ctx, cancel
	}
	rawPort := u.Port()
	if rawPort == "" {
		rawPort = "80"
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return ctx, cancel
	}
	ready, err := readDaemonReady(port)
	if err != nil || ready == nil {
		// An HTTP gateway bridge must never run without a pinned observer.
		// The SDK may finish connecting after this first readiness failure.
		cancel()
		return ctx, cancel
	}
	origin := "http://127.0.0.1:" + strconv.Itoa(port)
	go func() {
		for ctx.Err() == nil {
			event, err := WaitDaemonLifecycle(ctx, origin, ready.Installation)
			if ctx.Err() != nil {
				return
			}
			if err == nil && event.InstanceID == ready.Instance {
				if event.State != "running" {
					cancel()
					return
				}
				continue
			}
			// A foreign/malformed event cannot stop a healthy pinned instance.
			// If that instance has gone, exit rather than holding its EXE open
			// or silently attaching the same token to a successor installation.
			live, probeErr := readDaemonReady(port)
			if probeErr != nil || live == nil || live.Installation != ready.Installation || live.Instance != ready.Instance {
				cancel()
				return
			}
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return ctx, cancel
}
