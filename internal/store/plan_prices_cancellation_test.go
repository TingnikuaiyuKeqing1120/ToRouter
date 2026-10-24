package store

import (
	"reflect"
	"testing"
	"time"
)

func TestPlanPricesExitCancelsOnlyUnpaidFutureCharges(t *testing.T) {
	for _, operation := range []string{"declaration", "ordinary_edit", "delete_config", "delete_channel"} {
		t.Run(operation, func(t *testing.T) {
			s, c := planTestStore(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			config := planConfig(t, c.ID, "2040-01-01T12:00:00+08:00", planAmount(20))
			if err := s.SavePlanPrice(config, now); err != nil {
				t.Fatal(err)
			}
			paid := PlanCharge{ChannelID: c.ID, Month: "2040-03", Amount: planAmount(31), Currency: "CNY", PaidAt: now.Add(-time.Hour)}
			paidNow := PlanCharge{ChannelID: c.ID, Month: "2040-04", Amount: planAmount(32), Currency: "CNY", PaidAt: now}
			future := PlanCharge{ChannelID: c.ID, Month: "2040-02", Amount: planAmount(99), Currency: "USD", PaidAt: planTime(t, "2040-02-01T12:00:00+08:00")}
			for _, charge := range []PlanCharge{paid, paidNow, future} {
				if err := s.SavePlanCharge(charge, now); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch operation {
			case "declaration":
				err = s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:payg"}}, now)
			case "ordinary_edit":
				c.Tags = []string{"billing:free"}
				err = s.SaveChannelConfig(&c)
			case "delete_config":
				err = s.DeletePlanPrice(c.ID, now)
			case "delete_channel":
				err = s.DeleteChannel(c.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			charges := storedPlanCharges(t, s)
			if len(charges) != 2 || charges[0].Month != paid.Month || *charges[0].Amount != 31 || charges[1].Month != paidNow.Month || *charges[1].Amount != 32 {
				t.Fatalf("exit retained unpaid charge or removed advance payment: %+v", charges)
			}
			// Both month-page reads and later accounting snapshots must agree.
			later := planTime(t, "2041-03-02T12:00:00+08:00")
			view, err := s.GetPlanPrices(future.Month, later)
			if err != nil || len(view.Charges) != 0 || len(storedPlanCharges(t, s)) != 2 {
				t.Fatalf("canceled bill reappeared on later read: %+v %v", view, err)
			}
			if operation == "delete_channel" {
				if err := s.SavePlanCharge(future, later); err == nil {
					t.Fatal("deleted channel recreated a canceled future bill")
				}
				return
			}
			if operation == "delete_config" {
				// Recreating the config with a newly chosen future start does not
				// restore an old manually entered period or its old amount.
				config.FirstPaymentAt = planTime(t, "2042-01-01T12:00:00+08:00")
				if err = s.SavePlanPrice(config, later); err != nil {
					t.Fatal(err)
				}
			} else if err = s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:plan"}}, later); err != nil {
				t.Fatal(err)
			}
			view, err = s.GetPlanPrices(future.Month, later)
			if err != nil || len(view.Charges) != 0 {
				t.Fatalf("reentry restored canceled bill: %+v %v", view, err)
			}
			if err = s.SavePlanCharge(future, later); err != nil {
				t.Fatalf("explicitly entering canceled period failed: %v", err)
			}
		})
	}
}

func TestPlanPricesCancellationAndExitRollbackTogether(t *testing.T) {
	for _, operation := range []string{"declaration", "ordinary_edit", "delete_config", "delete_channel"} {
		t.Run(operation, func(t *testing.T) {
			s, c := planTestStore(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			config := planConfig(t, c.ID, "2040-01-01T12:00:00+08:00", planAmount(20))
			future := PlanCharge{ChannelID: c.ID, Month: "2040-02", Amount: planAmount(99), Currency: "USD", PaidAt: planTime(t, "2040-02-01T12:00:00+08:00")}
			if err := s.SavePlanPriceWithCharge(config, &future, now); err != nil {
				t.Fatal(err)
			}
			before := storedPlanCharges(t, s)
			var trigger string
			switch operation {
			case "declaration", "ordinary_edit":
				trigger = "CREATE TRIGGER fail_exit BEFORE UPDATE ON channels BEGIN SELECT RAISE(ABORT,'injected after cancellation'); END"
			case "delete_config":
				trigger = "CREATE TRIGGER fail_exit BEFORE DELETE ON plan_prices BEGIN SELECT RAISE(ABORT,'injected after cancellation'); END"
			case "delete_channel":
				trigger = "CREATE TRIGGER fail_exit BEFORE DELETE ON channels BEGIN SELECT RAISE(ABORT,'injected after cancellation'); END"
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "declaration":
				err = s.SaveChannelBilling([]ChannelBillingChange{{ChannelID: c.ID, BillingTag: "billing:payg"}}, now)
			case "ordinary_edit":
				c.Tags = []string{"billing:free"}
				err = s.SaveChannelConfig(&c)
			case "delete_config":
				err = s.DeletePlanPrice(c.ID, now)
			case "delete_channel":
				err = s.DeleteChannel(c.ID)
			}
			if err == nil {
				t.Fatal("injected exit failure was swallowed")
			}
			if after := storedPlanCharges(t, s); !reflect.DeepEqual(before, after) {
				t.Fatalf("future bill cancellation escaped failed exit transaction: %+v", after)
			}
			stored, err := s.Channel(c.ID)
			if err != nil || !channelIsPlan(stored) {
				t.Fatalf("channel exit escaped rollback: %+v %v", stored, err)
			}
			var active int64
			if err := s.db.QueryRow("SELECT active_from FROM plan_prices WHERE channel_id=?", c.ID).Scan(&active); err != nil {
				t.Fatalf("configuration exit escaped rollback: %v", err)
			}
		})
	}
}
