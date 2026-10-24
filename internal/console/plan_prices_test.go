package console

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestPlanPricesAPIContractAndDeclarationPreserveFields(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := store.Channel{Name: "Synthetic plan", APIKey: "synthetic-api-secret", BaseURL: "http://localhost:9999", Protocol: "openai",
		Models: []string{"m"}, ModelTags: map[string][]string{"m": {"billing:payg"}}, Tags: []string{"billing:free", "privacy:unknown"}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		if strings.Contains(w.Body.String(), c.APIKey) {
			t.Fatal("response leaked channel credentials")
		}
		return w
	}
	base := fmt.Sprintf("/plan-prices/%d", c.ID)
	first := time.Now().AddDate(0, 0, -1).UTC().Truncate(time.Second)
	month := first.Format("2006-01")
	config := fmt.Sprintf(`{"channel_id":999,"default_amount":null,"currency":"CNY","first_payment_at":%q,"timezone":"Asia/Shanghai"}`, first.Format(time.RFC3339))
	request("PUT", base, config, 400)
	request("PUT", "/channels/billing", fmt.Sprintf(`{"changes":[{"channel_id":%d,"billing_tag":"billing:plan"}]}`, c.ID), 200)
	request("PUT", base, config, 200)
	w := request("GET", "/plan-prices?month="+month, "", 200)
	var view store.PlanPrices
	if err = json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Prices) != 1 || len(view.Charges) != 1 || view.Prices[0].ChannelID != c.ID || view.Charges[0].Amount != nil || view.Prices[0].Timezone != "Asia/Shanghai" {
		t.Fatalf("round trip view=%+v err=%v", view, err)
	}
	request("PUT", base+"/charges/"+month, fmt.Sprintf(`{"amount":0,"currency":"USD","paid_at":%q}`, first.Format(time.RFC3339)), 200)
	w = request("GET", "/plan-prices?month="+month, "", 200)
	if err = json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Charges) != 1 || view.Charges[0].Amount == nil || *view.Charges[0].Amount != 0 || view.Charges[0].Currency != "USD" {
		t.Fatalf("zero correction=%+v err=%v", view, err)
	}
	actual, err := s.Channel(c.ID)
	if err != nil || actual.APIKey != c.APIKey || len(actual.ModelTags["m"]) != 1 || actual.ModelTags["m"][0] != "billing:payg" || len(actual.Tags) != 2 || actual.Tags[0] != "privacy:unknown" {
		t.Fatalf("declaration lost unrelated fields: %+v %v", actual, err)
	}
	request("DELETE", base, "", 200)
	w = request("GET", "/plan-prices?month="+month, "", 200)
	if err = json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Prices) != 0 || len(view.Charges) != 1 {
		t.Fatalf("config delete erased history: %+v %v", view, err)
	}
	request("DELETE", fmt.Sprintf("/channels/%d", c.ID), "", 200)
	request("PUT", base+"/charges/"+month, fmt.Sprintf(`{"amount":5,"currency":"CNY","paid_at":%q}`, first.Format(time.RFC3339)), 200)
	request("PUT", base+"/charges/2040-01", `{"amount":5,"currency":"CNY","paid_at":"2040-01-01T00:00:00Z"}`, 404)
}

func TestPlanPricesAPIAtomicRowAndInputValidation(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := store.Channel{Name: "Synthetic", APIKey: "synthetic-secret", BaseURL: "http://localhost:9999", Protocol: "openai", Tags: []string{"billing:plan"}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s %s=%d %s want %d", method, path, w.Code, w.Body.String(), want)
		}
		return w
	}
	base := fmt.Sprintf("/plan-prices/%d", c.ID)
	valid := `{"default_amount":20,"currency":"USD","first_payment_at":"2040-01-31T12:00:00+08:00","timezone":"Asia/Shanghai","charge":{"month":"2040-01","amount":10,"currency":"CNY","paid_at":"2040-01-31T12:00:00+08:00"}}`
	request("PUT", base, valid, 200)
	request("PUT", base, strings.Replace(strings.Replace(valid, `"default_amount":20`, `"default_amount":99`, 1), `"currency":"CNY"`, `"currency":"EUR"`, 1), 400)
	w := request("GET", "/plan-prices?month=2040-01", "", 200)
	var view store.PlanPrices
	if err = json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Prices) != 1 || len(view.Charges) != 1 || *view.Prices[0].DefaultAmount != 20 || *view.Charges[0].Amount != 10 {
		t.Fatalf("atomic row=%+v %v", view, err)
	}
	w = request("GET", "/plan-prices?month=2040-02", "", 200)
	if err = json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Charges) != 0 || len(view.Scheduled) != 1 || *view.Scheduled[0].Amount != 20 || view.Scheduled[0].PaidAt.Format(time.RFC3339) != "2040-02-29T04:00:00Z" {
		t.Fatalf("scheduled response=%+v %v", view, err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/plan-prices?month=2040-1", ""},
		{"PUT", "/plan-prices/0", valid},
		{"PUT", base, strings.Replace(valid, "Asia/Shanghai", "Local", 1)},
		{"PUT", base, strings.Replace(valid, `"default_amount":20`, `"default_amount":-1`, 1)},
		{"PUT", base, valid + `{}`},
		{"PUT", base + "/charges/2040-13", `{"amount":10,"currency":"USD","paid_at":"2040-01-01T00:00:00Z"}`},
		{"PUT", "/channels/billing", `{"changes":null}`},
		{"PUT", "/channels/billing", fmt.Sprintf(`{"changes":[{"channel_id":%d,"billing_tag":"plan"}]}`, c.ID)},
	} {
		request(tc.method, tc.path, tc.body, 400)
	}
	request("PUT", "/channels/billing", fmt.Sprintf(`{"changes":[{"channel_id":%d,"billing_tag":"billing:free"},{"channel_id":9999,"billing_tag":"billing:plan"}]}`, c.ID), 404)
	stored, err := s.Channel(c.ID)
	if err != nil || len(stored.Tags) != 1 || stored.Tags[0] != "billing:plan" {
		t.Fatalf("batch partially persisted=%+v %v", stored.Tags, err)
	}
}

