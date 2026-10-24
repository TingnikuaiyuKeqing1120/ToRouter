package store

import (
	"slices"
	"strconv"
)

type TagDeletionState struct {
	ID         string           `json:"id"`
	Group      bool             `json:"group"`
	Definition any              `json:"definition"`
	References []map[string]any `json:"references"`
	Version    string           `json:"version"`
}

func (s *Store) TagDeletionState(id string, group bool) (TagDeletionState, error) {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	return s.tagDeletionStateLocked(id, group)
}
func (s *Store) tagDeletionStateLocked(id string, group bool) (TagDeletionState, error) {
	state := TagDeletionState{ID: id, Group: group, References: []map[string]any{}}
	tags, err := s.ListTags()
	if err != nil {
		return state, err
	}
	ids := []string{}
	if group {
		groups, e := s.ListTagGroups()
		if e != nil {
			return state, e
		}
		for _, g := range groups {
			if g.ID == id {
				state.Definition = g
			}
		}
		for _, tag := range tags {
			if tag.GroupID == id {
				ids = append(ids, tag.ID)
			}
		}
	} else {
		for _, tag := range tags {
			if tag.ID == id {
				state.Definition = tag
				ids = append(ids, id)
			}
		}
	}
	if state.Definition == nil {
		return state, ErrNotFound
	}
	related := func(values []string) bool {
		for _, v := range values {
			if slices.Contains(ids, v) {
				return true
			}
		}
		return false
	}
	channels, err := s.ListChannels()
	if err != nil {
		return state, err
	}
	for _, c := range channels {
		found := related(c.Tags)
		for _, values := range c.ModelTags {
			found = found || related(values)
		}
		if found {
			state.References = append(state.References, map[string]any{"kind": "channel", "id": c.ID, "name": c.Name, "version": EntityVersion(struct {
				Tags   []string
				Models map[string][]string
			}{c.Tags, c.ModelTags})})
		}
	}
	models, err := s.ListDownstreamModels()
	if err != nil {
		return state, err
	}
	for _, m := range models {
		if related(m.Tags) {
			state.References = append(state.References, map[string]any{"kind": "public_model", "id": m.ID, "name": m.DisplayName, "version": EntityVersion(m.Tags)})
		}
	}
	keys, err := s.ListClientKeys()
	if err != nil {
		return state, err
	}
	for _, key := range keys {
		found := false
		for gid, values := range key.Scope.TagRules {
			if group && gid == id || related(values) {
				found = true
			}
		}
		if found {
			state.References = append(state.References, map[string]any{"kind": "client_key", "id": strconv.FormatInt(key.ID, 10), "name": key.Name, "version": EntityVersion(key.Scope)})
		}
	}
	state.Version = EntityVersion(state)
	return state, nil
}
