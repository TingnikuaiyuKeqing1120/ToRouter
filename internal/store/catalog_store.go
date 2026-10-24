package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

func (s *Store) initCatalog() error {
	schema := `
CREATE TABLE IF NOT EXISTS catalog_cache (
	source TEXT NOT NULL,
	id TEXT NOT NULL,
	name TEXT NOT NULL,
	provider TEXT NOT NULL,
	data TEXT NOT NULL,
	fetched_at INTEGER NOT NULL,
	PRIMARY KEY (source, id)
);
CREATE TABLE IF NOT EXISTS catalog_sources (
	source TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	enabled INTEGER NOT NULL,
	item_count INTEGER NOT NULL,
	last_fetched_at INTEGER,
	last_error TEXT
);
`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("init catalog tables: %w", err)
	}

	for _, migration := range []struct{ ddl, initialize string }{
		{"ALTER TABLE catalog_sources ADD COLUMN prefill_enabled INTEGER NOT NULL DEFAULT 1", "UPDATE catalog_sources SET prefill_enabled=0 WHERE source='litellm'"},
		{"ALTER TABLE catalog_sources ADD COLUMN priority INTEGER NOT NULL DEFAULT 0", "UPDATE catalog_sources SET priority=CASE source WHEN 'builtin' THEN 0 WHEN 'openrouter' THEN 1 ELSE 2 END"},
	} {
		if _, err := s.db.Exec(migration.ddl); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
		} else {
			if _, err := s.db.Exec(migration.initialize); err != nil {
				return err
			}
		}
	}
	// Ensure default sources exist in catalog_sources
	defaultSources := []struct {
		id   string
		name string
	}{
		{"builtin", "精选静态库 (官方)"},
		{"openrouter", "OpenRouter"},
		{"litellm", "LiteLLM"},
	}

	for _, ds := range defaultSources {
		var exists int
		_ = s.db.QueryRow("SELECT 1 FROM catalog_sources WHERE source = ?", ds.id).Scan(&exists)
		if exists == 0 {
			count := 0
			if ds.id == "builtin" {
				count = len(BuiltinCatalog)
			}
			_, _ = s.db.Exec(
				"INSERT INTO catalog_sources (source, name, enabled, item_count, last_fetched_at, last_error,prefill_enabled,priority) VALUES (?, ?, 1, ?, NULL, '',?,?)",
				ds.id, ds.name, count, ds.id != "litellm", map[string]int{"builtin": 0, "openrouter": 1, "litellm": 2}[ds.id],
			)
		}
	}

	return nil
}

