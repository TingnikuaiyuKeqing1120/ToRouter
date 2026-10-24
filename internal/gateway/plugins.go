package gateway

import (
	"time"

	route "torouter/internal/router"
	"torouter/internal/store"
)

const disabledPluginReason = "connection plugin disabled; enable it in Plugin Library"
const unavailablePluginReason = "compatible runtime unavailable; install or repair it in Plugin Library"

func disabledPluginRecovery() *recovery {
	return &recovery{Code: "connection_plugin_disabled", Actions: []string{"enable_connection_plugin"}, Detail: "open #/plugins?category=connect to enable the connection plugin; saved channels and credentials are retained"}
}

func (g *Gateway) pluginEnabled(c store.Channel) bool {
	if c.CredentialType == "" || c.CredentialType == store.CredentialAPIKey {
		return true
	}
	return g.Plugins != nil && g.Plugins.Enabled(c.CredentialType)
}

func (g *Gateway) enabledPluginCandidates(candidates []route.Attempt) []route.Attempt {
	enabled := make([]route.Attempt, 0, len(candidates))
	for _, candidate := range candidates {
		if g.pluginEnabled(candidate.Channel) {
			enabled = append(enabled, candidate)
		}
	}
	return enabled
}

// Toggle invalidates quota eligibility, not the saved configuration or facts
// from actual inference requests. Late quota observations cannot revive it.
func (g *Gateway) invalidatePluginQuota(credentialType string) {
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	state := g.runtimeLocked()
	channels, err := g.Store.ListChannels()
	if err != nil {
		// A read failure must not leave a stale quota gate active after enable.
		for key := range state.quotas {
			delete(state.quotas, key)
			state.resets[key] = time.Now()
			delete(state.bypass, key)
		}
		return
	}
	at := time.Now()
	for _, c := range channels {
		if c.CredentialType != credentialType {
			continue
		}
		for _, cred := range c.Credentials {
			key := quotaKey{c.ID, c.Generation, cred.ID}
			delete(state.quotas, key)
			state.resets[key] = at
			delete(state.bypass, key)
		}
	}
}
