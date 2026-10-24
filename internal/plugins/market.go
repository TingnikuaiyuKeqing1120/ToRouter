package plugins

import (
	"context"
	"errors"
	"sort"
	"strings"

	"torouter/internal/accessguard"
	"torouter/internal/compatprofile"
	"torouter/internal/marketbridge"
	"torouter/internal/marketprofile"
	"torouter/internal/pluginhost"
	"torouter/internal/pluginmarket"
	"torouter/internal/store"
)

func (m *Manager) byCredentialType(t string) *entry {
	m.registryMu.RLock()
	defer m.registryMu.RUnlock()
	return m.byType[t]
}

// AttachMarket loads persisted installations once at application startup.
// Tests and embeddings without a market keep existing native behavior.
func (m *Manager) AttachMarket(market *pluginmarket.Manager) error {
	m.marketMu.Lock()
	defer m.marketMu.Unlock()
	m.market = market
	return m.reloadMarket()
}

func (m *Manager) reloadMarket() error {
	m.manageMu.Lock()
	defer m.manageMu.Unlock()
	if m.market == nil {
		return nil
	}
	installed := m.market.Installed()
	defaults := map[string]map[string]bool{}
	for _, pkg := range installed {
		for _, p := range pkg.Providers {
			defaults[marketprofile.CredentialType(pkg.Package, p.ID)] = map[string]bool{"connect": true}
		}
	}
	prefs, err := m.store.InitPluginPreferences(defaults, nil)
	if err != nil {
		return err
	}
	active := map[string]bool{}
	changed := []*entry{}
	register := func(typ string, info Info, revision string, factory func() *NativeRuntime) {
		e := m.byCredentialType(typ)
		if e == nil {
			e = &entry{info: info, credentialType: typ, capability: "connect", enabled: prefs.Plugins[typ]["connect"], epoch: 1, changed: make(chan struct{}), factory: factory, marketRevision: revision}
			m.registryMu.Lock()
			m.entries[typ] = e
			m.byType[typ] = e
			m.registryMu.Unlock()
			m.ordered = append(m.ordered, e)
		} else {
			e.mu.Lock()
			if e.info.Version != info.Version || e.info.Package != info.Package || e.marketRevision != revision || e.enabled != prefs.Plugins[typ]["connect"] || (e.factory == nil) != (factory == nil) {
				e.epoch++
				close(e.changed)
				e.changed = make(chan struct{})
				e.runtime = nil
				changed = append(changed, e)
			}
			e.info = info
			e.factory = factory
			e.marketRevision = revision
			e.enabled = prefs.Plugins[typ]["connect"]
			e.mu.Unlock()
		}
		active[typ] = true
	}
	for _, pkg := range installed {
		for _, p := range pkg.Providers {
			typ := marketprofile.CredentialType(pkg.Package, p.ID)
			info := Info{ID: typ, Name: p.Name, Description: pkg.Description, Icon: pkg.Icon, Implementation: "compatible", Version: pkg.Version, CredentialType: typ, Package: pkg.Package, Community: pkg.Community, AuthMethods: p.Methods}
			if info.Name == "" {
				info.Name = p.ID
			}
			register(typ, info, pkg.ModulePath, func() *NativeRuntime {
				service := &marketbridge.Service{Store: m.store, Type: typ, Provider: p, RuntimePath: pkg.RuntimePath, ModulePath: pkg.ModulePath, Gates: &m.marketGates}
				return &NativeRuntime{Runtime: service, Market: service}
			})
		}
	}
	// Preserve management handles for accounts whose code is no longer installed.
	channels, err := m.store.ListChannels()
	if err != nil {
		return err
	}
	for _, c := range channels {
		if store.IsMarketOAuth(c.CredentialType) && !active[c.CredentialType] {
			_, id, _ := marketprofile.ParseCredentialType(c.CredentialType)
			info := Info{ID: c.CredentialType, Name: id, Implementation: "compatible", CredentialType: c.CredentialType}
			if e := m.byCredentialType(c.CredentialType); e != nil {
				e.mu.Lock()
				info = e.info
				info.Version = ""
				e.mu.Unlock()
			}
			register(c.CredentialType, info, "", nil)
		}
	}
	// Retain old handles but retire code for packages removed without accounts.
	for _, e := range m.ordered {
		if store.IsMarketOAuth(e.credentialType) && !active[e.credentialType] {
			e.mu.Lock()
			if e.factory != nil {
				e.epoch++
				close(e.changed)
				e.changed = make(chan struct{})
				e.factory = nil
				e.runtime = nil
				changed = append(changed, e)
			}
			e.mu.Unlock()
		}
	}
	for _, e := range changed {
		for _, listener := range m.listeners {
			listener(e.credentialType, e.enabled)
		}
	}
	return nil
}

type MarketProvider struct {
	marketprofile.Provider
	CredentialType string `json:"credential_type"`
}
type MarketItem struct {
	ID                 string           `json:"id"`
	Package            string           `json:"package"`
	Name               string           `json:"name"`
	Description        string           `json:"description"`
	Icon               string           `json:"icon"`
	Community          bool             `json:"community"`
	Version            string           `json:"version,omitempty"`
	InstalledVersion   string           `json:"installed_version,omitempty"`
	Installed          bool             `json:"installed"`
	UpdateAvailable    bool             `json:"update_available"`
	Providers          []string         `json:"providers"`
	InstalledProviders []MarketProvider `json:"installed_providers"`
}
type MarketList struct {
	Items      []MarketItem `json:"items"`
	Refreshing bool         `json:"refreshing"`
	Warning    string       `json:"warning,omitempty"`
}