// ListCatalogItems queries catalog items according to source filter and search query.
// sourceFilter: "all", "builtin", "external", "openrouter", "litellm"
func (s *Store) ListCatalogItems(sourceFilter string, query string) ([]CatalogItem, error) {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()

	var result []CatalogItem
	q := strings.TrimSpace(strings.ToLower(query))

	// 1. Built-in items
	includeBuiltin := sourceFilter == "" || sourceFilter == "all" || sourceFilter == "builtin"
	if includeBuiltin {
		for _, item := range BuiltinCatalog {
			if matchesCatalogQuery(item, q) {
				result = append(result, item)
			}
		}
	}

	// 2. External items from DB
	includeExternal := sourceFilter == "" || sourceFilter == "all" || sourceFilter == "external" || sourceFilter == "openrouter" || sourceFilter == "litellm"
	if includeExternal {
		var sqlQuery string
		var args []any

		if sourceFilter == "openrouter" || sourceFilter == "litellm" {
			sqlQuery = "SELECT data FROM catalog_cache WHERE source = ? ORDER BY id"
			args = append(args, sourceFilter)
		} else {
			sqlQuery = "SELECT data FROM catalog_cache ORDER BY id"
		}

		rows, err := s.db.Query(sqlQuery, args...)
		if err != nil {
			return nil, fmt.Errorf("query catalog_cache: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return nil, err
			}
			var item CatalogItem
			if err := json.Unmarshal([]byte(raw), &item); err != nil {
				continue
			}
			if matchesCatalogQuery(item, q) {
				result = append(result, item)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func matchesCatalogQuery(item CatalogItem, q string) bool {
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(item.ID), q) ||
		strings.Contains(strings.ToLower(item.Name), q) ||
		strings.Contains(strings.ToLower(item.Provider), q)
}

// SaveExternalCatalog updates the items for a specific external source in an atomic transaction.
func (s *Store) SaveExternalCatalog(source string, items []CatalogItem) error {
	if source != "openrouter" && source != "litellm" {
		return fmt.Errorf("unknown external catalog source %q", source)
	}
	seen := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || seen[item.ID] {
			return fmt.Errorf("catalog requires unique model ids")
		}
		seen[item.ID] = true
		offeringKeys := map[string]bool{}
		for _, o := range item.Offerings {
			if o.Key == "" || offeringKeys[o.Key] {
				return fmt.Errorf("catalog requires unique offering keys")
			}
			offeringKeys[o.Key] = true
			quoteKeys := map[string]bool{}
			for _, q := range o.Quotes {
				if q.Key == "" || quoteKeys[q.Key] {
					return fmt.Errorf("catalog requires unique quote keys")
				}
				quoteKeys[q.Key] = true
				if err := ValidatePriceTable(q.Table); err != nil {
					return fmt.Errorf("catalog quote %s/%s: %w", o.Key, q.Key, err)
				}
			}
		}
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Clear previous cache for this source
	if source == "openrouter" {
		cached := map[string]CatalogItem{}
		rows, e := tx.Query("SELECT data FROM catalog_cache WHERE source=?", source)
		if e != nil {
			return e
		}
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				break
			}
			var old CatalogItem
			if e = json.Unmarshal([]byte(raw), &old); e != nil {
				break
			}
			cached[old.ID] = old
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		for i := range items {
			keys := map[string]bool{}
			for _, off := range items[i].Offerings {
				keys[off.Key] = true
			}
			old := cached[items[i].ID]
			for _, off := range old.Offerings {
				if keys[off.Key] || off.Key == "directory-summary" {
					continue
				}
				for q := range off.Quotes {
					if off.Quotes[q].FetchedAt == nil {
						off.Quotes[q].FetchedAt = old.FetchedAt
					}
				}
				items[i].Offerings = append(items[i].Offerings, off)
			}
		}
	}
	if _, err := tx.Exec("DELETE FROM catalog_cache WHERE source = ?", source); err != nil {
		return fmt.Errorf("clear old catalog for %s: %w", source, err)
	}

	now := time.Now().Unix()
	nowTime := time.Now()
	stmt, err := tx.Prepare("INSERT INTO catalog_cache (source, id, name, provider, data, fetched_at) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, item := range items {
		item.Source = source
		item.FetchedAt = &nowTime
		for oi := range item.Offerings {
			for qi := range item.Offerings[oi].Quotes {
				q := &item.Offerings[oi].Quotes[qi]
				if q.FetchedAt == nil {
					q.FetchedAt = &nowTime
				}
			}
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := stmt.Exec(source, item.ID, item.Name, item.Provider, string(raw), now); err != nil {
			return fmt.Errorf("insert catalog item %s: %w", item.ID, err)
		}
	}

	// Update source status
	_, err = tx.Exec(
		"INSERT INTO catalog_sources (source, name, enabled, item_count, last_fetched_at, last_error) VALUES (?, ?, 1, ?, ?, '') "+
			"ON CONFLICT(source) DO UPDATE SET item_count = ?, last_fetched_at = ?, last_error = ''",
		source, source, len(items), now, len(items), now,
	)
	if err != nil {
		return fmt.Errorf("update catalog_sources: %w", err)
	}

	return tx.Commit()
}

func (s *Store) SaveCatalogSources(statuses []CatalogSourceStatus) error {
	return s.saveCatalogSources(statuses, "")
}

// CatalogPreferencesVersion excludes fetch status, which is not an editable preference.
func CatalogPreferencesVersion(statuses []CatalogSourceStatus) string {
	type preference struct {
		Source         string
		PrefillEnabled bool
		Priority       int
	}
	v := []preference{}
	for _, item := range statuses {
		v = append(v, preference{item.Source, item.PrefillEnabled, item.Priority})
	}
	slices.SortFunc(v, func(a, b preference) int { return strings.Compare(a.Source, b.Source) })
	return EntityVersion(v)
}

func (s *Store) SaveCatalogSourcesChecked(statuses []CatalogSourceStatus, expected string) error {
	if expected == "" {
		return ErrEditConflict
	}
	return s.saveCatalogSources(statuses, expected)
}

func (s *Store) saveCatalogSources(statuses []CatalogSourceStatus, expected string) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expected != "" {
		current := []CatalogSourceStatus{}
		rows, e := tx.Query("SELECT source,prefill_enabled,priority FROM catalog_sources ORDER BY source")
		if e != nil {
			return e
		}
		for rows.Next() {
			var item CatalogSourceStatus
			if e = rows.Scan(&item.Source, &item.PrefillEnabled, &item.Priority); e != nil {
				break
			}
			current = append(current, item)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		if CatalogPreferencesVersion(current) != expected {
			return ErrEditConflict
		}
	}
	seen := map[string]bool{}
	for _, st := range statuses {
		if st.Source != "builtin" && st.Source != "openrouter" && st.Source != "litellm" || seen[st.Source] || st.Priority < 0 {
			return fmt.Errorf("invalid catalog source preferences")
		}
		seen[st.Source] = true
		if _, err = tx.Exec("UPDATE catalog_sources SET prefill_enabled=?,priority=? WHERE source=?", st.PrefillEnabled, st.Priority, st.Source); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveCatalogItem refreshes one exact model/offering result without deleting
// the other successful rows for this source.
func (s *Store) SaveCatalogItem(source, id string, item CatalogItem) error {
	return s.saveCatalogItem(source, id, item, "")
}

func (s *Store) SaveCatalogItemChecked(source, id string, item CatalogItem, expected string) error {
	if expected == "" {
		return ErrEditConflict
	}
	return s.saveCatalogItem(source, id, item, expected)
}

func (s *Store) saveCatalogItem(source, id string, item CatalogItem, expected string) error {
	if source != "openrouter" && source != "litellm" || id == "" || item.ID != id {
		return fmt.Errorf("invalid exact catalog identity")
	}
	offeringKeys := map[string]bool{}
	for _, o := range item.Offerings {
		if o.Key == "" || offeringKeys[o.Key] {
			return fmt.Errorf("offering requires stable key")
		}
		offeringKeys[o.Key] = true
		quoteKeys := map[string]bool{}
		for _, q := range o.Quotes {
			if q.Key == "" || quoteKeys[q.Key] {
				return fmt.Errorf("quote requires stable key")
			}
			quoteKeys[q.Key] = true
			if err := ValidatePriceTable(q.Table); err != nil {
				return err
			}
		}
	}
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expected != "" {
		var raw string
		if err = tx.QueryRow("SELECT data FROM catalog_cache WHERE source=? AND id=?", source, id).Scan(&raw); err != nil {
			return ErrEditConflict
		}
		var previous CatalogItem
		if err = json.Unmarshal([]byte(raw), &previous); err != nil {
			return err
		}
		if EntityVersion(previous) != expected {
			return ErrEditConflict
		}
	}
	now := time.Now().UTC()
	item.Source = source
	item.FetchedAt = &now
	item.NeedsRefresh = false
	for oi := range item.Offerings {
		for qi := range item.Offerings[oi].Quotes {
			item.Offerings[oi].Quotes[qi].FetchedAt = &now
		}
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO catalog_cache(source,id,name,provider,data,fetched_at) VALUES(?,?,?,?,?,?) ON CONFLICT(source,id) DO UPDATE SET name=excluded.name,provider=excluded.provider,data=excluded.data,fetched_at=excluded.fetched_at", source, id, item.Name, item.Provider, string(raw), now.Unix()); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE catalog_sources SET item_count=(SELECT count(*) FROM catalog_cache WHERE source=?),last_fetched_at=?,last_error='' WHERE source=?", source, now.Unix(), source); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateCatalogSourceError records an error when fetching a source.
func (s *Store) UpdateCatalogSourceError(source string, fetchErr error) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	errMsg := ""
	if fetchErr != nil {
		errMsg = fetchErr.Error()
	}

	_, err := s.db.Exec("UPDATE catalog_sources SET last_error = ? WHERE source = ?", errMsg, source)
	return err
}

// GetCatalogSources returns statuses for all configured catalog sources.
func (s *Store) GetCatalogSources() ([]CatalogSourceStatus, error) {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()

	rows, err := s.db.Query("SELECT source, name, enabled, item_count, last_fetched_at, last_error,prefill_enabled,priority FROM catalog_sources ORDER BY source")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []CatalogSourceStatus
	for rows.Next() {
		var src, name, lastErr sql.NullString
		var enabled, count, prefill, priority int
		var lastFetched sql.NullInt64

		if err := rows.Scan(&src, &name, &enabled, &count, &lastFetched, &lastErr, &prefill, &priority); err != nil {
			return nil, err
		}

		st := CatalogSourceStatus{
			Source:         src.String,
			PrefillEnabled: prefill == 1, Priority: priority,
			Name:      name.String,
			Enabled:   enabled == 1,
			ItemCount: count,
			LastError: lastErr.String,
		}
		if st.Source == "builtin" {
			st.ItemCount = len(BuiltinCatalog)
		}
		if lastFetched.Valid && lastFetched.Int64 > 0 {
			t := time.Unix(lastFetched.Int64, 0)
			st.LastFetchedAt = &t
		}
		statuses = append(statuses, st)
	}

	slices.SortStableFunc(statuses, func(a, b CatalogSourceStatus) int { return a.Priority - b.Priority })
	return statuses, rows.Err()
}
