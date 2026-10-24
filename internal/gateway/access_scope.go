package gateway

import (
	"torouter/internal/store"
)

// requestScope freezes tag definitions with the authenticated key's scope.
// Retries and affinity only receive bindings admitted by this snapshot.
type requestScope struct {
	scope  store.ClientKeyScope
	tags   []store.Tag
	groups []store.TagGroup
}

func (g *Gateway) scopeSnapshot(scope store.ClientKeyScope) (requestScope, error) {
	snapshot := requestScope{scope: scope}
	// Billing also consumes this tag snapshot when the key has no tag rules.
	var err error
	snapshot.tags, err = g.Store.ListTags()
	if err != nil {
		return snapshot, err
	}
	snapshot.groups, err = g.Store.ListTagGroups()
	return snapshot, err
}

func (s requestScope) allows(c store.Channel, model string) bool {
	return s.scope.AllowsTags(store.ResolveModelTags(c, model, s.tags, s.groups))
}

func (s requestScope) filterModels(models []store.DownstreamModel, channels []store.Channel) []store.DownstreamModel {
	byID := map[int64]store.Channel{}
	for _, c := range channels {
		byID[c.ID] = c
	}
	filtered := []store.DownstreamModel{}
	for _, m := range models {
		if !s.scope.AllowsModel(m.ID) {
			continue
		}
		m.Bindings = append([]store.Binding(nil), m.Bindings...)
		allowed := m.Bindings[:0]
		for _, b := range m.Bindings {
			if c, ok := byID[b.ChannelID]; !ok || s.allows(c, b.Model) {
				allowed = append(allowed, b)
			}
		}
		m.Bindings = allowed
		filtered = append(filtered, m)
	}
	return filtered
}
