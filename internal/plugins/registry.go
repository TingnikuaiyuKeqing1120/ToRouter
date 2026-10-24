// Package plugins manages optional provider lifecycles; routing stays in the
// existing gateway and router.
package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/keydiscovery"
	"torouter/internal/marketprofile"
	"torouter/internal/pluginhost"
	"torouter/internal/pluginmarket"
	"torouter/internal/store"
)

var (
	ErrDisabled = errors.New("插件已停用或启用状态已变化")
	ErrConflict = errors.New("插件列表已变化，请刷新后重试")
	ErrInvalid  = errors.New("无效的插件或能力")
)

type List struct {
	Items      []Info                   `json:"items"`
	Version    string                   `json:"version"`
	Compatible pluginhost.RuntimeStatus `json:"compatible"`
}

type Info struct {
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	Description    string                     `json:"description"`
	Icon           string                     `json:"icon"`
	Implementation string                     `json:"implementation"`
	Featured       bool                       `json:"featured"`
	Version        string                     `json:"version"`
	CredentialType string                     `json:"credential_type,omitempty"`
	Package        string                     `json:"package,omitempty"`
	Community      bool                       `json:"community,omitempty"`
	AuthMethods    []marketprofile.AuthMethod `json:"auth_methods,omitempty"`
	Capabilities   []CapabilityInfo           `json:"capabilities"`
}

type CapabilityInfo struct {
	ID                 string  `json:"id"`
	Enabled            bool    `json:"enabled"`
	Available          bool    `json:"available"`
	UnavailableReason  string  `json:"unavailable_reason,omitempty"`
	AffectedChannelIDs []int64 `json:"affected_channel_ids"`
}

type entry struct {
	mu             sync.Mutex
	info           Info
	credentialType string
	capability     string
	enabled        bool
	epoch          uint64
	changed        chan struct{}
	runtime        *NativeRuntime
	factory        func() *NativeRuntime
	background     bool
	marketRevision string
}

type Manager struct {
	localOperations *codex.Operations
	store           *store.Store
	manageMu        sync.Mutex
	registryMu      sync.RWMutex
	marketMu        sync.Mutex
	market          *pluginmarket.Manager
	marketGates     sync.Map
	entries         map[string]*entry
	byType          map[string]*entry
	ordered         []*entry
	listeners       []func(string, bool)
	runMu           sync.Mutex
	running         bool
	compatible      *pluginhost.Installation
}

// LocalOperations exposes only store-backed reports/configuration. It does not
// construct a provider service or make its disabled capability usable.
func (m *Manager) LocalOperations(credentialType string) *codex.Operations {
	if credentialType == store.CredentialCodexOAuth {
		return m.localOperations
	}
	return nil
}

func (m *Manager) Enabled(credentialType string) bool {
	if credentialType == store.CredentialAPIKey || credentialType == "" {
		return true
	}
	e := m.byCredentialType(credentialType)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enabled
}

func (m *Manager) Epoch(credentialType string) uint64 {
	e := m.byCredentialType(credentialType)
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch
}

func (m *Manager) CheckEpoch(credentialType string, epoch uint64) error {
	if credentialType == store.CredentialAPIKey || credentialType == "" {
		return nil
	}
	e := m.byCredentialType(credentialType)
	if e == nil {
		return ErrInvalid
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.enabled || e.epoch != epoch {
		return fmt.Errorf("%w：%s", ErrDisabled, e.info.Name)
	}
	return nil
}

func (m *Manager) Existing(credentialType string) *NativeRuntime {
	e := m.byCredentialType(credentialType)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runtime
}

func (m *Manager) Native(ctx context.Context, credentialType string) (*NativeRuntime, context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, ctx, err
	}
	if err := accessguard.Check(ctx); err != nil {
		return nil, ctx, err
	}
	e := m.byCredentialType(credentialType)
	if e == nil {
		return nil, ctx, ErrInvalid
	}
	e.mu.Lock()
	if !e.enabled {
		e.mu.Unlock()
		return nil, ctx, fmt.Errorf("%w：%s", ErrDisabled, e.info.Name)
	}
	if e.factory == nil || e.info.Implementation == "compatible" && !store.IsMarketOAuth(credentialType) && !m.compatible.Status().Installed {
		e.mu.Unlock()
		return nil, ctx, pluginhost.ErrNotInstalled
	}
	if e.runtime == nil {
		e.runtime = e.factory()
	}
	runtime, epoch := e.runtime, e.epoch
	e.mu.Unlock()
	guarded := accessguard.With(ctx, func() error {
		if err := accessguard.Check(ctx); err != nil {
			return err
		}
		return m.CheckEpoch(credentialType, epoch)
	})
	return runtime, guarded, nil
}

