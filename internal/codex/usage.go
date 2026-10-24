package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"torouter/internal/accessguard"
)

// These snapshots expose only display-safe fields. Routing availability is
// derived separately by quota from a fresh general bucket; persisted display
// reports are not credential-health facts. Pointers preserve unknown values.
type UsageWindow struct {
	UsedPercent   *float64 `json:"used_percent,omitempty"`
	WindowSeconds *int64   `json:"window_seconds,omitempty"`
	ResetAt       *int64   `json:"reset_at,omitempty"`
}

type UsageBucket struct {
	// General records the root rate_limit's provenance; vendor IDs can name
	// unrelated additional buckets and must not establish routing scope.
	General      bool         `json:"general"`
	ID           string       `json:"id"`
	Name         string       `json:"name,omitempty"`
	Allowed      *bool        `json:"allowed,omitempty"`
	LimitReached *bool        `json:"limit_reached,omitempty"`
	Primary      *UsageWindow `json:"primary,omitempty"`
	Secondary    *UsageWindow `json:"secondary,omitempty"`
}

type Usage struct {
	CapturedAt       time.Time     `json:"captured_at"`
	PlanType         string        `json:"plan_type,omitempty"`
	Buckets          []UsageBucket `json:"buckets"`
	ResetCreditCount *int          `json:"reset_credit_count,omitempty"`
}

const usageBodyLimit = 1 << 20

// FetchUsage is a passive read. Never enable Reserve, probe generation, or
// forward credentials through redirects. A client copy preserves the injected
// transport and timeout without changing the other OAuth calls' behavior.
func FetchUsage(ctx context.Context, client *http.Client, eps Endpoints, accessToken, accountID string) (Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eps.Usage, nil)
	if err != nil {
		return Usage{}, errors.New("invalid usage endpoint")
	}
	ApplyRequestHeaders(req.Header, accessToken, accountID, "")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	resp, err := accessguard.Do(usageHTTPClient(client), req)
	if err != nil {
		if accessguard.Denied(err) {
			return Usage{}, err
		}
		if ctx.Err() != nil {
			return Usage{}, ctx.Err()
		}
		return Usage{}, errors.New("usage endpoint request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("usage endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, usageBodyLimit+1))
	if err != nil {
		return Usage{}, errors.New("could not read usage response")
	}
	if len(body) > usageBodyLimit {
		return Usage{}, errors.New("usage response exceeds size limit")
	}
	u, err := parseUsage(body, time.Now().UTC())
	if err != nil {
		return Usage{}, errors.New("usage endpoint returned an invalid JSON report")
	}
	// Only the documented display fields leave this parser; bound their text and
	// suppress a credential even if a misconfigured upstream reflects it there.
	clean := func(s string) string {
		if accessToken != "" {
			s = strings.ReplaceAll(s, accessToken, "[redacted]")
		}
		return firstN(s, 120)
	}
	u.PlanType = clean(u.PlanType)
	for i := range u.Buckets {
		u.Buckets[i].ID = clean(u.Buckets[i].ID)
		u.Buckets[i].Name = clean(u.Buckets[i].Name)
	}
	return u, nil
}

func usageHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	passive := *client
	passive.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &passive
}

type usageObject map[string]json.RawMessage

func usageField(obj usageObject, names ...string) json.RawMessage {
	for _, name := range names {
		if v, ok := obj[name]; ok {
			return v
		}
	}
	return nil
}

func usageString(obj usageObject, names ...string) string {
	var value string
	_ = json.Unmarshal(usageField(obj, names...), &value)
	return strings.TrimSpace(value)
}

