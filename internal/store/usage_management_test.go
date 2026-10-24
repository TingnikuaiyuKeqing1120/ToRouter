package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func managementStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.InitUsageManagement(); err != nil {
		t.Fatal(err)
	}
	return s
}

func correctionNumber(v float64) *float64 { return &v }

func addManagementUsage(t *testing.T, s *Store, u Usage) int64 {
	t.Helper()
	if err := s.AddUsage(u); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.db.QueryRow("SELECT MAX(id) FROM usage_logs").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func correctionPreview(t *testing.T, s *Store, req AmountCorrectionRequest) AmountCorrectionPreview {
	t.Helper()
	req.Operator = "operator"
	req.Reason = "fix invoice"
	p, err := s.PreviewAmountCorrection(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func correctionApply(t *testing.T, s *Store, p AmountCorrectionPreview) AmountCorrectionChange {
	t.Helper()
	v, err := s.ApplyAmountCorrection(context.Background(), p.ID, p.Operator)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func managementUsage(t *testing.T, s *Store, id int64) Usage {
	t.Helper()
	u, err := s.GetUsageByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUsagePageTraversesBeyondTenThousandAndFreezesCutoff(t *testing.T) {
	s := managementStore(t)
	ctx := context.Background()
	at := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO usage_logs(time,channel_id,model,status,data) VALUES(?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	const count = 10037
	for i := 0; i < count; i++ {
		u := Usage{Time: at, ChannelID: 7, Model: "page", Status: 200, InputTokens: 1}
		raw, _ := json.Marshal(u)
		if _, err = stmt.Exec(at.UnixMilli(), 7, "page", 200, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	addManagementUsage(t, s, Usage{Time: at, ChannelID: 8, Model: "page", Status: 200})
	addManagementUsage(t, s, Usage{Time: at, ChannelID: 7, Model: "other", Status: 200})
	addManagementUsage(t, s, Usage{Time: at, ChannelID: 7, Model: "page", Status: 500})
	addManagementUsage(t, s, Usage{Time: at, ChannelID: 7, Model: "page", Status: 200, NotSent: true})
	f := UsageFilter{From: at.Add(-time.Second), To: at.Add(time.Second), ChannelID: 7, Model: "page", Status: 200, ExcludeNotSent: true}
	summary, err := s.SummarizeUsage(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ListUsagePage(ctx, UsagePageRequest{Filter: f, Limit: 333})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := p.SnapshotMaxID
	later := addManagementUsage(t, s, Usage{Time: at, ChannelID: 7, Model: "page", Status: 200})
	seen := map[int64]bool{}
	last := int64(math.MaxInt64)
	for {
		if p.SnapshotMaxID != cutoff {
			t.Fatal("snapshot changed")
		}
		for _, u := range p.Items {
			if seen[u.ID] || u.ID >= last || u.ID == later {
				t.Fatalf("unstable traversal at %d", u.ID)
			}
			seen[u.ID] = true
			last = u.ID
		}
		if !p.HasMore {
			if p.NextCursor != "" {
				t.Fatal("terminal cursor")
			}
			break
		}
		if p.NextCursor == "" {
			t.Fatal("missing cursor")
		}
		p, err = s.ListUsagePage(ctx, UsagePageRequest{Filter: f, Cursor: p.NextCursor, Limit: 333})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != count || summary.Requests != count {
		t.Fatalf("page=%d summary=%d want=%d", len(seen), summary.Requests, count)
	}
	first, err := s.ListUsagePage(ctx, UsagePageRequest{Filter: f, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	changed := f
	changed.Status = 500
	if _, err = s.ListUsagePage(ctx, UsagePageRequest{Filter: changed, Cursor: first.NextCursor}); err == nil {
		t.Fatal("cursor accepted other filter")
	}
	if _, err = s.ListUsagePage(ctx, UsagePageRequest{Limit: 1001}); err == nil {
		t.Fatal("oversized page accepted")
	}
	if _, err = s.GetUsageByID(ctx, 1); err != nil {
		t.Fatal("oldest detail missing", err)
	}
	if _, err = s.GetUsageByID(ctx, later+10); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing ID", err)
	}
	if _, err = s.PreviewAmountCorrection(ctx, AmountCorrectionRequest{UsageFilter: &f, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "USD"}, Operator: "op", Reason: "range"}); err == nil {
		t.Fatal("oversized correction silently truncated")
	}
}

func TestAmountCorrectionUnknownZeroIndependentMetricsAndFacts(t *testing.T) {
	s := managementStore(t)
	ctx := context.Background()
	original := Usage{Time: time.Now().UTC(), ChannelID: 9, Model: "public", UpstreamModel: "up", Status: 200, InputTokens: 200, OutputTokens: 30, Attempts: []AttemptRecord{{}}, OfficialValue: correctionNumber(5), OfficialCurrency: "CNY"}
	id := addManagementUsage(t, s, original)
	outside := addManagementUsage(t, s, original)
	// Unknown future request fields must survive monetary corrections byte for
	// byte as JSON values, even though Usage does not know their schema.
	if _, err := s.db.Exec(`UPDATE usage_logs SET data=json_set(data,'$.original_request',json(?),'$.credential_id',?) WHERE id=?`, `{"text":"untrusted request","n":7}`, "credential-immutable", id); err != nil {
		t.Fatal(err)
	}
	var requestBefore string
	if err := s.db.QueryRow("SELECT json_extract(data,'$.original_request') FROM usage_logs WHERE id=?", id).Scan(&requestBefore); err != nil {
		t.Fatal(err)
	}
	pActual := correctionPreview(t, s, AmountCorrectionRequest{UsageIDs: []int64{id}, Actual: &AmountCorrectionValue{Amount: correctionNumber(0), Currency: "USD"}})
	pOfficial := correctionPreview(t, s, AmountCorrectionRequest{UsageIDs: []int64{id}, Official: &AmountCorrectionValue{Amount: correctionNumber(8), Currency: "CNY"}})
	if pActual.Impact[0].BeforeUnknown != 1 || pActual.Impact[0].AfterUnknown != 0 || pActual.Targets[0].Before.Amount != nil {
		t.Fatal("unknown treated as zero", pActual)
	}
	if _, err := s.ApplyAmountCorrection(ctx, pActual.ID, "another operator"); err == nil {
		t.Fatal("different principal applied preview")
	}
	change := correctionApply(t, s, pActual)
	again := correctionApply(t, s, pActual)
	if !reflect.DeepEqual(change, again) {
		t.Fatal("apply not idempotent")
	}
	u := managementUsage(t, s, id)
	if u.ActualCost == nil || *u.ActualCost != 0 || *u.OfficialValue != 5 || u.OfficialCurrency != "CNY" {
		t.Fatal("wrong metric changed", u)
	}
	if u.ActualPricing.Method != "manual_correction" || u.ActualPricing.Resolution.SourceKind != "manual_correction" || u.ActualPricing.Resolution.Table != nil || u.ActualPricing.Rates.Input != nil {
		t.Fatal("false computed provenance", u.ActualPricing)
	}
	correctionApply(t, s, pOfficial) // An independent pending preview stays valid.
	revert, err := s.PreviewAmountRevert(ctx, change.ID, "reviewer", "restore unknown")
	if err != nil {
		t.Fatal(err)
	}
	correctionApply(t, s, revert)
	u = managementUsage(t, s, id)
	if u.ActualCost != nil || u.Currency != "" || u.ActualPricing != nil || *u.OfficialValue != 8 {
		t.Fatal("revert overwrote independent amount", u)
	}
	if u.ChannelID != original.ChannelID || u.Model != original.Model || u.UpstreamModel != original.UpstreamModel || u.InputTokens != original.InputTokens || u.OutputTokens != original.OutputTokens || !u.Time.Equal(original.Time) || len(u.Attempts) != len(original.Attempts) {
		t.Fatal("facts modified", u)
	}
	var requestAfter, credential string
	if err = s.db.QueryRow("SELECT json_extract(data,'$.original_request'),json_extract(data,'$.credential_id') FROM usage_logs WHERE id=?", id).Scan(&requestAfter, &credential); err != nil {
		t.Fatal(err)
	}
	if requestAfter != requestBefore || credential != "credential-immutable" {
		t.Fatal("opaque facts modified")
	}
	unchanged := managementUsage(t, s, outside)
	if unchanged.ActualCost != nil || *unchanged.OfficialValue != 5 {
		t.Fatal("outside target modified")
	}
	audit, err := s.GetAmountCorrection(ctx, change.ID)
	if err != nil || audit.RevertedBy != revert.ID || audit.Operator != "operator" || audit.Reason != "fix invoice" || audit.Source != "manual_correction" {
		t.Fatal("audit missing", audit, err)
	}
}

func TestAmountCorrectionConflictRollsBackAndRevertRejectsLaterWrite(t *testing.T) {
	s := managementStore(t)
	ctx := context.Background()
	ids := []int64{}
	for i := 1; i <= 2; i++ {
		ids = append(ids, addManagementUsage(t, s, Usage{Status: 200, ActualCost: correctionNumber(float64(i)), Currency: "USD"}))
	}
	batch := correctionPreview(t, s, AmountCorrectionRequest{UsageIDs: ids, Actual: &AmountCorrectionValue{Amount: correctionNumber(7), Currency: "USD"}})
	one := correctionPreview(t, s, AmountCorrectionRequest{UsageIDs: ids[1:], Actual: &AmountCorrectionValue{Amount: correctionNumber(9), Currency: "USD"}})
	change := correctionApply(t, s, one)
	if _, err := s.ApplyAmountCorrection(ctx, batch.ID, batch.Operator); !errors.Is(err, ErrAmountCorrectionConflict) {
		t.Fatal("stale preview accepted", err)
	}
	if *managementUsage(t, s, ids[0]).ActualCost != 1 || *managementUsage(t, s, ids[1]).ActualCost != 9 {
		t.Fatal("conflicting batch partially wrote")
	}
	if _, err := s.GetAmountCorrection(ctx, batch.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed batch had audit", err)
	}
	revert, err := s.PreviewAmountRevert(ctx, change.ID, "operator", "undo")
	if err != nil {
		t.Fatal(err)
	}
	newer := correctionPreview(t, s, AmountCorrectionRequest{UsageIDs: ids[1:], Actual: &AmountCorrectionValue{Amount: correctionNumber(9), Currency: "USD"}})
	correctionApply(t, s, newer) // Even an equal numeric amount has later provenance.
	if _, err = s.ApplyAmountCorrection(ctx, revert.ID, revert.Operator); !errors.Is(err, ErrAmountCorrectionConflict) {
		t.Fatal("revert overwrote a later correction", err)
	}
	if _, err = s.PreviewAmountRevert(ctx, change.ID, "operator", "undo again"); !errors.Is(err, ErrAmountCorrectionConflict) {
		t.Fatal("stale revert preview accepted", err)
	}
}

func TestAmountCorrectionRejectsForbiddenFactsAndProvenance(t *testing.T) {
	s := managementStore(t)
	ctx := context.Background()
	notSent := addManagementUsage(t, s, Usage{NotSent: true, Status: 403})
	plan := addManagementUsage(t, s, Usage{Status: 200, ActualPricing: &UsagePricingBasis{Method: "recorded", Resolution: PriceResolution{Layer: "plan"}}})
	normal := addManagementUsage(t, s, Usage{Status: 200})
	for _, test := range []struct {
		name string
		req  AmountCorrectionRequest
	}{
		{"not_sent", AmountCorrectionRequest{UsageIDs: []int64{notSent}, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "USD"}}},
		{"plan_payg", AmountCorrectionRequest{UsageIDs: []int64{plan}, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "USD"}}},
		{"fake_method", AmountCorrectionRequest{UsageIDs: []int64{normal}, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "USD", Basis: &UsagePricingBasis{Method: "upstream_usage"}}}},
		{"fake_rate", AmountCorrectionRequest{UsageIDs: []int64{normal}, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "USD", Basis: &UsagePricingBasis{Rates: PriceRates{Input: correctionNumber(1)}}}}},
		{"fake_layer", AmountCorrectionRequest{UsageIDs: []int64{normal}, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "USD", Basis: &UsagePricingBasis{Resolution: PriceResolution{Layer: "payg"}}}}},
		{"nan", AmountCorrectionRequest{UsageIDs: []int64{normal}, Actual: &AmountCorrectionValue{Amount: correctionNumber(math.NaN()), Currency: "USD"}}},
		{"negative", AmountCorrectionRequest{UsageIDs: []int64{normal}, Actual: &AmountCorrectionValue{Amount: correctionNumber(-1), Currency: "USD"}}},
		{"currency", AmountCorrectionRequest{UsageIDs: []int64{normal}, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "EUR"}}},
		{"empty_targets", AmountCorrectionRequest{UsageIDs: []int64{}, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "USD"}}},
		{"duplicate", AmountCorrectionRequest{UsageIDs: []int64{normal, normal}, Actual: &AmountCorrectionValue{Amount: correctionNumber(1), Currency: "USD"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.req.Operator = "operator"
			test.req.Reason = "test"
			if _, err := s.PreviewAmountCorrection(ctx, test.req); err == nil {
				t.Fatal("forbidden correction accepted")
			}
		})
	}
	// An unallocated actual plan amount does not prevent correcting its separate
	// API equivalent, nor does that correction rewrite its plan identity.
	p := correctionPreview(t, s, AmountCorrectionRequest{UsageIDs: []int64{plan}, Official: &AmountCorrectionValue{Amount: correctionNumber(3), Currency: "USD"}})
	correctionApply(t, s, p)
	u := managementUsage(t, s, plan)
	if u.ActualCost != nil || u.ActualPricing.Resolution.Layer != "plan" || *u.OfficialValue != 3 {
		t.Fatal("plan identity changed", u)
	}
	// The public typed contract exposes no factual write fields.
	typ := reflect.TypeOf(AmountCorrectionRequest{})
	for _, field := range []string{"Time", "InputTokens", "ChannelID", "CredentialID", "Model", "NotSent", "RawRequest", "RawResponse"} {
		if _, ok := typ.FieldByName(field); ok {
			t.Fatal("fact exposed for writing", field)
		}
	}
}

func TestPlanAmountCorrectionAndConditionalRevertPreservePeriodFacts(t *testing.T) {
	s := managementStore(t)
	ctx := context.Background()
	paid := time.Now().AddDate(0, -1, 0).UnixMilli()
	for i := int64(1); i <= 2; i++ {
		if _, err := s.db.Exec("INSERT INTO plan_charges(channel_id,month,channel_name,amount,currency,paid_at) VALUES(?,?,?,?,?,?)", i, "2026-09", fmt.Sprint("historical-", i), nil, "CNY", paid); err != nil {
			t.Fatal(err)
		}
	}
	ref := PlanChargeRef{ChannelID: 1, Month: "2026-09"}
	p := correctionPreview(t, s, AmountCorrectionRequest{PlanCharges: []PlanChargeRef{ref}, PlanAmount: &AmountCorrectionValue{Amount: correctionNumber(0), Currency: "USD"}})
	if p.Impact[0].BeforeUnknown != 1 || p.Impact[0].AfterUnknown != 0 {
		t.Fatal("plan unknown/zero lost", p)
	}
	change := correctionApply(t, s, p)
	var amount *float64
	var currency, name string
	var gotPaid int64
	if err := s.db.QueryRow("SELECT amount,currency,channel_name,paid_at FROM plan_charges WHERE channel_id=1").Scan(&amount, &currency, &name, &gotPaid); err != nil {
		t.Fatal(err)
	}
	if amount == nil || *amount != 0 || currency != "USD" || name != "historical-1" || gotPaid != paid {
		t.Fatal("plan facts changed")
	}
	if err := s.db.QueryRow("SELECT amount FROM plan_charges WHERE channel_id=2").Scan(&amount); err != nil || amount != nil {
		t.Fatal("outside plan changed", err)
	}
	revert, err := s.PreviewAmountRevert(ctx, change.ID, "operator", "undo")
	if err != nil {
		t.Fatal(err)
	}
	correctionApply(t, s, revert)
	if err = s.db.QueryRow("SELECT amount,currency FROM plan_charges WHERE channel_id=1").Scan(&amount, &currency); err != nil || amount != nil || currency != "CNY" {
		t.Fatal("plan unknown not restored", err)
	}
	second := correctionPreview(t, s, AmountCorrectionRequest{PlanCharges: []PlanChargeRef{ref}, PlanAmount: &AmountCorrectionValue{Amount: correctionNumber(40), Currency: "CNY"}})
	if _, err = s.db.Exec("UPDATE plan_charges SET paid_at=paid_at+1 WHERE channel_id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyAmountCorrection(ctx, second.ID, second.Operator); !errors.Is(err, ErrAmountCorrectionConflict) {
		t.Fatal("changed period fact accepted", err)
	}
}

func TestAmountCorrectionPersistsAcrossStoreRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.InitUsageManagement(); err != nil {
		t.Fatal(err)
	}
	id := addManagementUsage(t, s, Usage{Status: 200})
	p := correctionPreview(t, s, AmountCorrectionRequest{UsageIDs: []int64{id}, Actual: &AmountCorrectionValue{Amount: correctionNumber(4), Currency: "USD"}})
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	change := correctionApply(t, s, p)
	stored, err := s.GetAmountCorrection(ctx, change.ID)
	if err != nil || stored.ID != p.ID {
		t.Fatal(stored, err)
	}
	revert, err := s.PreviewAmountRevert(ctx, change.ID, "operator", "restart undo")
	if err != nil {
		t.Fatal(err)
	}
	correctionApply(t, s, revert)
	if managementUsage(t, s, id).ActualCost != nil {
		t.Fatal("restart revert failed")
	}
}

func TestAmountCorrectionExpiryAndCleanupRetainReceiptsAndRevisionGuards(t *testing.T) {
	s := managementStore(t)
	ctx := context.Background()
	id := addManagementUsage(t, s, Usage{Status: 200})
	request := AmountCorrectionRequest{UsageIDs: []int64{id}, Actual: &AmountCorrectionValue{Amount: correctionNumber(2), Currency: "USD"}}
	expired := correctionPreview(t, s, request)
	if expired.ExpiresAt.Sub(expired.CreatedAt) != 10*time.Minute {
		t.Fatal("preview lifetime is not ten minutes")
	}
	if _, err := s.db.Exec("UPDATE amount_correction_previews SET data=json_set(data,'$.expires_at',?) WHERE id=?", time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAmountCorrection(ctx, expired.ID, expired.Operator); !errors.Is(err, ErrAmountCorrectionConflict) {
		t.Fatal("expired preview applied", err)
	}
	if managementUsage(t, s, id).ActualCost != nil {
		t.Fatal("expired preview wrote")
	}
	first := correctionPreview(t, s, request)
	change := correctionApply(t, s, first)
	second := correctionPreview(t, s, request)
	correctionApply(t, s, second)
	if err := s.CleanupUsageManagement(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAmountCorrectionPreview(ctx, first.ID, first.Operator); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired preview not cleaned", err)
	}
	receipt, err := s.ApplyAmountCorrection(ctx, first.ID, first.Operator)
	if err != nil || receipt.ID != change.ID {
		t.Fatal("cleanup broke idempotent receipt", err)
	}
	if _, err = s.ApplyAmountCorrection(ctx, first.ID, "other"); err == nil {
		t.Fatal("receipt leaked across principals")
	}
	if _, err = s.PreviewAmountRevert(ctx, change.ID, "operator", "old correction"); !errors.Is(err, ErrAmountCorrectionConflict) {
		t.Fatal("cleanup lost latest revision guard", err)
	}
	var versionCount int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM amount_correction_versions").Scan(&versionCount); err != nil || versionCount != 1 {
		t.Fatal("live revisions not retained", versionCount, err)
	}
	if _, err = s.db.Exec("DELETE FROM usage_logs WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err = s.CleanupUsageManagement(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM amount_correction_versions").Scan(&versionCount); err != nil || versionCount != 0 {
		t.Fatal("orphan revision not cleaned", versionCount, err)
	}
}

func TestAmountCorrectionWriteFailureRollsBackDataVersionsAndAudit(t *testing.T) {
	s := managementStore(t)
	ctx := context.Background()
	first := addManagementUsage(t, s, Usage{Status: 200})
	second := addManagementUsage(t, s, Usage{Status: 200})
	p := correctionPreview(t, s, AmountCorrectionRequest{UsageIDs: []int64{first, second}, Actual: &AmountCorrectionValue{Amount: correctionNumber(4), Currency: "USD"}})
	if _, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER abort_correction BEFORE UPDATE ON usage_logs WHEN NEW.id=%d BEGIN SELECT RAISE(ABORT, 'test write failure'); END", second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAmountCorrection(ctx, p.ID, p.Operator); err == nil {
		t.Fatal("injected failure ignored")
	}
	if managementUsage(t, s, first).ActualCost != nil || managementUsage(t, s, second).ActualCost != nil {
		t.Fatal("write failure partially committed")
	}
	var versions int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM amount_correction_versions").Scan(&versions); err != nil || versions != 0 {
		t.Fatal("versions partially committed", versions, err)
	}
	if _, err := s.GetAmountCorrection(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed write emitted receipt", err)
	}
}
