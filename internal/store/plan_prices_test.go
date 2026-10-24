package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func planTestStore(t *testing.T) (*Store, Channel) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := Channel{Name: "Monthly", APIKey: "synthetic-secret", BaseURL: "http://localhost:9999", Protocol: "openai", Enabled: true,
		Models: []string{"m"}, Tags: []string{"billing:plan", "privacy:unknown"}, ModelTags: map[string][]string{"m": {"billing:payg"}}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return s, c
}

func planTime(t *testing.T, text string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func planAmount(v float64) *float64 { return &v }

func planConfig(t *testing.T, id int64, first string, amount *float64) PlanPriceConfig {
	return PlanPriceConfig{ChannelID: id, DefaultAmount: amount, Currency: "CNY", FirstPaymentAt: planTime(t, first), Timezone: "Asia/Shanghai"}
}

func storedPlanCharges(t *testing.T, s *Store) []PlanCharge {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	out, err := listPlanChargesInTx(context.Background(), tx, UsageFilter{}, planTime(t, "9999-12-31T23:59:59Z"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPlanPricesMonthEndAndConcurrentAccrual(t *testing.T) {
	s, c := planTestStore(t)
	config := planConfig(t, c.ID, "2024-01-31T23:30:00+08:00", planAmount(98))
	first := config.FirstPaymentAt
	if err := s.SavePlanPrice(config, first); err != nil {
		t.Fatal(err)
	}
	now := planTime(t, "2024-04-30T23:30:00+08:00")
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.AccruePlanCharges(now) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	charges := storedPlanCharges(t, s)
	wantDates := []string{"2024-01-31T15:30:00Z", "2024-02-29T15:30:00Z", "2024-03-31T15:30:00Z", "2024-04-30T15:30:00Z"}
	if len(charges) != len(wantDates) {
		t.Fatalf("charges=%+v", charges)
	}
	for i, charge := range charges {
		if charge.PaidAt.Format(time.RFC3339) != wantDates[i] || charge.Amount == nil || *charge.Amount != 98 {
			t.Fatalf("charge %d=%+v, want date %s", i, charge, wantDates[i])
		}
	}
	view, err := s.GetPlanPrices("2024-02", now)
	if err != nil || len(view.Charges) != 1 || view.Charges[0].Month != "2024-02" || len(view.Prices) != 1 {
		t.Fatalf("month view=%+v err=%v", view, err)
	}
}

func TestPlanPricesOldDefaultFrozenCurrentCorrectionAndUnknown(t *testing.T) {
	s, c := planTestStore(t)
	config := planConfig(t, c.ID, "2025-01-10T12:00:00+08:00", planAmount(20))
	if err := s.SavePlanPrice(config, config.FirstPaymentAt); err != nil {
		t.Fatal(err)
	}
	// February was not read while the app was closed. Changing the default in
	// March must first land both already-due payments at the old price.
	config.DefaultAmount, config.Currency = planAmount(30), "USD"
	now := planTime(t, "2025-03-11T12:00:00+08:00")
	if err := s.SavePlanPrice(config, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePlanCharge(PlanCharge{ChannelID: c.ID, Month: "2025-03", Amount: planAmount(0), Currency: "USD", PaidAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AccruePlanCharges(planTime(t, "2025-04-10T12:00:00+08:00")); err != nil {
		t.Fatal(err)
	}
	config.DefaultAmount = nil
	if err := s.SavePlanPrice(config, planTime(t, "2025-04-11T12:00:00+08:00")); err != nil {
		t.Fatal(err)
	}
	if err := s.AccruePlanCharges(planTime(t, "2025-05-10T12:00:00+08:00")); err != nil {
		t.Fatal(err)
	}
	charges := storedPlanCharges(t, s)
	if len(charges) != 5 {
		t.Fatalf("charges=%+v", charges)
	}
	for i := 0; i < 2; i++ {
		if charges[i].Amount == nil || *charges[i].Amount != 20 || charges[i].Currency != "CNY" {
			t.Fatalf("old payment changed: %+v", charges[i])
		}
	}
	if charges[2].Amount == nil || *charges[2].Amount != 0 || charges[3].Amount == nil || *charges[3].Amount != 30 || charges[3].Currency != "USD" || charges[4].Amount != nil {
		t.Fatalf("zero, next default or unknown lost: %+v", charges)
	}
}

func TestPlanPricesDeclarationPausesButDisabledDoesNot(t *testing.T) {
	s, c := planTestStore(t)
	// Disable before configuring: activation affects calling, not subscription.
	c.Enabled = false
	if err := s.SaveChannelConfig(&c); err != nil {
		t.Fatal(err)
	}
	config := planConfig(t, c.ID, "2025-01-10T10:00:00+08:00", planAmount(10))
	if err := s.SavePlanPrice(config, config.FirstPaymentAt); err != nil {
		t.Fatal(err)
	}
	change := func(tag, at string) {
		t.Helper()
		if err := s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: tag}}, planTime(t, at)); err != nil {
			t.Fatal(err)
		}
	}
	change("billing:payg", "2025-02-11T00:00:00+08:00")
	change("billing:plan", "2025-05-11T00:00:00+08:00")
	if err := s.AccruePlanCharges(planTime(t, "2025-06-10T10:00:00+08:00")); err != nil {
		t.Fatal(err)
	}
	charges := storedPlanCharges(t, s)
	months := []string{}
	for _, charge := range charges {
		months = append(months, charge.Month)
	}
	if !reflect.DeepEqual(months, []string{"2025-01", "2025-02", "2025-06"}) {
		t.Fatalf("paused period was charged: %v", months)
	}
	actual, err := s.Channel(c.ID)
	if err != nil || actual.Enabled || actual.APIKey != c.APIKey || !reflect.DeepEqual(actual.ModelTags, c.ModelTags) || !containsString(actual.Tags, "privacy:unknown") {
		t.Fatalf("declaration changed independent fields: channel=%+v err=%v", actual, err)
	}
	// A new explicitly chosen first payment is the only way to backdate a
	// resumed configuration; merely changing its price must keep the gap.
	config.DefaultAmount = planAmount(12)
	if err := s.SavePlanPrice(config, planTime(t, "2025-06-11T00:00:00+08:00")); err != nil {
		t.Fatal(err)
	}
	if got := len(storedPlanCharges(t, s)); got != 3 {
		t.Fatalf("default edit refilled paused months: %d", got)
	}
	config.Timezone = "Asia/Tokyo"
	if err := s.SavePlanPrice(config, planTime(t, "2025-06-11T00:00:00+08:00")); err != nil {
		t.Fatal(err)
	}
	if got := len(storedPlanCharges(t, s)); got != 3 {
		t.Fatalf("timezone edit refilled paused months: %d", got)
	}
	config.FirstPaymentAt = planTime(t, "2025-01-09T10:00:00+08:00")
	if err := s.SavePlanPrice(config, planTime(t, "2025-06-11T00:00:00+08:00")); err != nil {
		t.Fatal(err)
	}
	if got := len(storedPlanCharges(t, s)); got != 6 {
		t.Fatalf("explicit schedule did not refill chosen range: %d", got)
	}
}

func TestPlanPricesReopenBackfillsClosedMonthsOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := Channel{Name: "Synthetic reopen", APIKey: "synthetic", BaseURL: "http://localhost:9999", Protocol: "openai", Tags: []string{"billing:plan"}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	config := planConfig(t, c.ID, "2025-01-31T23:30:00+08:00", planAmount(10))
	if err = s.SavePlanPrice(config, config.FirstPaymentAt); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		view, err := s.GetPlanPrices("2025-04", planTime(t, "2025-04-30T23:30:00+08:00"))
		if err != nil || len(view.Charges) != 1 || len(storedPlanCharges(t, s)) != 4 {
			t.Fatalf("reopen/read %d did not accrue exactly once: %+v %v", i, view, err)
		}
	}
}

func TestPlanPricesLifecycleFailuresRollback(t *testing.T) {
	for _, operation := range []string{"default", "declaration", "ordinary_edit", "delete_channel", "delete_config", "row"} {
		t.Run(operation, func(t *testing.T) {
			s, c := planTestStore(t)
			first := time.Now().AddDate(0, -2, 0)
			config := PlanPriceConfig{ChannelID: c.ID, DefaultAmount: planAmount(10), Currency: "USD", FirstPaymentAt: first, Timezone: "UTC"}
			if err := s.SavePlanPrice(config, first); err != nil {
				t.Fatal(err)
			}
			// Fail after accrual has inserted the outstanding months, so success
			// of the test proves transaction rollback rather than early validation.
			var trigger string
			switch operation {
			case "default", "row":
				trigger = "CREATE TRIGGER fail_plan BEFORE UPDATE ON plan_prices BEGIN SELECT RAISE(ABORT,'injected'); END"
			case "declaration", "ordinary_edit":
				trigger = "CREATE TRIGGER fail_plan BEFORE UPDATE ON channels BEGIN SELECT RAISE(ABORT,'injected'); END"
			case "delete_channel":
				trigger = "CREATE TRIGGER fail_plan BEFORE DELETE ON channels BEGIN SELECT RAISE(ABORT,'injected'); END"
			case "delete_config":
				trigger = "CREATE TRIGGER fail_plan BEFORE DELETE ON plan_prices BEGIN SELECT RAISE(ABORT,'injected'); END"
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "default":
				config.DefaultAmount = planAmount(99)
				err = s.SavePlanPrice(config, time.Now())
			case "row":
				err = s.SavePlanPriceWithCharge(config, &PlanCharge{Month: first.Format("2006-01"), Amount: planAmount(99), Currency: "USD", PaidAt: first}, time.Now())
			case "declaration":
				err = s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:free"}}, time.Now())
			case "ordinary_edit":
				c.Tags = []string{"billing:free"}
				err = s.SaveChannelConfig(&c)
			case "delete_channel":
				err = s.DeleteChannel(c.ID)
			case "delete_config":
				err = s.DeletePlanPrice(c.ID, time.Now())
			}
			if err == nil {
				t.Fatal("injected write failure was swallowed")
			}
			if got := len(storedPlanCharges(t, s)); got != 1 {
				t.Fatalf("partial accrual committed: %d", got)
			}
			stored, err := s.Channel(c.ID)
			if err != nil || !channelIsPlan(stored) {
				t.Fatalf("channel mutation was not rolled back: %+v %v", stored.Tags, err)
			}
			var amount float64
			var from sql.NullInt64
			if err = s.db.QueryRow("SELECT default_amount,active_from FROM plan_prices WHERE channel_id=?", c.ID).Scan(&amount, &from); err != nil || amount != 10 || !from.Valid {
				t.Fatalf("config mutation was not rolled back: %v %v %v", amount, from, err)
			}
		})
	}
}

func TestPlanPricesBatchDeclarationAllOrNothing(t *testing.T) {
	s, c := planTestStore(t)
	before, _ := s.Channel(c.ID)
	err := s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:free"}, {ChannelID: c.ID + 500, BillingTag: "billing:plan"}}, time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing channel: %v", err)
	}
	after, _ := s.Channel(c.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("first declaration changed despite later batch failure")
	}
}

func TestPlanPricesDeclarationUsesActualTagGroups(t *testing.T) {
	s, c := planTestStore(t)
	for _, tag := range []Tag{
		{ID: "custom-billing", GroupID: "billing", Name: "Custom billing"},
		{ID: "billing:privacy-custom", GroupID: "privacy", Name: "Private tag"},
	} {
		if err := s.SaveTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	c.Tags = []string{"custom-billing", "billing:privacy-custom"}
	if err := s.SaveChannelConfig(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:plan"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	actual, err := s.Channel(c.ID)
	if err != nil || !reflect.DeepEqual(actual.Tags, []string{"billing:privacy-custom", "billing:plan"}) {
		t.Fatalf("default billing changed incorrect group: %+v %v", actual.Tags, err)
	}
}

func TestPlanPricesDeletePreservesHistoryAndAllowsCorrectionsOnly(t *testing.T) {
	for _, kind := range []string{"configuration", "channel", "declaration"} {
		t.Run(kind, func(t *testing.T) {
			s, c := planTestStore(t)
			first := time.Now().AddDate(0, -1, 0)
			config := PlanPriceConfig{ChannelID: c.ID, DefaultAmount: planAmount(10), Currency: "USD", FirstPaymentAt: first, Timezone: "UTC"}
			if err := s.SavePlanPrice(config, first); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "configuration":
				err = s.DeletePlanPrice(c.ID, time.Now())
			case "channel":
				err = s.DeleteChannel(c.ID)
			case "declaration":
				c.Tags = []string{"billing:payg", "privacy:unknown"}
				err = s.SaveChannelConfig(&c)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := storedPlanCharges(t, s)
			if len(before) < 2 {
				t.Fatalf("delete did not settle due fees: %v", before)
			}
			if err = s.AccruePlanCharges(time.Now().AddDate(1, 0, 0)); err != nil || len(storedPlanCharges(t, s)) != len(before) {
				t.Fatalf("stopped configuration accrued future fees: %v", err)
			}
			charge := before[0]
			charge.Amount = planAmount(0)
			if err = s.SavePlanCharge(charge, time.Now()); err != nil {
				t.Fatal(err)
			}
			if got := storedPlanCharges(t, s)[0]; got.Amount == nil || *got.Amount != 0 || got.ChannelName != "Monthly" {
				t.Fatalf("historical correction lost: %+v", got)
			}
			if kind != "configuration" {
				charge.Month = "2040-01"
				if err = s.SavePlanCharge(charge, time.Now()); err == nil {
					t.Fatal("inactive/deleted channel created a new charge")
				}
			} else if stored, err := s.Channel(c.ID); err != nil || !channelIsPlan(stored) {
				t.Fatal("deleting configuration changed declaration")
			}
		})
	}
}

func TestPlanPricesFutureChargeSnapshotFiltersAndAtomicRow(t *testing.T) {
	s, c := planTestStore(t)
	now := planTime(t, "2025-01-01T00:00:00Z")
	config := planConfig(t, c.ID, "2025-02-10T12:00:00+08:00", planAmount(20))
	charge := PlanCharge{Month: "2025-02", Amount: nil, Currency: "CNY", PaidAt: config.FirstPaymentAt}
	if err := s.SavePlanPriceWithCharge(config, &charge, now); err != nil {
		t.Fatal(err)
	}
	view, err := s.GetPlanPrices("2025-02", now)
	if err != nil || len(view.Charges) != 1 || view.Charges[0].Amount != nil {
		t.Fatalf("explicit future edit unavailable: %+v %v", view, err)
	}
	for _, tc := range []struct {
		filter UsageFilter
		now    time.Time
		want   int
	}{
		{UsageFilter{}, now, 0},
		{UsageFilter{}, config.FirstPaymentAt, 1},
		{UsageFilter{ChannelID: c.ID + 1}, config.FirstPaymentAt, 0},
		{UsageFilter{From: config.FirstPaymentAt, To: config.FirstPaymentAt}, config.FirstPaymentAt, 1},
		{UsageFilter{From: config.FirstPaymentAt.Add(time.Millisecond)}, config.FirstPaymentAt, 0},
		{UsageFilter{Model: "m"}, config.FirstPaymentAt, 0},
		{UsageFilter{Status: 200}, config.FirstPaymentAt, 0},
	} {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		charges, err := listPlanChargesInTx(context.Background(), tx, tc.filter, tc.now)
		tx.Rollback()
		if err != nil || len(charges) != tc.want {
			t.Fatalf("filter %+v got %v err %v", tc, charges, err)
		}
	}
	// A bad charge after the default write must roll the entire row back.
	config.DefaultAmount = planAmount(100)
	charge.Currency = "invalid"
	if err := s.SavePlanPriceWithCharge(config, &charge, now); err == nil {
		t.Fatal("invalid current charge accepted")
	}
	view, err = s.GetPlanPrices("2025-02", now)
	if err != nil || *view.Prices[0].DefaultAmount != 20 {
		t.Fatalf("partial row save %+v %v", view, err)
	}
	// Database errors cannot be passed off as a successful empty ledger.
	if _, err := s.db.Exec("DROP TABLE plan_charges"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPlanPrices("2025-02", now); err == nil {
		t.Fatal("read database error swallowed")
	}
}

func TestPlanPricesValidation(t *testing.T) {
	s, c := planTestStore(t)
	now := time.Now()
	for i, change := range []func(*PlanPriceConfig){
		func(v *PlanPriceConfig) { v.DefaultAmount = planAmount(-1) },
		func(v *PlanPriceConfig) { v.Timezone = "Local" },
		func(v *PlanPriceConfig) { v.Timezone = "" },
		func(v *PlanPriceConfig) { v.Timezone = "Nowhere/Invalid" },
		func(v *PlanPriceConfig) { v.Currency = "EUR" },
		func(v *PlanPriceConfig) { v.FirstPaymentAt = time.Time{} },
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			config := planConfig(t, c.ID, "2025-01-01T00:00:00Z", nil)
			change(&config)
			if err := s.SavePlanPrice(config, now); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	for _, month := range []string{"", "2025-1", "2025-13", "0000-01", "2025-01x"} {
		if err := ValidatePlanMonth(month); err == nil {
			t.Fatalf("invalid month %q accepted", month)
		}
	}
}
