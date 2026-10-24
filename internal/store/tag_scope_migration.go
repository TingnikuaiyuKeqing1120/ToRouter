package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// NormalizeImportedChannelTags is a loading-boundary conversion, never a
// runtime fallback. The caller must retain the source backup before importing.
func (s *Store) NormalizeImportedChannelTags(c *Channel) ([]string, error) {
	tags, err := s.ListTags()
	if err != nil {
		return nil, err
	}
	index := map[string]Tag{}
	for _, tag := range tags {
		index[tag.ID] = tag
	}
	summary := []string{}
	normalize := func(ids []string, object string) ([]string, error) {
		byGroup := map[string][]string{}
		order := []string{}
		for _, id := range ids {
			tag, ok := index[id]
			if !ok {
				return nil, fmt.Errorf("%s: unknown tag %q", object, id)
			}
			if _, ok := byGroup[tag.GroupID]; !ok {
				order = append(order, tag.GroupID)
			}
			if !slices.Contains(byGroup[tag.GroupID], id) {
				byGroup[tag.GroupID] = append(byGroup[tag.GroupID], id)
			}
		}
		out := []string{}
		for _, group := range order {
			values := byGroup[group]
			if invalidTagGroup(group, values) {
				values = []string{UnknownTagID(group)}
				summary = append(summary, object+":"+group+" -> unknown")
			}
			out = append(out, values...)
		}
		return out, nil
	}
	if c.Tags, err = normalize(c.Tags, "channel"); err != nil {
		return nil, err
	}
	// Sort model IDs so the conversion summary is reproducible.
	models := []string{}
	for model := range c.ModelTags {
		models = append(models, model)
	}
	slices.Sort(models)
	for _, model := range models {
		if c.ModelTags[model], err = normalize(c.ModelTags[model], "model "+model); err != nil {
			return nil, err
		}
	}
	return summary, nil
}

func (s *Store) normalizeImportedDownstreamTags(models []DownstreamModel, prices []Price) ([]DownstreamModel, error) {
	tags, err := s.ListTags()
	if err != nil {
		return nil, err
	}
	groups, err := s.ListTagGroups()
	if err != nil {
		return nil, err
	}
	normalized := slices.Clone(models)
	summary := []string{}
	for i, m := range normalized {
		c := Channel{Tags: FilterDownstreamModelTags(m.Tags, tags, groups)}
		changes, e := s.NormalizeImportedChannelTags(&c)
		if e != nil {
			return nil, e
		}
		normalized[i].Tags = c.Tags
		for _, line := range changes {
			summary = append(summary, m.ID+": "+line)
		}
	}
	if len(summary) == 0 {
		return normalized, nil
	}
	// Preserve both the current database and the exact decoded incoming model
	// payload before reducing ambiguous imported facts to unknown.
	var databasePath string
	if err = s.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&databasePath); err != nil {
		return nil, err
	}
	backupDir := filepath.Join(filepath.Dir(databasePath), "backups", "tag-scope-import-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if err = os.MkdirAll(backupDir, 0700); err != nil {
		return nil, err
	}
	if _, err = s.db.Exec("VACUUM INTO ?", filepath.Join(backupDir, "router.db")); err != nil {
		return nil, err
	}
	key, err := os.ReadFile(filepath.Join(filepath.Dir(databasePath), "master.key"))
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(backupDir, "master.key"), key, 0600); err != nil {
		return nil, err
	}
	source, err := json.Marshal(struct {
		Models  []DownstreamModel `json:"models"`
		Prices  []Price           `json:"prices"`
		Changes []string          `json:"changes"`
	}{models, prices, summary})
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(backupDir, "source.json"), source, 0600); err != nil {
		return nil, err
	}
	return normalized, nil
}

func (s *Store) upgradeTagScopes(dir string) error {
	var done int
	if err := s.db.QueryRow("SELECT count(*) FROM meta WHERE key='tag_scopes_v1'").Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return nil
	}
	type record struct{ table, id, raw string }
	records := []record{}
	for _, table := range []string{"channels", "downstream_models"} {
		rows, err := s.db.Query("SELECT id,data FROM " + table + " ORDER BY id")
		if err != nil {
			return err
		}
		for rows.Next() {
			r := record{table: table}
			if err = rows.Scan(&r.id, &r.raw); err != nil {
				rows.Close()
				return err
			}
			records = append(records, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	changes := []record{}
	summary := []string{}
	for _, r := range records {
		var stored struct {
			Tags      []string            `json:"tags"`
			ModelTags map[string][]string `json:"model_tags"`
		}
		if err := json.Unmarshal([]byte(r.raw), &stored); err != nil {
			return err
		}
		c := Channel{Tags: stored.Tags, ModelTags: stored.ModelTags}
		converted, err := s.NormalizeImportedChannelTags(&c)
		if err != nil {
			return err
		}
		if len(converted) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal([]byte(r.raw), &fields); err != nil {
			return err
		}
		fields["tags"], _ = json.Marshal(c.Tags)
		if r.table == "channels" {
			fields["model_tags"], _ = json.Marshal(c.ModelTags)
		}
		raw, _ := json.Marshal(fields)
		r.raw = string(raw)
		changes = append(changes, r)
		for _, line := range converted {
			summary = append(summary, fmt.Sprintf("%s %s %s", r.table, r.id, line))
		}
	}
	var err error
	var backup string
	if len(changes) > 0 {
		backupDir := filepath.Join(dir, "backups", "tag-scopes-"+time.Now().UTC().Format("20060102T150405.000000000"))
		if err = os.MkdirAll(backupDir, 0700); err != nil {
			return err
		}
		backup = filepath.Join(backupDir, "router.db")
		if _, err = s.db.Exec("VACUUM INTO ?", backup); err != nil {
			return fmt.Errorf("backup before tag conversion: %w", err)
		}
		key, e := os.ReadFile(filepath.Join(dir, "master.key"))
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(filepath.Dir(backup), "master.key"), key, 0600); e != nil {
			return e
		}
		if err = os.Chmod(backup, 0600); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range changes {
		if _, err = tx.Exec("UPDATE "+r.table+" SET data=? WHERE id=?", r.raw, r.id); err != nil {
			return err
		}
	}
	report, _ := json.Marshal(struct {
		Backup  string   `json:"backup"`
		Changes []string `json:"changes"`
	}{backup, summary})
	if _, err = tx.Exec("INSERT INTO meta(key,value) VALUES('tag_scopes_v1',?)", string(report)); err != nil {
		return err
	}
	return tx.Commit()
}

// TagScopeMigrationSummary exposes the persisted one-time conversion report.
func (s *Store) TagScopeMigrationSummary() (json.RawMessage, error) {
	var raw string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key='tag_scopes_v1'").Scan(&raw)
	return json.RawMessage(raw), err
}
