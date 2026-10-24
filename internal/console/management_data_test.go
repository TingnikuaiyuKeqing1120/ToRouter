package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/management"
	"torouter/internal/store"
)

type dataAuthTransport struct {
	base  http.RoundTripper
	token string
}

func (t dataAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(copy)
}

type dataHarness struct {
	store *store.Store
	tools *managementTools
	http  *httptest.Server
}

func newDataHarness(t *testing.T) *dataHarness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := httptest.NewUnstartedServer(nil)
	tools := &managementTools{store: st, origin: "http://" + h.Listener.Addr().String()}
	ops := tools.dataOperations()
	tools.server = management.New(st, tools.origin, ops)
	h.Config.Handler = tools.server.Handler()
	h.Start()
	t.Cleanup(h.Close)
	return &dataHarness{st, tools, h}
}
func (h *dataHarness) client(t *testing.T, permissions ...string) (*mcp.ClientSession, int64) {
	t.Helper()
	connection, token, err := h.store.CreateManagementConnection("data test", permissions)
	if err != nil {
		t.Fatal(err)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	t.Cleanup(base.CloseIdleConnections)
	client := mcp.NewClient(&mcp.Implementation{Name: "data-operations-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.http.URL + "/mcp", HTTPClient: &http.Client{Transport: dataAuthTransport{base, token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, connection.ID
}
func dataCall(t *testing.T, client *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(name, err)
	}
	if result.IsError {
		t.Fatalf("%s: %v", name, result.Content)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func dataSuccess(t *testing.T, client *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	out := dataCall(t, client, name, args)
	if status, ok := out["status"].(string); ok {
		if status != "completed" {
			t.Fatalf("%s returned %v", name, out)
		}
		value, ok := out["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing result: %v", name, out)
		}
		return value
	}
	return out
}
func dataWrite(args map[string]any) map[string]any {
	args["request_id"] = uuid.NewString()
	return args
}
func dataDenied(t *testing.T, client *mcp.ClientSession, name string, args map[string]any) {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err == nil && !result.IsError {
		t.Fatalf("%s was allowed: %+v", name, result)
	}
}
func dataUsageID(t *testing.T, s *store.Store, u store.Usage) int64 {
	t.Helper()
	if err := s.AddUsage(u); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListUsagePage(context.Background(), store.UsagePageRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items[0].ID
}

func TestManagementDataReadSensitiveMoneyAndArtifactBoundaries(t *testing.T) {
	h := newDataHarness(t)
	ordinary, _ := h.client(t, management.Read, management.Manage)
	maintainer, _ := h.client(t, management.Read, management.Manage, management.Maintain, management.Sensitive)
	id := dataUsageID(t, h.store, store.Usage{Model: "m", Status: 500, Error: "private-request-body", Attempts: []store.AttemptRecord{{ErrorReason: "private-upstream-body", CredentialMaskKey: "private-credential-mask"}}})
	public := dataSuccess(t, ordinary, "get_usage_log", map[string]any{"id": id})
	raw, _ := json.Marshal(public)
	if strings.Contains(string(raw), "private-") {
		t.Fatal("default log leaked private material", string(raw))
	}
	dataDenied(t, ordinary, "read_usage_log_sensitive", map[string]any{"id": id})
	sensitive := dataSuccess(t, maintainer, "read_usage_log_sensitive", map[string]any{"id": id})
	if sensitive["error"] != "private-request-body" {
		t.Fatal("explicit sensitive detail lost", sensitive)
	}
	dataDenied(t, ordinary, "preview_amount_correction", dataWrite(map[string]any{"usage_ids": []int64{id}, "actual": map[string]any{"amount": 0, "currency": "USD"}, "reason": "invoice"}))
	preview := dataSuccess(t, maintainer, "preview_amount_correction", dataWrite(map[string]any{"usage_ids": []int64{id}, "actual": map[string]any{"amount": 0, "currency": "USD"}, "reason": "invoice"}))
	change := dataSuccess(t, maintainer, "apply_amount_correction", dataWrite(map[string]any{"preview_id": preview["preview_id"]}))
	u, err := h.store.GetUsageByID(context.Background(), id)
	if err != nil || u.ActualCost == nil || *u.ActualCost != 0 || u.OfficialValue != nil || u.Error != "private-request-body" {
		t.Fatal("amount operation altered facts", u, err)
	}
	revert := dataSuccess(t, maintainer, "preview_amount_revert", dataWrite(map[string]any{"change_id": change["id"], "reason": "undo"}))
	dataSuccess(t, maintainer, "apply_amount_correction", dataWrite(map[string]any{"preview_id": revert["preview_id"]}))
	u, _ = h.store.GetUsageByID(context.Background(), id)
	if u.ActualCost != nil {
		t.Fatal("unknown amount not restored")
	}
	export := dataSuccess(t, ordinary, "export_usage_csv", dataWrite(map[string]any{"filter": map[string]any{}}))
	part := dataSuccess(t, ordinary, "read_export", map[string]any{"id": export["id"], "limit": 64})
	if part["content"] == "" || part["next_offset"].(float64) > 64 || part["has_more"] != true {
		t.Fatal("export not paged", part)
	}
	dataDenied(t, maintainer, "read_export", map[string]any{"id": export["id"]})
	dataDenied(t, maintainer, "preview_amount_correction", dataWrite(map[string]any{"usage_ids": []int64{id}, "actual": map[string]any{"amount": 3, "currency": "USD"}, "reason": "invoice", "input_tokens": 99}))
}

func TestManagementDataBackfillFreezeAndCatalogAtomicPreview(t *testing.T) {
	h := newDataHarness(t)
	client, _ := h.client(t, management.Read, management.Manage, management.Maintain)
	c := catalogHTTPChannel(t, h.store, "provider/model")
	price := store.Price{ChannelID: c.ID, Model: "provider/model", Origin: "manual", Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "USD", UnitTokens: 1000, Rates: store.PriceRates{Input: ptr(1.0)}}}}
	if err := h.store.SavePrices([]store.Price{price}); err != nil {
		t.Fatal(err)
	}
	first := dataUsageID(t, h.store, store.Usage{ChannelID: c.ID, Model: "public", UpstreamModel: "provider/model", InputTokens: 100, Status: 200})
	request := map[string]any{"filter": map[string]any{"channel_id": c.ID}, "amounts": []string{"actual"}, "mode": "current"}
	p := dataSuccess(t, client, "preview_usage_backfill", dataWrite(map[string]any{"request": request}))
	later := dataUsageID(t, h.store, store.Usage{ChannelID: c.ID, Model: "public", UpstreamModel: "provider/model", InputTokens: 100, Status: 200})
	dataSuccess(t, client, "apply_usage_backfill", dataWrite(map[string]any{"preview_id": p["preview_id"]}))
	u, _ := h.store.GetUsageByID(context.Background(), first)
	v, _ := h.store.GetUsageByID(context.Background(), later)
	if u.ActualCost == nil || v.ActualCost != nil {
		t.Fatal("backfill adopted post-preview record")
	}
	p = dataSuccess(t, client, "preview_usage_backfill", dataWrite(map[string]any{"request": request}))
	price.Selection.Table.Rates.Input = ptr(8.0)
	if err := h.store.SavePrices([]store.Price{price}); err != nil {
		t.Fatal(err)
	}
	conflict := dataCall(t, client, "apply_usage_backfill", dataWrite(map[string]any{"preview_id": p["preview_id"]}))
	if conflict["status"] != "failed" {
		t.Fatal("changed price not rejected", conflict)
	}
	item := catalogHTTPItem("provider/model", 10000)
	if err := h.store.SaveExternalCatalog("openrouter", []store.CatalogItem{item}); err != nil {
		t.Fatal(err)
	}
	prefill := store.PrefillRequest{Targets: []store.PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: item.ID, Ref: &store.CatalogModelRef{Source: "openrouter", ID: item.ID}}}, Groups: []string{"capabilities"}}
	p = dataSuccess(t, client, "preview_catalog_prefill", dataWrite(map[string]any{"request": prefill}))
	item.MaxOutputTokens = ptr(int64(11000))
	if err := h.store.SaveCatalogItem("openrouter", item.ID, item); err != nil {
		t.Fatal(err)
	}
	conflict = dataCall(t, client, "apply_catalog_prefill", dataWrite(map[string]any{"preview_id": p["preview_id"]}))
	if conflict["status"] != "failed" {
		t.Fatal("changed catalog proposal accepted", conflict)
	}
	current, _ := h.store.Channel(c.ID)
	if current.ModelConfigs[item.ID].MaxOutputTokens != nil {
		t.Fatal("conflicting prefill partially wrote")
	}
	p = dataSuccess(t, client, "preview_catalog_prefill", dataWrite(map[string]any{"request": prefill}))
	dataSuccess(t, client, "apply_catalog_prefill", dataWrite(map[string]any{"preview_id": p["preview_id"]}))
	current, _ = h.store.Channel(c.ID)
	if current.ModelConfigs[item.ID].MaxOutputTokens == nil || *current.ModelConfigs[item.ID].MaxOutputTokens != 11000 {
		t.Fatal("prefill failed", current.ModelConfigs)
	}
}

