package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"torouter/internal/auth"
	"torouter/internal/health"
	"torouter/internal/quota"
	route "torouter/internal/router"
	"torouter/internal/store"
)

type quotaKey struct {
	channel, generation int64
	credential          string
}

const maxSuccessfulPaths = 10000

type successfulPath struct {
	key      health.Key
	sequence uint64
}
type runtimeState struct {
	paths       map[string]successfulPath
	nextPath    uint64
	validKey    func(health.Key) bool
	validQuotas map[quotaKey]struct{}
	quotas      map[quotaKey]quota.Observation
	resets      map[quotaKey]time.Time
	bypass      map[quotaKey]bool
}

func (g *Gateway) runtimeLocked() *runtimeState {
	if g.runtime == nil {
		g.runtime = &runtimeState{paths: map[string]successfulPath{}, quotas: map[quotaKey]quota.Observation{}, resets: map[quotaKey]time.Time{}, bypass: map[quotaKey]bool{}}
	}
	return g.runtime
}
func combination(a route.Attempt, credential string) health.Key {
	return health.Key{ChannelID: a.Channel.ID, Generation: a.Channel.Generation, Endpoint: strings.TrimRight(a.Channel.EndpointBaseURL(a.Endpoint), "/"), Format: a.Endpoint.Format, Model: a.Model, CredentialID: credential}
}
func (g *Gateway) previousPath(scope string) (health.Key, bool) {
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	k, ok := g.runtimeLocked().paths[scope]
	return k.key, ok
}
func (g *Gateway) rememberPath(scope string, k health.Key) {
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	state := g.runtimeLocked()
	if state.validKey != nil && !state.validKey(k) {
		return
	}
	entry, exists := state.paths[scope]
	if !exists {
		if len(state.paths) >= maxSuccessfulPaths {
			// Same capacity policy as affinity: evict the oldest half, without an
			// idle timer. Sequence metadata lives only with live entries.
			scopes := make([]string, 0, len(state.paths))
			for s := range state.paths {
				scopes = append(scopes, s)
			}
			sort.Slice(scopes, func(i, j int) bool { return state.paths[scopes[i]].sequence < state.paths[scopes[j]].sequence })
			for _, s := range scopes[:len(scopes)/2] {
				delete(state.paths, s)
			}
		}
		state.nextPath++
		entry.sequence = state.nextPath
	}
	entry.key = k
	state.paths[scope] = entry
}
func preferPath(candidates []route.Attempt, k health.Key) []route.Attempt {
	for i, a := range candidates {
		if combination(a, k.CredentialID) == k {
			out := append([]route.Attempt{a}, candidates[:i]...)
			return append(out, candidates[i+1:]...)
		}
	}
	return candidates
}
func (g *Gateway) quotaObservation(c store.Channel, cred store.CredentialItem) quota.Observation {
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	return g.runtimeLocked().quotas[quotaKey{c.ID, c.Generation, cred.ID}]
}
func quotaUnavailable(o quota.Observation) bool {
	return o.Available != nil && !*o.Available && (o.ExpiresAt.IsZero() || time.Now().Before(o.ExpiresAt))
}
func (g *Gateway) pickCredential(ctx context.Context, a route.Attempt, previous health.Key, reuse bool, after string) (store.CredentialItem, bool) {
	if !g.pluginEnabled(a.Channel) {
		return store.CredentialItem{}, false
	}
	if reuse && after == "" && a.Channel.DispatchMode != store.DispatchModeRoundRobin && combination(a, previous.CredentialID) == previous && g.Breaker.Available(previous) {
		for _, cred := range a.Channel.Credentials {
			if cred.ID == previous.CredentialID && cred.Enabled {
				return cred, true
			}
		}
	}
	for _, cred := range g.credentials().Order(a.Channel, combination(a, ""), after) {
		k := combination(a, cred.ID)
		if !g.Breaker.Available(k) {
			continue
		}
		bypass := g.quotaBypass(a.Channel, cred.ID, false)
		o := g.quotaObservation(a.Channel, cred)
		if !bypass && g.QuotaCheck != nil {
			o = g.QuotaCheck(ctx, a.Channel, cred)
			g.ObserveQuota(a.Channel, cred, o)
			// Read the accepted observation, so a result invalidated by reset cannot
			// exclude the credential in this in-flight selection either.
			o = g.quotaObservation(a.Channel, cred)
		}
		if !bypass && quotaUnavailable(o) {
			continue
		}
		g.credentials().Selected(k)
		return cred, true
	}
	return store.CredentialItem{}, false
}
func (g *Gateway) ObserveQuota(c store.Channel, cred store.CredentialItem, o quota.Observation) {
	if !g.pluginEnabled(c) {
		return
	}
	if _, err := g.reconcileRuntime(); err != nil {
		return
	}
	if o.ChannelID != c.ID || o.Generation != c.Generation || o.CredentialID != cred.ID {
		return
	}
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	state := g.runtimeLocked()
	k := quotaKey{c.ID, c.Generation, cred.ID}
	if _, ok := state.validQuotas[k]; !ok {
		return
	}
	if !state.resets[k].IsZero() && !o.CheckedAt.After(state.resets[k]) {
		return
	}
	if old := state.quotas[k]; !old.CheckedAt.IsZero() && o.CheckedAt.Before(old.CheckedAt) {
		return
	}
	state.quotas[k] = o
}
func (g *Gateway) ResetStatus(channelID int64, credentialID, model string) error {
	channels, err := g.reconcileRuntime()
	if err != nil {
		return err
	}
	for _, c := range channels {
		if c.ID != channelID {
			continue
		}
		for _, cred := range c.Credentials {
			if cred.ID != credentialID {
				continue
			}
			if model != "" && !hasRuntimeModel(c, model) {
				return errors.New("unknown upstream model")
			}
			g.Breaker.Reset(channelID, credentialID, model)
			g.runtimeMu.Lock()
			defer g.runtimeMu.Unlock()
			state := g.runtimeLocked()
			if model == "" {
				k := quotaKey{c.ID, c.Generation, cred.ID}
				delete(state.quotas, k)
				state.resets[k] = time.Now()
				state.bypass[k] = true
			}
			return nil
		}
		return errors.New("unknown credential")
	}
	return errors.New("unknown channel")
}
func (g *Gateway) RuntimeStatus() health.StatusSnapshot {
	result := health.StatusSnapshot{Rows: []health.StatusRow{}}
	channels, err := g.reconcileRuntime()
	if err != nil {
		return result
	}
	records := g.Breaker.Snapshot()
	for _, c := range channels {
		for _, cred := range c.Credentials {
			o := g.quotaObservation(c, cred)
			row := health.StatusRow{ChannelID: c.ID, ChannelName: c.Name, CredentialID: cred.ID, CredentialName: cred.Label, QuotaSupported: quota.Supported(c, cred), Models: []health.ModelStatus{}}
			if row.CredentialName == "" {
				row.CredentialName = cred.ID
			}
			if quotaUnavailable(o) {
				row.Red = true
				row.Reason = o.Reason
			}
			if o.Error != "" {
				row.Reason = o.Error
			}
			if !g.pluginEnabled(c) {
				row.Reason = "接入插件已停用，请在插件库启用；账号与配置已保留"
			}
			for _, model := range statusModels(c, cred.ID, records) {
				m := health.ModelStatus{Model: model, Name: model, Upstream: c.Name}
				for k, v := range records {
					if k.ChannelID != c.ID || k.Generation != c.Generation || k.CredentialID != cred.ID || k.Model != model {
						continue
					}
					// Ignore removed endpoints even if a directly edited DB did not bump generation.
					valid := false
					for _, ep := range c.Endpoints {
						if combination(route.Attempt{Channel: c, Endpoint: ep, Model: model}, cred.ID) == k {
							valid = true
						}
					}
					if !valid {
						continue
					}
					m.Failures = max(m.Failures, v.Failures)
					if v.Red {
						m.Red = true
						if v.Failures >= 4 {
							m.Reason = "连续不可用"
						} else if m.Reason == "" {
							m.Reason = "失败冷却中"
						}
					}
					if !v.Until.IsZero() && (m.CooldownUntil == nil || v.Until.After(*m.CooldownUntil)) {
						until := v.Until
						m.CooldownUntil = &until
					}
					if v.LastRequest != nil && (m.LastRequest == nil || v.LastRequest.At.After(m.LastRequest.At)) {
						m.LastRequest = v.LastRequest
					}
				}
				row.Failures = max(row.Failures, m.Failures)
				if m.Red {
					row.Red = true
					if row.Reason == "" {
						row.Reason = m.Reason
					}
				}
				if m.CooldownUntil != nil && (row.CooldownUntil == nil || m.CooldownUntil.After(*row.CooldownUntil)) {
					row.CooldownUntil = m.CooldownUntil
				}
				row.Models = append(row.Models, m)
			}
			sort.Slice(row.Models, func(i, j int) bool {
				a, b := row.Models[i], row.Models[j]
				if a.Red != b.Red {
					return a.Red
				}
				return a.Model < b.Model
			})
			if row.Red {
				result.ProblemCount++
			}
			result.Rows = append(result.Rows, row)
		}
	}
	sort.Slice(result.Rows, func(i, j int) bool {
		a, b := result.Rows[i], result.Rows[j]
		if a.Red != b.Red {
			return a.Red
		}
		if a.ChannelName != b.ChannelName {
			return a.ChannelName < b.ChannelName
		}
		if a.ChannelID != b.ChannelID {
			return a.ChannelID < b.ChannelID
		}
		return a.CredentialID < b.CredentialID
	})
	return result
}

