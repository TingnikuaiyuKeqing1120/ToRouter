package quota

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

const maxEntries = 1024

type cacheKey struct {
	channel    int64
	credential string
}

type cacheEntry struct {
	identity    [32]byte
	observation Observation
	done        chan struct{}
}

// Service only performs upstream reads when Check is explicitly called. It has
// no background workers and its bounded cache contains no credential material.
type Service struct {
	store      *store.Store
	codex      *codex.Service
	plugins    *plugins.Manager
	client     *http.Client
	mu         sync.Mutex
	entries    map[cacheKey]*cacheEntry
	lastPrune  time.Time
	resetEpoch uint64
}

func New(st *store.Store, codexService *codex.Service, client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	passive := *client
	passive.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Service{store: st, codex: codexService, client: &passive, entries: make(map[cacheKey]*cacheEntry)}
}

func NewWithPlugins(st *store.Store, manager *plugins.Manager, client *http.Client) *Service {
	s := New(st, nil, client)
	s.plugins = manager
	return s
}

const codexPluginDisabled = "Codex 插件已停用，额度自动刷新已暂停"

func (s *Service) codexAccess(ctx context.Context) (*codex.Service, context.Context, error) {
	if s.plugins == nil {
		return s.codex, ctx, nil
	}
	native, guarded, err := s.plugins.Native(ctx, store.CredentialCodexOAuth)
	if err != nil {
		return nil, guarded, err
	}
	return native.Codex, guarded, nil
}

func quotaAccessError(err error) string {
	if errors.Is(err, plugins.ErrDisabled) {
		return "Codex 插件已停用或启用状态已变化"
	}
	if accessguard.Denied(err) {
		return err.Error()
	}
	return "Codex 额度查询失败，请检查账号授权或网络"
}

// Supported is a local identity check, with no upstream requests.
func (s *Service) Supported(c store.Channel, cred store.CredentialItem) bool {
	return Supported(c, cred)
}

// Supported identifies the two implemented sources without making a request.
func Supported(c store.Channel, cred store.CredentialItem) bool {
	return c.CredentialType == store.CredentialCodexOAuth && cred.OAuth != nil || deepSeekTarget(c, cred)
}

func (s *Service) empty(c store.Channel, cred store.CredentialItem) Observation {
	return Observation{ChannelID: c.ID, Generation: c.Generation, CredentialID: cred.ID, Supported: s.Supported(c, cred)}
}

// identity follows configuration and account changes, while allowing a bearer
// rotation for an identified Codex account to retain its snapshot.
func identity(c store.Channel, cred store.CredentialItem) [32]byte {
	var oauth any
	if o := cred.OAuth; o != nil {
		fallback := ""
		if o.Account == "" && o.Subject == "" {
			fallback = o.RefreshToken
		}
		oauth = []string{o.Provider, o.ClientID, o.Account, o.Subject, o.APIBase, fallback}
	}
	raw, _ := json.Marshal([]any{c.Generation, c.CredentialType, c.BaseURL, c.Endpoints, cred.ID, cred.APIKey, oauth})
	return sha256.Sum256(raw)
}

func (s *Service) current(c store.Channel, cred store.CredentialItem) bool {
	if s.store == nil {
		return false
	}
	live, err := s.store.Channel(c.ID)
	if err != nil {
		return false
	}
	for _, member := range live.Credentials {
		if member.ID == cred.ID {
			return identity(live, member) == identity(c, cred)
		}
	}
	return false
}

func (s *Service) interval() time.Duration {
	seconds := store.DefaultQuotaRefreshSeconds
	if s.store != nil {
		if settings, err := s.store.GetSettings(); err == nil && settings.QuotaRefreshSeconds != nil {
			seconds = *settings.QuotaRefreshSeconds
		}
	}
	return time.Duration(seconds) * time.Second
}

func copyObservation(o Observation) Observation {
	o.Balances = append([]Balance(nil), o.Balances...)
	if o.Available != nil {
		v := *o.Available
		o.Available = &v
	}
	return o
}

func freshObservation(o Observation, interval time.Duration, now time.Time) Observation {
	if o.CheckedAt.IsZero() {
		return o
	}
	expires := o.ExpiresAt
	if interval > 0 {
		configured := o.CheckedAt.Add(interval)
		if expires.IsZero() || configured.Before(expires) {
			expires = configured
		}
	}
	o.ExpiresAt = expires
	if !expires.IsZero() && !now.Before(expires) {
		o.Available, o.Reason = nil, ""
	}
	return copyObservation(o)
}

