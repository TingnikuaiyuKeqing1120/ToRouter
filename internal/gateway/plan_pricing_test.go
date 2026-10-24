package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestPlanPricingFreezesTagsBeforeUpstreamIncludingErrors(t *testing.T) {
	for _, status := range []int{200, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			g, s := setup(t)
			entered, release := make(chan struct{}), make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				w.WriteHeader(status)
				io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
			}))
			defer up.Close()
			add(t, s, up.URL, "openai", 0)
			channels, _ := s.ListChannels()
			c := channels[0]
			c.Tags = []string{"billing:plan"}
			if err := s.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			if err := s.SavePrices([]store.Price{priceRow(c.ID, "test", "CNY", store.PriceRates{Input: pricingNumber(999)}), priceRow(0, "test", "USD", store.PriceRates{Input: pricingNumber(2)})}); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- invoke(g, "/v1/chat/completions", `{"model":"test","messages":[]}`) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not start")
			}
			c.Tags = []string{"billing:payg"}
			saveErr := s.SaveChannel(&c)
			close(release)
			if saveErr != nil {
				t.Fatal(saveErr)
			}
			select {
			case w := <-done:
				if w.Code != status {
					t.Fatal(w.Code, w.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request did not complete")
			}
			rows, err := s.ListUsage(store.UsageFilter{})
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
			u := rows[0]
			if u.ActualCost != nil || u.ActualPricing.Resolution.Layer != "plan" || u.ActualPricing.MissingReason != "plan_unallocated" {
				t.Fatal("plan snapshot changed", u)
			}
			if status == 200 && (u.OfficialValue == nil || *u.OfficialValue != 2) {
				t.Fatal("official price lost", u)
			}
		})
	}
}

func TestFallbackPricesSelectedModelWithinMixedPlanChannel(t *testing.T) {
	g, s := setup(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "plan-model" {
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	}))
	defer up.Close()
	c := store.Channel{Name: "mixed", APIKey: "secret", Models: []string{"plan-model", "payg-model"}, Tags: []string{"billing:plan"}, ModelTags: map[string][]string{"payg-model": {"billing:payg"}}, Enabled: true, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: up.URL}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "public", Enabled: true, Fallback: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "plan-model", Enabled: true}, {ChannelID: c.ID, Model: "payg-model", Enabled: true, Priority: 1}}}); err != nil {
		t.Fatal(err)
	}
	settings, _ := s.GetSettings()
	settings.RetriesPerChannel = 1
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePrices([]store.Price{priceRow(c.ID, "plan-model", "USD", store.PriceRates{Input: pricingNumber(999)}), priceRow(c.ID, "payg-model", "USD", store.PriceRates{Input: pricingNumber(3)})}); err != nil {
		t.Fatal(err)
	}
	w := invoke(g, "/v1/chat/completions", `{"model":"public","messages":[]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	rows, err := s.ListUsage(store.UsageFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if rows[0].UpstreamModel != "payg-model" || rows[0].ActualCost == nil || *rows[0].ActualCost != 3 || rows[0].ActualPricing.Resolution.Layer != "channel" {
		t.Fatal("fallback used wrong model billing", rows[0])
	}
	if rows[1].UpstreamModel != "plan-model" || rows[1].ActualCost != nil || rows[1].ActualPricing.Resolution.Layer != "plan" {
		t.Fatal(rows[1])
	}
}
