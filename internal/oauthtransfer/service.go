package oauthtransfer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/store"
)

type Target struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}
type Row struct {
	ID             string   `json:"id"`
	Label          string   `json:"label"`
	CredentialType string   `json:"credential_type"`
	Enabled        bool     `json:"enabled"`
	Status         string   `json:"status"`
	IdentityNote   string   `json:"identity_note"`
	Targets        []Target `json:"targets"`
}
type Preview struct {
	Target    *Target        `json:"target,omitempty"`
	ID        string         `json:"id"`
	ExpiresAt time.Time      `json:"expires_at"`
	Rows      []Row          `json:"rows"`
	Errors    []PreviewError `json:"errors,omitempty"`
}
type PreviewError struct {
	File    int    `json:"file"`
	Message string `json:"message"`
}
type Export struct {
	File     string `json:"file"`
	Filename string `json:"filename"`
	Count    int    `json:"count"`
	Encoding string `json:"encoding,omitempty"`
	MIMEType string `json:"mime_type"`
}
type Selection struct {
	CandidateID     string `json:"candidate_id"`
	TargetChannelID int64  `json:"target_channel_id"`
	ExpectedVersion string `json:"expected_version"`
	Name            string `json:"name,omitempty"`
}
type Result struct {
	CandidateIDs []string `json:"candidate_ids"`
	ChannelID    int64    `json:"channel_id"`
	Added        int      `json:"added"`
	Skipped      int      `json:"skipped"`
	Status       string   `json:"status"`
	Message      string   `json:"message,omitempty"`
}
type ImportResponse struct {
	Results []Result `json:"results"`
	Preview Preview  `json:"preview"`
}

func (r ImportResponse) OperationStatus() string {
	failed, success := false, false
	for _, item := range r.Results {
		if item.Status == "failed" {
			failed = true
		} else {
			success = true
		}
	}
	if failed && success {
		return "partial"
	}
	if failed {
		return "failed"
	}
	return "completed"
}

type preparedItem struct {
	id        string
	value     store.OAuthTransferItem
	completed *store.OAuthTransferItem
}
type session struct {
	mu        sync.Mutex
	id, owner string
	expires   time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	items     []preparedItem
	errors    []PreviewError
	closed    bool
	target    *Target
	kind      string
	completed map[string]Result
}
type Service struct {
	store    *store.Store
	complete Completer
	mu       sync.Mutex
	sessions map[string]*session
}

