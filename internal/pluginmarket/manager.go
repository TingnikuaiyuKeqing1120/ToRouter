// Package pluginmarket discovers and installs user-selected supplier plugins.
// Executed plugins are trusted local code, not an operating-system sandbox.
package pluginmarket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"torouter/internal/marketprofile"
)

type Item struct {
	Package     string   `json:"package"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Icon        string   `json:"icon,omitempty"`
	Community   bool     `json:"community"`
	Version     string   `json:"version,omitempty"`
	Repository  string   `json:"repository,omitempty"`
	Providers   []string `json:"providers"`
}

type Catalogue struct {
	Items      []Item    `json:"items"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
	Refreshing bool      `json:"refreshing"`
	Warning    string    `json:"warning,omitempty"`
}

type Installed struct {
	ID          string                   `json:"id"`
	Package     string                   `json:"package"`
	Name        string                   `json:"name"`
	Version     string                   `json:"version"`
	Community   bool                     `json:"community"`
	Icon        string                   `json:"icon,omitempty"`
	Description string                   `json:"description"`
	Providers   []marketprofile.Provider `json:"providers"`
	ModulePath  string                   `json:"-"`
	RuntimePath string                   `json:"-"`
}

type Inspector func(context.Context, Installed) ([]marketprofile.Provider, error)

type installedRecord struct {
	Installed
	Module  string `json:"module"`
	Runtime string `json:"runtime"`
}

