package quota

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"torouter/internal/store"
)

func officialDeepSeek(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.deepseek.com") && (u.Port() == "" || u.Port() == "443") && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func deepSeekTarget(c store.Channel, cred store.CredentialItem) bool {
	if c.CredentialType != "" && c.CredentialType != store.CredentialAPIKey || cred.APIKey == "" || cred.OAuth != nil {
		return false
	}
	if len(c.Endpoints) == 0 {
		return officialDeepSeek(c.BaseURL)
	}
	for _, endpoint := range c.Endpoints {
		if !officialDeepSeek(c.EndpointBaseURL(endpoint)) {
			return false
		}
	}
	return true
}

var decimalAmount = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

func parseBalanceSnapshot(body []byte) (*bool, []Balance, error) {
	var wire struct {
		Available *bool `json:"is_available"`
		Balances  []struct {
			Currency string  `json:"currency"`
			Total    *string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, nil, err
	}
	balances := []Balance{}
	currencies := make(map[string]int)
	for _, balance := range wire.Balances {
		currencies[balance.Currency]++
	}
	for _, balance := range wire.Balances {
		// An ambiguous repeated currency is unknown; never pick or add rows.
		if currencies[balance.Currency] == 1 && (balance.Currency == "CNY" || balance.Currency == "USD") && balance.Total != nil && len(*balance.Total) <= 128 && decimalAmount.MatchString(*balance.Total) {
			balances = append(balances, Balance{Currency: balance.Currency, Total: *balance.Total})
		}
	}
	if wire.Available == nil {
		return nil, balances, nil
	}
	if !*wire.Available {
		return wire.Available, balances, nil
	}
	known, unknown := false, false
	for _, balance := range wire.Balances {
		if balance.Currency != "CNY" && balance.Currency != "USD" {
			unknown = true
			continue
		}
		if balance.Total == nil || len(*balance.Total) > 128 || !decimalAmount.MatchString(*balance.Total) {
			unknown = true
			continue
		}
		amount, ok := new(big.Rat).SetString(*balance.Total)
		if !ok {
			unknown = true
			continue
		}
		known = true
		if amount.Sign() > 0 {
			v := true
			return &v, balances, nil
		}
	}
	if known && !unknown {
		v := false
		return &v, balances, nil
	}
	return nil, balances, nil
}

func parseBalance(body []byte) (*bool, error) {
	available, _, err := parseBalanceSnapshot(body)
	return available, err
}

func (s *Service) deepSeek(ctx context.Context, key string) (*bool, []Balance, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.deepseek.com/user/balance", nil)
	if err != nil {
		return nil, nil, "DeepSeek 余额查询不可用"
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, "DeepSeek 余额查询失败，请检查网络"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, "DeepSeek 余额查询返回异常状态"
	}
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return nil, nil, "DeepSeek 余额响应读取失败"
	}
	available, balances, err := parseBalanceSnapshot(body)
	if err != nil {
		return nil, nil, "DeepSeek 余额响应格式无效"
	}
	return available, balances, ""
}
