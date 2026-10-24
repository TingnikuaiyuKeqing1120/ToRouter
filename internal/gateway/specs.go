package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
	"torouter/internal/auth"
	"torouter/internal/pricing"
	route "torouter/internal/router"
	"torouter/internal/store"
)

var specFormats = []string{store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent}

// Channels and private endpoint/credential data never enter this projection.
type PublicPrice struct {
	Status        string                 `json:"status"`
	MissingReason string                 `json:"missing_reason,omitempty"`
	Layer         string                 `json:"layer"`
	Model         string                 `json:"model"`
	Table         *store.PriceTable      `json:"table"`
	Source        *store.CatalogPriceRef `json:"source"`
	FetchedAt     *time.Time             `json:"fetched_at"`
}

func publicPrice(r store.PriceResolution) PublicPrice {
	if r.Layer == "plan" {
		r.MissingReason = "plan_unallocated"
	}
	return PublicPrice{r.Status, r.MissingReason, r.Layer, r.Model, r.Table, r.Ref, r.FetchedAt}
}

type SpecBinding struct {
	Order        int                          `json:"order"`
	Name         string                       `json:"name"`
	Model        string                       `json:"model"`
	Formats      []string                     `json:"formats"`
	Capabilities map[string]any               `json:"capabilities"`
	Sources      map[string]string            `json:"sources"`
	Defaults     map[string]any               `json:"defaults"`
	Inapplicable map[string]map[string]string `json:"inapplicable"`
	PAYG         PublicPrice                  `json:"payg"`
}
type SpecDifference struct {
	Field    string           `json:"field"`
	Bindings []map[string]any `json:"bindings"`
	Declared any              `json:"declared"`
}
type ModelSpecItem struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	ContextLength   any                    `json:"context_length"`
	MaxInputTokens  any                    `json:"max_input_tokens"`
	MaxOutputTokens any                    `json:"max_output_tokens"`
	OutputLimit     any                    `json:"output_limit"`
	Streaming       any                    `json:"streaming"`
	Modalities      map[string]any         `json:"modalities"`
	Thinking        any                    `json:"thinking"`
	Sources         map[string]string      `json:"sources"`
	Differences     []SpecDifference       `json:"differences"`
	Warnings        []string               `json:"warnings"`
	Recommendation  string                 `json:"recommendation"`
	Bindings        []SpecBinding          `json:"bindings"`
	Pricing         map[string]PublicPrice `json:"pricing"`
}
type ModelSpecsResponse struct {
	Object string          `json:"object"`
	Data   []ModelSpecItem `json:"data"`
}

func (g *Gateway) ModelSpecs(w http.ResponseWriter, r *http.Request) {
	g.modelSpecs(w, r, nil)
}

func (g *Gateway) ClientModelSpecs(w http.ResponseWriter, r *http.Request) {
	key, ok := auth.ClientFromContextOK(r.Context())
	if !ok {
		writeError(w, "openai", 401, "client API key required")
		return
	}
	if g.Store != nil && !g.Store.VerifyClientKey(key) {
		writeError(w, "openai", 401, "client API key revoked or configuration changed")
		return
	}
	scope, err := g.scopeSnapshot(key.Scope)
	if err != nil {
		writeError(w, "openai", 500, "cannot read access scope")
		return
	}
	g.modelSpecs(w, r, &scope)
}

