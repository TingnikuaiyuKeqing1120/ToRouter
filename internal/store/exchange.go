package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// MaxExchangeRetries is the maximum number of automatic retries allowed in a single day after failures.
const MaxExchangeRetries = 10

// ExchangeRetryPeriod is the minimum cooldown duration before an automatic retry attempt.
const ExchangeRetryPeriod = 10 * time.Second

// ExchangeRate stores the USD-to-CNY reference used by display and price editing.
type ExchangeRate struct {
	Available   bool    `json:"available"`
	Base        string  `json:"base"`
	Quote       string  `json:"quote"`
	Rate        float64 `json:"rate"`
	Date        string  `json:"date"`
	AttemptedOn string  `json:"attempted_on"`
	FetchedAt   string  `json:"fetched_at"`
	Stale       bool    `json:"stale"`
	Error       string  `json:"error"`
	SourceURL   string  `json:"source_url"`
	SourceID    string  `json:"source_id"`
	SourceName  string  `json:"source_name"`
	CheckedAt   string  `json:"checked_at"`
	Retries     int     `json:"retries,omitempty"`
}

// DailyExchangeRate persists the attempt before calling the network.
// If today's rate is already successfully fetched, it is reused. If a previous
// attempt today failed, automatic retry is allowed after ExchangeRetryPeriod (10s)
// up to MaxExchangeRetries (10 times a day). Concurrent callers share an in-flight
// attempt, and force=true immediately refreshes regardless of daily cache.
func (s *Store) DailyExchangeRate(now time.Time, force bool, fetch func() (ExchangeRate, error)) (ExchangeRate, error) {
	startedGeneration := s.exchangeGeneration.Load()
	s.exchangeMu.Lock()
	defer s.exchangeMu.Unlock()
	today := now.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")
	v := ExchangeRate{Base: "USD", Quote: "CNY", Stale: true}
	var raw string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key='exchange_usd_cny_v1'").Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return v, err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			return v, err
		}
	}
	completed, _ := time.Parse(time.RFC3339Nano, v.CheckedAt)
	if s.exchangeGeneration.Load() != startedGeneration {
		return v, nil
	}

	if !force {
		// If today's rate is already successfully fetched without error, reuse it.
		if v.AttemptedOn == today && v.Error == "" && !v.Stale {
			return v, nil
		}

		if v.AttemptedOn == today {
			// Failed attempt today: enforce daily retry limit and 10s cooldown.
			if v.Retries >= MaxExchangeRetries {
				return v, nil
			}
			diff := now.Sub(completed)
			if diff < 0 {
				diff = -diff
			}
			if !completed.IsZero() && diff < ExchangeRetryPeriod {
				return v, nil
			}
			v.Retries++
		} else {
			// First attempt of a new day.
			v.AttemptedOn = today
			v.Retries = 0
		}
	} else {
		// Manual force refresh resets daily retries and targets today.
		v.AttemptedOn = today
		v.Retries = 0
	}

	v.Stale = true
	v.Error = "汇率查询未完成，可手动重试"
	v.CheckedAt = now.UTC().Format(time.RFC3339Nano)
	if err = s.saveExchangeRate(v); err != nil {
		return v, err
	}
	fresh, fetchErr := fetch()
	if fetchErr != nil {
		// The network adapter returns bounded, non-sensitive error messages.
		v.Error = fetchErr.Error()
	} else {
		v.Available, v.Stale, v.Error = true, false, ""
		v.Rate, v.Date, v.FetchedAt = fresh.Rate, fresh.Date, now.UTC().Format(time.RFC3339)
		v.SourceID, v.SourceName, v.SourceURL = fresh.SourceID, fresh.SourceName, fresh.SourceURL
	}
	v.CheckedAt = now.UTC().Format(time.RFC3339Nano)
	err = s.saveExchangeRate(v)
	if err == nil {
		s.exchangeGeneration.Add(1)
	}
	return v, err
}

func (s *Store) saveExchangeRate(v ExchangeRate) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO meta(key,value) VALUES('exchange_usd_cny_v1',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw))
	return err
}
