package gateway

import (
	"sync"
	"torouter/internal/health"
	"torouter/internal/store"
)

// CredentialManager owns only dispatch order. Health is exclusively keyed by
// the complete outbound combination in health.Breaker.
type CredentialManager struct {
	mu      sync.Mutex
	cursors map[health.Key]string
}

func NewCredentialManager() *CredentialManager {
	return &CredentialManager{cursors: map[health.Key]string{}}
}
func (cm *CredentialManager) Order(c store.Channel, key health.Key, after string) []store.CredentialItem {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	key.CredentialID = ""
	enabled := []store.CredentialItem{}
	for _, cred := range c.Credentials {
		if cred.Enabled {
			enabled = append(enabled, cred)
		}
	}
	if len(enabled) == 0 {
		return enabled
	}
	cursor := cm.cursors[key]
	start := 0
	for i, cred := range enabled {
		if after != "" && cred.ID == after {
			start = (i + 1) % len(enabled)
			break
		}
		if after == "" && cred.ID == cursor {
			if c.DispatchMode == store.DispatchModeRoundRobin {
				start = (i + 1) % len(enabled)
			} else if c.ChainType == store.ChainTypeCyclic {
				start = i
			}
		}
	}
	result := append([]store.CredentialItem{}, enabled[start:]...)
	result = append(result, enabled[:start]...)
	if after != "" {
		filtered := result[:0]
		for _, cred := range result {
			if cred.ID != after {
				filtered = append(filtered, cred)
			}
		}
		result = filtered
	}
	if len(result) > 0 && c.DispatchMode == store.DispatchModeRoundRobin {
		cm.cursors[key] = result[0].ID
	}
	return result
}
func (cm *CredentialManager) Selected(key health.Key) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	id := key.CredentialID
	key.CredentialID = ""
	cm.cursors[key] = id
}

func (cm *CredentialManager) reconcile(valid func(health.Key) bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for k := range cm.cursors {
		if !valid(k) {
			delete(cm.cursors, k)
		}
	}
}
