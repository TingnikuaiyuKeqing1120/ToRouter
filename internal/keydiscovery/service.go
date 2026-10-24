package keydiscovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/oauthtransfer"
	"torouter/internal/store"
)

type PluginAccess interface {
	SourceEpoch(string) (uint64, error)
	CheckSourceEpoch(string, uint64) error
}

type Service struct {
	complete func(context.Context, []store.OAuthTransferItem) ([]store.OAuthTransferItem, error)
	rootFor  func(string) (string, error)
	store    *store.Store
	plugins  PluginAccess
	origin   string
	mu       sync.Mutex
	sessions map[string]*session
}

type sourceScan struct {
	source     Source
	root       string
	resources  map[string]map[string]string
	epoch      uint64
	status     ScanStatus
	candidates []Candidate
}

type privateRow struct {
	oauth     *store.OAuthTransferItem
	key       string
	endpoints []store.Endpoint
}

type session struct {
	mu        sync.Mutex
	id, owner string
	expires   time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	sources   map[string]*sourceScan
	rowIDs    map[string]string
	rows      map[string]privateRow
	closed    bool
	target    *Target
	kind      string
	completed map[string]ImportResult
}

func New(st *store.Store, plugins PluginAccess, origin string, completers ...func(context.Context, []store.OAuthTransferItem) ([]store.OAuthTransferItem, error)) *Service {
	out := &Service{store: st, plugins: plugins, origin: origin, sessions: map[string]*session{}}
	if len(completers) > 0 {
		out.complete = completers[0]
	}
	return out
}

func opaqueID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("secure random unavailable")
	}
	return hex.EncodeToString(b[:])
}

func (s *Service) Sources() ([]SourceStatus, error) {
	out := []SourceStatus{}
	for _, source := range Sources() {
		_, err := s.plugins.SourceEpoch(source.ID)
		out = append(out, SourceStatus{Source: source, Enabled: err == nil})
	}
	return out, nil
}

func defaultRoot(id string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrInvalid
	}
	switch id {
	case "source-codex":
		if v := os.Getenv("CODEX_HOME"); v != "" {
			return v, nil
		}
		return filepath.Join(home, ".codex"), nil
	case "source-claude":
		if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
			return v, nil
		}
		return filepath.Join(home, ".claude"), nil
	case "source-opencode":
		if v := os.Getenv("OPENCODE_CONFIG_DIR"); v != "" {
			return v, nil
		}
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			return filepath.Join(v, "opencode"), nil
		}
		return filepath.Join(home, ".config", "opencode"), nil
	case "source-copilot":
		// Copilot language server 1.408.0 (github/copilot.vim): absolute XDG
		// override, otherwise USERPROFILE/AppData/Local on Windows, ~/.config elsewhere.
		if v := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(v) {
			return filepath.Join(v, "github-copilot"), nil
		}
		if runtime.GOOS == "windows" {
			return filepath.Join(home, "AppData", "Local", "github-copilot"), nil
		}
		return filepath.Join(home, ".config", "github-copilot"), nil
	case "source-zcode":
		return filepath.Join(home, ".zcode", "v2"), nil
	case "source-ccswitch":
		return filepath.Join(home, ".cc-switch"), nil
	case "source-magpie":
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			return filepath.Join(v, "magpie"), nil
		}
		return filepath.Join(home, ".config", "magpie"), nil
	case "source-codex-manager":
		if v := os.Getenv("CODEXMANAGER_DB_PATH"); filepath.IsAbs(v) {
			return filepath.Dir(v), nil
		}
		if runtime.GOOS == "windows" {
			if appData := os.Getenv("APPDATA"); appData != "" {
				return filepath.Join(appData, "com.codexmanager.desktop"), nil
			}
			return filepath.Join(home, "AppData", "Roaming", "com.codexmanager.desktop"), nil
		}
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, "Library", "Application Support", "com.codexmanager.desktop"), nil
		}
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "com.codexmanager.desktop"), nil
		}
		return filepath.Join(home, ".local", "share", "com.codexmanager.desktop"), nil
	}
	return "", ErrInvalid
}

