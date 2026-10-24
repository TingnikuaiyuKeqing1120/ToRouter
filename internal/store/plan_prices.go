package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

type PlanPriceConfig struct {
	ChannelID      int64     `json:"channel_id"`
	DefaultAmount  *float64  `json:"default_amount"`
	Currency       string    `json:"currency"`
	FirstPaymentAt time.Time `json:"first_payment_at"`
	Timezone       string    `json:"timezone"`
}

type PlanCharge struct {
	ChannelID   int64     `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	Month       string    `json:"month"`
	Amount      *float64  `json:"amount"`
	Currency    string    `json:"currency"`
	PaidAt      time.Time `json:"paid_at"`
}

type PlanPrices struct {
	Month     string            `json:"month"`
	Prices    []PlanPriceConfig `json:"prices"`
	Charges   []PlanCharge      `json:"charges"`
	Scheduled []PlanCharge      `json:"scheduled"`
}

type ChannelBillingChange struct {
	ChannelID  int64  `json:"channel_id"`
	BillingTag string `json:"billing_tag"`
}

func validatePlanAmount(amount *float64, currency string) error {
	if amount != nil && (math.IsNaN(*amount) || math.IsInf(*amount, 0) || *amount < 0) {
		return NewConfigurationError("金额须为非负数，留空表示未知")
	}
	if currency != "USD" && currency != "CNY" {
		return NewConfigurationError("币种须为 USD 或 CNY")
	}
	return nil
}

func ValidatePlanMonth(month string) error {
	parsed, err := time.Parse("2006-01", month)
	if err != nil || parsed.Format("2006-01") != month || parsed.Year() < 1 {
		return NewConfigurationError("月份格式须为 YYYY-MM")
	}
	return nil
}

func channelIsPlan(c Channel) bool { return containsString(c.Tags, "billing:plan") }

// PlanPriceState is the exact editable schedule and stored charges used for CAS.
// It deliberately contains no channel credentials or request allocation.
type PlanPriceState struct {
	ChannelID  int64            `json:"channel_id"`
	Plan       bool             `json:"plan"`
	Config     *PlanPriceConfig `json:"config"`
	ActiveFrom *int64           `json:"active_from"`
	Charges    []PlanCharge     `json:"charges"`
}

func planPriceStateInTx(tx *sql.Tx, id int64) (PlanPriceState, error) {
	out := PlanPriceState{ChannelID: id, Charges: []PlanCharge{}}
	c, _, err := planChannelInTx(tx, id)
	if err != nil {
		return out, err
	}
	out.Plan = channelIsPlan(c)
	config := PlanPriceConfig{ChannelID: id}
	var first string
	err = tx.QueryRow("SELECT default_amount,currency,first_payment_at,timezone,active_from FROM plan_prices WHERE channel_id=?", id).Scan(&config.DefaultAmount, &config.Currency, &first, &config.Timezone, &out.ActiveFrom)
	if err == nil {
		config.FirstPaymentAt, err = time.Parse(time.RFC3339Nano, first)
		if err != nil {
			return out, err
		}
		out.Config = &config
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	rows, err := tx.Query("SELECT channel_name,month,amount,currency,paid_at FROM plan_charges WHERE channel_id=? ORDER BY month", id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		item := PlanCharge{ChannelID: id}
		var paid int64
		if err = rows.Scan(&item.ChannelName, &item.Month, &item.Amount, &item.Currency, &paid); err != nil {
			break
		}
		item.PaidAt = time.UnixMilli(paid).UTC()
		out.Charges = append(out.Charges, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	return out, err
}

func (s *Store) GetPlanPriceState(id int64) (PlanPriceState, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return PlanPriceState{}, err
	}
	defer tx.Rollback()
	out, err := planPriceStateInTx(tx, id)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func PlanScheduleNeedsMaintenance(config PlanPriceConfig, previous *PlanPriceConfig, now time.Time) bool {
	if previous != nil && previous.FirstPaymentAt.Equal(config.FirstPaymentAt) && previous.Timezone == config.Timezone {
		return false
	}
	loc, err := UsageLocation(config.Timezone)
	if err != nil {
		return false
	}
	current := now.In(loc)
	start := time.Date(current.Year(), current.Month(), 1, 0, 0, 0, 0, loc)
	return config.FirstPaymentAt.Before(start)
}

func planChannelInTx(tx *sql.Tx, id int64) (Channel, string, error) {
	var raw string
	if err := tx.QueryRow("SELECT data FROM channels WHERE id=?", id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Channel{}, "", ErrNotFound
		}
		return Channel{}, "", err
	}
	var c Channel
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Channel{}, "", err
	}
	c.ID = id
	return c, raw, nil
}

// planPaymentAt repeats the original local wall clock, clamping month-end
// without allowing February's shorter date to move subsequent payments.
func planPaymentAt(first time.Time, year int, month time.Month, loc *time.Location) time.Time {
	first = first.In(loc)
	day := first.Day()
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
	if day > last {
		day = last
	}
	return time.Date(year, month, day, first.Hour(), first.Minute(), first.Second(), first.Nanosecond(), loc)
}

// accruePlanChargesInTx closes its read cursor before writing. Store has one
// SQLite connection; every statement inside this operation uses the same tx.
func accruePlanChargesInTx(tx *sql.Tx, now time.Time, channelID int64) error {
	query := `SELECT p.channel_id,p.default_amount,p.currency,p.first_payment_at,p.timezone,p.active_from,c.data
 FROM plan_prices p JOIN channels c ON c.id=p.channel_id WHERE p.active_from IS NOT NULL`
	args := []any{}
	if channelID != 0 {
		query += " AND p.channel_id=?"
		args = append(args, channelID)
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	type pending struct {
		config PlanPriceConfig
		from   int64
		name   string
	}
	configs := []pending{}
	for rows.Next() {
		var p pending
		var first, raw string
		if err = rows.Scan(&p.config.ChannelID, &p.config.DefaultAmount, &p.config.Currency, &first, &p.config.Timezone, &p.from, &raw); err != nil {
			break
		}
		p.config.FirstPaymentAt, err = time.Parse(time.RFC3339Nano, first)
		if err != nil {
			break
		}
		var c Channel
		if err = json.Unmarshal([]byte(raw), &c); err != nil {
			break
		}
		if channelIsPlan(c) {
			p.name = c.Name
			configs = append(configs, p)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range configs {
		loc, err := UsageLocation(p.config.Timezone)
		if err != nil {
			return err
		}
		first := p.config.FirstPaymentAt.In(loc)
		from := time.UnixMilli(p.from).In(loc)
		if from.Before(first) {
			from = first
		}
		end := now.In(loc)
		for year, month := from.Year(), from.Month(); year < end.Year() || year == end.Year() && month <= end.Month(); {
			paid := planPaymentAt(first, year, month, loc)
			if !paid.Before(first) && !paid.Before(from) && !paid.After(now) {
				period := fmt.Sprintf("%04d-%02d", year, month)
				if _, err = tx.Exec(`INSERT INTO plan_charges(channel_id,month,channel_name,amount,currency,paid_at)
 VALUES(?,?,?,?,?,?) ON CONFLICT(channel_id,month) DO NOTHING`, p.config.ChannelID, period, p.name, p.config.DefaultAmount, p.config.Currency, paid.UnixMilli()); err != nil {
					return err
				}
			}
			month++
			if month > 12 {
				year++
				month = 1
			}
		}
	}
	return nil
}

func (s *Store) AccruePlanCharges(now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = accruePlanChargesInTx(tx, now, 0); err != nil {
		return err
	}
	return tx.Commit()
}

// listPlanChargesInTx lets the usage summary read requests and charges from
// one snapshot. Request-only filters have no honest attribution for month fees.
func listPlanChargesInTx(ctx context.Context, tx *sql.Tx, f UsageFilter, now time.Time) ([]PlanCharge, error) {
	out := []PlanCharge{}
	if f.Model != "" || f.Status != 0 {
		return out, nil
	}
	query := "SELECT channel_id,channel_name,month,amount,currency,paid_at FROM plan_charges WHERE paid_at<=?"
	args := []any{now.UnixMilli()}
	if !f.From.IsZero() {
		query += " AND paid_at>=?"
		args = append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		query += " AND paid_at<=?"
		args = append(args, f.To.UnixMilli())
	}
	if f.ChannelID != 0 {
		query += " AND channel_id=?"
		args = append(args, f.ChannelID)
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY paid_at,channel_id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c PlanCharge
		var paid int64
		if err = rows.Scan(&c.ChannelID, &c.ChannelName, &c.Month, &c.Amount, &c.Currency, &paid); err != nil {
			return nil, err
		}
		c.PaidAt = time.UnixMilli(paid).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetPlanPrices(month string, now time.Time) (PlanPrices, error) {
	out := PlanPrices{Month: month, Prices: []PlanPriceConfig{}, Charges: []PlanCharge{}, Scheduled: []PlanCharge{}}
	if err := ValidatePlanMonth(month); err != nil {
		return out, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = accruePlanChargesInTx(tx, now, 0); err != nil {
		return out, err
	}
	rows, err := tx.Query(`SELECT p.channel_id,p.default_amount,p.currency,p.first_payment_at,p.timezone,p.active_from,COALESCE(json_extract(c.data,'$.name'),'')
 FROM plan_prices p JOIN channels c ON c.id=p.channel_id
 WHERE EXISTS (SELECT 1 FROM json_each(c.data,'$.tags') WHERE value='billing:plan') ORDER BY p.channel_id`)
	if err != nil {
		return out, err
	}
	type schedule struct {
		config PlanPriceConfig
		active sql.NullInt64
		name   string
	}
	schedules := []schedule{}
	for rows.Next() {
		var p PlanPriceConfig
		var first string
		var active sql.NullInt64
		var name string
		if err = rows.Scan(&p.ChannelID, &p.DefaultAmount, &p.Currency, &first, &p.Timezone, &active, &name); err != nil {
			break
		}
		p.FirstPaymentAt, err = time.Parse(time.RFC3339Nano, first)
		if err != nil {
			break
		}
		out.Prices = append(out.Prices, p)
		schedules = append(schedules, schedule{config: p, active: active, name: name})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.Query("SELECT channel_id,channel_name,month,amount,currency,paid_at FROM plan_charges WHERE month=? ORDER BY channel_id", month)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var c PlanCharge
		var paid int64
		if err = rows.Scan(&c.ChannelID, &c.ChannelName, &c.Month, &c.Amount, &c.Currency, &paid); err != nil {
			break
		}
		c.PaidAt = time.UnixMilli(paid).UTC()
		out.Charges = append(out.Charges, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	// Project pending payments with the same wall-clock/month-end/DST rule as
	// accrual. Existing actual or explicitly edited future charges take priority.
	stored := map[int64]bool{}
	for _, charge := range out.Charges {
		stored[charge.ChannelID] = true
	}
	period, _ := time.Parse("2006-01", month) // Validated before opening the transaction.
	for _, item := range schedules {
		p := item.config
		if stored[p.ChannelID] || !item.active.Valid {
			continue
		}
		loc, err := UsageLocation(p.Timezone)
		if err != nil {
			return out, err
		}
		paid := planPaymentAt(p.FirstPaymentAt, period.Year(), period.Month(), loc)
		if paid.Before(p.FirstPaymentAt) || paid.Before(time.UnixMilli(item.active.Int64)) || !paid.After(now) {
			continue
		}
		out.Scheduled = append(out.Scheduled, PlanCharge{ChannelID: p.ChannelID, ChannelName: item.name, Month: month, Amount: p.DefaultAmount, Currency: p.Currency, PaidAt: paid.UTC()})
	}
	return out, tx.Commit()
}

func (s *Store) SavePlanPrice(config PlanPriceConfig, now time.Time) error {
	return s.SavePlanPriceWithCharge(config, nil, now)
}

// SavePlanPriceWithCharge saves one editable table row atomically. A nil
// charge changes only the default schedule; existing charges remain frozen.
func (s *Store) SavePlanPriceWithCharge(config PlanPriceConfig, charge *PlanCharge, now time.Time) error {
	return s.savePlanPriceWithCharge(config, charge, now, "", true)
}

func (s *Store) SavePlanPriceChecked(config PlanPriceConfig, now time.Time, expected string, allowHistoricalSchedule bool) error {
	if expected == "" {
		return ErrEditConflict
	}
	return s.savePlanPriceWithCharge(config, nil, now, expected, allowHistoricalSchedule)
}

func (s *Store) savePlanPriceWithCharge(config PlanPriceConfig, charge *PlanCharge, now time.Time, expected string, allowHistoricalSchedule bool) error {
	if err := validatePlanAmount(config.DefaultAmount, config.Currency); err != nil {
		return err
	}
	if config.FirstPaymentAt.IsZero() || config.FirstPaymentAt.Year() < 1 || config.Timezone == "" {
		return NewConfigurationError("请填写首次支付时间和时区")
	}
	if _, err := UsageLocation(config.Timezone); err != nil {
		return NewConfigurationError("请选择有效时区")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, _, err := planChannelInTx(tx, config.ChannelID)
	if err != nil {
		return err
	}
	if !channelIsPlan(c) {
		return NewConfigurationError("请先将渠道声明为套餐")
	}
	if expected != "" {
		state, e := planPriceStateInTx(tx, config.ChannelID)
		if e != nil {
			return e
		}
		if EntityVersion(state) != expected {
			return ErrEditConflict
		}
		if !allowHistoricalSchedule && PlanScheduleNeedsMaintenance(config, state.Config, now) {
			return NewConfigurationError("历史记账范围变更须使用维护预览")
		}
	}
	if err = accruePlanChargesInTx(tx, now, config.ChannelID); err != nil {
		return err
	}
	var previousFirst string
	var active sql.NullInt64
	err = tx.QueryRow("SELECT first_payment_at,active_from FROM plan_prices WHERE channel_id=?", config.ChannelID).Scan(&previousFirst, &active)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	first := config.FirstPaymentAt.Format(time.RFC3339Nano)
	previous, parseErr := time.Parse(time.RFC3339Nano, previousFirst)
	if errors.Is(err, sql.ErrNoRows) || parseErr != nil || !previous.Equal(config.FirstPaymentAt) {
		// Explicitly changing the start schedule authorizes its chosen range.
		active = sql.NullInt64{Int64: config.FirstPaymentAt.UnixMilli(), Valid: true}
	} else if !active.Valid {
		active = sql.NullInt64{Int64: now.UnixMilli(), Valid: true}
	}
	if _, err = tx.Exec(`INSERT INTO plan_prices(channel_id,default_amount,currency,first_payment_at,timezone,active_from)
 VALUES(?,?,?,?,?,?) ON CONFLICT(channel_id) DO UPDATE SET default_amount=excluded.default_amount,currency=excluded.currency,
 first_payment_at=excluded.first_payment_at,timezone=excluded.timezone,active_from=excluded.active_from`, config.ChannelID, config.DefaultAmount, config.Currency, first, config.Timezone, active); err != nil {
		return err
	}
	if err = accruePlanChargesInTx(tx, now, config.ChannelID); err != nil {
		return err
	}
	if charge != nil {
		value := *charge
		value.ChannelID = config.ChannelID
		if err = savePlanChargeInTx(tx, value, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeletePlanPrice(channelID int64, now time.Time) error {
	return s.deletePlanPrice(channelID, now, "")
}

func (s *Store) DeletePlanPriceChecked(channelID int64, now time.Time, expected string) error {
	if expected == "" {
		return ErrEditConflict
	}
	return s.deletePlanPrice(channelID, now, expected)
}

func (s *Store) deletePlanPrice(channelID int64, now time.Time, expected string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expected != "" {
		state, e := planPriceStateInTx(tx, channelID)
		if e != nil {
			return e
		}
		if EntityVersion(state) != expected {
			return ErrEditConflict
		}
	}
	if err = accruePlanChargesInTx(tx, now, channelID); err != nil {
		return err
	}
	if err = cancelFuturePlanChargesInTx(tx, channelID, now); err != nil {
		return err
	}
	res, err := tx.Exec("DELETE FROM plan_prices WHERE channel_id=?", channelID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) SavePlanCharge(charge PlanCharge, now time.Time) error {
	return s.savePlanCharge(charge, now, "")
}

func (s *Store) SavePlanChargeChecked(charge PlanCharge, now time.Time, expected string) error {
	if expected == "" {
		return ErrEditConflict
	}
	return s.savePlanCharge(charge, now, expected)
}

func (s *Store) savePlanCharge(charge PlanCharge, now time.Time, expected string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expected != "" {
		state, e := planPriceStateInTx(tx, charge.ChannelID)
		if e != nil {
			return e
		}
		if EntityVersion(state) != expected {
			return ErrEditConflict
		}
		zone := "UTC"
		if state.Config != nil {
			zone = state.Config.Timezone
		}
		loc, e := UsageLocation(zone)
		if e != nil {
			return e
		}
		current := now.In(loc)
		start := time.Date(current.Year(), current.Month(), 1, 0, 0, 0, 0, loc)
		if charge.Month != current.Format("2006-01") || charge.PaidAt.Before(start) || charge.PaidAt.In(loc).Format("2006-01") != charge.Month {
			return NewConfigurationError("历史期次只能使用金额更正维护流程")
		}
		for _, old := range state.Charges {
			if old.Month == charge.Month && old.PaidAt.Before(start) {
				return NewConfigurationError("已发生历史支付只能使用金额更正维护流程")
			}
		}
	}
	if err = savePlanChargeInTx(tx, charge, now); err != nil {
		return err
	}
	return tx.Commit()
}

func savePlanChargeInTx(tx *sql.Tx, charge PlanCharge, now time.Time) error {
	if err := ValidatePlanMonth(charge.Month); err != nil {
		return err
	}
	if err := validatePlanAmount(charge.Amount, charge.Currency); err != nil {
		return err
	}
	if charge.PaidAt.IsZero() || charge.PaidAt.Year() < 1 {
		return NewConfigurationError("请填写支付时间")
	}
	if err := accruePlanChargesInTx(tx, now, charge.ChannelID); err != nil {
		return err
	}
	var name string
	err := tx.QueryRow("SELECT channel_name FROM plan_charges WHERE channel_id=? AND month=?", charge.ChannelID, charge.Month).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		c, _, channelErr := planChannelInTx(tx, charge.ChannelID)
		if channelErr != nil {
			return channelErr
		}
		if !channelIsPlan(c) {
			return NewConfigurationError("已退出套餐的渠道只能更正已有账单")
		}
		name = c.Name
	} else if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO plan_charges(channel_id,month,channel_name,amount,currency,paid_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(channel_id,month) DO UPDATE SET amount=excluded.amount,currency=excluded.currency,paid_at=excluded.paid_at`, charge.ChannelID, charge.Month, name, charge.Amount, charge.Currency, charge.PaidAt.UnixMilli())
	if err != nil {
		return err
	}
	return nil
}

