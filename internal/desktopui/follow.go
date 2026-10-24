package desktopui

import (
	"context"
	"time"

	"torouter/internal/app"
)

func (c *nativeClient) rememberPosition(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-c.ready:
	}
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.placement.save()
		}
	}
}

// Explicit stop is immediate. Unknown disconnects tolerate normal handover;
// only the selected installation may recover the window or stop its follower.
func (c *nativeClient) followBackend(ctx context.Context) {
	backend := c.snapshot()
	if backend == nil || backend.installationID == "" {
		return
	}
	var recovering time.Time
	for ctx.Err() == nil {
		backend = c.snapshot()
		event, err := c.waitLifecycle(ctx, backend.origin, backend.installationID)
		if ctx.Err() != nil {
			return
		}
		if backend != c.snapshot() {
			recovering = time.Time{}
			continue
		}
		if err == nil && (event.State == "stopped" || event.State == "updating") {
			c.quit()
			return
		}
		if err == nil && event.State == "running" {
			recovering = time.Time{}
			continue
		}
		if recovering.IsZero() {
			recovering = time.Now()
		}
		port := backend.port
		if err == nil && event.State == "restarting" {
			port = event.TargetPort
		}
		for ctx.Err() == nil {
			if time.Since(recovering) > 30*time.Second {
				c.quit()
				return
			}
			// Ready is checked immediately; the 25-second watch heartbeat does
			// not consume a reconnect deadline after the successor is ready.
			if app.CheckInstallation(port, backend.installationID) == nil {
				cfg := c.cfg
				cfg.port = port
				if c.secondLaunchArgs(cfg) == nil {
					recovering = time.Time{}
					break
				}
			}
			if resolved, resolveErr := app.ResolvePort(c.cfg.dataDir, 0); resolveErr == nil {
				port = resolved
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}
