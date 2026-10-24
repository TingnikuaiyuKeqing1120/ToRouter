package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type ExchangeSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func AvailableExchangeSources() []ExchangeSource {
	return []ExchangeSource{
		{ID: "ecb", Name: "欧洲央行", URL: "https://www.ecb.europa.eu/stats/policy_and_exchange_rates/euro_reference_exchange_rates/html/index.en.html"},
		{ID: "frankfurter", Name: "Frankfurter", URL: "https://frankfurter.dev/"},
		{ID: "exchangerate_api", Name: "ExchangeRate-API", URL: "https://www.exchangerate-api.com"},
	}
}
func (s *Store) GetExchangeSources() ([]string, error) {
	var raw string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key='exchange_sources'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{"ecb", "frankfurter", "exchangerate_api"}, nil
	}
	if err != nil {
		return nil, err
	}
	var sources []string
	if err = json.Unmarshal([]byte(raw), &sources); err != nil {
		return nil, err
	}
	return sources, nil
}
func (s *Store) SaveExchangeSources(sources []string) error {
	return s.saveExchangeSources(sources, "")
}

func (s *Store) SaveExchangeSourcesChecked(sources []string, expected string) error {
	if expected == "" {
		return ErrEditConflict
	}
	return s.saveExchangeSources(sources, expected)
}

func (s *Store) saveExchangeSources(sources []string, expected string) error {
	if sources == nil {
		return errors.New("sources must be an array")
	}
	known := map[string]bool{}
	for _, source := range AvailableExchangeSources() {
		known[source.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range sources {
		if !known[id] || seen[id] {
			return fmt.Errorf("unknown or duplicate exchange source: %s", id)
		}
		seen[id] = true
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expected != "" {
		var old string
		err = tx.QueryRow("SELECT value FROM meta WHERE key='exchange_sources'").Scan(&old)
		current := []string{"ecb", "frankfurter", "exchangerate_api"}
		if err == nil {
			if err = json.Unmarshal([]byte(old), &current); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if EntityVersion(current) != expected {
			return ErrEditConflict
		}
	}
	if _, err = tx.Exec("INSERT INTO meta(key,value) VALUES('exchange_sources',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

// CachedExchangeRate never fetches or records a new attempt.
func (s *Store) CachedExchangeRate() (ExchangeRate, error) {
	value := ExchangeRate{Base: "USD", Quote: "CNY", Stale: true}
	var raw string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key='exchange_usd_cny_v1'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	err = json.Unmarshal([]byte(raw), &value)
	if value.AttemptedOn != time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02") {
		value.Stale = true
	}
	return value, err
}