func parseUsage(body []byte, captured time.Time) (Usage, error) {
	var root usageObject
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return Usage{}, errors.New("expected usage object")
	}
	u := Usage{CapturedAt: captured, PlanType: usageString(root, "plan_type"), Buckets: []UsageBucket{}}
	if raw := usageField(root, "rate_limit_reset_credits", "rateLimitResetCredits"); len(raw) > 0 && string(raw) != "null" {
		var credits usageObject
		if err := json.Unmarshal(raw, &credits); err != nil || credits == nil {
			return Usage{}, errors.New("invalid reset credits summary")
		}
		count, err := resetCreditCount(usageField(credits, "available_count", "availableCount"))
		if err != nil {
			return Usage{}, err
		}
		u.ResetCreditCount = count
	}
	appendBucket := func(id string, raw json.RawMessage) error {
		bucket, err := parseUsageBucket(id, raw, captured.Unix())
		if err != nil {
			return err
		}
		if bucket != nil {
			u.Buckets = append(u.Buckets, *bucket)
		}
		return nil
	}
	if err := appendBucket("rate_limit", usageField(root, "rate_limit", "rateLimit")); err != nil {
		return Usage{}, err
	}
	if len(u.Buckets) == 1 {
		u.Buckets[0].General = true
	}
	// Sort object keys so repeated snapshots have stable display order.
	keys := make([]string, 0, len(root))
	for key := range root {
		if key != "rate_limit" && key != "rateLimit" && (strings.HasSuffix(key, "_rate_limit") || strings.HasSuffix(key, "RateLimit")) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := appendBucket(key, root[key]); err != nil {
			return Usage{}, err
		}
	}
	extra := usageField(root, "additional_rate_limits", "additionalRateLimits")
	if len(extra) == 0 || string(extra) == "null" {
		return u, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(extra, &list); err == nil {
		for i, raw := range list {
			if err := appendBucket(fmt.Sprintf("additional_rate_limits[%d]", i), raw); err != nil {
				return Usage{}, err
			}
		}
		return u, nil
	}
	var byID usageObject
	if err := json.Unmarshal(extra, &byID); err != nil {
		return Usage{}, err
	}
	keys = keys[:0]
	for key := range byID {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := appendBucket(key, byID[key]); err != nil {
			return Usage{}, err
		}
	}
	return u, nil
}

func parseUsageBucket(id string, raw json.RawMessage, now int64) (*UsageBucket, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entry usageObject
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, err
	}
	rate := entry
	if nested := usageField(entry, "rate_limit", "rateLimit"); len(nested) > 0 {
		rate = nil
		if err := json.Unmarshal(nested, &rate); err != nil {
			return nil, err
		}
	}
	field := func(names ...string) json.RawMessage {
		if v := usageField(entry, names...); len(v) != 0 {
			return v
		}
		return usageField(rate, names...)
	}
	b := &UsageBucket{ID: id}
	for _, names := range [][]string{{"limit_id", "limitId"}, {"metered_feature", "meteredFeature"}} {
		var value string
		_ = json.Unmarshal(field(names...), &value)
		if strings.TrimSpace(value) != "" {
			b.ID = strings.TrimSpace(value)
			break
		}
	}
	_ = json.Unmarshal(field("limit_name", "limitName"), &b.Name)
	for _, item := range []struct {
		raw json.RawMessage
		dst any
	}{
		{field("allowed"), &b.Allowed}, {field("limit_reached", "limitReached"), &b.LimitReached},
	} {
		if len(item.raw) > 0 {
			if err := json.Unmarshal(item.raw, item.dst); err != nil {
				return nil, err
			}
		}
	}
	var err error
	b.Primary, err = parseUsageWindow(usageField(rate, "primary_window", "primaryWindow"), now)
	if err != nil {
		return nil, err
	}
	b.Secondary, err = parseUsageWindow(usageField(rate, "secondary_window", "secondaryWindow"), now)
	return b, err
}

func parseUsageWindow(raw json.RawMessage, now int64) (*UsageWindow, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var wire struct {
		UsedPercent *float64 `json:"used_percent"`
		Seconds     *int64   `json:"limit_window_seconds"`
		ResetAt     *int64   `json:"reset_at"`
		ResetAfter  *int64   `json:"reset_after_seconds"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	if wire.ResetAt == nil && wire.ResetAfter != nil && *wire.ResetAfter >= 0 && *wire.ResetAfter <= math.MaxInt64-now {
		reset := now + *wire.ResetAfter
		wire.ResetAt = &reset
	}
	return &UsageWindow{UsedPercent: wire.UsedPercent, WindowSeconds: wire.Seconds, ResetAt: wire.ResetAt}, nil
}