// prune is opportunistic: deleted/replaced entities disappear on the next read
// after a minute. A hard limit also bounds rapid configuration churn.
func (s *Service) prune(now time.Time) {
	s.mu.Lock()
	due := now.Sub(s.lastPrune) >= time.Minute
	if due {
		s.lastPrune = now
	}
	s.mu.Unlock()
	if due && s.store != nil {
		channels, err := s.store.ListChannels()
		if err != nil {
			return
		}
		valid := make(map[cacheKey][32]byte)
		for _, c := range channels {
			for _, cred := range c.Credentials {
				valid[cacheKey{c.ID, cred.ID}] = identity(c, cred)
			}
		}
		s.mu.Lock()
		for k, e := range s.entries {
			if id, ok := valid[k]; !ok || id != e.identity {
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}

// Cached never contacts the upstream. Expired facts lose their availability
// value; they cannot keep an account red after the observation's validity ends.
func (s *Service) Cached(c store.Channel, cred store.CredentialItem) Observation {
	base := s.empty(c, cred)
	if c.CredentialType == store.CredentialCodexOAuth && s.plugins != nil && !s.plugins.Enabled(c.CredentialType) {
		base.Error = codexPluginDisabled
		return base
	}
	s.prune(time.Now())
	if !base.Supported || !s.current(c, cred) {
		return base
	}
	interval := s.interval()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[cacheKey{c.ID, cred.ID}]; e != nil && e.identity == identity(c, cred) {
		return freshObservation(e.observation, interval, time.Now())
	}
	return base
}

// Check coalesces simultaneous readers, including explicit refreshes. An old
// request whose entry was reset/replaced returns unknown and cannot republish.
func (s *Service) Check(ctx context.Context, c store.Channel, cred store.CredentialItem, force bool) Observation {
	started := time.Now().UTC()
	s.mu.Lock()
	epoch := s.resetEpoch
	s.mu.Unlock()
	base := s.empty(c, cred)
	s.prune(time.Now())
	if !base.Supported || !s.current(c, cred) {
		return base
	}
	var codexSvc *codex.Service
	if c.CredentialType == store.CredentialCodexOAuth {
		var err error
		codexSvc, ctx, err = s.codexAccess(ctx)
		if err != nil {
			base.Error = quotaAccessError(err)
			return base
		}
	}
	budget := 2 * time.Second
	if force {
		budget = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	interval := s.interval()
	k, id := cacheKey{c.ID, cred.ID}, identity(c, cred)
	s.mu.Lock()
	if epoch != s.resetEpoch {
		s.mu.Unlock()
		return base
	}
	e := s.entries[k]
	if e != nil && e.identity != id {
		delete(s.entries, k)
		e = nil
	}
	if e != nil && e.done != nil {
		done := e.done
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			base.Error = "额度查询超时或已取消"
			return base
		case <-done:
			return s.Cached(c, cred)
		}
	}
	if !force && (interval == 0 || e != nil && !e.observation.CheckedAt.IsZero() && time.Now().Before(freshObservation(e.observation, interval, time.Now()).ExpiresAt)) {
		s.mu.Unlock()
		return s.Cached(c, cred)
	}
	if len(s.entries) >= maxEntries && e == nil {
		var oldest cacheKey
		var at time.Time
		for key, candidate := range s.entries {
			if at.IsZero() || candidate.observation.CheckedAt.Before(at) {
				oldest, at = key, candidate.observation.CheckedAt
			}
		}
		delete(s.entries, oldest)
	}
	e = &cacheEntry{identity: id, observation: base, done: make(chan struct{})}
	s.entries[k] = e
	s.mu.Unlock()

	result := base
	if c.CredentialType == store.CredentialCodexOAuth {
		if codexSvc == nil {
			result.Error = "Codex 额度查询不可用"
		} else if usage, err := codexSvc.UsageForCredential(ctx, c, cred.ID); err != nil {
			result.Error = quotaAccessError(err)
		} else {
			result.Available, result.ExpiresAt = codexAvailability(usage, time.Now())
			if result.Available != nil && !*result.Available {
				result.Reason = "额度不足"
			}
		}
	} else {
		result.Available, result.Balances, result.Error = s.deepSeek(ctx, cred.APIKey)
		if result.Available != nil && !*result.Available {
			result.Reason = "余额不足"
		}
	}
	denied := accessguard.Check(ctx)
	if denied != nil {
		result = base
		result.Error = quotaAccessError(denied)
	} else {
		result.CheckedAt = started
	}
	if interval > 0 && !result.CheckedAt.IsZero() {
		expires := result.CheckedAt.Add(interval)
		if result.ExpiresAt.IsZero() || expires.Before(result.ExpiresAt) {
			result.ExpiresAt = expires
		}
	}
	current := s.current(c, cred)
	s.mu.Lock()
	if s.entries[k] == e && current && epoch == s.resetEpoch {
		e.observation = result
	} else {
		result = base
	}
	close(e.done)
	e.done = nil
	s.mu.Unlock()
	return freshObservation(result, interval, time.Now())
}

// Reset also detaches in-flight writers; deleting an entry is its epoch change.
func (s *Service) Reset(channelID int64, credID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A single bounded epoch also covers a Check that is validating the store
	// before reserving its entry. Unrelated cached facts remain untouched.
	s.resetEpoch++
	for k := range s.entries {
		if k.channel == channelID && (credID == "" || k.credential == credID) {
			delete(s.entries, k)
		}
	}
}
