package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestRecordEstimatedUsageNeverProducesKnownAmounts(t *testing.T) {
	g, s := setup(t)
	prices := []store.Price{priceRow(0, "alias", "USD", store.PriceRates{Input: pricingNumber(2)}), priceRow(1, "real", "CNY", store.PriceRates{Input: pricingNumber(3)})}
	for _, estimated := range []bool{true, false} {
		g.record(store.Usage{Model: "alias", UpstreamModel: "real", ChannelID: 1, Estimated: estimated}, store.Settings{Prices: prices}, time.Now())
		rows, err := s.ListUsage(store.UsageFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		u := rows[0]
		if estimated {
			if u.OfficialValue != nil || u.ActualCost != nil || u.Currency != "" || u.OfficialCurrency != "" {
				t.Fatalf("unknown usage became known zero: %+v", u)
			}
		} else if u.OfficialValue == nil || u.ActualCost == nil || *u.OfficialValue != 0 || *u.ActualCost != 0 {
			t.Fatalf("measured zero usage lost known amounts: %+v", u)
		}
	}
}

func TestResponseUsageAvailabilityControlsPricing(t *testing.T) {
	for _, tc := range []struct {
		name, body                          string
		stream, truncated, oversized, known bool
		status                              int
	}{
		{name: "json_missing_usage", body: `{"choices":[{"message":{"content":"ok"}}]}`, status: 200},
		{name: "json_known_zero_usage", body: `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, known: true, status: 200},
		{name: "body_read_failure_before_parse", body: `{"choices":[]}`, truncated: true, status: 502},
		{name: "body_limit_before_parse", oversized: true, status: 502},
		{name: "stream_missing_usage", body: "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", stream: true, status: 200},
		{name: "stream_known_zero_usage", body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n", stream: true, known: true, status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s := setup(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				if tc.truncated {
					w.Header().Set("Content-Length", "1024")
				}
				if tc.oversized {
					io.CopyN(w, strings.NewReader(strings.Repeat("x", maxBody+1)), maxBody+1)
					return
				}
				io.WriteString(w, tc.body)
			}))
			defer up.Close()
			add(t, s, up.URL, "openai", 0)
			channels, err := s.ListChannels()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SavePrices([]store.Price{priceRow(0, "test", "USD", store.PriceRates{Input: pricingNumber(2)}), priceRow(channels[0].ID, "test", "CNY", store.PriceRates{Input: pricingNumber(3)})}); err != nil {
				t.Fatal(err)
			}
			body := `{"model":"test","messages":[]}`
			if tc.stream {
				body = `{"model":"test","messages":[],"stream":true}`
			}
			w := invoke(g, "/v1/chat/completions", body)
			if w.Code != tc.status {
				t.Fatalf("status %d want %d", w.Code, tc.status)
			}
			rows, err := s.ListUsage(store.UsageFilter{})
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
			u := rows[0]
			if u.Estimated == tc.known {
				t.Fatalf("usage availability: %+v", u)
			}
			if tc.known {
				if u.OfficialValue == nil || u.ActualCost == nil || *u.OfficialValue != 0 || *u.ActualCost != 0 {
					t.Fatalf("known zero lost: %+v", u)
				}
			} else if u.OfficialValue != nil || u.ActualCost != nil {
				t.Fatalf("unknown usage classified as free: %+v", u)
			}
		})
	}
}
