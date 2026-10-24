package pricing

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"time"
	"torouter/internal/store"
)

var ErrPreviewConflict = errors.New("configuration or source changed; preview again")

type BackfillExpected struct {
	Key        string                `json:"key"`
	Resolution store.PriceResolution `json:"resolution"`
}
type BackfillPrice struct {
	Key   string           `json:"key"`
	Table store.PriceTable `json:"table"`
}
type BackfillRequest struct {
	Filter     store.BackfillFilter `json:"filter"`
	MaxUsageID *int64               `json:"max_usage_id,omitempty"`
	Amounts    []string             `json:"amounts"`
	Mode       string               `json:"mode"`
	Prices     []BackfillPrice      `json:"prices,omitempty"`
	Note       string               `json:"note,omitempty"`
	Expected   []BackfillExpected   `json:"expected_resolution,omitempty"`
}
type BackfillGroup struct {
	Key               string                `json:"key"`
	Metric            string                `json:"metric"`
	ChannelID         int64                 `json:"channel_id"`
	ChannelName       string                `json:"channel_name"`
	Model             string                `json:"model"`
	PublicModels      []string              `json:"public_models,omitempty"`
	Records           int                   `json:"records"`
	CalculableAmounts int                   `json:"calculable_amounts"`
	MissingReasons    map[string]int        `json:"missing_reasons"`
	FirstTime         string                `json:"first_time"`
	LastTime          string                `json:"last_time"`
	Resolution        store.PriceResolution `json:"resolution"`
}
type BackfillPreview struct {
	Request           BackfillRequest    `json:"request"`
	Examined          int                `json:"examined"`
	CalculableAmounts int                `json:"calculable_amounts"`
	ExistingAmounts   int                `json:"existing_amounts"`
	MissingAmounts    int                `json:"missing_amounts"`
	MissingReasons    map[string]int     `json:"missing_reasons"`
	Groups            []BackfillGroup    `json:"groups"`
	Expected          []BackfillExpected `json:"expected_resolution"`
	AppliedAmounts    int                `json:"applied_amounts"`
}

func normalizeBackfill(req *BackfillRequest, apply bool) error {
	if req.Mode == "" {
		req.Mode = "current"
	}
	if req.Mode != "current" && req.Mode != "manual" {
		return errors.New("mode must be current or manual")
	}
	if len(req.Amounts) == 0 {
		req.Amounts = []string{"actual", "official"}
	}
	if len(req.Amounts) > 2 {
		return errors.New("choose actual and/or official")
	}
	seen := map[string]bool{}
	for _, a := range req.Amounts {
		if a != "actual" && a != "official" || seen[a] {
			return errors.New("choose distinct actual and/or official amounts")
		}
		seen[a] = true
	}
	if len(req.Note) > 2000 {
		return errors.New("note exceeds 2000 characters")
	}
	if req.Mode == "manual" {
		if len(req.Prices) == 0 {
			return errors.New("manual prices are required")
		}
		keys := map[string]bool{}
		for _, price := range req.Prices {
			if price.Key == "" || keys[price.Key] {
				return errors.New("choose distinct nonempty price keys")
			}
			keys[price.Key] = true
			if err := store.ValidatePriceTable(price.Table); err != nil {
				return err
			}
		}
	} else if req.Prices != nil {
		return errors.New("current mode does not accept manual prices")
	}
	if apply && req.MaxUsageID == nil {
		return errors.New("preview is required")
	}
	return nil
}
func comparableResolution(r store.PriceResolution) store.PriceResolution { r.FetchedAt = nil; return r }

func sameBackfillResolution(a, b store.PriceResolution) bool {
	// Disabled rule drafts contain RawMessage: JSON clients may change their
	// whitespace or property order. Compare JSON values, preserving numeric
	// precision so an actual source change still invalidates the preview.
	decode := func(r store.PriceResolution) (any, error) {
		raw, err := json.Marshal(comparableResolution(r))
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		err = decoder.Decode(&value)
		return normalizeBackfillNumbers(value), err
	}
	left, leftErr := decode(a)
	right, rightErr := decode(b)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(left, right)
}

func normalizeBackfillNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		mantissa, exponent := string(v), new(big.Int)
		if index := strings.IndexAny(mantissa, "eE"); index >= 0 {
			if _, ok := exponent.SetString(mantissa[index+1:], 10); !ok {
				return v
			}
			mantissa = mantissa[:index]
		}
		sign := ""
		if strings.HasPrefix(mantissa, "-") {
			sign, mantissa = "-", mantissa[1:]
		}
		fractionalDigits := 0
		if index := strings.IndexByte(mantissa, '.'); index >= 0 {
			fractionalDigits = len(mantissa) - index - 1
			mantissa = mantissa[:index] + mantissa[index+1:]
		}
		digits := strings.TrimLeft(mantissa, "0")
		if digits == "" {
			return json.Number("0")
		}
		trimmed := strings.TrimRight(digits, "0")
		exponent.Add(exponent, big.NewInt(int64(len(digits)-len(trimmed)-fractionalDigits)))
		// Keep significant digits and exponent separate: expanding 1e1000000
		// would turn a tiny disabled draft into a huge allocation under the lock.
		return json.Number(sign + trimmed + "e" + exponent.String())
	case map[string]any:
		for key, item := range v {
			v[key] = normalizeBackfillNumbers(item)
		}
	case []any:
		for index, item := range v {
			v[index] = normalizeBackfillNumbers(item)
		}
	}
	return value
}
func backfillKey(u store.Usage, metric string) string {
	identity := []any{metric, u.Model}
	if metric == "actual" {
		identity = []any{metric, u.ChannelID, u.UpstreamModel}
		if u.ChannelID <= 0 || u.UpstreamModel == "" {
			identity = append(identity, u.Model)
		}
	}
	raw, _ := json.Marshal(identity)
	hash := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(hash[:])
}
func backfill(s *store.Store, req BackfillRequest, apply bool) (BackfillPreview, error) {
	result := BackfillPreview{Groups: []BackfillGroup{}, Expected: []BackfillExpected{}, MissingReasons: map[string]int{}}
	if err := normalizeBackfill(&req, apply); err != nil {
		return result, err
	}
	maxID := int64(-1)
	if req.MaxUsageID != nil {
		maxID = *req.MaxUsageID
	}
	expected := map[string]store.PriceResolution{}
	for _, e := range req.Expected {
		expected[e.Key] = comparableResolution(e.Resolution)
	}
	prices := map[string]store.PriceTable{}
	for _, price := range req.Prices {
		prices[price.Key] = price.Table
	}
	groups := map[string]int{}
	firstTimes, lastTimes := map[string]time.Time{}, map[string]time.Time{}
	max, err := s.UsagePricingTransaction(req.Filter, maxID, apply, func(snapshot store.UsagePricingSnapshot) ([]store.UsagePricingPatch, error) {
		patches := []store.UsagePricingPatch{}
		foundPrices := map[string]bool{}
		for _, u := range snapshot.Usage {
			patch := store.UsagePricingPatch{ID: u.ID}
			examined := false
			actual, official := ResolveUsagePrices(snapshot.Settings.Prices, snapshot.Items, u.ChannelID, u.UpstreamModel, u.Model)
			if u.ChannelID <= 0 || u.UpstreamModel == "" {
				actual = store.PriceResolution{Status: "missing", Layer: "channel", Model: u.UpstreamModel, ChannelID: u.ChannelID, MissingReason: "upstream_identity_missing"}
			}
			for _, metric := range req.Amounts {
				if metric == "actual" && u.ActualPricing != nil && u.ActualPricing.Resolution.Layer == "plan" {
					continue
				}
				key := backfillKey(u, metric)
				table, selected := prices[key]
				if req.Mode == "manual" {
					if !selected {
						continue
					}
					foundPrices[key] = true
				}
				examined = true
				old := u.ActualCost
				resolution := actual
				if metric == "official" {
					old = u.OfficialValue
					resolution = official
				}
				if old != nil {
					result.ExistingAmounts++
					continue
				}
				if req.Mode == "manual" {
					resolution = store.PriceResolution{Status: "resolved", Layer: "manual", ChannelID: u.ChannelID, Model: u.UpstreamModel, Table: cloneTable(&table)}
					if metric == "official" {
						resolution.ChannelID = 0
						resolution.Model = u.Model
					}
				}
				if _, ok := groups[key]; !ok {
					if apply {
						old, found := expected[key]
						if !found || !sameBackfillResolution(old, resolution) {
							return nil, ErrPreviewConflict
						}
					}
					groups[key] = len(result.Groups)
					group := BackfillGroup{Key: key, Metric: metric, Model: u.Model, Resolution: resolution, MissingReasons: map[string]int{}}
					if metric == "actual" {
						group.ChannelID, group.Model = u.ChannelID, u.UpstreamModel
						group.PublicModels = []string{}
					}
					result.Groups = append(result.Groups, group)
					result.Expected = append(result.Expected, BackfillExpected{Key: key, Resolution: comparableResolution(resolution)})
				}
				group := &result.Groups[groups[key]]
				group.Records++
				when := u.Time.UTC().Format(time.RFC3339Nano)
				if group.FirstTime == "" || u.Time.Before(firstTimes[key]) {
					group.FirstTime, firstTimes[key] = when, u.Time
				}
				if group.LastTime == "" || !u.Time.Before(lastTimes[key]) {
					group.LastTime = when
					lastTimes[key] = u.Time
					if metric == "actual" {
						group.ChannelName = u.ChannelName
					}
				}
				if metric == "actual" {
					found := false
					for _, model := range group.PublicModels {
						found = found || model == u.Model
					}
					if !found {
						group.PublicModels = append(group.PublicModels, u.Model)
					}
				}
				method := "backfill_current"
				if req.Mode == "manual" {
					method = "backfill_manual"
				}
				calc := Compute(resolution, u, method, req.Note)
				if calc.Amount == nil {
					result.MissingAmounts++
					result.MissingReasons[calc.MissingReason]++
					group.MissingReasons[calc.MissingReason]++
					continue
				}
				result.CalculableAmounts++
				group.CalculableAmounts++
				basis := calc.Basis
				if metric == "actual" {
					patch.ActualCost = calc.Amount
					patch.Currency = basis.Currency
					patch.ActualBasis = &basis
				} else {
					patch.OfficialValue = calc.Amount
					patch.OfficialCurrency = basis.Currency
					patch.OfficialBasis = &basis
				}
			}
			if examined {
				result.Examined++
			}
			if patch.ActualCost != nil || patch.OfficialValue != nil {
				patches = append(patches, patch)
			}
		}
		for key := range prices {
			if !foundPrices[key] {
				return nil, errors.New("manual price key is outside the selected range")
			}
		}
		return patches, nil
	})
	if err != nil {
		return result, err
	}
	req.MaxUsageID = &max
	req.Expected = nil
	result.Request = req
	for i := range result.Groups {
		sort.Strings(result.Groups[i].PublicModels)
	}
	sort.Slice(result.Groups, func(i, j int) bool {
		a, b := result.Groups[i], result.Groups[j]
		if a.Metric != b.Metric {
			return a.Metric < b.Metric
		}
		if a.ChannelName != b.ChannelName {
			return a.ChannelName < b.ChannelName
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Key < b.Key
	})
	if apply {
		result.AppliedAmounts = result.CalculableAmounts
	}
	return result, nil
}
func PreviewBackfill(s *store.Store, req BackfillRequest) (BackfillPreview, error) {
	return backfill(s, req, false)
}
func ApplyBackfill(s *store.Store, req BackfillRequest) (BackfillPreview, error) {
	return backfill(s, req, true)
}