// Runtime reasons intentionally avoid upstream bodies, URLs and arbitrary
// transport error text, any of which may contain account or secret material.
func safeRuntimeReason(reason string) string {
	if reason == "" {
		return ""
	}
	// Match the HTTP classification before generic words: "upstream" itself
	// contains "stream", but a rejected non-streaming call is not a stream error.
	if i := strings.Index(reason, "upstream returned HTTP "); i >= 0 {
		var status int
		if _, err := fmt.Sscanf(reason[i:], "upstream returned HTTP %d", &status); err == nil && status >= 100 && status <= 599 {
			return fmt.Sprintf("upstream returned HTTP %d", status)
		}
	}
	for _, known := range []string{"connection plugin disabled", "client access scope denies", "client cancelled", "client disconnected", "cooling", "quota", "no enabled channel", "unsupported cross-protocol", "state", "configuration", "connection failed", "incomplete", "invalid JSON", "stream"} {
		if strings.Contains(reason, known) {
			if known == "stream" {
				found := false
				for _, word := range strings.Fields(reason) {
					if strings.Trim(word, ":,;.()[]") == "stream" {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			return known
		}
	}
	return "request could not be completed"
}

type notSentWriter struct {
	http.ResponseWriter
	status     int
	body       []byte
	attempts   []store.AttemptRecord
	diagnostic json.RawMessage
}

func (w *notSentWriter) observeError(_ int, rec *recovery) {
	if rec != nil && rec.Diagnostic != nil {
		w.diagnostic, _ = json.Marshal(rec.Diagnostic)
	}
}

func (w *notSentWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *notSentWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if len(w.body) < 2048 {
		n := min(len(b), 2048-len(w.body))
		w.body = append(w.body, b[:n]...)
	}
	return w.ResponseWriter.Write(b)
}
func (w *notSentWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (g *Gateway) recordNotSent(r *http.Request, in requestInfo, start time.Time, w *notSentWriter) {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.body, &envelope)
	client := auth.ClientFromContext(r.Context())
	status := w.status
	if status == 0 {
		status = 499
	}
	reason := safeRuntimeReason(envelope.Error.Message)
	if reason == "" {
		reason = "request was not sent upstream"
	}
	for i := range w.attempts {
		w.attempts[i].ErrorReason = safeRuntimeReason(w.attempts[i].ErrorReason)
	}
	if err := g.Store.AddUsage(store.Usage{RequestID: w.Header().Get(requestIDHeader), Diagnostic: w.diagnostic, NotSent: true, Attempts: w.attempts, Time: start, Model: in.model, Inbound: in.format, ClientKeyID: client.ID, ClientKeyName: client.Name, Status: status, DurationMS: time.Since(start).Milliseconds(), Error: reason}); err != nil {
		slog.Error("not-sent request could not be saved", "error", err)
	}
}

func (g *Gateway) fail(lease health.Lease) {
	if !g.Breaker.Failure(lease) {
		return
	}
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	state := g.runtimeLocked()
	for scope, k := range state.paths {
		if k.key == lease.Key() {
			delete(state.paths, scope)
		}
	}
}
func (g *Gateway) unavailableReason(a route.Attempt) string {
	if !g.pluginEnabled(a.Channel) {
		return disabledPluginReason
	}
	for _, cred := range a.Channel.Credentials {
		if cred.Enabled && quotaUnavailable(g.quotaObservation(a.Channel, cred)) {
			return "no available credential: quota or balance exhausted"
		}
	}
	return "all candidate combinations are cooling down"
}

func (g *Gateway) quotaBypass(c store.Channel, credID string, consume bool) bool {
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	state := g.runtimeLocked()
	k := quotaKey{c.ID, c.Generation, credID}
	v := state.bypass[k]
	if consume {
		delete(state.bypass, k)
	}
	return v
}

func (g *Gateway) failRequest(ctx context.Context, lease health.Lease) {
	if ctx.Err() != nil {
		g.Breaker.Release(lease)
		return
	}
	g.fail(lease)
}

func (g *Gateway) rememberPaths(scope, session, fingerprint string, k health.Key) {
	g.rememberPath(scope, k)
	if session != "" && session != scope {
		g.rememberPath(session, k)
	}
	if fingerprint != "" && fingerprint != scope {
		g.rememberPath(fingerprint, k)
	}
}

// Reconciliation is demand-driven by requests/status/quota observations. Read
// the current store snapshot while serializing reconciliation, so an older
// request snapshot cannot roll the active configuration backwards.
func (g *Gateway) reconcileRuntime() ([]store.Channel, error) {
	g.runtimeMu.Lock()
	defer g.runtimeMu.Unlock()
	channels, err := g.Store.ListChannels()
	if err != nil {
		return nil, err
	}
	// Keep linear membership sets, never the catalogue x credential x endpoint
	// product: a provider catalogue can contain tens of thousands of models.
	type endpoint struct{ format, url string }
	type channelScope struct {
		generation    int64
		models, creds map[string]bool
		endpoints     map[endpoint]bool
	}
	current := map[int64]channelScope{}
	credentials := map[quotaKey]struct{}{}
	for _, c := range channels {
		scope := channelScope{generation: c.Generation, models: map[string]bool{}, creds: map[string]bool{}, endpoints: map[endpoint]bool{}}
		for _, m := range c.Models {
			scope.models[m] = true
		}
		for _, m := range c.AvailableModels {
			scope.models[m] = true
		}
		for _, cred := range c.Credentials {
			scope.creds[cred.ID] = true
			credentials[quotaKey{c.ID, c.Generation, cred.ID}] = struct{}{}
		}
		for _, ep := range c.Endpoints {
			scope.endpoints[endpoint{ep.Format, strings.TrimRight(c.EndpointBaseURL(ep), "/")}] = true
		}
		current[c.ID] = scope
	}
	valid := func(k health.Key) bool {
		c, ok := current[k.ChannelID]
		return ok && c.generation == k.Generation && c.models[k.Model] && (k.CredentialID == "" || c.creds[k.CredentialID]) && c.endpoints[endpoint{k.Format, k.Endpoint}]
	}
	state := g.runtimeLocked()
	state.validKey = valid
	state.validQuotas = credentials
	for scope, p := range state.paths {
		if !valid(p.key) {
			delete(state.paths, scope)
		}
	}
	for k := range state.quotas {
		if _, ok := credentials[k]; !ok {
			delete(state.quotas, k)
		}
	}
	for k := range state.resets {
		if _, ok := credentials[k]; !ok {
			delete(state.resets, k)
		}
	}
	for k := range state.bypass {
		if _, ok := credentials[k]; !ok {
			delete(state.bypass, k)
		}
	}
	g.Breaker.Reconcile(valid)
	g.credentials().reconcile(valid)
	return channels, nil
}

func hasRuntimeModel(c store.Channel, model string) bool {
	for _, m := range c.Models {
		if m == model {
			return true
		}
	}
	for _, m := range c.AvailableModels {
		if m == model {
			return true
		}
	}
	return false
}
func statusModels(c store.Channel, credential string, records map[health.Key]health.State) []string {
	models := append([]string{}, c.Models...)
	seen := map[string]bool{}
	for _, m := range models {
		seen[m] = true
	}
	for k, v := range records {
		if k.ChannelID == c.ID && k.Generation == c.Generation && k.CredentialID == credential && (v.LastRequest != nil || v.Failures > 0) && !seen[k.Model] {
			seen[k.Model] = true
			models = append(models, k.Model)
		}
	}
	return models
}