func New(st *store.Store, complete ...Completer) *Service {
	s := &Service{store: st, sessions: map[string]*session{}}
	if len(complete) > 0 {
		s.complete = complete[0]
	}
	return s
}
func identifier() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrInvalid
	}
	return hex.EncodeToString(b[:]), nil
}
func (s *Service) Export(ctx context.Context, refs []store.OAuthTransferRef) (Export, error) {
	if err := accessguard.Check(ctx); err != nil {
		return Export{}, err
	}
	items, err := s.store.ExportOAuthCredentials(refs)
	if err != nil {
		return Export{}, ErrInvalid
	}
	var files []NativeFile
	total := 0
	for _, item := range items {
		file, err := EncodeNativeCredential(item)
		if err != nil {
			return Export{}, err
		}
		total += len(file.Content)
		if total > MaxTotalBytes {
			return Export{}, ErrLimit
		}
		files = append(files, file)
	}
	if len(files) == 1 {
		return Export{File: files[0].Content, Filename: files[0].Filename, Count: 1, MIMEType: "application/json"}, nil
	}
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for i, file := range files {
		entry, err := writer.Create(fmt.Sprintf("%03d/%s", i+1, file.Filename))
		if err != nil {
			return Export{}, ErrInvalid
		}
		if _, err = entry.Write([]byte(file.Content)); err != nil {
			return Export{}, ErrInvalid
		}
	}
	if writer.Close() != nil {
		return Export{}, ErrInvalid
	}
	return Export{File: base64.StdEncoding.EncodeToString(buf.Bytes()), Filename: "credentials.zip", Count: len(files), Encoding: "base64", MIMEType: "application/zip"}, nil
}
func (s *Service) Prepare(ctx context.Context, owner, file, kind string, targetIDs ...int64) (Preview, error) {
	return s.PrepareFiles(ctx, owner, []string{file}, kind, targetIDs...)
}
func (s *Service) PrepareFiles(ctx context.Context, owner string, files []string, kind string, targetIDs ...int64) (Preview, error) {
	if owner == "" || len(files) == 0 || len(files) > 100 {
		return Preview{}, ErrInvalid
	}
	total := 0
	var items []store.OAuthTransferItem
	var issues []PreviewError
	for i, file := range files {
		total += len(file)
		if total > MaxTotalBytes {
			return Preview{}, ErrLimit
		}
		parsed, err := ParseNativeCredentialsForType(ctx, kind, []byte(file))
		if err != nil {
			if len(files) == 1 {
				return Preview{}, err
			}
			issues = append(issues, PreviewError{File: i + 1, Message: "凭证格式或材料无效"})
			continue
		}
		items = append(items, parsed...)
	}
	if len(items) == 0 {
		return Preview{}, ErrFile
	}
	if len(items) > 100 {
		return Preview{}, ErrLimit
	}
	id, err := identifier()
	if err != nil {
		return Preview{}, err
	}
	p := &session{id: id, owner: owner, expires: time.Now().Add(10 * time.Minute), errors: issues, completed: map[string]Result{}}
	if len(targetIDs) > 1 || len(targetIDs) == 1 && targetIDs[0] < 0 {
		return Preview{}, ErrInvalid
	}
	if len(targetIDs) == 1 && targetIDs[0] > 0 {
		c, err := s.store.Channel(targetIDs[0])
		if err != nil || kind == "" || c.CredentialType != kind {
			return Preview{}, ErrConflict
		}
		p.target, p.kind = &Target{c.ID, c.Name, store.EntityVersion(c)}, kind
		for _, item := range items {
			if item.CredentialType != kind || !transferTargetMatches(item, c) {
				return Preview{}, ErrInvalid
			}
		}
	}
	for _, item := range items {
		identity := store.OAuthTransferMaterialIdentity(item)
		if identity == "" {
			return Preview{}, ErrFile
		}
		rowID, err := identifier()
		if err != nil {
			return Preview{}, err
		}
		p.items = append(p.items, preparedItem{id: rowID, value: item})
	}
	p.ctx, p.cancel = context.WithDeadline(context.Background(), p.expires)
	s.mu.Lock()
	if len(s.sessions) >= 16 {
		s.mu.Unlock()
		p.cancel()
		return Preview{}, ErrLimit
	}
	s.sessions[id] = p
	s.mu.Unlock()
	context.AfterFunc(p.ctx, func() { s.Cancel(owner, id) })
	out, err := s.Get(ctx, owner, id)
	if err != nil {
		s.Cancel(owner, id)
	}
	return out, err
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
func check(ctx context.Context, p *session) error {
	if p.closed || p.ctx.Err() != nil {
		return ErrExpired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return accessguard.Check(ctx)
}
func (s *Service) Cancel(owner, id string) error {
	s.mu.Lock()
	p := s.sessions[id]
	if p == nil || p.owner != owner {
		s.mu.Unlock()
		return nil
	}
	delete(s.sessions, id)
	s.mu.Unlock()
	p.cancel()
	p.mu.Lock()
	p.closed = true
	p.items = nil
	p.mu.Unlock()
	return nil
}
func (s *Service) Get(ctx context.Context, owner, id string) (Preview, error) {
	p, err := s.lookup(owner, id)
	if err != nil {
		return Preview{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := check(ctx, p); err != nil {
		return Preview{}, err
	}
	return s.preview(p)
}
func (s *Service) preview(p *session) (Preview, error) {
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
	identities := identityLinks{parents: map[string]string{}}
	var existingAliases [][]string
	for _, channel := range channels {
		endpoints := append([]store.Endpoint(nil), channel.Endpoints...)
		for index := range endpoints {
			endpoints[index].BaseURL = channel.EndpointBaseURL(endpoints[index])
		}
		for _, credential := range channel.Credentials {
			if credential.OAuth == nil {
				continue
			}
			aliases := store.OAuthTransferMaterialAliases(store.OAuthTransferItem{CredentialType: channel.CredentialType, Endpoints: endpoints, OAuth: credential.OAuth})
			if len(aliases) > 0 {
				identities.link(aliases)
				existingAliases = append(existingAliases, aliases)
			}
		}
	}
	fileAliases := make([][]string, len(p.items))
	for index, item := range p.items {
		fileAliases[index] = store.OAuthTransferMaterialAliases(item.value)
		if len(fileAliases[index]) == 0 {
			return Preview{}, ErrFile
		}
		identities.link(fileAliases[index])
	}
	exists := map[string]bool{}
	for _, aliases := range existingAliases {
		exists[identities.find(aliases[0])] = true
	}
	groupIndexes := map[string]int{}
	var representatives []preparedItem
	var groupKeys []string
	// Preserve the complete file's evidence, including rows already present in
	// the destination. Prefer a copy with stable account identity over its
	// anonymous token-only twin; neither can become a second refresh owner.
	for index, item := range p.items {
		key := identities.find(fileAliases[index][0])
		if slot, ok := groupIndexes[key]; ok {
			old := representatives[slot].value.OAuth
			current := item.value.OAuth
			if old.Account == "" && old.Subject == "" && (current.Account != "" || current.Subject != "") {
				representatives[slot] = item
			}
		} else {
			groupIndexes[key] = len(representatives)
			representatives = append(representatives, item)
			groupKeys = append(groupKeys, key)
		}
	}
	out := Preview{ID: p.id, ExpiresAt: p.expires, Rows: []Row{}, Errors: p.errors, Target: p.target}
	for index, item := range representatives {
		value := item.value
		label := strings.TrimSpace(value.Label)
		if label == "" {
			label = "凭据 " + strconv.Itoa(index+1)
		}
		row := Row{ID: item.id, Label: label, CredentialType: value.CredentialType, Enabled: value.Enabled, Status: "new", Targets: []Target{}}
		if exists[groupKeys[index]] {
			row.Status = "exists"
		}
		if value.CredentialType == store.CredentialCopilotOAuth || value.OAuth.Subject == "" && value.OAuth.Account == "" {
			row.IdentityNote = "仅能识别相同凭证材料；不同令牌可能属于同一账号。"
		}
		identity := store.APIKeyEndpointIdentity(store.Channel{Endpoints: value.Endpoints})
		for _, channel := range channels {
			if p.target == nil || channel.ID != p.target.ID {
				continue
			}
			if channel.CredentialType == value.CredentialType && identity != "" && transferTargetMatches(value, channel) {
				row.Targets = append(row.Targets, Target{channel.ID, channel.Name, store.EntityVersion(channel)})
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}
func (s *Service) Import(ctx context.Context, owner, id string, items []Selection) (ImportResponse, error) {
	p, err := s.lookup(owner, id)
	if err != nil {
		return ImportResponse{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := check(ctx, p); err != nil {
		return ImportResponse{}, err
	}
	if len(items) == 0 || len(items) > 100 {
		return ImportResponse{}, ErrInvalid
	}
	current, err := s.preview(p)
	if err != nil {
		return ImportResponse{}, err
	}
	rows := map[string]Row{}
	values := map[string]store.OAuthTransferItem{}
	for _, row := range current.Rows {
		rows[row.ID] = row
	}
	for _, item := range p.items {
		values[item.id] = item.value
		if item.completed != nil {
			values[item.id] = *item.completed
		}
	}
	type batch struct {
		input store.OAuthTransferImport
		ids   []string
	}
	batches := map[string]*batch{}
	order := []string{}
	seen := map[string]bool{}
	replayed := []Result{}
	for _, selection := range items {
		if p.target != nil && selection.TargetChannelID != 0 && selection.TargetChannelID != p.target.ID || p.target == nil && selection.TargetChannelID != 0 {
			return ImportResponse{}, ErrConflict
		}
		if previous, ok := p.completed[selection.CandidateID]; ok {
			if !seen[previous.CandidateIDs[0]] {
				replayed = append(replayed, previous)
				seen[previous.CandidateIDs[0]] = true
			}
			continue
		}
		if p.target != nil {
			if selection.TargetChannelID != 0 && selection.TargetChannelID != p.target.ID || selection.ExpectedVersion != "" && selection.ExpectedVersion != p.target.Version {
				return ImportResponse{}, ErrConflict
			}
			selection.TargetChannelID, selection.ExpectedVersion = p.target.ID, p.target.Version
		}
		row, ok := rows[selection.CandidateID]
		if !ok || seen[row.ID] || row.Status != "new" {
			return ImportResponse{}, ErrInvalid
		}
		seen[row.ID] = true
		if selection.TargetChannelID == 0 {
			if selection.ExpectedVersion != "" {
				return ImportResponse{}, ErrInvalid
			}
		} else {
			valid := false
			for _, target := range row.Targets {
				if target.ID == selection.TargetChannelID && target.Version == selection.ExpectedVersion {
					valid = true
				}
			}
			if !valid {
				return ImportResponse{}, ErrConflict
			}
		}
		value := values[row.ID]
		if selection.TargetChannelID > 0 {
			destination, err := s.store.Channel(selection.TargetChannelID)
			if err != nil {
				return ImportResponse{}, ErrConflict
			}
			value.Endpoints = append([]store.Endpoint(nil), destination.Endpoints...)
			for i := range value.Endpoints {
				value.Endpoints[i].BaseURL = destination.EndpointBaseURL(value.Endpoints[i])
			}
		}
		key := strconv.FormatInt(selection.TargetChannelID, 10)
		if selection.TargetChannelID == 0 {
			key += ":" + value.CredentialType + ":" + store.APIKeyEndpointIdentity(store.Channel{Endpoints: value.Endpoints})
		}
		b := batches[key]
		if b == nil {
			name := strings.TrimSpace(selection.Name)
			if name == "" {
				name = "导入渠道"
			}
			b = &batch{input: store.OAuthTransferImport{ChannelID: selection.TargetChannelID, ExpectedVersion: selection.ExpectedVersion, Name: name}}
			batches[key] = b
			order = append(order, key)
		}
		b.input.Items = append(b.input.Items, value)
		b.ids = append(b.ids, row.ID)
	}
	out := ImportResponse{Results: replayed}
	for _, key := range order {
		b := batches[key]
		completed, completeErr := s.CompleteImportItems(ctx, b.input.Items)
		var result store.OAuthTransferImportResult
		err := completeErr

		for i, id := range b.ids {
			for j := range p.items {
				if p.items[j].id == id {
					cached := completed[i]
					p.items[j].completed = &cached
				}
			}
		}
		if err == nil {
			b.input.Items = completed
			result, err = s.store.ImportOAuthCredentials(b.input, func() error {
				if err := check(ctx, p); err != nil {
					return err
				}
				// This callback runs under Store's channel lifecycle lock. Include
				// whole-file alias evidence in the last check, not just the selected
				// subset passed to Store, including concurrent destination changes.
				latest, err := s.preview(p)
				if err != nil {
					return err
				}
				for _, id := range b.ids {
					valid := false
					for _, row := range latest.Rows {
						if row.ID == id && row.Status == "new" {
							valid = true
						}
					}
					if !valid {
						return ErrConflict
					}
				}
				return nil
			})
		}
		row := Result{CandidateIDs: b.ids, ChannelID: result.ChannelID, Added: result.Added, Skipped: result.Skipped, Status: "completed"}
		if err != nil {
			row.Status = "failed"
			row.Message = "目标或授权已变化，未写入此批；请重新核对预览。"
			if completeErr != nil {
				row.Message = "未能补齐凭证信息，请检查网络或凭证是否有效后重试。"
			}
		}
		if err == nil || result.Added > 0 {
			for _, id := range b.ids {
				p.completed[id] = row
			}
			if p.target != nil {
				p.target = &Target{p.target.ID, p.target.Name, result.ChannelVersion}
			}
		}
		out.Results = append(out.Results, row)
	}
	out.Preview, err = s.preview(p)
	if err != nil {
		return out, err
	}
	return out, nil
}

type identityLinks struct{ parents map[string]string }

func (u *identityLinks) find(key string) string {
	parent, ok := u.parents[key]
	if !ok {
		u.parents[key] = key
		return key
	}
	if parent != key {
		u.parents[key] = u.find(parent)
	}
	return u.parents[key]
}
func (u *identityLinks) link(aliases []string) {
	if len(aliases) == 0 {
		return
	}
	root := u.find(aliases[0])
	for _, alias := range aliases[1:] {
		other := u.find(alias)
		if other != root {
			u.parents[other] = root
		}
	}
}

// Native files carry credentials, so use the provider URL to offer a target;
// selection retains that channel's actual protocol declarations unchanged.
func transferTargetMatches(item store.OAuthTransferItem, c store.Channel) bool {
	for _, incoming := range item.Endpoints {
		found := false
		for _, existing := range c.Endpoints {
			if store.DiscoveryURLIdentity(incoming.BaseURL) == store.DiscoveryURLIdentity(c.EndpointBaseURL(existing)) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return len(item.Endpoints) > 0
}