type catalogueCache struct {
	Schema    int               `json:"schema"`
	Sources   map[string][]Item `json:"sources"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Discovery evidence rules are part of the cache format. An older candidate
// list cannot bypass today's supplier filter after a binary upgrade.
const catalogueSchema = 1

type Manager struct {
	mu                                                          sync.RWMutex
	installMu                                                   sync.Mutex
	refreshMu                                                   sync.Mutex
	root                                                        string
	client                                                      *http.Client
	registryURL, npmURL, githubURL, rawURL, distURL, bunRelease string
	cache                                                       catalogueCache
	trusted                                                     map[string]Item
	installed                                                   map[string]Installed
	queries                                                     map[string]time.Time
	refreshing                                                  bool
	attempt                                                     time.Time
	warning                                                     string
	stateErr                                                    error
	// Seams are per manager so fixtures never alter process-wide settings.
	prepareRuntime func(context.Context) (string, error)
	dependencies   func(context.Context, string, string) error
}

func New(dataDir string) *Manager {
	if absolute, err := filepath.Abs(dataDir); err == nil {
		dataDir = absolute
	}
	m := &Manager{root: filepath.Join(dataDir, "plugins", "market"), client: &http.Client{Timeout: 15 * time.Second},
		registryURL: "https://raw.githubusercontent.com/magpie-community/plugins/main/registry.json",
		npmURL:      "https://registry.npmjs.org", githubURL: "https://api.github.com", rawURL: "https://raw.githubusercontent.com",
		distURL: "https://raw.githubusercontent.com/magpie-community/plugins/dist", bunRelease: "https://github.com/oven-sh/bun/releases/download",
		installed: map[string]Installed{}, queries: map[string]time.Time{}, trusted: map[string]Item{}}
	m.cache.Sources = map[string][]Item{}
	m.cache.Schema = catalogueSchema
	if b, e := os.ReadFile(filepath.Join(m.root, "catalogue.json")); e == nil {
		var c catalogueCache
		if json.Unmarshal(b, &c) == nil && c.Sources != nil && c.Schema == catalogueSchema {
			m.cache = c
			m.attempt = c.UpdatedAt
		}
	}
	// Only the reviewed registry source may grant community provenance.
	trusted, _ := parseRegistry(builtinRegistry)
	if cached := m.cache.Sources["community"]; len(cached) > 0 {
		trusted = cached
	}
	for _, it := range trusted {
		if npmName.MatchString(it.Package) && len(it.Providers) > 0 {
			it.Community = true
			m.trusted[it.Package] = it
		}
	}
	m.cache.Sources["community"] = trusted
	if b, e := os.ReadFile(filepath.Join(m.root, "active.json")); e == nil {
		var records []installedRecord
		if e = json.Unmarshal(b, &records); e != nil {
			m.stateErr = errors.New("供应商安装记录损坏")
		} else {
			for _, r := range records {
				x := r.Installed
				x.Description = briefDescription(x.Description, x.Name)
				x.ModulePath = r.Module
				x.RuntimePath = r.Runtime
				if x.ID != marketprofile.PackageID(x.Package) || !within(filepath.Join(m.root, "packages", x.ID), x.ModulePath) {
					m.stateErr = errors.New("供应商安装记录无效")
					continue
				}
				m.installed[x.ID] = x
			}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		m.stateErr = errors.New("无法读取供应商安装记录")
	}
	m.prepareRuntime = m.ensureBun
	m.dependencies = m.installDependencies
	return m
}

// Catalogue never waits for the network. Its detached refresh has a bounded
// lifetime, and survives the HTTP request which first asked for the list.
func (m *Manager) Catalogue(ctx context.Context, query string) (Catalogue, error) {
	q := strings.TrimSpace(query)
	if len(q) > 200 {
		return Catalogue{}, errors.New("搜索文字过长")
	}
	m.mu.Lock()
	stale := time.Since(m.attempt) > 10*time.Minute
	queryStale := q != "" && time.Since(m.queries[q]) > 10*time.Minute
	if !m.refreshing && (stale || queryStale) {
		m.refreshing = true
		m.attempt = time.Now()
		if q != "" {
			m.queries[q] = time.Now()
		}
		go func() {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
			defer cancel()
			_ = m.refresh(c, q)
		}()
	}
	out := Catalogue{Items: m.itemsLocked(q), UpdatedAt: m.cache.UpdatedAt, Refreshing: m.refreshing, Warning: m.warning}
	m.mu.Unlock()
	return out, nil
}

func (m *Manager) itemsLocked(query string) []Item {
	items := map[string]Item{}
	for key, list := range m.cache.Sources {
		if strings.HasPrefix(key, "query:") && key != "query:"+query {
			continue
		}
		for _, it := range list {
			it.Description = briefDescription(it.Description, it.Name)
			it.Community = false
			if trusted, ok := m.trusted[it.Package]; ok {
				it.Community = true
				it.Name = trusted.Name
				it.Providers = append([]string(nil), trusted.Providers...)
				if trusted.Description != "" {
					it.Description = trusted.Description
				}
				if trusted.Icon != "" {
					it.Icon = trusted.Icon
				}
			}
			old, ok := items[it.Package]
			if !ok || it.Version != "" || old.Version == "" {
				items[it.Package] = it
			}
		}
	}
	out := make([]Item, 0, len(items))
	q := strings.ToLower(query)
	for _, it := range items {
		if q == "" || strings.Contains(strings.ToLower(it.Name+" "+it.Package+" "+it.Description+" "+strings.Join(it.Providers, " ")), q) {
			it.Providers = append([]string{}, it.Providers...)
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Community != out[j].Community {
			return out[i].Community
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (m *Manager) Refresh(ctx context.Context) error {
	return m.RefreshQuery(ctx, "")
}

// RefreshQuery bypasses the listing TTL for an explicit user retry/search.
func (m *Manager) RefreshQuery(ctx context.Context, query string) error {
	query = strings.TrimSpace(query)
	if len(query) > 200 {
		return errors.New("搜索文字过长")
	}
	m.mu.Lock()
	m.refreshing = true
	m.attempt = time.Now()
	if query != "" {
		m.queries[query] = time.Now()
	}
	m.mu.Unlock()
	return m.refresh(ctx, query)
}

func (m *Manager) Installed() []Installed {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Installed, 0, len(m.installed))
	for _, v := range m.installed {
		out = append(out, cloneInstalled(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (m *Manager) Lookup(id string) (Installed, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	x, ok := m.installed[id]
	return cloneInstalled(x), ok
}
func cloneInstalled(x Installed) Installed {
	b, _ := json.Marshal(x)
	var out Installed
	_ = json.Unmarshal(b, &out)
	out.ModulePath = x.ModulePath
	out.RuntimePath = x.RuntimePath
	return out
}

// Remove unregisters the package only. Immutable version files remain available
// to already-running requests; channels and encrypted credentials live elsewhere.
func (m *Manager) Remove(id string) error {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stateErr != nil {
		return m.stateErr
	}
	if _, ok := m.installed[id]; !ok {
		return errors.New("供应商未安装")
	}
	next := make(map[string]Installed, len(m.installed))
	for k, v := range m.installed {
		if k != id {
			next[k] = v
		}
	}
	if err := m.saveInstalled(next); err != nil {
		return err
	}
	m.installed = next
	return nil
}
func (m *Manager) saveInstalled(next map[string]Installed) error {
	records := make([]installedRecord, 0, len(next))
	for _, x := range next {
		records = append(records, installedRecord{x, x.ModulePath, x.RuntimePath})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return writeJSON(filepath.Join(m.root, "active.json"), records)
}
func writeJSON(name string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(name), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(name), ".write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), name)
}
func within(root, file string) bool {
	rel, e := filepath.Rel(root, file)
	return e == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (m *Manager) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	// Runtime/package archives may take longer than small catalogue metadata.
	// Keep them cancellable and bounded without a 15-second whole-file cutoff.
	if limit >= 64<<20 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("User-Agent", "to-router-provider-market")
	req.Header.Set("Accept", "application/json")
	client := m.client
	if limit >= 64<<20 {
		copy := *m.client
		copy.Timeout = 0
		client = &copy
	}
	res, e := client.Do(req)
	if e != nil {
		return nil, errors.New("市场网络连接失败")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, httpStatusError(res.StatusCode)
	}
	return readLimited(res.Body, limit)
}

type httpStatusError int

func (e httpStatusError) Error() string { return fmt.Sprintf("市场来源返回 HTTP %d", int(e)) }
func notFound(err error) bool {
	var status httpStatusError
	return errors.As(err, &status) && int(status) == http.StatusNotFound
}