func TestPlanPricesAPILocalizedValidation(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := store.Channel{Name: "Synthetic", APIKey: "synthetic-secret", BaseURL: "http://localhost:9999", Protocol: "openai", Tags: []string{"billing:plan"}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	valid := `{"default_amount":20,"currency":"USD","first_payment_at":"2040-01-31T12:00:00+08:00","timezone":"Asia/Shanghai"}`
	base := fmt.Sprintf("/plan-prices/%d", c.ID)
	for _, tc := range []struct {
		method, path, body, message string
		status                      int
	}{
		{"PUT", base, strings.Replace(valid, `"default_amount":20`, `"default_amount":-1`, 1), "金额须为非负数", 400},
		{"PUT", base, strings.Replace(valid, "USD", "EUR", 1), "币种须为", 400},
		{"PUT", base, strings.Replace(valid, "Asia/Shanghai", "Invalid/Nowhere", 1), "请选择有效时区", 400},
		{"PUT", base, strings.Replace(valid, "Asia/Shanghai", "", 1), "请填写首次支付时间和时区", 400},
		{"PUT", "/plan-prices/9999", valid, "渠道或套餐配置不存在", 404},
		{"GET", "/plan-prices?month=2040-1", "", "月份格式须为", 400},
		{"PUT", base + "/charges/2040-01", `{"amount":20,"currency":"USD"}`, "请填写支付时间", 400},
		{"PUT", "/channels/billing", `{"changes":null}`, "套餐声明格式无效", 400},
		{"PUT", "/channels/billing", fmt.Sprintf(`{"changes":[{"channel_id":%d,"billing_tag":"invalid"}]}`, c.ID), "请选择有效的计费类型", 400},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.message) {
			t.Fatalf("%s %s=%d %s, want %d containing %q", tc.method, tc.path, w.Code, w.Body.String(), tc.status, tc.message)
		}
	}
}

func TestPlanPricesAPIExitRemovesPendingButKeepsPaidHistory(t *testing.T) {
	for _, operation := range []string{"declaration", "delete_config", "delete_channel"} {
		t.Run(operation, func(t *testing.T) {
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			c := store.Channel{Name: "Synthetic", APIKey: "synthetic-secret", BaseURL: "http://localhost:9999", Protocol: "openai", Tags: []string{"billing:plan"}}
			if err = s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			future, _ := time.Parse(time.RFC3339, "2040-01-01T12:00:00+08:00")
			config := store.PlanPriceConfig{ChannelID: c.ID, Currency: "CNY", FirstPaymentAt: future, Timezone: "Asia/Shanghai"}
			if err = s.SavePlanPrice(config, now); err != nil {
				t.Fatal(err)
			}
			for _, charge := range []store.PlanCharge{
				{ChannelID: c.ID, Month: "2040-01", Currency: "CNY", PaidAt: future},
				{ChannelID: c.ID, Month: "2040-02", Currency: "CNY", PaidAt: now.Add(-time.Hour)},
			} {
				if err = s.SavePlanCharge(charge, now); err != nil {
					t.Fatal(err)
				}
			}
			h := New(s, "http://127.0.0.1:17878", Hooks{})
			method, path, body := "DELETE", fmt.Sprintf("/plan-prices/%d", c.ID), ""
			if operation == "declaration" {
				method, path, body = "PUT", "/channels/billing", fmt.Sprintf(`{"changes":[{"channel_id":%d,"billing_tag":"billing:free"}]}`, c.ID)
			} else if operation == "delete_channel" {
				path = fmt.Sprintf("/channels/%d", c.ID)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
			if w.Code != 200 {
				t.Fatalf("exit=%d %s", w.Code, w.Body.String())
			}
			for month, expected := range map[string]int{"2040-01": 0, "2040-02": 1} {
				w = httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", "/plan-prices?month="+month, nil))
				var view store.PlanPrices
				if err = json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 || len(view.Charges) != expected {
					t.Fatalf("month %s=%d %s err %v", month, w.Code, w.Body.String(), err)
				}
			}
		})
	}
}
