package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"torouter/internal/store"
)

var defaultClient = &http.Client{Timeout: 20 * time.Second}

const openRouterURL = "https://openrouter.ai/api/v1/models"
const liteLLMURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
const maxPayloadBytes = 32 << 20

func getJSON(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "to-router-catalog/2.0")
	resp, err := defaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog source returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPayloadBytes {
		return nil, fmt.Errorf("catalog payload exceeds size limit")
	}
	return b, nil
}

type rawObject map[string]json.RawMessage

func number(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		v, e := strconv.ParseFloat(s, 64)
		if e != nil {
			return nil
		}
		n = v
	}
	if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func integer(raw json.RawMessage) *int64 {
	n := number(raw)
	if n == nil || *n <= 0 || *n > 1e12 || math.Trunc(*n) != *n {
		return nil
	}
	v := int64(*n)
	return &v
}
func str(raw json.RawMessage) string { var v string; _ = json.Unmarshal(raw, &v); return v }
func stringsValue(raw json.RawMessage) []string {
	var v []string
	_ = json.Unmarshal(raw, &v)
	return v
}
func boolValue(raw json.RawMessage) *bool  { var v *bool; _ = json.Unmarshal(raw, &v); return v }
func object(raw json.RawMessage) rawObject { var v rawObject; _ = json.Unmarshal(raw, &v); return v }
func scaled(raw json.RawMessage, scale float64) *float64 {
	n := number(raw)
	if n == nil {
		return nil
	}
	v := *n * scale
	if math.IsInf(v, 0) {
		return nil
	}
	return &v
}
func ordered(items []store.CatalogItem) []store.CatalogItem {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// FetchOpenRouter requests only the directory; exact provider endpoints are on demand.
func FetchOpenRouter(ctx context.Context) ([]store.CatalogItem, error) {
	b, e := getJSON(ctx, openRouterURL)
	if e != nil {
		return nil, e
	}
	return ParseOpenRouter(b, time.Now().UTC())
}
func ParseOpenRouter(body []byte, at time.Time) ([]store.CatalogItem, error) {
	var payload struct {
		Data []rawObject `json:"data"`
	}
	if e := json.Unmarshal(body, &payload); e != nil {
		return nil, e
	}
	if payload.Data == nil {
		return nil, fmt.Errorf("OpenRouter response lacks data array")
	}
	items := make([]store.CatalogItem, 0, len(payload.Data))
	for _, m := range payload.Data {
		id := str(m["id"])
		if id == "" {
			continue
		}
		arch := object(m["architecture"])
		top := object(m["top_provider"])
		item := store.CatalogItem{ID: id, OriginalID: id, Name: str(m["name"]), Source: "openrouter", Provider: strings.Split(id, "/")[0], ContextLength: integer(m["context_length"]), MaxOutputTokens: integer(top["max_completion_tokens"]), Modalities: store.CatalogModalities{Input: stringsValue(arch["input_modalities"]), Output: stringsValue(arch["output_modalities"])}, DocURL: "https://openrouter.ai/" + id, FetchedAt: &at, Thinking: store.CatalogThinking{Modes: []store.NativeThinkingMode{}}, Offerings: []store.CatalogOffering{}}
		item.APIFormats = []string{"openai_chat_completions"}
		item.OutputTokenField = "max_tokens"
		r := object(m["reasoning"])
		supported := r != nil
		for _, p := range stringsValue(m["supported_parameters"]) {
			if p == "reasoning" || p == "reasoning_effort" {
				supported = true
			}
		}
		if supported {
			item.Thinking.Supported = &supported
			item.Thinking.Mandatory = boolValue(r["mandatory"])
			mode := store.NativeThinkingMode{ID: "openrouter-reasoning", APIFormat: "openai_chat_completions", Control: "effort", OutputTokenField: "max_tokens", Efforts: stringsValue(r["supported_efforts"])}
			if effort := str(r["default_effort"]); effort != "" {
				mode.DefaultEffort = &effort
			}
			item.Thinking.Modes = append(item.Thinking.Modes, mode)
			// These controls describe OpenRouter's accepted request, not the
			// original vendor's field names or budget conversions.
			if item.Thinking.Mandatory != nil || boolValue(r["default_enabled"]) != nil {
				item.Thinking.Modes = append(item.Thinking.Modes, store.NativeThinkingMode{ID: "openrouter-enabled", APIFormat: "openai_chat_completions", Control: "enabled", OutputTokenField: "max_tokens"})
			}
			if budget := boolValue(r["supports_max_tokens"]); budget != nil && *budget {
				item.Thinking.Modes = append(item.Thinking.Modes, store.NativeThinkingMode{ID: "openrouter-budget", APIFormat: "openai_chat_completions", Control: "budget", OutputTokenField: "max_tokens"})
			}
		}
		item.Offerings = append(item.Offerings, store.CatalogOffering{Key: "directory-summary", Provider: "OpenRouter", SourceKind: "aggregator_summary", Quotes: []store.CatalogQuote{{Key: "usd-token", Name: "Directory summary", SourceKind: "aggregator_summary", SourceURL: openRouterURL, Table: openRouterTable(object(m["pricing"])), Notes: append([]string{"Source summary; not a particular provider offering."}, openRouterFeeNotes(object(m["pricing"]))...)}}})
		items = append(items, item)
	}
	return ordered(items), nil
}
func openRouterTable(p rawObject) store.PriceTable {
	t := store.PriceTable{Currency: "USD", UnitTokens: 1000000, Rates: store.PriceRates{Input: scaled(p["prompt"], 1e6), Output: scaled(p["completion"], 1e6), CacheRead: scaled(p["input_cache_read"], 1e6), CacheWrite: scaled(p["input_cache_write"], 1e6)}, Rules: []store.PriceRule{}}
	keys := map[string]string{"prompt": "input", "completion": "output", "input_cache_read": "cache_read", "input_cache_write": "cache_write"}
	for k, v := range p {
		if _, ok := keys[k]; ok || k == "overrides" {
			continue
		}
		if openRouterAdditionalFee(k) {
			continue
		}
		if k == "discount" && number(v) != nil && *number(v) == 0 {
			continue
		}
		t.UncoveredConditions = append(t.UncoveredConditions, "Uncovered OpenRouter pricing field: "+k)
	}
	var overrides []rawObject
	if len(p["overrides"]) > 0 && json.Unmarshal(p["overrides"], &overrides) != nil {
		t.UncoveredConditions = append(t.UncoveredConditions, "Unrecognized pricing overrides")
	}
	for _, o := range overrides {
		from := integer(o["min_prompt_tokens"])
		if from == nil {
			t.UncoveredConditions = append(t.UncoveredConditions, "Override lacks supported threshold")
			continue
		}
		changes := map[string]*float64{}
		for k, role := range keys {
			if raw, ok := o[k]; ok {
				changes[role] = scaled(raw, 1e6)
			}
		}
		for k := range o {
			if _, ok := keys[k]; !ok && k != "min_prompt_tokens" {
				t.UncoveredConditions = append(t.UncoveredConditions, "Uncovered override field: "+k)
			}
		}
		t.Rules = append(t.Rules, store.PriceRule{ID: fmt.Sprintf("prompt-ge-%d", *from), Enabled: true, When: store.PriceRuleWhen{InputRange: &store.PriceInputRange{FromTokens: *from}}, Action: store.PriceRuleAction{RateChanges: changes}})
	}
	sort.Strings(t.UncoveredConditions)
	return t
}
func openRouterAdditionalFee(k string) bool {
	return k == "web_search" || k == "request" || k == "image"
}
func openRouterFeeNotes(p rawObject) []string {
	notes := []string{}
	for k := range p {
		if openRouterAdditionalFee(k) {
			notes = append(notes, "Additional source fee "+k+" is outside the four token categories.")
		}
	}
	sort.Strings(notes)
	return notes
}
func FetchOpenRouterEndpoints(ctx context.Context, id string) ([]store.CatalogOffering, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("OpenRouter id must be author/slug")
	}
	endpoint := openRouterURL + "/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/endpoints"
	b, e := getJSON(ctx, endpoint)
	if e != nil {
		return nil, e
	}
	return ParseOpenRouterEndpoints(b, endpoint)
}
func ParseOpenRouterEndpoints(body []byte, sourceURL string) ([]store.CatalogOffering, error) {
	var p struct {
		Data struct {
			Endpoints []rawObject `json:"endpoints"`
		} `json:"data"`
	}
	if e := json.Unmarshal(body, &p); e != nil {
		return nil, e
	}
	if p.Data.Endpoints == nil {
		return nil, fmt.Errorf("OpenRouter response lacks endpoints")
	}
	out := []store.CatalogOffering{}
	seen := map[string]bool{}
	for _, e := range p.Data.Endpoints {
		tag := str(e["tag"])
		if tag == "" {
			return nil, fmt.Errorf("endpoint lacks stable provider tag")
		}
		key := tag
		if q := str(e["quantization"]); q != "" && q != "unknown" {
			key += "/quantization:" + q
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate endpoint identity %q", key)
		}
		seen[key] = true
		out = append(out, store.CatalogOffering{Key: key, Provider: str(e["provider_name"]), Region: str(e["region"]), ServiceTier: tag, SourceKind: "hosted", Quotes: []store.CatalogQuote{{Key: "usd-token", Name: str(e["name"]), Table: openRouterTable(object(e["pricing"])), SourceKind: "hosted", SourceURL: sourceURL, Notes: openRouterFeeNotes(object(e["pricing"]))}}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func FetchLiteLLM(ctx context.Context) ([]store.CatalogItem, error) {
	var last error
	for _, u := range []string{liteLLMURL, "https://cdn.jsdelivr.net/gh/BerriAI/litellm@main/model_prices_and_context_window.json"} {
		b, e := getJSON(ctx, u)
		if e != nil {
			last = e
			continue
		}
		items, e := ParseLiteLLM(b, time.Now().UTC())
		if e == nil {
			return items, nil
		}
		last = e
	}
	return nil, fmt.Errorf("LiteLLM fetch: %w", last)
}

var liteRateKeys = map[string]string{"input_cost_per_token": "input", "output_cost_per_token": "output", "cache_read_input_token_cost": "cache_read", "cache_creation_input_token_cost": "cache_write"}

func ParseLiteLLM(body []byte, at time.Time) ([]store.CatalogItem, error) {
	var p map[string]rawObject
	if e := json.Unmarshal(body, &p); e != nil {
		return nil, e
	}
	if p == nil {
		return nil, fmt.Errorf("LiteLLM response lacks dictionary")
	}
	out := []store.CatalogItem{}
	for id, m := range p {
		if strings.HasPrefix(id, "sample_") {
			continue
		}
		mode := str(m["mode"])
		if mode != "chat" && mode != "completion" {
			continue
		}
		item := store.CatalogItem{ID: id, OriginalID: id, Name: id, Source: "litellm", Provider: str(m["litellm_provider"]), MaxInputTokens: integer(m["max_input_tokens"]), MaxOutputTokens: integer(m["max_output_tokens"]), ContextLength: integer(m["context_window"]), Modalities: store.CatalogModalities{Input: stringsValue(m["supported_modalities"]), Output: stringsValue(m["supported_output_modalities"])}, Thinking: store.CatalogThinking{Supported: boolValue(m["supports_reasoning"]), Modes: []store.NativeThinkingMode{}}, DocURL: str(m["source"]), FetchedAt: &at, Offerings: []store.CatalogOffering{}}
		for _, endpoint := range stringsValue(m["supported_endpoints"]) {
			switch endpoint {
			case "/v1/chat/completions":
				item.APIFormats = append(item.APIFormats, "openai_chat_completions")
			case "/v1/responses":
				item.APIFormats = append(item.APIFormats, "openai_responses")
			case "/v1/messages":
				item.APIFormats = append(item.APIFormats, "anthropic_messages")
			}
		}
		// max_tokens is a legacy output/input fallback, never a total context window.
		if item.Modalities.Input == nil {
			item.Modalities.Input = []string{"text"}
			for field, mod := range map[string]string{"supports_vision": "image", "supports_audio_input": "audio"} {
				if v := boolValue(m[field]); v != nil && *v {
					item.Modalities.Input = append(item.Modalities.Input, mod)
				}
			}
			sort.Strings(item.Modalities.Input)
		}
		if item.Modalities.Output == nil {
			item.Modalities.Output = []string{"text"}
			if v := boolValue(m["supports_audio_output"]); v != nil && *v {
				item.Modalities.Output = append(item.Modalities.Output, "audio")
			}
		}
		currency := str(m["currency"])
		if currency == "" {
			currency = "USD"
		}
		unit := int64(1000000)
		if n := integer(m["unit_tokens"]); n != nil {
			unit = *n
		}
		for _, tier := range []string{"", "batches", "flex", "priority", "ultrafast", "balanced"} {
			suffix := ""
			if tier != "" {
				suffix = "_" + tier
			}
			table, exists := liteTable(m, suffix, currency, unit)
			if tier != "" && !exists {
				continue
			}
			name := tier
			if name == "" {
				name = "standard"
			}
			item.Offerings = append(item.Offerings, store.CatalogOffering{Key: item.Provider + "/" + name, Provider: item.Provider, ServiceTier: name, SourceKind: "third_party", Quotes: []store.CatalogQuote{{Key: strings.ToLower(currency) + "-token", Table: table, SourceKind: "third_party", SourceURL: liteLLMURL, Notes: append([]string{"LiteLLM-maintained metadata; not independently verified first-party pricing."}, liteAdditionalFeeNotes(m)...)}}})
		}
		out = append(out, item)
	}
	return ordered(out), nil
}
func liteTable(m rawObject, suffix, currency string, unit int64) (store.PriceTable, bool) {
	t := store.PriceTable{Currency: currency, UnitTokens: unit, Rules: []store.PriceRule{}}
	exists := false
	rates := map[string]*float64{}
	for key, role := range liteRateKeys {
		if raw, ok := m[key+suffix]; ok {
			rates[role] = scaled(raw, float64(unit))
			exists = true
		}
	}
	if _, present := m["cache_read_input_token_cost"+suffix]; !present {
		if raw, ok := m["input_cost_per_token_cache_hit"+suffix]; ok {
			rates["cache_read"] = scaled(raw, float64(unit))
			exists = true
		}
	}
	t.Rates = store.PriceRates{Input: rates["input"], Output: rates["output"], CacheRead: rates["cache_read"], CacheWrite: rates["cache_write"]}
	thresholds := map[int64]map[string]*float64{}
	for key, role := range liteRateKeys {
		prefix := key + "_above_"
		for field, raw := range m {
			if !strings.HasPrefix(field, prefix) {
				continue
			}
			tail := strings.TrimPrefix(field, prefix)
			if suffix != "" {
				if !strings.HasSuffix(tail, suffix) {
					continue
				}
				tail = strings.TrimSuffix(tail, suffix)
			}
			if !strings.HasSuffix(tail, "k_tokens") {
				continue
			}
			n, e := strconv.ParseInt(strings.TrimSuffix(tail, "k_tokens"), 10, 64)
			if e != nil || n <= 0 || n > 1000000000 {
				continue
			}
			from := n*1000 + 1
			if thresholds[from] == nil {
				thresholds[from] = map[string]*float64{}
			}
			thresholds[from][role] = scaled(raw, float64(unit))
		}
	}
	starts := []int64{}
	for n := range thresholds {
		starts = append(starts, n)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	for _, n := range starts {
		t.Rules = append(t.Rules, store.PriceRule{ID: fmt.Sprintf("input-gt-%d", n-1), Enabled: true, When: store.PriceRuleWhen{InputRange: &store.PriceInputRange{FromTokens: n}}, Action: store.PriceRuleAction{RateChanges: thresholds[n]}})
	}
	if suffix == "" {
		if off := object(m["off_peak_pricing"]); off != nil {
			addOffPeak(&t, off)
		}
	}
	for key := range m {
		if strings.Contains(key, "cost") && !supportedLiteRateField(key) && !liteAdditionalFee(key) {
			t.UncoveredConditions = append(t.UncoveredConditions, "Uncovered source rate: "+key)
		}
		if strings.HasPrefix(key, "regional_processing_uplift_") {
			t.UncoveredConditions = append(t.UncoveredConditions, "Uncovered regional selector: "+key)
		}
	}
	if currency != "USD" && currency != "CNY" {
		t.UncoveredConditions = append(t.UncoveredConditions, "Unsupported currency "+currency)
	}
	sort.Strings(t.UncoveredConditions)
	return t, exists
}
func liteAdditionalFee(key string) bool {
	return strings.Contains(key, "per_session") || strings.Contains(key, "per_query") || strings.Contains(key, "per_call") || strings.Contains(key, "per_request") || strings.Contains(key, "per_gb") || strings.Contains(key, "storage")
}
func liteAdditionalFeeNotes(m rawObject) []string {
	notes := []string{}
	for key := range m {
		if strings.Contains(key, "cost") && liteAdditionalFee(key) {
			notes = append(notes, "Additional source fee "+key+" is outside the four token categories.")
		}
	}
	sort.Strings(notes)
	return notes
}

func supportedLiteRateField(key string) bool {
	for _, suffix := range []string{"", "_batches", "_flex", "_priority", "_ultrafast", "_balanced"} {
		candidate := key
		if suffix != "" {
			if !strings.HasSuffix(candidate, suffix) {
				continue
			}
			candidate = strings.TrimSuffix(candidate, suffix)
		}
		if candidate == "input_cost_per_token_cache_hit" {
			return true
		}
		for base := range liteRateKeys {
			if candidate == base {
				return true
			}
			tail := strings.TrimPrefix(candidate, base+"_above_")
			if tail == candidate || !strings.HasSuffix(tail, "k_tokens") {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimSuffix(tail, "k_tokens"), 10, 64)
			if err == nil && n > 0 && n <= 1000000000 {
				return true
			}
		}
	}
	return false
}
func addOffPeak(t *store.PriceTable, off rawObject) {
	changes := map[string]*float64{}
	for key, role := range liteRateKeys {
		if raw, ok := off[key]; ok {
			changes[role] = scaled(raw, float64(t.UnitTokens))
		}
	}
	if len(changes) == 0 {
		t.UncoveredConditions = append(t.UncoveredConditions, "Off-peak schedule lacks independently specified token rates")
		return
	}
	var windows []rawObject
	_ = json.Unmarshal(off["windows"], &windows)
	if len(windows) == 0 {
		windows = []rawObject{off}
	}
	for _, w := range windows {
		for field := range w {
			if field != "hours_utc" && field != "weekdays" && field != "windows" {
				if _, rate := liteRateKeys[field]; !rate {
					t.UncoveredConditions = append(t.UncoveredConditions, "Uncovered off-peak condition: "+field)
				}
			}
		}
		hours := stringsValue(w["hours_utc"])
		if h := str(w["hours_utc"]); h != "" {
			hours = []string{h}
		}
		days := parseWeekdays(w["weekdays"])
		if len(w["weekdays"]) > 0 && len(days) == 0 {
			t.UncoveredConditions = append(t.UncoveredConditions, "Unrecognized off-peak weekdays")
			continue
		}
		if len(hours) == 0 {
			t.UncoveredConditions = append(t.UncoveredConditions, "Off-peak rates lack supported UTC window")
			continue
		}
		for _, h := range hours {
			pair := strings.Split(h, "-")
			if len(pair) != 2 {
				t.UncoveredConditions = append(t.UncoveredConditions, "Unrecognized off-peak window "+h)
				continue
			}
			if _, e := time.Parse("15:04", pair[0]); e != nil {
				t.UncoveredConditions = append(t.UncoveredConditions, "Unrecognized off-peak window "+h)
				continue
			}
			if _, e := time.Parse("15:04", pair[1]); e != nil {
				t.UncoveredConditions = append(t.UncoveredConditions, "Unrecognized off-peak window "+h)
				continue
			}
			t.Rules = append(t.Rules, store.PriceRule{ID: "off-peak-" + h + "-" + fmt.Sprint(days), Enabled: true, When: store.PriceRuleWhen{TimeWindow: &store.PriceTimeWindow{Start: pair[0], End: pair[1], AllDay: pair[0] == pair[1], Timezone: "UTC"}, Weekdays: days, Timezone: "UTC"}, Action: store.PriceRuleAction{RateChanges: changes}})
		}
	}
}
func parseWeekdays(raw json.RawMessage) []int {
	var ints []int
	if json.Unmarshal(raw, &ints) == nil {
		for _, n := range ints {
			if n < 1 || n > 7 {
				return nil
			}
		}
		return ints
	}
	names := stringsValue(raw)
	out := []int{}
	lookup := map[string]int{"monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6, "sunday": 7}
	for _, name := range names {
		n := lookup[strings.ToLower(name)]
		if n == 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}