func (m *Manager) Resolve(ctx context.Context, credentialType string) (Runtime, context.Context, error) {
	runtime, guarded, err := m.Native(ctx, credentialType)
	if err != nil {
		return nil, guarded, err
	}
	return resolvedRuntime{Runtime: runtime.Runtime, access: guarded}, guarded, nil
}

func (m *Manager) Snapshot() (List, error) {
	m.manageMu.Lock()
	defer m.manageMu.Unlock()
	return m.snapshotLocked()
}

func (m *Manager) snapshotLocked() (List, error) {
	preferences, err := m.store.GetPluginPreferences()
	if err != nil {
		return List{}, err
	}
	channels, err := m.store.ListChannels()
	if err != nil {
		return List{}, err
	}
	list := List{Items: make([]Info, 0, len(m.ordered)), Compatible: m.compatible.Status()}
	epochs := map[string]uint64{}
	for _, e := range m.ordered {
		ids := []int64{}
		for _, channel := range channels {
			if e.capability == "connect" && channel.CredentialType == e.credentialType {
				ids = append(ids, channel.ID)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		e.mu.Lock()
		info := e.info
		available := e.factory != nil || e.capability != "connect"
		epochs[e.info.ID] = e.epoch
		e.mu.Unlock()
		info.Capabilities = []CapabilityInfo{{ID: e.capability, Enabled: preferences.Plugins[info.ID][e.capability], Available: true, AffectedChannelIDs: ids}}
		if store.IsMarketOAuth(e.credentialType) && !available {
			info.Capabilities[0].Available = false
			info.Capabilities[0].UnavailableReason = "插件已卸载，请在插件库重新安装"
		} else if info.Implementation == "compatible" && !store.IsMarketOAuth(e.credentialType) && !list.Compatible.Installed {
			info.Capabilities[0].Available = false
			info.Capabilities[0].UnavailableReason = list.Compatible.Reason
		}
		list.Items = append(list.Items, info)
	}
	raw, err := json.Marshal(struct {
		Preferences store.PluginPreferences `json:"preferences"`
		Items       []Info                  `json:"items"`
		Epochs      map[string]uint64       `json:"epochs"`
	}{preferences, list.Items, epochs})
	if err != nil {
		return List{}, err
	}
	hash := sha256.Sum256(raw)
	list.Version = hex.EncodeToString(hash[:])
	return list, nil
}

func (m *Manager) OnChange(listener func(credentialType string, enabled bool)) {
	if listener == nil {
		return
	}
	m.manageMu.Lock()
	defer m.manageMu.Unlock()
	m.listeners = append(m.listeners, listener)
}

// Reload refreshes in-memory plugin capability states from the underlying store.
func (m *Manager) Reload() error {
	if m == nil || m.store == nil {
		return nil
	}
	m.marketMu.Lock()
	defer m.marketMu.Unlock()
	if m.market != nil {
		if err := m.reloadMarket(); err != nil {
			return err
		}
	}
	m.manageMu.Lock()
	defer m.manageMu.Unlock()
	preferences, err := m.store.GetPluginPreferences()
	if err != nil {
		return err
	}
	for _, e := range m.ordered {
		enabled := preferences.Plugins[e.info.ID][e.capability]
		e.mu.Lock()
		changed := e.enabled != enabled
		if changed {
			e.enabled = enabled
			e.epoch++
			close(e.changed)
			e.changed = make(chan struct{})
		}
		e.mu.Unlock()
		if changed && e.capability == "connect" {
			for _, listener := range m.listeners {
				listener(e.credentialType, enabled)
			}
		}
	}
	return nil
}

func (m *Manager) SetCapability(ctx context.Context, id, capability string, enabled bool, expectedVersion string) (List, error) {
	m.manageMu.Lock()
	defer m.manageMu.Unlock()
	if err := ctx.Err(); err != nil {
		return List{}, err
	}
	if err := accessguard.Check(ctx); err != nil {
		return List{}, err
	}
	e := m.entries[id]
	if e == nil || capability != e.capability {
		return List{}, ErrInvalid
	}
	if expectedVersion == "" {
		return List{}, ErrConflict
	}
	current, err := m.snapshotLocked()
	if err != nil {
		return List{}, err
	}
	if current.Version != expectedVersion {
		return List{}, ErrConflict
	}
	if enabled && (e.factory == nil && e.capability == "connect" || e.info.Implementation == "compatible" && !store.IsMarketOAuth(e.credentialType) && !current.Compatible.Installed) {
		return List{}, pluginhost.ErrNotInstalled
	}
	if err = accessguard.Check(ctx); err != nil {
		return List{}, err
	}
	if _, err = m.store.SetPluginCapability(id, capability, enabled); err != nil {
		return List{}, err
	}
	e.mu.Lock()
	changed := e.enabled != enabled
	if changed {
		e.enabled = enabled
		e.epoch++
		close(e.changed)
		e.changed = make(chan struct{})
	}
	e.mu.Unlock()
	// Callbacks may inspect Enabled/Existing/Epoch; no entry lock is held.
	// Serialize their delivery with management writes so disable notifications
	// cannot arrive after a later enable action.
	if changed && e.capability == "connect" {
		for _, listener := range m.listeners {
			listener(e.credentialType, enabled)
		}
	}
	return m.snapshotLocked()
}

// Source capability epochs are independent from provider connections. Turning
// off a reader invalidates its pending preview, not keys already imported.
func (m *Manager) SourceEpoch(id string) (uint64, error) {
	m.registryMu.RLock()
	e := m.entries[id]
	m.registryMu.RUnlock()
	if e == nil || e.capability != "discover_api_key" {
		return 0, ErrInvalid
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.enabled {
		return 0, ErrDisabled
	}
	return e.epoch, nil
}
func (m *Manager) CheckSourceEpoch(id string, epoch uint64) error {
	current, err := m.SourceEpoch(id)
	if err != nil {
		return err
	}
	if current != epoch {
		return ErrDisabled
	}
	return nil
}

func (m *Manager) registerSources() {
	for _, source := range keydiscovery.Sources() {
		e := &entry{info: Info{ID: source.ID, Name: source.Name, Description: "选择工具后扫描已支持的 API Key 与账号凭证；只读本地配置", Icon: source.Icon, Implementation: "native", Featured: true, Version: "1"},
			capability: "discover_api_key", enabled: true, epoch: 1, changed: make(chan struct{})}
		m.entries[source.ID] = e
		m.ordered = append(m.ordered, e)
	}
}

// InstallCompatible is local-console-only; it is not a remote management tool.
func (m *Manager) InstallCompatible(ctx context.Context, runtimePath string) (List, error) {
	m.manageMu.Lock()
	defer m.manageMu.Unlock()
	if err := accessguard.Check(ctx); err != nil {
		return List{}, err
	}
	if err := m.compatible.Install(ctx, runtimePath); err != nil {
		return List{}, err
	}
	return m.snapshotLocked()
}

// Run supervises the one native background worker. Stop notifications do not
// cancel HTTP contexts: the old worker finishes its current response/write
// before this loop starts another epoch on the same runtime.
func (m *Manager) Run(ctx context.Context) {
	m.runMu.Lock()
	if m.running {
		m.runMu.Unlock()
		return
	}
	m.running = true
	m.runMu.Unlock()
	defer func() {
		m.runMu.Lock()
		m.running = false
		m.runMu.Unlock()
	}()
	e := m.byCredentialType(store.CredentialCodexOAuth)
	for ctx.Err() == nil {
		e.mu.Lock()
		enabled, stop := e.enabled, e.changed
		e.mu.Unlock()
		if !enabled {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				continue
			}
		}
		runtime, guarded, err := m.Native(ctx, e.credentialType)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				continue
			}
		}
		if e.background {
			runtime.Run(guarded, stop)
		}
	}
}
