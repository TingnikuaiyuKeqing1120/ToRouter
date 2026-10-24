package store

import (
	"encoding/json"
	"errors"
	"time"
)

type BackfillFilter struct {
	From          time.Time `json:"from,omitempty"`
	To            time.Time `json:"to,omitempty"`
	Model         string    `json:"model,omitempty"`
	ChannelID     int64     `json:"channel_id,omitempty"`
	UpstreamModel string    `json:"upstream_model,omitempty"`
}
type UsagePricingSnapshot struct {
	Settings Settings
	Items    []CatalogItem
	Usage    []Usage
	MaxID    int64
}
type UsagePricingPatch struct {
	ID               int64
	ActualCost       *float64
	Currency         string
	ActualBasis      *UsagePricingBasis
	OfficialValue    *float64
	OfficialCurrency string
	OfficialBasis    *UsagePricingBasis
}

// UsagePricingTransaction gives the calculator one consistent database view.
// The callback is pure: it must not call Store methods on the single connection.
func (s *Store) UsagePricingTransaction(f BackfillFilter, maxID int64, update bool, calculate func(UsagePricingSnapshot) ([]UsagePricingPatch, error)) (int64, error) {
	if !f.From.IsZero() && !f.To.IsZero() && f.From.After(f.To) {
		return 0, errors.New("from must not be later than to")
	}
	if f.ChannelID < 0 || maxID < -1 {
		return 0, errors.New("invalid range")
	}
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	s.settingsSaveMu.Lock()
	defer s.settingsSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	snapshot := UsagePricingSnapshot{MaxID: maxID, Usage: []Usage{}}
	if maxID == -1 {
		if err = tx.QueryRow("SELECT COALESCE(MAX(id),0) FROM usage_logs").Scan(&snapshot.MaxID); err != nil {
			return 0, err
		}
	}
	snapshot.Settings, err = catalogSettingsTx(tx)
	if err != nil {
		return 0, err
	}
	snapshot.Items, err = catalogItemsTx(tx)
	if err != nil {
		return 0, err
	}
	query := "SELECT id,data FROM usage_logs WHERE id<=? AND COALESCE(json_extract(data,'$.not_sent'),0)=0"
	args := []any{snapshot.MaxID}
	if !f.From.IsZero() {
		query += " AND time>=?"
		args = append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		query += " AND time<=?"
		args = append(args, f.To.UnixMilli())
	}
	if f.Model != "" {
		query += " AND model=?"
		args = append(args, f.Model)
	}
	if f.ChannelID != 0 {
		query += " AND channel_id=?"
		args = append(args, f.ChannelID)
	}
	query += " ORDER BY id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var raw string
		var u Usage
		if err = rows.Scan(&u.ID, &raw); err != nil {
			break
		}
		id := u.ID
		if err = json.Unmarshal([]byte(raw), &u); err != nil {
			break
		}
		u.ID = id
		if f.UpstreamModel == "" || u.UpstreamModel == f.UpstreamModel {
			snapshot.Usage = append(snapshot.Usage, u)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return 0, err
	}
	patches, err := calculate(snapshot)
	if err != nil {
		return snapshot.MaxID, err
	}
	if !update {
		return snapshot.MaxID, nil
	}
	original := map[int64]Usage{}
	for _, u := range snapshot.Usage {
		original[u.ID] = u
	}
	seen := map[int64]bool{}
	for _, p := range patches {
		u, ok := original[p.ID]
		if !ok || seen[p.ID] {
			return snapshot.MaxID, errors.New("invalid usage patch")
		}
		seen[p.ID] = true
		if p.ActualCost != nil && u.ActualCost == nil && (u.ActualPricing == nil || u.ActualPricing.Resolution.Layer != "plan") {
			u.ActualCost = p.ActualCost
			u.Currency = p.Currency
			u.ActualPricing = p.ActualBasis
		}
		if p.OfficialValue != nil && u.OfficialValue == nil {
			u.OfficialValue = p.OfficialValue
			u.OfficialCurrency = p.OfficialCurrency
			u.OfficialPricing = p.OfficialBasis
		}
		if err = normalizeUsagePricing(&u); err != nil {
			return snapshot.MaxID, err
		}
		body, err := json.Marshal(u)
		if err != nil {
			return snapshot.MaxID, err
		}
		if _, err = tx.Exec("UPDATE usage_logs SET data=? WHERE id=?", string(body), u.ID); err != nil {
			return snapshot.MaxID, err
		}
	}
	return snapshot.MaxID, tx.Commit()
}
