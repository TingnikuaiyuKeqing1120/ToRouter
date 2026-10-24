package cliview

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"torouter/internal/app"
)

var errDaemonStopped = errors.New("daemon stopped")
var errDaemonUnavailable = errors.New("daemon unavailable")

// A restart may first drain for ten seconds, then wait for its child readiness.
// A disconnected observer must allow that handover even if it missed the event.
const daemonReconnectGrace = 30 * time.Second

func followDaemon(ctx context.Context, origin, installation string, resolvePort func() (int, error)) error {
	var deadline time.Time
	var restartingInstance string
	for ctx.Err() == nil {
		// Readiness is immediate, whereas the first running watch heartbeat
		// takes 25 seconds. Do not spend the handover budget waiting for it.
		if !deadline.IsZero() {
			endpoint, parseErr := url.Parse(origin)
			if parseErr != nil {
				return errDaemonUnavailable
			}
			port, _ := strconv.Atoi(endpoint.Port())
			if app.CheckInstallation(port, installation) == nil {
				deadline = time.Time{}
			}
		}
		watchCtx := ctx
		cancel := func() {}
		if !deadline.IsZero() {
			if time.Now().After(deadline) {
				return errDaemonUnavailable
			}
			watchCtx, cancel = context.WithDeadline(ctx, deadline)
		}
		value, err := app.WaitDaemonLifecycle(watchCtx, origin, installation)
		cancel()
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err == nil {
			switch value.State {
			case "stopped", "updating":
				return errDaemonStopped
			case "running":
				deadline = time.Time{}
				continue
			case "restarting":
				if value.InstanceID != restartingInstance {
					deadline = time.Now().Add(daemonReconnectGrace)
					restartingInstance = value.InstanceID
				}
				origin = fmt.Sprintf("http://127.0.0.1:%d", value.TargetPort)
			}
		} else {
			if deadline.IsZero() {
				deadline = time.Now().Add(daemonReconnectGrace)
			}
			// The new active record also recovers a cross-port restart whose
			// event fell between two watch requests. Never start a daemon here.
			if resolvePort != nil {
				if port, resolveErr := resolvePort(); resolveErr == nil && port > 0 {
					candidate := fmt.Sprintf("http://127.0.0.1:%d", port)
					if app.CheckInstallation(port, installation) == nil {
						origin = candidate
						deadline = time.Time{}
					}
				}
			}
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
	return context.Cause(ctx)
}