func TestManagementDataPricesSourcesAndHistoricalPlanGuards(t *testing.T) {
	h := newDataHarness(t)
	client, _ := h.client(t, management.Read, management.Manage)
	maintenance, _ := h.client(t, management.Read, management.Manage, management.Maintain)
	price := store.Price{Model: "p", Origin: "manual", Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "USD", UnitTokens: 1000, Rates: store.PriceRates{Input: ptr(1.0)}}}}
	dataSuccess(t, client, "update_prices", dataWrite(map[string]any{"edits": []store.PriceEdit{{ChannelID: 0, Model: "p", ExpectedVersion: "absent", Price: &price}}}))
	prices, _ := h.store.GetPrices()
	version := store.EntityVersion(prices[0])
	price.Selection.Table.Rates.Input = ptr(4.0)
	result := dataCall(t, client, "update_prices", dataWrite(map[string]any{"edits": []store.PriceEdit{{Model: "p", ExpectedVersion: version, Price: &price}, {Model: "missing", ExpectedVersion: "bad", Price: &price}}}))
	if result["status"] != "failed" {
		t.Fatal("invalid batch accepted", result)
	}
	prices, _ = h.store.GetPrices()
	if *prices[0].Selection.Table.Rates.Input != 1 {
		t.Fatal("price batch partially saved")
	}
	result = dataCall(t, client, "update_prices", dataWrite(map[string]any{"edits": []store.PriceEdit{{Model: "p", ExpectedVersion: version, Delete: true}}}))
	if result["status"] != "failed" {
		t.Fatal("ordinary price delete bypassed maintain", result)
	}
	sources := dataSuccess(t, client, "get_catalog_sources", map[string]any{})
	dataSuccess(t, client, "update_catalog_sources", dataWrite(map[string]any{"expected_version": sources["version"], "sources": []map[string]any{{"source": "builtin", "prefill_enabled": false, "priority": 0}}}))
	result = dataCall(t, client, "update_catalog_sources", dataWrite(map[string]any{"expected_version": sources["version"], "sources": []map[string]any{{"source": "builtin", "prefill_enabled": true, "priority": 0}}}))
	if result["status"] != "failed" {
		t.Fatal("stale source version accepted", result)
	}
	fx := dataSuccess(t, client, "get_exchange_sources", map[string]any{})
	dataSuccess(t, client, "update_exchange_sources", dataWrite(map[string]any{"expected_version": fx["version"], "sources": []string{}}))
	result = dataCall(t, client, "update_exchange_sources", dataWrite(map[string]any{"expected_version": fx["version"], "sources": []string{"ecb"}}))
	if result["status"] != "failed" {
		t.Fatal("stale exchange source version accepted")
	}
	c := catalogHTTPChannel(t, h.store, "plan-model")
	c.Tags = []string{"billing:plan", "privacy:unknown"}
	if err := h.store.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	config := store.PlanPriceConfig{ChannelID: c.ID, DefaultAmount: ptr(10.0), Currency: "USD", FirstPaymentAt: start.AddDate(0, -1, 0), Timezone: "UTC"}
	state, err := h.store.GetPlanPriceState(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	denied := dataCall(t, client, "configure_plan_price", dataWrite(map[string]any{"config": config, "expected_version": store.EntityVersion(state)}))
	if denied["status"] != "failed" {
		t.Fatal("historical schedule bypassed maintain", denied)
	}
	preview := dataSuccess(t, maintenance, "configure_plan_price", dataWrite(map[string]any{"config": config, "expected_version": store.EntityVersion(state)}))
	if preview["status"] != "preview" {
		t.Fatal("historical schedule mutated without preview", preview)
	}
	dataSuccess(t, maintenance, "configure_plan_price", dataWrite(map[string]any{"config": config, "expected_version": store.EntityVersion(state), "preview_id": preview["preview_id"]}))
	state, err = h.store.GetPlanPriceState(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Charges) < 2 {
		t.Fatal("approved schedule did not accrue")
	}
	oldCharge := state.Charges[0]
	historical := managedPlanCharge{ChannelID: oldCharge.ChannelID, Month: oldCharge.Month, Amount: ptr(1.0), Currency: oldCharge.Currency, PaidAt: oldCharge.PaidAt}
	denied = dataCall(t, client, "save_plan_charge", dataWrite(map[string]any{"charge": historical, "expected_version": store.EntityVersion(state)}))
	if denied["status"] == "completed" {
		t.Fatal("ordinary save overwrote historical invoice", denied)
	}
	config.DefaultAmount = ptr(12.0)
	dataSuccess(t, client, "configure_plan_price", dataWrite(map[string]any{"config": config, "expected_version": store.EntityVersion(state)}))
	after, _ := h.store.GetPlanPriceState(c.ID)
	if *after.Charges[0].Amount != 10 {
		t.Fatal("default change rewrote historical charge")
	}
	stop := dataSuccess(t, maintenance, "stop_plan_accounting", dataWrite(map[string]any{"channel_id": c.ID, "expected_version": store.EntityVersion(after)}))
	dataSuccess(t, maintenance, "stop_plan_accounting", dataWrite(map[string]any{"channel_id": c.ID, "expected_version": store.EntityVersion(after), "preview_id": stop["preview_id"]}))
	final, _ := h.store.GetPlanPriceState(c.ID)
	if final.Config != nil || len(final.Charges) != len(after.Charges) {
		t.Fatal("stop removed historical fees", final)
	}
}

