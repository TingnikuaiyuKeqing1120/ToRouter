package codex

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"torouter/internal/store"
)

type UsageReport struct {
	OK bool `json:"ok"`
	*Usage
	Cached         bool       `json:"cached"`
	Stale          bool       `json:"stale"`
	RefreshDue     bool       `json:"refresh_due"`
	RefreshSeconds int        `json:"refresh_seconds"`
	NextRefreshAt  *time.Time `json:"next_refresh_at,omitempty"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
	Detail         string     `json:"detail,omitempty"`
}

func (svc *Service) quotaInterval() (int, error) {
	return quotaInterval(svc.Store)
}

func quotaInterval(st *store.Store) (int, error) {
	settings, err := st.GetSettings()
	if err != nil {
		return 0, err
	}
	if settings.QuotaRefreshSeconds == nil {
		return 300, nil
	}
	return *settings.QuotaRefreshSeconds, nil
}

func usageReport(record store.QuotaCache, interval int) UsageReport {
	report := UsageReport{Cached: true, RefreshSeconds: interval, Detail: record.Detail}
	var snapshot Usage
	if len(record.Snapshot) > 0 && json.Unmarshal(record.Snapshot, &snapshot) == nil && !snapshot.CapturedAt.IsZero() {
		report.Usage = &snapshot
		report.OK = true
	}
	var reference time.Time
	if report.OK {
		reference = snapshot.CapturedAt
	}
	if !record.LastAttemptAt.IsZero() {
		report.LastAttemptAt = &record.LastAttemptAt
		if record.LastAttemptAt.After(reference) {
			reference = record.LastAttemptAt
		}
	}
	report.Stale = report.OK && (record.Detail != "" || interval > 0 && time.Since(snapshot.CapturedAt) >= time.Duration(interval)*time.Second)
	if interval > 0 {
		due := reference.Add(time.Duration(interval) * time.Second)
		report.RefreshDue = reference.IsZero() || !time.Now().Before(due)
		if !reference.IsZero() {
			report.NextRefreshAt = &due
		}
	}
	return report
}

// CachedUsage never acquires the network/lifecycle lock or contacts an upstream.
func (svc *Service) CachedUsage(id int64, credentialID string) (UsageReport, error) {
	return CachedUsageFromStore(svc.Store, id, credentialID)
}

// CachedUsageFromStore reads the persisted display report without constructing
// an OAuth service or acquiring its network/lifecycle lock.
func CachedUsageFromStore(st *store.Store, id int64, credentialID string) (UsageReport, error) {
	interval, err := quotaInterval(st)
	if err != nil {
		return UsageReport{}, err
	}
	record, err := st.LoadQuotaCache(id, credentialID)
	if err != nil {
		return UsageReport{}, err
	}
	return usageReport(record, interval), nil
}

// RefreshUsage is owned by this HTTP request. Waiters reuse an attempt that
// completed after they arrived, including failures. No background writer exists.
func (svc *Service) RefreshUsage(ctx context.Context, id int64, credentialID string, force bool) (UsageReport, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	lock := svc.lockFor(id)
	if err := lockUsage(ctx, lock); err != nil {
		return UsageReport{}, err
	}
	defer lock.Unlock()
	interval, err := svc.quotaInterval()
	if err != nil {
		return UsageReport{}, err
	}
	record, err := svc.Store.LoadQuotaCache(id, credentialID)
	if err != nil {
		return UsageReport{}, err
	}
	report := usageReport(record, interval)
	if !record.LastAttemptAt.Before(started) || !force && !report.RefreshDue {
		return report, nil
	}
	c, err := svc.Store.Channel(id)
	if err != nil {
		return UsageReport{}, err
	}
	snapshot, receiptChannel, readErr := svc.usageForCredentialLocked(ctx, c, credentialID)
	record.LastAttemptAt = time.Now().UTC()
	if readErr != nil {
		record.Detail = readErr.Error()
	} else {
		record.Detail = ""
		record.Snapshot, err = json.Marshal(snapshot)
		if err != nil {
			return UsageReport{}, err
		}
	}
	if err := svc.Store.SaveQuotaCache(receiptChannel, credentialID, record); err != nil {
		return UsageReport{}, errors.New("could not save quota snapshot; account may have changed")
	}
	report = usageReport(record, interval)
	report.Cached = readErr != nil
	return report, nil
}
