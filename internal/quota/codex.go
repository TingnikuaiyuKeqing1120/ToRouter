package quota

import (
	"time"

	"torouter/internal/codex"
)

func codexAvailability(usage codex.Usage, now time.Time) (*bool, time.Time) {
	for _, bucket := range usage.Buckets {
		if !bucket.General {
			continue
		}
		var expires time.Time
		known, exhausted, windows, expired := false, false, 0, 0
		for _, window := range []*codex.UsageWindow{bucket.Primary, bucket.Secondary} {
			if window == nil {
				continue
			}
			windows++
			if window.ResetAt != nil && *window.ResetAt > 0 {
				reset := time.Unix(*window.ResetAt, 0).UTC()
				if !now.Before(reset) {
					expired++
					continue
				}
				if expires.IsZero() || reset.Before(expires) {
					expires = reset
				}
			}
			if window.UsedPercent != nil && *window.UsedPercent >= 0 {
				known = true
				exhausted = exhausted || *window.UsedPercent >= 100
			}
		}
		// A report whose windows already reset is not a current account fact.
		if windows > 0 && windows == expired {
			return nil, time.Time{}
		}
		// Account-level flags cannot identify which window exhausted. Once a
		// reported window has reset, only an explicit still-current exhausted
		// window is sufficient evidence to retain an unavailable observation.
		if expired > 0 && !exhausted {
			return nil, time.Time{}
		}
		if bucket.Allowed != nil {
			known = true
			exhausted = exhausted || !*bucket.Allowed
		}
		if bucket.LimitReached != nil {
			known = true
			exhausted = exhausted || *bucket.LimitReached
		}
		if known {
			available := !exhausted
			return &available, expires
		}
		return nil, time.Time{}
	}
	return nil, time.Time{}
}