type dataRoundTripFunc func(*http.Request) (*http.Response, error)

func (f dataRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManagementDataExternalRefreshUsesIsolatedTransportAndRedactsFailures(t *testing.T) {
	h := newDataHarness(t)
	client, connectionID := h.client(t, management.Read, management.Manage)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var calls atomic.Int32
	http.DefaultTransport = dataRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "openrouter.ai" {
			return nil, errors.New("unexpected host")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"author/model","name":"Model","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`)), Request: r}, nil
	})
	dataSuccess(t, client, "fetch_catalog", dataWrite(map[string]any{"source": "openrouter"}))
	if calls.Load() != 1 {
		t.Fatal("refresh did not use isolated transport")
	}
	http.DefaultTransport = dataRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("private-provider-diagnostic-token")
	})
	failed := dataCall(t, client, "fetch_catalog", dataWrite(map[string]any{"source": "openrouter"}))
	raw, _ := json.Marshal(failed)
	if failed["status"] != "failed" || strings.Contains(string(raw), "private-provider") {
		t.Fatal("provider error exposed or wrong state", failed)
	}
	sources := dataSuccess(t, client, "get_catalog_sources", map[string]any{})
	raw, _ = json.Marshal(sources)
	if strings.Contains(string(raw), "private-provider") {
		t.Fatal("source status leaked provider error")
	}
	items, _ := h.store.ListCatalogItems("openrouter", "")
	if len(items) != 1 {
		t.Fatal("failed refresh cleared successful catalog")
	}
	// An expired cached rate remains available, indicates expiry and never fetches.
	before := calls.Load()
	yesterday := time.Now().AddDate(0, 0, -1)
	if _, err := h.store.DailyExchangeRate(yesterday, true, func() (store.ExchangeRate, error) {
		return store.ExchangeRate{Rate: 7, Date: yesterday.Format("2006-01-02")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	cached := dataSuccess(t, client, "get_exchange_rate", map[string]any{})
	if cached["available"] != true || cached["stale"] != true || cached["rate"] != float64(7) {
		t.Fatal("old cache did not retain rate and indicate expiry", cached)
	}
	if calls.Load() != before {
		t.Fatal("cached read sent network request")
	}
	enabled := false
	if _, err := h.store.UpdateManagementConnection(connectionID, store.ManagementPatch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	dataDenied(t, client, "fetch_catalog", dataWrite(map[string]any{"source": "openrouter"}))
	if calls.Load() != before {
		t.Fatal("revoked authorization still sent request")
	}
}
