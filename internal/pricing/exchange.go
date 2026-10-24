package pricing

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"torouter/internal/store"
)

type ExchangeService struct {
	Store     *store.Store
	Client    *http.Client
	Endpoints map[string]string // Test injection; user configuration selects built-ins only.
	Now       func() time.Time
}

func NewExchangeService(s *store.Store) *ExchangeService {
	return &ExchangeService{Store: s, Client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Endpoints: map[string]string{
		"ecb":              "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml",
		"frankfurter":      "https://api.frankfurter.dev/v2/rate/USD/CNY",
		"exchangerate_api": "https://open.er-api.com/v6/latest/USD",
	}, Now: time.Now}
}
func (s *ExchangeService) Get(ctx context.Context) (store.ExchangeRate, error) {
	return s.read(ctx, false)
}
func (s *ExchangeService) Refresh(ctx context.Context) (store.ExchangeRate, error) {
	return s.read(ctx, true)
}
func (s *ExchangeService) read(ctx context.Context, force bool) (store.ExchangeRate, error) {
	now := s.Now()
	return s.Store.DailyExchangeRate(now, force, func() (store.ExchangeRate, error) {
		sources, err := s.Store.GetExchangeSources()
		if err != nil {
			return store.ExchangeRate{}, errors.New("无法读取汇率来源配置")
		}
		if len(sources) == 0 {
			return store.ExchangeRate{}, errors.New("未启用汇率来源")
		}
		// Decouple network fetch context from the single incoming HTTP request context.
		// A client tab refresh or abort should not cancel the daily background fetch or taint the daily cache.
		fetchTimeout := time.Duration(len(sources)*8) * time.Second
		if fetchTimeout < 8*time.Second {
			fetchTimeout = 8 * time.Second
		}
		fetchCtx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		var failures []string
		for _, id := range sources {
			if fetchCtx.Err() != nil {
				return store.ExchangeRate{}, errors.New("汇率查询已取消或超时")
			}
			var source store.ExchangeSource
			for _, candidate := range store.AvailableExchangeSources() {
				if candidate.ID == id {
					source = candidate
					break
				}
			}
			rate, date, err := s.fetch(fetchCtx, id, now)
			if err == nil {
				return store.ExchangeRate{Rate: rate, Date: date, SourceID: id, SourceName: source.Name, SourceURL: source.URL}, nil
			}
			failures = append(failures, source.Name+"："+err.Error())
		}
		return store.ExchangeRate{}, errors.New(strings.Join(failures, "；"))
	})
}
func (s *ExchangeService) fetch(ctx context.Context, id string, now time.Time) (float64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Endpoints[id], nil)
	if err != nil {
		return 0, "", errors.New("地址无效")
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return 0, "", errors.New("连接失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return 0, "", errors.New("响应读取失败")
	}
	rate, date, err := parseExchange(id, raw)
	if err != nil {
		return 0, "", errors.New("响应格式无效")
	}
	parsed, err := time.Parse("2006-01-02", date)
	today := now.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")
	if err != nil || parsed.Format("2006-01-02") != date || date > today || !positiveRate(rate) {
		return 0, "", errors.New("日期或汇率无效")
	}
	return rate, date, nil
}
func positiveRate(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
func parseExchange(id string, raw []byte) (float64, string, error) {
	invalid := errors.New("invalid exchange response")
	switch id {
	case "ecb":
		var v struct {
			Days []struct {
				Date  string `xml:"time,attr"`
				Rates []struct {
					Currency string  `xml:"currency,attr"`
					Rate     float64 `xml:"rate,attr"`
				} `xml:"Cube"`
			} `xml:"Cube>Cube"`
		}
		if xml.Unmarshal(raw, &v) != nil || len(v.Days) != 1 {
			return 0, "", invalid
		}
		var usd, cny float64
		for _, rate := range v.Days[0].Rates {
			switch rate.Currency {
			case "USD":
				usd = rate.Rate
			case "CNY":
				cny = rate.Rate
			}
		}
		if !positiveRate(usd) || !positiveRate(cny) {
			return 0, "", invalid
		}
		return cny / usd, v.Days[0].Date, nil
	case "frankfurter":
		var v struct {
			Base  string  `json:"base"`
			Quote string  `json:"quote"`
			Rate  float64 `json:"rate"`
			Date  string  `json:"date"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Base != "USD" || v.Quote != "CNY" {
			return 0, "", invalid
		}
		return v.Rate, v.Date, nil
	case "exchangerate_api":
		var v struct {
			Result  string             `json:"result"`
			Base    string             `json:"base_code"`
			Updated int64              `json:"time_last_update_unix"`
			Rates   map[string]float64 `json:"rates"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Result != "success" || v.Base != "USD" || v.Updated <= 0 {
			return 0, "", invalid
		}
		return v.Rates["CNY"], time.Unix(v.Updated, 0).UTC().Format("2006-01-02"), nil
	}
	return 0, "", invalid
}
