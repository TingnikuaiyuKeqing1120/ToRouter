package gateway

import (
	"testing"
	"time"
	"torouter/internal/health"
	route "torouter/internal/router"
)

func combinationForCredential(t *testing.T, g *Gateway, id int64, credential string) health.Key {
	t.Helper()
	channels, err := g.Store.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range channels {
		if c.ID == id {
			if credential == "" {
				credential = c.Credentials[0].ID
			}
			return combination(route.Attempt{Channel: c, Endpoint: c.Endpoints[0], Model: c.Models[0]}, credential)
		}
	}
	t.Fatalf("channel %d missing", id)
	return health.Key{}
}
func testCombination(t *testing.T, g *Gateway, id int64) health.Key {
	return combinationForCredential(t, g, id, "")
}
func healthPhase(g *Gateway, id int64) string {
	for k, v := range g.Breaker.Snapshot() {
		if k.ChannelID != id || v.Failures == 0 {
			continue
		}
		if v.Probing {
			return "probing"
		}
		if time.Now().Before(v.Until) {
			return "cooling"
		}
		return "half_open"
	}
	return ""
}
func failedCombinations(g *Gateway) map[health.Key]health.State {
	out := map[health.Key]health.State{}
	for k, v := range g.Breaker.Snapshot() {
		if v.Failures > 0 {
			out[k] = v
		}
	}
	return out
}
