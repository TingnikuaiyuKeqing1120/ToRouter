package codex

import "torouter/internal/store"

// RejectAccessOnly records an explicit upstream 401 for the exact bearer that
// was sent. Callers must not use model eligibility failures (403) as evidence.
// New consent, rotation and membership changes fence stale responses.
func (svc *Service) RejectAccessOnly(expected store.Channel, credentialID, rejectedToken string) error {
	lock := svc.lockFor(expected.ID)
	lock.Lock()
	defer lock.Unlock()
	return svc.Store.RejectAccessOnlyCredential(expected, credentialID, rejectedToken)
}
