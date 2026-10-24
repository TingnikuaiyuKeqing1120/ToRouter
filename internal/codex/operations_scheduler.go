package codex

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"
	"torouter/internal/accessguard"
	"torouter/internal/store"
)

// Run owns its sequential work. Cancellation reaches every HTTP call and Run
// returns only after the current call and its durable final receipt finish.
func (ops *Operations) Run(ctx context.Context) {
	ops.RunUntil(ctx, nil)
}

// RunUntil stops at the next tick boundary without cancelling an in-flight
// request or its durable receipt. Process cancellation retains Run's behavior.
func (ops *Operations) RunUntil(ctx context.Context, stop <-chan struct{}) {
	if ops.svc == nil {
		return
	}
	ticker := time.NewTicker(ops.tickInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil || accessguard.Check(ctx) != nil {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		ops.tick(ctx, ops.now())
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func (ops *Operations) tick(ctx context.Context, now time.Time) {
	if ops.svc == nil || ctx.Err() != nil || accessguard.Check(ctx) != nil {
		return
	}
	channels, err := ops.store.ListChannels()
	if err != nil {
		return
	}
	interval, err := ops.svc.quotaInterval()
	if err != nil {
		return
	}
	if interval == 0 {
		interval = 60
	}
	for _, c := range channels {
		for _, cred := range c.Credentials {
			if ctx.Err() != nil || accessguard.Check(ctx) != nil {
				return
			}
			cfg := operationConfig(c, cred.ID)
			if !operationEnabled(c, cred.ID) || (!cfg.Warmup.Enabled && !cfg.AutoReset.Enabled) {
				continue
			}
			identity, err := store.CredentialOperationsIdentity(c, cred.ID)
			if err != nil {
				continue
			}
			ops.tickCredential(ctx, c, cred.ID, identity, cfg, now, time.Duration(interval)*time.Second)
		}
	}
}

func (ops *Operations) tickCredential(ctx context.Context, c store.Channel, cid, identity string, cfg store.CredentialOperationsConfig, now time.Time, interval time.Duration) {
	if ops.svc == nil {
		return
	}
	raw, err := ops.store.LoadCredentialOperationState(c.ID, cid)
	if err != nil {
		return
	}
	s, err := decodeOperationState(raw)
	if err != nil {
		return
	}
	at := now.Format("15:04")
	if cfg.Warmup.Enabled && cfg.Warmup.Mode == "daily" && containsDailyTime(cfg.Warmup.DailyTimes, at) && !(s.DailyDate == now.Format("2006-01-02") && containsDailyTime(s.DailyTimes, at)) {
		usage, err := ops.svc.RefreshUsage(ctx, c.ID, cid, true)
		if err == nil && usage.OK && usage.Detail == "" && usage.Usage != nil && dailyWarmupAvailable(*usage.Usage, now) {
			date := now.Format("2006-01-02")
			_, _ = ops.execute(ctx, c.ID, cid, identity, uuid.NewString(), "warmup", &scheduleClaim{config: cfg, revision: scheduleRevision(c, cid), apply: func(s *operationState) error {
				if ops.now().Format("2006-01-02 15:04") != now.Format("2006-01-02 15:04") {
					return store.ErrCredentialActionBusy
				}
				if s.DailyDate == date && containsDailyTime(s.DailyTimes, at) {
					return store.ErrCredentialActionBusy
				}
				if s.DailyDate != date {
					s.DailyTimes = nil
				}
				s.DailyDate = date
				s.DailyTimes = append(s.DailyTimes, at)
				return nil
			}})
		}
	}
	if !cfg.AutoReset.Enabled && !(cfg.Warmup.Enabled && cfg.Warmup.Mode == "reset") {
		return
	}
	knownResetDue := false
	if cfg.Warmup.Enabled && cfg.Warmup.Mode == "reset" {
		cached, readErr := ops.svc.CachedUsage(c.ID, cid)
		if readErr == nil && cached.OK && cached.Usage != nil {
			resetAt, dueAt, known := naturalWarmupTime(*cached.Usage)
			knownResetDue = known && now.Unix() >= dueAt && resetAt > s.LastNaturalReset && s.LastPoll.Unix() < dueAt
		}
	}
	if !knownResetDue && s.ScheduleRevision == scheduleRevision(c, cid) && !s.LastPoll.IsZero() && now.Before(s.LastPoll.Add(interval)) {
		return
	}
	live, err := ops.target(c.ID, cid, identity, true)
	if err != nil || scheduleRevision(live, cid) != scheduleRevision(c, cid) || !sameOperationConfig(cfg, operationConfig(live, cid)) {
		return
	}
	// Persist the read cadence, including failures. Manual quota mode still
	// obtains fresh evidence for explicitly enabled automatic rules every 60s.
	_, err = ops.mutate(c, cid, true, func(s *operationState) error {
		if s.ScheduleRevision != scheduleRevision(c, cid) {
			s.ScheduleRevision = scheduleRevision(c, cid)
		}
		s.LastPoll = now.UTC()
		return nil
	})
	if err != nil {
		return
	}
	usage, usageErr := ops.svc.RefreshUsage(ctx, c.ID, cid, true)
	usageKnown := usageErr == nil && usage.OK && usage.Detail == "" && usage.Usage != nil
	usageEvidence := Usage{}
	if usageKnown {
		usageEvidence = *usage.Usage
	}
	// A queued read may have crossed an enable/configuration edit.
	live, err = ops.target(c.ID, cid, identity, true)
	if err != nil || scheduleRevision(live, cid) != scheduleRevision(c, cid) || !sameOperationConfig(cfg, operationConfig(live, cid)) {
		return
	}
	if cfg.AutoReset.Enabled {
		report, err := ops.RefreshCards(ctx, c.ID, cid, identity)
		if err != nil || report.ResetCardsError != "" || report.ResetCards == nil {
			return
		}
		cards := report.ResetCards
		matched, known := resetRulesMatch(cfg.AutoReset.Rules, usageEvidence, *cards, now)
		if known && !matched {
			// Only evidence collected after the latest reset can rearm it.
			// An older quota read may have waited behind a newly claimed POST.
			sampleResetID := s.LastResetID
			prior := ops.receipt(s, sampleResetID)
			if prior == nil || prior.State != "running" && prior.State != "unknown" {
				_, _ = ops.mutate(live, cid, true, func(current *operationState) error {
					if current.LastResetID == sampleResetID {
						current.ResetLatched = false
					}
					return nil
				})
			}
		}
		if matched && hasAvailableResetCredit(*cards, now) {
			_, resetErr := ops.execute(ctx, c.ID, cid, identity, uuid.NewString(), "reset", &scheduleClaim{config: cfg, revision: scheduleRevision(c, cid), apply: func(s *operationState) error {
				if s.ResetLatched {
					return store.ErrCredentialActionBusy
				}
				s.ResetLatched = true
				return nil
			}})
			// This quota sample describes the time before the potential reset.
			// Never use it to trigger a second action in the same observation.
			if resetErr == nil {
				return
			}
		}
	}
	if usageKnown && cfg.Warmup.Enabled && cfg.Warmup.Mode == "reset" {
		resetAt, dueAt, known := naturalWarmupTime(*usage.Usage)
		if !known || now.Unix() < dueAt {
			return
		}
		_, _ = ops.execute(ctx, c.ID, cid, identity, uuid.NewString(), "warmup", &scheduleClaim{config: cfg, revision: scheduleRevision(c, cid), apply: func(s *operationState) error {
			if resetAt <= s.LastNaturalReset {
				return store.ErrCredentialActionBusy
			}
			s.LastNaturalReset = resetAt
			return nil
		}})
	}
}

func containsDailyTime(times []string, at string) bool {
	for _, value := range times {
		if value == at {
			return true
		}
	}
	return false
}

func generalWindows(usage Usage) map[int64]*UsageWindow {
	windows := map[int64]*UsageWindow{}
	for _, bucket := range usage.Buckets {
		if !bucket.General {
			continue
		}
		for _, window := range []*UsageWindow{bucket.Primary, bucket.Secondary} {
			if window != nil && window.WindowSeconds != nil && *window.WindowSeconds > 0 {
				windows[*window.WindowSeconds] = window
			}
		}
	}
	return windows
}

func hasAvailableResetCredit(cards ResetCredits, now time.Time) bool {
	for _, card := range cards.Credits {
		if card.Available && card.Status == "available" && card.RedeemedAt == nil && (card.ExpiresAt == nil || *card.ExpiresAt > now.Unix()) {
			return true
		}
	}
	return false
}

// known is false if any absent/unknown input could change whether every group
// is false. Such a sample cannot rearm an already claimed reset situation.
func resetRulesMatch(rules []store.CredentialResetRule, usage Usage, cards ResetCredits, now time.Time) (matched, known bool) {
	windows := generalWindows(usage)
	known = true
	for _, rule := range rules {
		values := []bool{}
		knownValues := []bool{}
		if rule.RemainingBelow != nil {
			window := windows[rule.WindowSeconds]
			valid := window != nil && window.UsedPercent != nil && !math.IsNaN(*window.UsedPercent) && !math.IsInf(*window.UsedPercent, 0) && *window.UsedPercent >= 0 && *window.UsedPercent <= 100
			value := false
			if valid {
				value = 100-*window.UsedPercent < *rule.RemainingBelow
			}
			values = append(values, value)
			knownValues = append(knownValues, valid)
		}
		if rule.ExpiresWithinDays != nil {
			value, valid := false, true
			for _, card := range cards.Credits {
				if !card.Available || card.Status != "available" || card.RedeemedAt != nil {
					continue
				}
				if card.ExpiresAt == nil {
					valid = false
					continue
				}
				remaining := float64(*card.ExpiresAt-now.Unix()) / 86400
				if remaining > 0 && remaining < *rule.ExpiresWithinDays {
					value = true
				}
			}
			if value {
				valid = true
			}
			// No reported list is not evidence of an empty list.
			if cards.Credits == nil || !value && (cards.AvailableCount == nil || *cards.AvailableCount > 0 && len(cards.Credits) == 0) {
				valid = false
			}
			values = append(values, value)
			knownValues = append(knownValues, valid)
		}
		group, groupKnown := ruleConditions(values, knownValues, rule.Join)
		if group && groupKnown {
			return true, true
		}
		if !groupKnown {
			known = false
		}
	}
	return false, known
}

func ruleConditions(values, known []bool, join string) (bool, bool) {
	if len(values) == 0 {
		return false, false
	}
	if join == "or" {
		allKnown := true
		for i, v := range values {
			if known[i] && v {
				return true, true
			}
			if !known[i] {
				allKnown = false
			}
		}
		return false, allKnown
	}
	allKnown := true
	for i, v := range values {
		if known[i] && !v {
			return false, true
		}
		if !known[i] {
			allKnown = false
		}
	}
	return allKnown, allKnown
}

// A Codex rolling window starts counting after the next request. Preheat
// once the reported five-hour reset time arrives, even if it was not exhausted.
// A still-exhausted longer window delays that request until its own reset time.
func naturalWarmupTime(usage Usage) (resetAt int64, dueAt int64, known bool) {
	windows := generalWindows(usage)
	five := windows[18000]
	if five == nil || five.ResetAt == nil || *five.ResetAt <= 0 {
		return 0, 0, false
	}
	resetAt, dueAt = *five.ResetAt, *five.ResetAt
	for seconds, window := range windows {
		if seconds <= 18000 {
			continue
		}
		if !knownUsedPercent(window) {
			return 0, 0, false
		}
		if *window.UsedPercent >= 100 {
			if window.ResetAt == nil || *window.ResetAt <= 0 {
				return 0, 0, false
			}
			if *window.ResetAt > dueAt {
				dueAt = *window.ResetAt
			}
		}
	}
	return resetAt, dueAt, true
}

func dailyWarmupAvailable(usage Usage, now time.Time) bool {
	windows := generalWindows(usage)
	if windows[18000] == nil {
		return false
	}
	for _, window := range windows {
		if !knownUsedPercent(window) {
			return false
		}
		if *window.UsedPercent >= 100 && (window.ResetAt == nil || *window.ResetAt > now.Unix()) {
			return false
		}
	}
	return true
}

func knownUsedPercent(window *UsageWindow) bool {
	return window != nil && window.UsedPercent != nil && !math.IsNaN(*window.UsedPercent) && !math.IsInf(*window.UsedPercent, 0) && *window.UsedPercent >= 0 && *window.UsedPercent <= 100
}
