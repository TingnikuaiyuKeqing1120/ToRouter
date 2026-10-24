package store

import (
	"encoding/json"
	"slices"
)

// moveTagLocked validates the new facts before changing group identity. A model
// override losing its final old-group tag stays explicitly unknown in that
// group, so moving a tag cannot silently restore channel inheritance.
func (s *Store) moveTagLocked(old, tag Tag) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	refs, _, err := tagReferenceChanges(tx, tag.ID)
	if err != nil {
		return err
	}
	if len(refs.ClientKeyNames) > 0 {
		return &TagInUseError{References: refs}
	}
	rows, err := tx.Query("SELECT data FROM tags")
	if err != nil {
		return err
	}
	tags := []Tag{}
	oldGroupTags := map[string]bool{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var current Tag
		if err = json.Unmarshal([]byte(raw), &current); err != nil {
			rows.Close()
			return err
		}
		if current.GroupID == old.GroupID {
			oldGroupTags[current.ID] = true
		}
		if current.ID == tag.ID {
			current = tag
		}
		tags = append(tags, current)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	changes := []tagReferenceChange{}
	for _, table := range []string{"channels", "downstream_models"} {
		rows, err = tx.Query("SELECT id,data FROM " + table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, raw string
			if err = rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			var value struct {
				Tags      []string            `json:"tags"`
				ModelTags map[string][]string `json:"model_tags"`
			}
			if err = json.Unmarshal([]byte(raw), &value); err != nil {
				rows.Close()
				return err
			}
			if slices.Contains(value.Tags, tag.ID) {
				if err = validateObjectTags(value.Tags, tags); err != nil {
					rows.Close()
					return err
				}
			}
			changed := false
			if table == "channels" {
				for model, ids := range value.ModelTags {
					if !slices.Contains(ids, tag.ID) {
						continue
					}
					oldGroupStillExplicit := false
					for _, id := range ids {
						if id != tag.ID && oldGroupTags[id] {
							oldGroupStillExplicit = true
							break
						}
					}
					if !oldGroupStillExplicit {
						ids = append(slices.Clone(ids), UnknownTagID(old.GroupID))
						value.ModelTags[model] = ids
						changed = true
					}
					if err = validateObjectTags(ids, tags); err != nil {
						rows.Close()
						return err
					}
				}
			}
			if changed {
				var fields map[string]json.RawMessage
				if err = json.Unmarshal([]byte(raw), &fields); err != nil {
					rows.Close()
					return err
				}
				fields["model_tags"], _ = json.Marshal(value.ModelTags)
				data, _ := json.Marshal(fields)
				changes = append(changes, tagReferenceChange{table: table, id: id, data: string(data)})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(tag)
	if _, err = tx.Exec("UPDATE tags SET group_id=?,data=? WHERE id=?", tag.GroupID, string(raw), tag.ID); err != nil {
		return err
	}
	for _, change := range changes {
		if _, err = tx.Exec("UPDATE "+change.table+" SET data=? WHERE id=?", change.data, change.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