func (s *Service) Begin(ctx context.Context, owner string, ids []string, options ...BeginOptions) (Preview, error) {
	if len(ids) < 1 || len(ids) > len(Sources()) || owner == "" {
		return Preview{}, ErrInvalid
	}
	if err := accessguard.Check(ctx); err != nil {
		return Preview{}, err
	}
	child, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	p := &session{id: opaqueID(), owner: owner, expires: time.Now().Add(10 * time.Minute), ctx: child, cancel: cancel, sources: map[string]*sourceScan{}, rowIDs: map[string]string{}, rows: map[string]privateRow{}}
	p.completed = map[string]ImportResult{}
	if len(options) > 1 {
		cancel()
		return Preview{}, ErrInvalid
	}
	if len(options) == 1 {
		o := options[0]
		if o.TargetChannelID < 0 || o.TargetChannelID == 0 && o.CredentialType != "" {
			cancel()
			return Preview{}, ErrInvalid
		}
		if o.TargetChannelID > 0 {
			c, err := s.store.Channel(o.TargetChannelID)
			if err != nil || o.CredentialType == "" || c.CredentialType != o.CredentialType {
				cancel()
				return Preview{}, ErrInvalid
			}
			p.target = &Target{c.ID, c.Name, store.EntityVersion(c)}
			p.kind = c.CredentialType
		}
	}
	for _, id := range ids {
		source, ok := Lookup(id)
		if !ok {
			cancel()
			return Preview{}, ErrInvalid
		}
		epoch, err := s.plugins.SourceEpoch(id)
		if err != nil {
			cancel()
			return Preview{}, err
		}
		resolver := s.rootFor
		if resolver == nil {
			resolver = defaultRoot
		}
		root, err := resolver(id)
		if err != nil {
			cancel()
			return Preview{}, err
		}
		p.sources[id] = &sourceScan{source: source, root: root, epoch: epoch, status: ScanStatus{ID: id, Name: source.Name, Status: "pending", Diagnostics: []Diagnostic{}}}

	}
	s.mu.Lock()
	if len(s.sessions) >= 16 {
		s.mu.Unlock()
		cancel()
		return Preview{}, ErrInvalid
	}
	s.sessions[p.id] = p
	s.mu.Unlock()
	context.AfterFunc(child, func() { s.Cancel(owner, p.id) })
	return s.Get(ctx, owner, p.id)
}

func (s *Service) lookup(owner, id string) (*session, error) {
	s.mu.Lock()
	p := s.sessions[id]
	s.mu.Unlock()
	if p == nil || p.owner != owner {
		return nil, ErrExpired
	}
	return p, nil
}

func (s *Service) check(ctx context.Context, p *session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.closed || p.ctx.Err() != nil {
		return ErrExpired
	}
	if err := accessguard.Check(ctx); err != nil {
		return err
	}
	for id, scan := range p.sources {
		if s.plugins.CheckSourceEpoch(id, scan.epoch) != nil {
			return ErrConflict
		}

	}
	return nil
}