// cancelFuturePlanChargesInTx cancels pending entries when the subscription
// stops. An advance payment remains history even if its period is in the future.
func cancelFuturePlanChargesInTx(tx *sql.Tx, channelID int64, now time.Time) error {
	_, err := tx.Exec("DELETE FROM plan_charges WHERE channel_id=? AND paid_at>?", channelID, now.UnixMilli())
	return err
}

// transitionPlanChannelInTx is called before the channel changes. Paused
// configs keep their schedule but resume only from the new declaration time.
func transitionPlanChannelInTx(tx *sql.Tx, old, updated Channel, now time.Time) error {
	if channelIsPlan(old) {
		if err := accruePlanChargesInTx(tx, now, old.ID); err != nil {
			return err
		}
	}
	if channelIsPlan(old) == channelIsPlan(updated) {
		return nil
	}
	if channelIsPlan(old) {
		if err := cancelFuturePlanChargesInTx(tx, old.ID, now); err != nil {
			return err
		}
	}
	var from any
	if channelIsPlan(updated) {
		from = now.UnixMilli()
	}
	_, err := tx.Exec("UPDATE plan_prices SET active_from=? WHERE channel_id=?", from, old.ID)
	return err
}

func (s *Store) SaveChannelBilling(changes []ChannelBillingChange, now time.Time) error {
	return s.saveChannelBilling(changes, now, nil)
}

