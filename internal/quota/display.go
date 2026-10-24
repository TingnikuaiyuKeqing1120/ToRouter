package quota

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/codex"
	"torouter/internal/store"
)

func (s *Service) RefreshSeconds() int { return int(s.interval() / time.Second) }

func displayBase(c store.Channel, cred store.CredentialItem) Snapshot {
	name := strings.TrimSpace(cred.Label)
	if name == "" {
		name = "凭证"
		for i, member := range c.Credentials {
			if member.ID == cred.ID {
				name = fmt.Sprintf("凭证 %d", i+1)
				break
			}
		}
	}
	kind := "balance"
	if c.CredentialType == store.CredentialCodexOAuth {
		kind = "windows"
	}
	return Snapshot{ChannelID: c.ID, CredentialID: cred.ID, ChannelName: c.Name, CredentialName: name,
		Kind: kind, Enabled: c.Enabled && cred.Enabled, Balances: []Balance{}, Windows: []Window{}, Stale: true}
}

func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (s *Service) balanceDisplay(c store.Channel, cred store.CredentialItem, o Observation) Snapshot {
	v := displayBase(c, cred)
	v.Balances = append(v.Balances, o.Balances...)
	v.CheckedAt, v.ExpiresAt, v.Error = timePointer(o.CheckedAt), timePointer(o.ExpiresAt), o.Error
	if interval := s.interval(); interval > 0 && !o.CheckedAt.IsZero() {
		v.NextRefreshAt = timePointer(o.CheckedAt.Add(interval))
	}
	v.Stale = o.CheckedAt.IsZero() || o.Error != "" || !o.ExpiresAt.IsZero() && !time.Now().Before(o.ExpiresAt)
	return v
}

func (s *Service) codexDisplay(c store.Channel, cred store.CredentialItem, report codex.UsageReport) Snapshot {
	v := displayBase(c, cred)
	v.NextRefreshAt = report.NextRefreshAt
	v.Stale = !report.OK || report.Stale
	if report.Detail != "" {
		v.Error = "Codex 额度查询失败，请检查账号授权或网络"
	}
	if report.Usage == nil {
		return v
	}
	v.CheckedAt = timePointer(report.CapturedAt)
	if report.RefreshSeconds > 0 {
		v.ExpiresAt = timePointer(report.CapturedAt.Add(time.Duration(report.RefreshSeconds) * time.Second))
	}
	now := time.Now()
	for i, bucket := range report.Buckets {
		label := fmt.Sprintf("额度 %d", i+1)
		if bucket.General {
			label = "通用额度"
		}
		for n, window := range []*codex.UsageWindow{bucket.Primary, bucket.Secondary} {
			if window == nil {
				continue
			}
			w := Window{ID: fmt.Sprintf("%d-%d", i, n), Label: label}
			if n == 0 {
				w.Label += " · 主窗口"
			} else {
				w.Label += " · 次窗口"
			}
			if window.WindowSeconds != nil && *window.WindowSeconds > 0 {
				w.Label = fmt.Sprintf("%s · %g小时", label, float64(*window.WindowSeconds)/3600)
			}
			if window.ResetAt != nil && *window.ResetAt > 0 {
				reset := time.Unix(*window.ResetAt, 0).UTC()
				w.ResetsAt = &reset
				if !now.Before(reset) {
					v.Stale = true
				} else if v.ExpiresAt == nil || reset.Before(*v.ExpiresAt) {
					v.ExpiresAt = &reset
				}
			}
			if window.UsedPercent != nil && !math.IsNaN(*window.UsedPercent) && !math.IsInf(*window.UsedPercent, 0) && *window.UsedPercent >= 0 && *window.UsedPercent <= 100 && (w.ResetsAt == nil || now.Before(*w.ResetsAt)) {
				remaining := 100 - *window.UsedPercent
				w.RemainingPercent = &remaining
			}
			v.Windows = append(v.Windows, w)
		}
	}
	return v
}