func (s *Service) Cancel(owner, id string) error {
	s.mu.Lock()
	p := s.sessions[id]
	if p != nil && p.owner == owner {
		delete(s.sessions, id)
	} else {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	p.cancel()
	p.mu.Lock()
	p.closed = true
	p.rows = nil
	p.rowIDs = nil
	for _, scan := range p.sources {
		scan.candidates = nil
	}
	p.mu.Unlock()
	return nil
}

func (s *Service) Scan(ctx context.Context, owner, id, sourceID string) (Preview, error) {
	p, err := s.lookup(owner, id)
	if err != nil {
		return Preview{}, err
	}
	p.mu.Lock()
	if err = s.check(ctx, p); err != nil {
		p.mu.Unlock()
		return Preview{}, err
	}
	scan := p.sources[sourceID]
	if scan == nil || scan.status.Status == "scanning" {
		p.mu.Unlock()
		return Preview{}, ErrInvalid
	}
	if scan.status.Status != "pending" {
		out, err := s.preview(p)
		p.mu.Unlock()
		return out, err
	}
	scan.status.Status = "scanning"
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	guard := func() error { p.mu.Lock(); defer p.mu.Unlock(); return s.check(ctx, p) }
	resources, readErr := newResources(scan.root, scan.source, guard)
	if resources != nil {
		defer resources.Close()
	}
	var candidates []Candidate
	var diagnostics []Diagnostic
	var files map[string]string
	openCodeEndpoints := map[string][]store.Endpoint{}
	snapshots := map[string]map[string]string{}
	if readErr == nil && sourceID == "source-zcode" {
		folder, e := zcodeConfigRoot(ctx, resources, scan.root)
		if e != nil {
			readErr = e
		} else if folder != scan.root {
			snapshots[scan.root] = resources.Fingerprints()
			resources.Close()
			resources, readErr = newResources(folder, scan.source, guard)
			if readErr == nil {
				defer resources.Close()
				snapshots[folder] = nil
			}
		}
	}
	if readErr == nil {
		switch sourceID {
		case "source-codex":
			candidates, diagnostics, readErr = readCodex(ctx, resources)
		case "source-claude":
			candidates, diagnostics, readErr = readClaude(ctx, resources)
		case "source-copilot":
			candidates, diagnostics, readErr = readCopilot(ctx, resources)
		case "source-opencode":
			candidates, diagnostics, readErr = readOpenCode(ctx, resources)
			openCodeEndpoints, err = openCodeConfiguredEndpoints(ctx, resources)
			if err != nil {
				readErr = err
			}
		case "source-zcode":
			candidates, diagnostics, readErr = readZCode(ctx, resources)
		case "source-ccswitch":
			candidates, diagnostics, readErr = readCCSwitch(ctx, resources)
		case "source-codex-manager":
			candidates, diagnostics, readErr = readCodexManager(ctx, resources)
		case "source-magpie":
			candidates, diagnostics, readErr = readMagpie(ctx, resources)
		}
		if sourceID == "source-magpie" || sourceID == "source-codex-manager" || sourceID == "source-ccswitch" {
			cs, ds, e := readGatewayOAuth(ctx, resources, sourceID)
			candidates = append(candidates, cs...)
			diagnostics = append(diagnostics, ds...)
			if e != nil {
				readErr = e
			} else if len(cs) > 0 {
				readErr = nil
			}
		}
		files = resources.Fingerprints()
		snapshots[resources.rootPath] = files
		if closeErr := resources.Close(); closeErr != nil && readErr == nil {
			readErr = closeErr
		}
	}
	if sourceID == "source-opencode" {
		root := openCodeDataRoot()
		if s.rootFor != nil {
			root = scan.root
		}
		extra, e := newResources(root, scan.source, guard)
		if e == nil {
			cs, ds, e := readOpenCodeAuth(ctx, extra, openCodeEndpoints)
			candidates = append(candidates, cs...)
			diagnostics = append(diagnostics, ds...)
			if snapshots[root] == nil {
				snapshots[root] = map[string]string{}
			}
			for name, value := range extra.Fingerprints() {
				snapshots[root][name] = value
			}
			extra.Close()
			if e != nil {
				readErr = e
			}
		} else if !errors.Is(e, ErrMissing) {
			diagnostics = append(diagnostics, diagnostic("invalid_format"))
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err = s.check(ctx, p); err != nil {
		scan.status.Status = "error"
		d := diagnostic("invalid_format")
		d.SourceID = sourceID
		scan.status.Diagnostics = []Diagnostic{d}
		return Preview{}, err
	}
	scan.status.Status = "complete"
	keyBytes := 0
	for _, candidate := range candidates {
		keyBytes += len(candidate.Key)
		if candidate.OAuth != nil {
			b, _ := json.Marshal(candidate.OAuth)
			keyBytes += len(b)
		}
	}
	if len(candidates) > 1000 || keyBytes > 4<<20 {
		candidates = nil
		readErr = ErrInvalid
		diagnostics = append(diagnostics, diagnostic("limit"))
	}
	if errors.Is(readErr, ErrMissing) {
		readErr = nil
		diagnostics = append(diagnostics, diagnostic("not_found"))
	}
	if readErr != nil {
		scan.status.Status = "error"
		candidates = nil
		if len(diagnostics) == 0 {
			diagnostics = append(diagnostics, diagnostic("invalid_format"))
		}
	}
	for i := range diagnostics {
		diagnostics[i].SourceID = sourceID
	}
	scan.status.Diagnostics = diagnostics
	scan.resources = snapshots
	for _, candidate := range candidates {
		accepted, code := s.accept(candidate)
		if code != "" {
			d := diagnostic(code)
			d.SourceID = sourceID
			scan.status.Diagnostics = append(scan.status.Diagnostics, d)
			continue
		}
		scan.candidates = append(scan.candidates, accepted)
	}
	return s.preview(p)
}

func normalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.User != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		host := u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		u.Host = host
	}
	return u.String()
}

func (s *Service) accept(c Candidate) (Candidate, string) {
	if c.OAuth != nil {
		if store.ValidateOAuthTransferMaterial(*c.OAuth) != nil {
			return Candidate{}, "incomplete"
		}
		c.Endpoints = append([]store.Endpoint(nil), c.OAuth.Endpoints...)
		c.Disabled = !c.OAuth.Enabled
		return c, ""
	}
	if len(c.Key) == 0 || len(c.Key) > 16384 || strings.TrimSpace(c.Key) == "" || strings.ContainsAny(c.Key, "\r\n\x00") {
		return Candidate{}, "incomplete"
	}
	switch c.Key {
	case "magpie", "PROXY_MANAGED", "copilot_placeholder", "codex_oauth_placeholder", "xai_oauth_placeholder":
		return Candidate{}, "proxy_skipped"
	}
	if strings.HasPrefix(c.Key, "enc:") || strings.Contains(c.Key, "{env:") || strings.Contains(c.Key, "${") || strings.Contains(c.Key, "{file:") {
		return Candidate{}, "reference_skipped"
	}
	if len(c.Endpoints) == 0 || len(c.Endpoints) > 4 {
		return Candidate{}, "unsupported"
	}
	for i := range c.Endpoints {
		ep := &c.Endpoints[i]
		ep.BaseURL = normalizeURL(ep.BaseURL)
		if ep.BaseURL == "" {
			return Candidate{}, "unsupported"
		}
		if sameOrigin(ep.BaseURL, s.origin) {
			return Candidate{}, "proxy_skipped"
		}
	}
	test := store.Channel{Name: "Imported", CredentialType: store.CredentialAPIKey, APIKey: c.Key, Endpoints: c.Endpoints, Enabled: true}
	test.Normalize()
	if store.ValidateChannel(test) != nil {
		return Candidate{}, "unsupported"
	}
	return c, ""
}

func sameOrigin(a, b string) bool {
	x, e1 := url.Parse(a)
	y, e2 := url.Parse(b)
	if e1 != nil || e2 != nil {
		return false
	}
	host := func(u *url.URL) string {
		h := strings.ToLower(u.Hostname())
		if h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.") {
			h = "loopback"
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		return u.Scheme + "://" + h + ":" + port
	}
	return host(x) == host(y)
}

func (s *Service) Get(ctx context.Context, owner, id string) (Preview, error) {
	p, err := s.lookup(owner, id)
	if err != nil {
		return Preview{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err = s.check(ctx, p); err != nil {
		return Preview{}, err
	}
	return s.preview(p)
}

func candidateKind(c Candidate) string {
	if c.OAuth != nil {
		return c.OAuth.CredentialType
	}
	return store.CredentialAPIKey
}
func candidateIdentity(c Candidate) string {
	if c.OAuth != nil {
		item := *c.OAuth
		o := item.OAuth
		// A Codex user alone does not identify its workspace. Only the complete
		// user/workspace pair joins full and refresh-only copies across sources.
		if item.CredentialType == store.CredentialCodexOAuth {
			if o.Subject != "" && o.Account != "" {
				return store.OAuthTransferMaterialIdentity(item)
			}
		} else if identity := store.OAuthTransferIdentity(item); identity != "" {
			return identity
		}
		material := *o
		material.ExpiresAt = time.Time{}
		material.UpdatedAt = time.Time{}
		if !store.IsCompatibleOAuth(item.CredentialType) {
			material.PluginState = ""
		}
		material.AccountLabel = ""
		return store.EntityVersion([]any{item.CredentialType, material})
	}
	return store.EntityVersion([]string{store.CredentialAPIKey, c.Key})
}

func privateKind(row privateRow) string {
	if row.oauth != nil {
		return row.oauth.CredentialType
	}
	return store.CredentialAPIKey
}
func (s *Service) preview(p *session) (Preview, error) {
	out := Preview{ID: p.id, ExpiresAt: p.expires, Sources: []ScanStatus{}, Groups: []Group{}, Target: p.target, CredentialType: p.kind}
	type merged struct {
		privateRow
		sources  map[string]SourceRef
		disabled bool
		exists   bool
	}
	byURL := map[string]map[string]*merged{}
	channels, err := s.store.ListChannels()
	if err != nil {
		return Preview{}, ErrInvalid
	}
	if p.target != nil {
		c, err := s.store.Channel(p.target.ID)
		if err != nil || c.CredentialType != p.kind || store.EntityVersion(c) != p.target.Version {
			return Preview{}, ErrConflict
		}
	}
	existingKeys := map[string]map[string]bool{}
	existingOAuth := map[string]bool{}
	for _, channel := range channels {
		eps := append([]store.Endpoint(nil), channel.Endpoints...)
		for i := range eps {
			eps[i].BaseURL = channel.EndpointBaseURL(eps[i])
		}
		for _, cred := range channel.Credentials {
			if channel.CredentialType == store.CredentialAPIKey {
				for _, ep := range eps {
					address := normalizeURL(ep.BaseURL)
					if existingKeys[address] == nil {
						existingKeys[address] = map[string]bool{}
					}
					existingKeys[address][cred.APIKey] = true
				}
			} else if cred.OAuth != nil {
				item := store.OAuthTransferItem{CredentialType: channel.CredentialType, Endpoints: eps, OAuth: cred.OAuth}
				existingOAuth[candidateIdentity(Candidate{OAuth: &item})] = true
			}
		}
	}
	for _, source := range Sources() {
		scan := p.sources[source.ID]
		if scan == nil {
			continue
		}
		status := scan.status
		status.Diagnostics = append([]Diagnostic{}, status.Diagnostics...)
		out.Sources = append(out.Sources, status)
		for _, candidate := range scan.candidates {
			if p.target != nil && candidateKind(candidate) != p.kind {
				continue
			}
			exists := false
			if candidate.OAuth != nil {
				exists = existingOAuth[candidateIdentity(candidate)]
			}
			for _, ep := range candidate.Endpoints {
				address := normalizeURL(ep.BaseURL)
				if p.target != nil && store.ValidateDiscoveryTarget(channels, address, p.target.ID) != nil {
					continue
				}
				if address == "" {
					continue
				}
				if exists || candidate.OAuth == nil && existingKeys[address][candidate.Key] {
					continue
				}
				groupKey := address + "\x00" + candidateKind(candidate)
				if candidate.OAuth != nil {
					groupKey += "\x00" + store.APIKeyEndpointIdentity(store.Channel{Endpoints: candidate.Endpoints})
				}
				if byURL[groupKey] == nil {
					byURL[groupKey] = map[string]*merged{}
				}
				identity := candidateIdentity(candidate)
				row := byURL[groupKey][identity]
				if row == nil {
					row = &merged{privateRow: privateRow{key: candidate.Key, oauth: candidate.OAuth}, sources: map[string]SourceRef{}, disabled: candidate.Disabled}
					byURL[groupKey][identity] = row
				}
				if row.oauth != nil && candidate.OAuth != nil && (row.oauth.OAuth.AccessToken == "" && candidate.OAuth.OAuth.AccessToken != "" || store.ValidateOAuthTransferItem(*row.oauth) != nil && store.ValidateOAuthTransferItem(*candidate.OAuth) == nil) {
					// Keep the complete usable copy so selecting an already valid
					// token does not rotate a weaker copy from another source.
					row.oauth = candidate.OAuth
				}
				row.sources[source.ID] = SourceRef{source.ID, source.Name, source.Icon}
				row.disabled = row.disabled && candidate.Disabled
				found := false
				for _, old := range row.endpoints {
					if old.Format == ep.Format {
						found = true
					}
				}
				if !found {
					ep.BaseURL = address
					row.endpoints = append(row.endpoints, ep)
				}
			}
		}
	}
	addresses := make([]string, 0, len(byURL))
	for address := range byURL {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	p.rows = map[string]privateRow{}
	for _, groupKey := range addresses {
		address := strings.SplitN(groupKey, "\x00", 2)[0]
		group := Group{ID: "url-" + store.EntityVersion(groupKey), URLDisplay: address, Formats: []string{}, Rows: []Row{}, Targets: []Target{}}
		if p.target != nil {
			group.Targets = append(group.Targets, *p.target)
			group.TargetCount = 1
			group.Existing = true
		}
		keys := make([]string, 0, len(byURL[address]))
		for key := range byURL[groupKey] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := byURL[groupKey][key]
			lookup := groupKey + "\x00" + key
			if p.rowIDs[lookup] == "" {
				p.rowIDs[lookup] = opaqueID()
			}
			id := p.rowIDs[lookup]
			p.rows[id] = item.privateRow
			kind := privateKind(item.privateRow)
			mask := store.MaskKey(item.key)
			if item.oauth != nil {
				mask = store.MaskKey(item.oauth.OAuth.AccessToken)
				if item.oauth.OAuth.AccessToken == "" {
					mask = store.MaskKey(item.oauth.OAuth.RefreshToken)
				}
				if item.oauth.OAuth.GitHubToken != "" {
					mask = store.MaskKey(item.oauth.OAuth.GitHubToken)
				}
			}
			row := Row{ID: id, CredentialType: kind, MaskedKey: mask, Status: "new", Disabled: item.disabled, Formats: []string{}, Targets: append([]Target{}, group.Targets...), Sources: []SourceRef{}}
			for _, source := range item.sources {
				row.Sources = append(row.Sources, source)
			}
			sort.Slice(row.Sources, func(i, j int) bool { return row.Sources[i].ID < row.Sources[j].ID })

			for _, ep := range item.endpoints {
				row.Formats = append(row.Formats, ep.Format)
				if !contains(group.Formats, ep.Format) {
					group.Formats = append(group.Formats, ep.Format)
				}
			}
			sort.Strings(row.Formats)
			group.Rows = append(group.Rows, row)
		}
		sort.Strings(group.Formats)
		out.Groups = append(out.Groups, group)
	}
	return out, nil
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (s *Service) verify(ctx context.Context, p *session) error {
	if err := s.check(ctx, p); err != nil {
		return err
	}
	for _, scan := range p.sources {
		for root, files := range scan.resources {
			if err := verifyFiles(ctx, root, scan.source, files, func() error { return s.check(ctx, p) }); err != nil {
				return ErrConflict
			}
		}
	}
	return s.check(ctx, p)
}

func (s *Service) Import(ctx context.Context, owner, id string, items []Selection) (ImportResponse, error) {
	p, err := s.lookup(owner, id)
	if err != nil {
		return ImportResponse{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err = s.check(ctx, p); err != nil {
		return ImportResponse{}, err
	}
	if len(items) == 0 || len(items) > 100 {
		return ImportResponse{}, ErrInvalid
	}
	for _, scan := range p.sources {
		if scan.status.Status == "pending" || scan.status.Status == "scanning" {
			return ImportResponse{}, ErrInvalid
		}
	}
	preview, err := s.preview(p)
	if err != nil {
		return ImportResponse{}, err
	}
	groups := map[string]Group{}
	for _, group := range preview.Groups {
		for _, row := range group.Rows {
			groups[row.ID] = group
		}
	}
	type batch struct {
		input store.APIKeyImport
		oauth []store.OAuthTransferItem
		ids   []string
	}
	batches := map[string]*batch{}
	order := []string{}
	seen := map[string]bool{}
	replayed := []ImportResult{}
	for _, selected := range items {
		if p.target != nil && selected.TargetChannelID != 0 && selected.TargetChannelID != p.target.ID || p.target == nil && selected.TargetChannelID != 0 {
			return ImportResponse{}, ErrConflict
		}
		if previous, ok := p.completed[selected.CandidateID]; ok {
			if !seen[previous.CandidateID] {
				replayed = append(replayed, previous)
				seen[previous.CandidateID] = true
			}
			continue
		}
		row, ok := p.rows[selected.CandidateID]
		group := groups[selected.CandidateID]
		if !ok || seen[selected.CandidateID] || group.CopyOnly {
			return ImportResponse{}, ErrInvalid
		}
		seen[selected.CandidateID] = true
		if selected.TargetChannelID < 0 {
			return ImportResponse{}, ErrInvalid
		}
		if p.target != nil {
			target := *p.target
			if selected.TargetChannelID != 0 && selected.TargetChannelID != target.ID || selected.ExpectedVersion != "" && selected.ExpectedVersion != target.Version {
				return ImportResponse{}, ErrConflict
			}
			selected.TargetChannelID, selected.ExpectedVersion = target.ID, target.Version
		} else if selected.TargetChannelID != 0 || selected.ExpectedVersion != "" {
			return ImportResponse{}, ErrConflict
		}
		key := group.ID
		b := batches[key]
		if b == nil {
			name := strings.TrimSpace(selected.Name)
			if name == "" {
				u, _ := url.Parse(group.URLDisplay)
				name = u.Hostname()
			}
			endpoints := []store.Endpoint{}
			enabled := true
			b = &batch{input: store.APIKeyImport{ChannelID: selected.TargetChannelID, ExpectedVersion: selected.ExpectedVersion, Name: name, Endpoints: endpoints, Enabled: &enabled, DisplayIcon: selected.DisplayIcon, DiscoveryURL: group.URLDisplay}}
			batches[key] = b
			order = append(order, key)
		} else if b.input.DisplayIcon != selected.DisplayIcon || strings.TrimSpace(selected.Name) != "" && b.input.Name != strings.TrimSpace(selected.Name) {
			return ImportResponse{}, ErrInvalid
		}
		if row.oauth == nil {
			b.input.Keys = append(b.input.Keys, row.key)
		} else {
			item := *row.oauth
			item.Enabled = true
			b.oauth = append(b.oauth, item)
		}
		for _, ep := range row.endpoints {
			found := false
			for _, old := range b.input.Endpoints {
				found = found || old.Format == ep.Format
			}
			if !found {
				b.input.Endpoints = append(b.input.Endpoints, ep)
			}
		}
		b.ids = append(b.ids, selected.CandidateID)
	}
	guard := func() error { return s.verify(ctx, p) }
	if err = guard(); err != nil {
		return ImportResponse{}, err
	}
	out := ImportResponse{Results: replayed}
	for _, key := range order {
		b := batches[key]
		result := store.APIKeyImportResult{}
		if len(b.oauth) > 0 {
			err = nil
			if s.complete != nil {
				original := append([]store.OAuthTransferItem(nil), b.oauth...)
				b.oauth, err = s.complete(ctx, b.oauth)
				if len(b.oauth) != len(original) {
					err = ErrInvalid
				}
				if len(b.oauth) == len(original) {
					for i, old := range original {
						oldIdentity := candidateIdentity(Candidate{OAuth: &old})
						for _, scan := range p.sources {
							for j := range scan.candidates {
								candidate := &scan.candidates[j]
								if candidate.OAuth != nil && candidateIdentity(*candidate) == oldIdentity {
									updated := b.oauth[i]
									candidate.OAuth = &updated
									candidate.Endpoints = append([]store.Endpoint(nil), updated.Endpoints...)
									newIdentity := candidateIdentity(*candidate)
									for lookup, rowID := range p.rowIDs {
										if strings.HasSuffix(lookup, "\x00"+oldIdentity) {
											p.rowIDs[strings.TrimSuffix(lookup, oldIdentity)+newIdentity] = rowID
										}
									}
								}
							}
						}
					}
				}
			}
			if err == nil {
				value, e := s.store.ImportOAuthCredentials(store.OAuthTransferImport{ChannelID: b.input.ChannelID, ExpectedVersion: b.input.ExpectedVersion, Name: b.input.Name, Items: b.oauth, Enabled: b.input.Enabled, DisplayIcon: b.input.DisplayIcon, DiscoveryURL: b.input.DiscoveryURL}, guard)
				result = store.APIKeyImportResult{ChannelID: value.ChannelID, ChannelVersion: value.ChannelVersion, Added: value.Added, Skipped: value.Skipped}
				err = e
			}
		} else {
			result, err = s.store.ImportAPIKeys(b.input, guard)
		}
		item := ImportResult{CandidateID: b.ids[0], CandidateIDs: b.ids, ChannelID: result.ChannelID, Added: result.Added, Skipped: result.Skipped, Status: "imported"}
		if err != nil {
			item.Status = "failed"
			item.Message = "来源或目标已变化，请重新扫描后核对。"
		} else if result.Added == 0 {
			item.Status = "skipped"
		}
		if err == nil || result.Added > 0 {
			for _, id := range b.ids {
				p.completed[id] = item
			}
			if p.target != nil {
				p.target = &Target{p.target.ID, p.target.Name, result.ChannelVersion}
			}
		}
		out.Results = append(out.Results, item)
	}
	out.Preview, err = s.preview(p)
	return out, err
}

func (s *Service) Copy(ctx context.Context, owner, id string, ids []string) (CopyResponse, error) {
	p, err := s.lookup(owner, id)
	if err != nil {
		return CopyResponse{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(ids) == 0 || len(ids) > 100 {
		return CopyResponse{}, ErrInvalid
	}
	if err = s.verify(ctx, p); err != nil {
		return CopyResponse{}, err
	}
	preview, err := s.preview(p)
	if err != nil {
		return CopyResponse{}, err
	}
	rows := map[string]CopyItem{}
	for _, group := range preview.Groups {
		for _, row := range group.Rows {
			rows[row.ID] = CopyItem{CredentialType: row.CredentialType, URL: group.URLDisplay, Formats: row.Formats, Sources: row.Sources}
		}
	}
	out := CopyResponse{Items: []CopyItem{}}
	seen := map[string]bool{}
	for _, id := range ids {
		value, ok := rows[id]
		if !ok || seen[id] {
			return CopyResponse{}, ErrInvalid
		}
		seen[id] = true
		row := p.rows[id]
		if row.oauth == nil {
			value.APIKey = row.key
		} else {
			native, e := oauthtransfer.EncodeNativeCredential(*row.oauth)
			if e != nil {
				return CopyResponse{}, ErrInvalid
			}
			value.Filename = native.Filename
			value.Content = native.Content
		}
		out.Items = append(out.Items, value)
	}
	if err = s.check(ctx, p); err != nil {
		return CopyResponse{}, err
	}
	return out, nil
}