func (s *Store) SaveChannelBillingChecked(changes []ChannelBillingChange, now time.Time, expected map[int64]string) error {
	if len(changes) < 1 || len(changes) > 100 || len(expected) != len(changes) {
		return NewConfigurationError("choose 1..100 billing changes with versions")
	}
	return s.saveChannelBilling(changes, now, expected)
}

func (s *Store) saveChannelBilling(changes []ChannelBillingChange, now time.Time, expected map[int64]string) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id FROM tags WHERE group_id='billing'")
	if err != nil {
		return err
	}
	billingTags := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		billingTags[id] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, change := range changes {
		if change.ChannelID <= 0 || seen[change.ChannelID] {
			return NewConfigurationError("渠道编号须为正整数且不能重复")
		}
		seen[change.ChannelID] = true
		switch change.BillingTag {
		case "billing:free", "billing:plan", "billing:payg", "billing:unknown":
		default:
			return NewConfigurationError("请选择有效的计费类型")
		}
		c, raw, err := planChannelInTx(tx, change.ChannelID)
		if err != nil {
			return err
		}
		if expected != nil {
			opened, e := s.openChannel(change.ChannelID, raw)
			if e != nil {
				return e
			}
			if expected[change.ChannelID] == "" || EntityVersion(opened) != expected[change.ChannelID] {
				return ErrEditConflict
			}
		}
		updated := c
		updated.Tags = []string{}
		for _, tag := range c.Tags {
			if !billingTags[tag] {
				updated.Tags = append(updated.Tags, tag)
			}
		}
		updated.Tags = append(updated.Tags, change.BillingTag)
		if err = validateObjectTagsInTx(tx, updated.Tags); err != nil {
			return err
		}
		if err = transitionPlanChannelInTx(tx, c, updated, now); err != nil {
			return err
		}
		// Replace exactly the default group, retaining sealed credentials and
		// every other field as stored, including model-independent tag groups.
		var data map[string]json.RawMessage
		if err = json.Unmarshal([]byte(raw), &data); err != nil {
			return err
		}
		data["tags"], err = json.Marshal(updated.Tags)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE channels SET data=? WHERE id=?", string(encoded), change.ChannelID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