func (g *Gateway) modelSpecs(w http.ResponseWriter, r *http.Request, scope *requestScope) {
	format := r.URL.Query().Get("api_format")
	if format != "" && !slices.Contains(specFormats, format) {
		writeError(w, "openai", 400, "invalid api_format")
		return
	}
	result, err := g.readModelSpecs(format, scope)
	if err != nil {
		writeError(w, "openai", 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// ReadModelSpecs shares the exact management projection with the Web endpoint.
func (g *Gateway) ReadModelSpecs(format string) (ModelSpecsResponse, error) {
	return g.readModelSpecs(format, nil)
}
func (g *Gateway) readModelSpecs(format string, scope *requestScope) (ModelSpecsResponse, error) {
	if g.pluginErr != nil {
		return ModelSpecsResponse{}, errors.New("cannot load plugin preferences")
	}
	if format != "" && !slices.Contains(specFormats, format) {
		return ModelSpecsResponse{}, errors.New("invalid api_format")
	}
	models, err := g.Store.ListDownstreamModels()
	if err != nil {
		return ModelSpecsResponse{}, errors.New("cannot read models")
	}
	channels, err := g.Store.ListChannels()
	if err != nil {
		return ModelSpecsResponse{}, errors.New("cannot read channels")
	}
	if scope != nil {
		models = scope.filterModels(models, channels)
	}
	billingScope := scope
	if billingScope == nil {
		// Management keeps all bindings, but uses the same final billing tags
		// as client discovery and live requests when projecting current prices.
		snapshot, err := g.scopeSnapshot(store.DefaultClientKeyScope())
		if err != nil {
			return ModelSpecsResponse{}, errors.New("cannot read billing tags")
		}
		billingScope = &snapshot
	}
	settings, err := g.Store.GetSettings()
	if err != nil {
		return ModelSpecsResponse{}, errors.New("cannot read settings")
	}
	items, err := g.Store.ListCatalogItems("", "")
	if err != nil {
		return ModelSpecsResponse{}, errors.New("cannot read catalog")
	}
	byID := map[int64]store.Channel{}
	for _, c := range channels {
		byID[c.ID] = c
	}
	data := []ModelSpecItem{}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		bindings := []SpecBinding{}
		ordered := slices.Clone(m.Bindings)
		slices.SortStableFunc(ordered, func(a, b store.Binding) int {
			if a.Priority < b.Priority {
				return -1
			}
			if a.Priority > b.Priority {
				return 1
			}
			return 0
		})
		for _, b := range ordered {
			c, ok := byID[b.ChannelID]
			if !b.Enabled || !ok || !c.Enabled || !g.pluginEnabled(c) {
				continue
			}
			if scope != nil && route.BindingStructureReason(c, b) != "" {
				continue
			}
			formats := []string{}
			for _, f := range specFormats {
				if _, ok := route.PickModelEndpoint(c, b.Model, f); ok {
					formats = append(formats, f)
				}
			}
			if len(formats) == 0 || format != "" && !slices.Contains(formats, format) {
				continue
			}
			if format != "" {
				formats = []string{format}
			}
			resolved := store.ResolveConfig(c.ModelConfigs[b.Model])
			defs := map[string]any{}
			inapp := map[string]map[string]string{}
			if settings.RequestDefaultsEnabled {
				for _, f := range formats {
					ep, _ := route.PickModelEndpoint(c, b.Model, f)
					defs[f], inapp[f] = specDefaults(resolved.Config, &settings.RequestDefaults, ep.Format, c.CredentialType == store.CredentialChatGPTOAuth)
				}
			}

			actual, _ := pricing.ResolveRequestPrices(settings.Prices, items, c, billingScope.tags, billingScope.groups, b.Model, m.ID)
			bindings = append(bindings, SpecBinding{len(bindings), c.Name, b.Model, formats, capabilities(resolved.Config), resolved.Sources, defs, inapp, publicPrice(actual)})
		}
		if scope != nil && len(bindings) == 0 {
			continue
		}
		name := m.DisplayName
		if strings.TrimSpace(name) == "" {
			name = m.ID
		}
		values, sources, diffs := consistentSpecs(bindings)
		warnings := []string{}
		recommendation := ""
		if len(diffs) > 0 {
			warnings = append(warnings, "bindings have differing or unknown specifications")
			recommendation = "Split differing specifications into separate public model IDs; complete unknown upstream model specifications."
		}
		_, official := pricing.ResolveUsagePrices(settings.Prices, items, 0, "", m.ID)
		data = append(data, ModelSpecItem{m.ID, name, values["context_length"], values["max_input_tokens"], values["max_output_tokens"], values["output_limit"], values["streaming"], map[string]any{"input": values["input_modalities"], "output": values["output_modalities"]}, map[string]any{"supported": values["thinking_supported"], "mandatory": values["thinking_mandatory"], "modes": values["thinking_modes"], "options": values["thinking_options"]}, sources, diffs, warnings, recommendation, bindings, map[string]PublicPrice{"official": publicPrice(official)}})
	}
	return ModelSpecsResponse{"list", data}, nil
}

func nullable(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		return rv.Elem().Interface()
	}
	return v
}
func capabilities(c store.ModelConfig) map[string]any {
	var supported, mandatory, modes, options any
	if c.NativeThinking != nil {
		supported = nullable(c.NativeThinking.Supported)
		mandatory = nullable(c.NativeThinking.Mandatory)
		if c.NativeThinking.Modes != nil {
			modes = c.NativeThinking.Modes
		}
	}
	if values, known := store.ThinkingOptions(c); known {
		options = values
	}
	return map[string]any{"context_length": nullable(c.ContextWindow), "max_input_tokens": nullable(c.MaxInputTokens), "max_output_tokens": nullable(c.MaxOutputTokens), "output_limit": nullable(c.OutputLimit), "streaming": nullable(c.Streaming), "input_modalities": nullable(c.InputModalities), "output_modalities": nullable(c.OutputModalities), "thinking_supported": supported, "thinking_mandatory": mandatory, "thinking_modes": modes, "thinking_options": options}
}
func equalSpec(a, b any) bool {
	if aa, ok := a.([]string); ok {
		bb, ok := b.([]string)
		if !ok {
			return false
		}
		aa = slices.Clone(aa)
		bb = slices.Clone(bb)
		slices.Sort(aa)
		slices.Sort(bb)
		return slices.Equal(aa, bb)
	}
	return reflect.DeepEqual(a, b)
}
func consistentSpecs(bindings []SpecBinding) (map[string]any, map[string]string, []SpecDifference) {
	values := capabilities(store.ModelConfig{})
	sources := map[string]string{}
	diffs := []SpecDifference{}
	for field := range values {
		var first any
		consistent := len(bindings) > 0
		entries := []map[string]any{}
		for i, b := range bindings {
			v := b.Capabilities[field]
			entries = append(entries, map[string]any{"name": b.Name, "model": b.Model, "known": v != nil, "value": v})
			if i == 0 {
				first = v
			}
			if v == nil || !equalSpec(first, v) {
				consistent = false
			}
		}
		if consistent {
			values[field] = first
			sources[field] = "consistent_bindings"
		} else {
			sources[field] = "unknown"
		}
		mismatch := !consistent
		if mismatch && len(bindings) > 0 {
			diffs = append(diffs, SpecDifference{field, entries, nil})
		}
	}
	slices.SortFunc(diffs, func(a, b SpecDifference) int { return strings.Compare(a.Field, b.Field) })
	return values, sources, diffs
}
func specDefaults(cfg store.ModelConfig, d *store.ModelDefaults, format string, siwc bool) (any, map[string]string) {
	applied := map[string]any{}
	why := map[string]string{}
	if d == nil {
		return applied, why
	}
	if d.MaxOutputTokens != nil {
		if *d.MaxOutputTokens == 0 {
			why["max_output_tokens"] = "disabled"
		} else if siwc {
			why["max_output_tokens"] = "siwc"
		} else if outputKey(format, cfg) == "" {
			why["max_output_tokens"] = "unknown_field"
		} else {
			applied["max_output_tokens"] = *d.MaxOutputTokens
		}
	}
	if d.Temperature != nil {
		if siwc {
			why["temperature"] = "siwc"
		} else if format == store.FormatAnthropicMessages && *d.Temperature > 1 {
			why["temperature"] = "outside_native_range"
		} else {
			applied["temperature"] = *d.Temperature
		}
	}
	if d.Thinking != nil {
		normalized, _, reason := normalizeThinking(d.Thinking, cfg.NativeThinking, format)
		if reason != "" {
			why["thinking"] = reason
		} else {
			applied["thinking"] = normalized
		}
	}
	return applied, why
}