func (m *Manager) MarketCatalogue(ctx context.Context, query string) (MarketList, error) {
	out := MarketList{Items: []MarketItem{}}
	if m.market == nil {
		return out, nil
	}
	catalogue, err := m.market.Catalogue(ctx, query)
	if err != nil {
		return out, err
	}
	out.Refreshing = catalogue.Refreshing
	out.Warning = catalogue.Warning
	installed := map[string]pluginmarket.Installed{}
	for _, pkg := range m.market.Installed() {
		installed[pkg.Package] = pkg
	}
	seen := map[string]bool{}
	for _, item := range catalogue.Items {
		// Featured packages are already managed by their existing fixed adapters.
		skip := item.Community && (item.Package == "@magpie-community/opencode-zen-free-auth" || item.Package == "@magpie-community/opencode-devin-auth")
		for _, p := range compatprofile.Profiles() {
			if item.Community && item.Package == "@magpie-community/opencode-"+p.Package+"-auth" {
				skip = true
			}
		}
		if _, present := installed[item.Package]; skip && !present {
			continue
		}
		v := MarketItem{ID: marketprofile.PackageID(item.Package), Package: item.Package, Name: item.Name, Description: item.Description, Icon: item.Icon, Community: item.Community, Version: item.Version, Providers: item.Providers, InstalledProviders: []MarketProvider{}}
		if pkg, ok := installed[item.Package]; ok {
			enrichMarketItem(&v, pkg)
		}
		out.Items = append(out.Items, v)
		seen[item.Package] = true
	}
	for _, pkg := range installed {
		search := strings.ToLower(pkg.Name + " " + pkg.Package + " " + pkg.Description)
		for _, provider := range pkg.Providers {
			search += " " + strings.ToLower(provider.Name+" "+provider.ID)
		}
		if !seen[pkg.Package] && strings.Contains(search, strings.ToLower(strings.TrimSpace(query))) {
			v := MarketItem{ID: pkg.ID, Package: pkg.Package, Name: pkg.Name, Description: pkg.Description, Icon: pkg.Icon, Community: pkg.Community, Providers: []string{}, InstalledProviders: []MarketProvider{}}
			enrichMarketItem(&v, pkg)
			out.Items = append(out.Items, v)
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		if out.Items[i].Community != out.Items[j].Community {
			return out.Items[i].Community
		}
		return out.Items[i].Name < out.Items[j].Name
	})
	return out, nil
}
func enrichMarketItem(v *MarketItem, p pluginmarket.Installed) {
	v.Installed = true
	v.InstalledVersion = p.Version
	v.UpdateAvailable = v.Version != "" && v.Version != p.Version
	for _, provider := range p.Providers {
		v.InstalledProviders = append(v.InstalledProviders, MarketProvider{provider, marketprofile.CredentialType(p.Package, provider.ID)})
	}
}

func (m *Manager) RefreshMarket(ctx context.Context, queries ...string) error {
	if m.market == nil {
		return errors.New("此实例未配置供应商市场")
	}
	if len(queries) > 0 {
		return m.market.RefreshQuery(ctx, queries[0])
	}
	return m.market.Refresh(ctx)
}
func (m *Manager) InstallMarket(ctx context.Context, pkg string) (MarketItem, error) {
	m.marketMu.Lock()
	defer m.marketMu.Unlock()
	if m.market == nil {
		return MarketItem{}, errors.New("此实例未配置供应商市场")
	}
	if err := accessguard.Check(ctx); err != nil {
		return MarketItem{}, err
	}
	p, err := m.market.Install(ctx, pkg, func(ctx context.Context, p pluginmarket.Installed) ([]marketprofile.Provider, error) {
		return pluginhost.InspectMarket(ctx, p.RuntimePath, p.ModulePath)
	})
	if err != nil {
		return MarketItem{}, err
	}
	if err = m.reloadMarket(); err != nil {
		return MarketItem{}, err
	}
	v := MarketItem{ID: p.ID, Package: p.Package, Name: p.Name, Description: p.Description, Icon: p.Icon, Community: p.Community, Version: p.Version, Providers: []string{}, InstalledProviders: []MarketProvider{}}
	enrichMarketItem(&v, p)
	return v, nil
}
func (m *Manager) RemoveMarket(ctx context.Context, id string) error {
	m.marketMu.Lock()
	defer m.marketMu.Unlock()
	if m.market == nil {
		return ErrInvalid
	}
	if err := accessguard.Check(ctx); err != nil {
		return err
	}
	if err := m.market.Remove(id); err != nil {
		return err
	}
	return m.reloadMarket()
}
func (m *Manager) MarketProvider(ctx context.Context, id, provider string) (*marketbridge.Service, context.Context, error) {
	if m.market == nil {
		return nil, ctx, ErrInvalid
	}
	pkg, ok := m.market.Lookup(id)
	if !ok {
		return nil, ctx, pluginhost.ErrNotInstalled
	}
	typ := marketprofile.CredentialType(pkg.Package, provider)
	r, ctx, err := m.Native(ctx, typ)
	if err != nil {
		return nil, ctx, err
	}
	if r.Market == nil {
		return nil, ctx, ErrInvalid
	}
	return r.Market, ctx, nil
}
