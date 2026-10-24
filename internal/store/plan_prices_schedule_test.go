package store

import (
	"reflect"
	"testing"
	"time"
)

func TestPlanPricesScheduledProjectionMatchesAccrual(t *testing.T) {
	for _, tc := range []struct {
		name, first, zone, month string
		amount                   *float64
	}{
		{"dst_gap", "2024-01-10T02:30:00-05:00", "America/New_York", "2024-03", planAmount(10)},
		{"dst_overlap", "2024-01-03T01:30:00-05:00", "America/New_York", "2024-11", planAmount(20)},
		{"month_end_unknown", "2024-01-31T23:30:00+08:00", "Asia/Shanghai", "2024-02", nil},
		{"month_end_zero", "2025-01-31T23:30:00+08:00", "Asia/Shanghai", "2025-02", planAmount(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := planTestStore(t)
			config := PlanPriceConfig{ChannelID: c.ID, DefaultAmount: tc.amount, Currency: "CNY", FirstPaymentAt: planTime(t, tc.first), Timezone: tc.zone}
			now := config.FirstPaymentAt.Add(-time.Minute)
			if err := s.SavePlanPrice(config, now); err != nil {
				t.Fatal(err)
			}
			view, err := s.GetPlanPrices(tc.month, now)
			if err != nil || len(view.Charges) != 0 || len(view.Scheduled) != 1 || len(storedPlanCharges(t, s)) != 0 {
				t.Fatalf("projection created actual bills or missed schedule: %+v %v", view, err)
			}
			projected := view.Scheduled[0]
			if projected.ChannelID != c.ID || projected.ChannelName != c.Name || projected.Month != tc.month || projected.Currency != "CNY" || !reflect.DeepEqual(projected.Amount, tc.amount) {
				t.Fatalf("projection lost channel/default metadata: %+v", projected)
			}
			// At the projected instant, actual accrual must land exactly what
			// the page showed, including DST gaps/overlaps and unknown/zero.
			view, err = s.GetPlanPrices(tc.month, projected.PaidAt)
			if err != nil || len(view.Charges) != 1 || len(view.Scheduled) != 0 || !reflect.DeepEqual(view.Charges[0], projected) {
				t.Fatalf("displayed schedule and actual charge diverged: scheduled=%+v actual=%+v err=%v", projected, view, err)
			}
		})
	}
}

func TestPlanPricesScheduledRespectsExplicitChargeAndActivationBounds(t *testing.T) {
	s, c := planTestStore(t)
	config := planConfig(t, c.ID, "2025-01-31T23:30:00+08:00", planAmount(10))
	now := config.FirstPaymentAt.Add(-time.Minute)
	if err := s.SavePlanPrice(config, now); err != nil {
		t.Fatal(err)
	}
	view, err := s.GetPlanPrices("2024-12", now)
	if err != nil || len(view.Scheduled) != 0 {
		t.Fatalf("schedule predates first payment: %+v %v", view, err)
	}
	explicit := PlanCharge{ChannelID: c.ID, Month: "2025-02", Amount: planAmount(99), Currency: "USD", PaidAt: planTime(t, "2025-02-01T00:00:00Z")}
	if err := s.SavePlanCharge(explicit, now); err != nil {
		t.Fatal(err)
	}
	view, err = s.GetPlanPrices(explicit.Month, now)
	if err != nil || len(view.Scheduled) != 0 || len(view.Charges) != 1 || *view.Charges[0].Amount != 99 {
		t.Fatalf("explicit future charge did not take priority: %+v %v", view, err)
	}
	if err = s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:free"}}, now); err != nil {
		t.Fatal(err)
	}
	view, err = s.GetPlanPrices("2025-03", now)
	if err != nil || len(view.Scheduled) != 0 {
		t.Fatalf("inactive channel has schedule: %+v %v", view, err)
	}
	resume := planTime(t, "2025-06-01T00:00:00+08:00")
	if err = s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:plan"}}, resume); err != nil {
		t.Fatal(err)
	}
	view, err = s.GetPlanPrices("2025-03", resume)
	if err != nil || len(view.Scheduled) != 0 || len(view.Charges) != 0 {
		t.Fatalf("paused period projected on resume: %+v %v", view, err)
	}
	view, err = s.GetPlanPrices("2025-06", resume)
	if err != nil || len(view.Scheduled) != 1 {
		t.Fatalf("active future month not projected: %+v %v", view, err)
	}
}
