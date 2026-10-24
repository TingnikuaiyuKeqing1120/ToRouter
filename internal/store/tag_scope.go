package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

func validateObjectTagsInTx(tx *sql.Tx, ids []string) error {
	rows, err := tx.Query("SELECT data FROM tags")
	if err != nil {
		return err
	}
	tags := []Tag{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var tag Tag
		if err = json.Unmarshal([]byte(raw), &tag); err != nil {
			rows.Close()
			return err
		}
		tags = append(tags, tag)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return validateObjectTags(ids, tags)
}

func UnknownTagID(groupID string) string { return groupID + ":unknown" }

// ResolveModelTags resolves each authorization group independently. Absent model
// groups inherit; explicit unknown stays independent. Invalid stored facts fail
// closed for that group instead of becoming a channel default.
func ResolveModelTags(channel Channel, model string, tags []Tag, groups []TagGroup) map[string][]string {
	index := map[string]Tag{}
	for _, tag := range tags {
		index[tag.ID] = tag
	}
	out := map[string][]string{}
	for _, ids := range [][]string{channel.Tags, channel.ModelTags[model]} {
		for _, id := range ids {
			if _, ok := index[id]; !ok {
				return out
			}
		}
	}
	for _, group := range groups {
		if group.ID == "modality" {
			continue
		}
		values := func(ids []string) []string {
			selected := []string{}
			for _, id := range ids {
				if tag, ok := index[id]; ok && tag.GroupID == group.ID && !slices.Contains(selected, id) {
					selected = append(selected, id)
				}
			}
			return selected
		}
		selected := values(channel.ModelTags[model])
		if len(selected) == 0 {
			selected = values(channel.Tags)
		}
		if len(selected) == 0 {
			selected = []string{UnknownTagID(group.ID)}
		}
		if invalidTagGroup(group.ID, selected) {
			selected = []string{}
		}
		out[group.ID] = selected
	}
	return out
}

func invalidTagGroup(group string, ids []string) bool {
	return len(ids) > 1 && (group == "billing" || group == "privacy" || slices.Contains(ids, UnknownTagID(group)))
}

func validateObjectTags(ids []string, tags []Tag) error {
	index := map[string]Tag{}
	for _, tag := range tags {
		index[tag.ID] = tag
	}
	byGroup := map[string][]string{}
	for _, id := range ids {
		tag, ok := index[id]
		if !ok {
			return fmt.Errorf("unknown tag %q", id)
		}
		if !slices.Contains(byGroup[tag.GroupID], id) {
			byGroup[tag.GroupID] = append(byGroup[tag.GroupID], id)
		}
	}
	for group, selected := range byGroup {
		if invalidTagGroup(group, selected) {
			return fmt.Errorf("conflicting tags in group %q", group)
		}
	}
	return nil
}

func (s *Store) validateChannelTags(c Channel) error {
	tags, err := s.ListTags()
	if err != nil {
		return err
	}
	if err = validateObjectTags(c.Tags, tags); err != nil {
		return err
	}
	for model, ids := range c.ModelTags {
		if model == "" {
			return errors.New("model_tags requires model ID")
		}
		if !containsString(c.Models, model) && !containsString(c.AvailableModels, model) {
			return fmt.Errorf("model_tags references unknown model %q", model)
		}
		if err = validateObjectTags(ids, tags); err != nil {
			return fmt.Errorf("model %q: %w", model, err)
		}
	}
	return nil
}
