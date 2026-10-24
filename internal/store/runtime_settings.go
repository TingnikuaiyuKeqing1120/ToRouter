package store

import "errors"

const (
	ReasoningPreserve     = "preserve"
	ReasoningAvailability = "availability"
)

func normalizeReasoningPolicy(s *Settings) error {
	if s.ReasoningPolicy == "" {
		s.ReasoningPolicy = ReasoningPreserve
	}
	if s.ReasoningPolicy != ReasoningPreserve && s.ReasoningPolicy != ReasoningAvailability {
		return errors.New("reasoning_policy must be preserve or availability")
	}
	return nil
}

// The old field was a ceiling on upstream Retry-After, not an initial failure
// delay. Retire it at the loading boundary instead of silently reusing its value.
func (s *Store) upgradeFailureCooldown() error {
	_, err := s.db.Exec(`UPDATE settings SET data=json_remove(
		json_set(data,'$.failure_cooldown_seconds',
			CASE WHEN COALESCE(json_extract(data,'$.failure_cooldown_seconds'),0)=0
			THEN ? ELSE json_extract(data,'$.failure_cooldown_seconds') END),
		'$.retry_after_max_seconds')
		WHERE json_type(data,'$.retry_after_max_seconds') IS NOT NULL
		OR COALESCE(json_extract(data,'$.failure_cooldown_seconds'),0)=0`, DefaultFailureCooldownSeconds)
	return err
}