// Snapshot only reads local caches, including Codex's existing persisted cache.
func (s *Service) Snapshot(c store.Channel, cred store.CredentialItem) Snapshot {
	if c.CredentialType != store.CredentialCodexOAuth {
		return s.balanceDisplay(c, cred, s.Cached(c, cred))
	}
	v := displayBase(c, cred)
	if s.current(c, cred) {
		svc := s.codex
		if s.plugins != nil {
			if native := s.plugins.Existing(c.CredentialType); native != nil {
				svc = native.Codex
			}
		}
		var report codex.UsageReport
		var err error
		if svc != nil {
			report, err = svc.CachedUsage(c.ID, cred.ID)
		} else if s.plugins != nil {
			report, err = codex.CachedUsageFromStore(s.store, c.ID, cred.ID)
		}
		if err == nil {
			v = s.codexDisplay(c, cred, report)
		}
	}
	if s.plugins != nil && !s.plugins.Enabled(c.CredentialType) {
		v.Enabled, v.NextRefreshAt, v.Error = false, nil, codexPluginDisabled
	}
	return v
}

// RefreshSnapshot reuses Codex's single display cache/refresh lock. The returned
// observation lets the host update routing health without a second request.
func (s *Service) RefreshSnapshot(ctx context.Context, c store.Channel, cred store.CredentialItem, force bool) (Snapshot, Observation) {
	if c.CredentialType != store.CredentialCodexOAuth {
		o := s.Check(ctx, c, cred, force)
		return s.balanceDisplay(c, cred, o), o
	}
	budget := 2 * time.Second
	if force {
		budget = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	s.mu.Lock()
	epoch := s.resetEpoch
	s.mu.Unlock()
	o := s.empty(c, cred)
	if !s.current(c, cred) {
		return displayBase(c, cred), o
	}
	svc, guarded, err := s.codexAccess(ctx)
	if err != nil {
		v := s.Snapshot(c, cred)
		v.Error = quotaAccessError(err)
		o.Error = v.Error
		return v, o
	}
	if svc == nil {
		return displayBase(c, cred), o
	}
	report, err := svc.RefreshUsage(guarded, c.ID, cred.ID, force)
	if denied := accessguard.Check(guarded); denied != nil {
		v := s.Snapshot(c, cred)
		v.Error = quotaAccessError(denied)
		o.Error = v.Error
		return v, o
	}
	if err != nil || !s.current(c, cred) {
		v := displayBase(c, cred)
		v.Error = quotaAccessError(err)
		o.Error = v.Error
		return v, o
	}
	v := s.codexDisplay(c, cred, report)
	o.Error = v.Error
	// Display caches are not new routing facts. In particular, a response
	// completed after Reset may remain displayable, but a later cached reader
	// must not turn its late CapturedAt into a new availability observation.
	// RefreshUsage also marks concurrent joiners and failed attempts Cached.
	if report.Cached {
		return v, o
	}
	if report.LastAttemptAt != nil {
		o.CheckedAt = *report.LastAttemptAt
	}
	if report.Usage != nil && o.Error == "" {
		o.CheckedAt = report.CapturedAt
	}
	if report.RefreshSeconds > 0 && !o.CheckedAt.IsZero() {
		o.ExpiresAt = o.CheckedAt.Add(time.Duration(report.RefreshSeconds) * time.Second)
	}
	if report.Usage != nil && !report.Stale && o.Error == "" {
		o.CheckedAt = report.CapturedAt
		o.Available, o.ExpiresAt = codexAvailability(*report.Usage, time.Now())
		if report.RefreshSeconds > 0 {
			expires := report.CapturedAt.Add(time.Duration(report.RefreshSeconds) * time.Second)
			if o.ExpiresAt.IsZero() || expires.Before(o.ExpiresAt) {
				o.ExpiresAt = expires
			}
		}
		if o.Available != nil && !*o.Available {
			o.Reason = "额度不足"
		}
	}
	s.mu.Lock()
	if epoch != s.resetEpoch {
		o = s.empty(c, cred)
	} else if !o.CheckedAt.IsZero() {
		// Updating the existing availability cache keeps subsequent routing
		// checks consistent with this refresh and fences older in-flight reads.
		k := cacheKey{c.ID, cred.ID}
		if s.entries[k] != nil || len(s.entries) < maxEntries {
			s.entries[k] = &cacheEntry{identity: identity(c, cred), observation: copyObservation(o)}
		}
	}
	s.mu.Unlock()
	return v, o
}
