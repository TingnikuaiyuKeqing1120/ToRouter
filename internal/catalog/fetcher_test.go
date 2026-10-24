package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestOpenRouterPreservesUnknownPartialPriceAndReasoning(t *testing.T) {
	items, err := ParseOpenRouter([]byte(`{"data":[{"id":"author/model","name":"Model","context_length":0,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"top_provider":{"max_completion_tokens":null},"pricing":{"prompt":"0.000000000017","completion":null,"input_cache_read":"0"},"reasoning":{"supported_efforts":["low","max"],"default_effort":"low"}}]}`), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	m := items[0]
	if err := store.ValidateModelConfig(store.ModelConfig{NativeThinking: &m.Thinking}); err != nil {
		t.Fatalf("external thinking must be valid for prefill: %v", err)
	}
	q := m.Offerings[0].Quotes[0]
	if m.ContextLength != nil || m.MaxInputTokens != nil || m.MaxOutputTokens != nil || m.Defaults != nil {
		t.Fatal("unknown limits or output budget were invented")
	}
	if q.Table.Rates.Output != nil || q.Table.Rates.CacheWrite != nil || q.Table.Rates.CacheRead == nil || *q.Table.Rates.CacheRead != 0 {
		t.Fatalf("partial price or free cache lost: %+v", q.Table.Rates)
	}
	if q.Table.Rates.Input == nil || *q.Table.Rates.Input <= 0 || *q.Table.Rates.Input >= .0001 {
		t.Fatal("small positive rate was rounded")
	}
	if q.SourceKind != "aggregator_summary" || m.Thinking.Supported == nil || !*m.Thinking.Supported || !reflect.DeepEqual(m.Thinking.Modes[0].Efforts, []string{"low", "max"}) {
		t.Fatal("summary or native reasoning semantics lost")
	}
}

func TestOpenRouterReasoningControlsUseDeclaredMetadata(t *testing.T) {
	items, err := ParseOpenRouter([]byte(`{"data":[{"id":"author/optional","reasoning":{"mandatory":false,"default_enabled":true,"supported_efforts":["low","high"],"default_effort":"low","supports_max_tokens":true}},{"id":"author/mandatory","reasoning":{"mandatory":true,"supported_efforts":["high"]}},{"id":"author/unknown","supported_parameters":["reasoning"]}]}`), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.CatalogItem{}
	for _, item := range items {
		byID[item.ID] = item
		if err := store.ValidateModelConfig(store.ModelConfig{NativeThinking: &item.Thinking}); err != nil {
			t.Fatalf("metadata cannot be prefilled: %s: %v", item.ID, err)
		}
	}
	optional := byID["author/optional"].Thinking
	if optional.Mandatory == nil || *optional.Mandatory || len(optional.Modes) != 3 || optional.Modes[0].DefaultEffort == nil || *optional.Modes[0].DefaultEffort != "low" {
		t.Fatalf("optional/default/budget metadata lost: %+v", optional)
	}
	mandatory := byID["author/mandatory"].Thinking
	if mandatory.Mandatory == nil || !*mandatory.Mandatory || len(mandatory.Modes) != 2 {
		t.Fatalf("mandatory metadata lost: %+v", mandatory)
	}
	unknown := byID["author/unknown"].Thinking
	if unknown.Mandatory != nil || len(unknown.Modes) != 1 || unknown.Modes[0].Efforts != nil || unknown.Modes[0].DefaultEffort != nil {
		t.Fatalf("unknown controls invented: %+v", unknown)
	}
}

func TestOpenRouterExactOfferingIdentityAndThreshold(t *testing.T) {
	b := []byte(`{"data":{"endpoints":[{"provider_name":"Azure","tag":"azure/eu","pricing":{"prompt":"0.0000022","completion":"0.000011"}},{"provider_name":"OpenAI","tag":"openai/flex","pricing":{"prompt":"0.000001","completion":"0.000005","overrides":[{"min_prompt_tokens":272000,"prompt":"0.000002","completion":null}]}},{"provider_name":"OpenAI","tag":"openai","pricing":{"prompt":"0.000002","completion":"0.00001"}}]}}`)
	offerings, err := ParseOpenRouterEndpoints(b, "https://openrouter.ai/api/v1/models/openai/model/endpoints")
	if err != nil {
		t.Fatal(err)
	}
	if len(offerings) != 3 || offerings[0].Key != "azure/eu" || offerings[2].Key != "openai/flex" {
		t.Fatalf("different scopes collapsed: %+v", offerings)
	}
	flex := offerings[2].Quotes[0]
	r := flex.Table.Rules[0]
	if r.When.InputRange.FromTokens != 272000 || r.Action.RateChanges["output"] != nil || *r.Action.RateChanges["input"] != 2 || flex.SourceKind != "hosted" {
		t.Fatal("exact threshold, explicit unknown override or hosted identity lost")
	}
	var p map[string]any
	_ = json.Unmarshal(b, &p)
	data := p["data"].(map[string]any)
	endpoints := data["endpoints"].([]any)
	data["endpoints"] = append(endpoints, endpoints[0])
	dup, _ := json.Marshal(p)
	if _, err := ParseOpenRouterEndpoints(dup, ""); err == nil {
		t.Fatal("ambiguous endpoint identities were silently accepted")
	}
}

func TestLiteLLMIndependentLimitsCurrencyCacheAndOrderedTiers(t *testing.T) {
	items, err := ParseLiteLLM([]byte(`{"model":{"mode":"chat","litellm_provider":"vendor","max_tokens":4096,"max_input_tokens":100000,"max_output_tokens":16384,"currency":"CNY","unit_tokens":1000,"input_cost_per_token":0.000002,"cache_read_input_token_cost":0,"cache_creation_input_token_cost":null,"supports_reasoning":false,"input_cost_per_token_above_128k_tokens":0.000004,"output_cost_per_token_above_200k_tokens":0.00001,"input_cost_per_token_priority":0.000008},"unknown":{"mode":"chat","max_tokens":9999},"image":{"mode":"image_generation","max_tokens":100},"sample_spec":{"mode":"chat"}}`), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("non-chat/sample rows leaked: %d", len(items))
	}
	m := items[0]
	q := m.Offerings[0].Quotes[0]
	if m.ContextLength != nil || *m.MaxInputTokens != 100000 || *m.MaxOutputTokens != 16384 || m.Thinking.Supported == nil || *m.Thinking.Supported {
		t.Fatal("legacy max_tokens fabricated a context window or boolean unknown lost")
	}
	if q.Table.Currency != "CNY" || q.Table.UnitTokens != 1000 || *q.Table.Rates.Input != .002 || q.Table.Rates.Output != nil || q.Table.Rates.CacheWrite != nil || *q.Table.Rates.CacheRead != 0 {
		t.Fatalf("currency, unit, null or partial rates lost: %+v", q)
	}
	if len(q.Table.Rules) != 2 || q.Table.Rules[0].When.InputRange.FromTokens != 128001 || q.Table.Rules[1].When.InputRange.FromTokens != 200001 {
		t.Fatal("strict thresholds or numerical tier order lost")
	}
	if len(m.Offerings) != 2 || m.Offerings[1].Key != "vendor/priority" || *m.Offerings[1].Quotes[0].Table.Rates.Input != .008 {
		t.Fatal("service tier conflated")
	}
	u := items[1]
	if u.ContextLength != nil || u.MaxInputTokens != nil || u.MaxOutputTokens != nil || u.Thinking.Supported != nil {
		t.Fatal("legacy max_tokens or absent reasoning projected as known")
	}
}

func TestLiteLLMOffPeakShapesAndUncoveredRates(t *testing.T) {
	for _, fixture := range []string{
		`{"input_cost_per_token":0.000001,"windows":[{"hours_utc":["00:00-01:00","10:00-00:00"],"weekdays":[1,2,3,4,5]},{"hours_utc":"00:00-00:00","weekdays":[6,7]}]}`,
		`{"input_cost_per_token":0.000001,"windows":[{"hours_utc":"00:00-01:00","weekdays":["monday","tuesday"]},{"hours_utc":"10:00-00:00","weekdays":["friday"]},{"hours_utc":"00:00-00:00","weekdays":["saturday","sunday"]}]}`,
	} {
		body := []byte(`{"model":{"mode":"chat","input_cost_per_token":0.000002,"output_cost_per_token":0.000004,"cache_creation_input_token_cost_above_1hr":0.000005,"off_peak_pricing":` + fixture + `}}`)
		items, err := ParseLiteLLM(body, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		table := items[0].Offerings[0].Quotes[0].Table
		if len(table.Rules) != 3 || !table.Rules[2].When.TimeWindow.AllDay || table.Rules[2].When.TimeWindow.Timezone != "UTC" || len(table.UncoveredConditions) == 0 {
			t.Fatalf("off-peak shape lost: %+v", table)
		}
		if err := store.ValidatePriceTable(table); err != nil {
			t.Fatal(err)
		}
	}
	items, err := ParseLiteLLM([]byte(`{"model":{"mode":"chat","off_peak_pricing":{"hours_utc":"16:00-00:00","input_cost_per_token":0.000001}}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Offerings[0].Quotes[0].Table.Rules[0].When.TimeWindow.Start != "16:00" {
		t.Fatal("single off-peak object lost")
	}
}

func TestBuiltinCatalogSchemaAndQuoteIntegrity(t *testing.T) {
	body, err := os.ReadFile("../store/builtin_models.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var items []store.CatalogItem
	if err := dec.Decode(&items); err != nil {
		t.Fatal(err)
	}
	models := map[string]bool{}
	providers := map[string]bool{}
	for _, m := range items {
		if m.Source != "builtin" || m.ID == "" || m.DocURL == "" || m.VerifiedAt == "" || models[m.ID] {
			t.Fatalf("invalid model identity/provenance: %s", m.ID)
		}
		models[m.ID] = true
		providers[m.Provider] = true
		if m.Defaults != nil && m.Defaults.MaxOutputTokens != nil {
			t.Fatal("capability cap entered request defaults")
		}
		seen := map[string]bool{}
		for _, o := range m.Offerings {
			if seen[o.Key] || o.Key == "" {
				t.Fatalf("duplicate/empty offering %s/%s", m.ID, o.Key)
			}
			seen[o.Key] = true
			quotes := map[string]bool{}
			for _, q := range o.Quotes {
				if quotes[q.Key] || q.SourceKind != "original" || q.SourceURL == "" || q.VerifiedAt == "" {
					t.Fatalf("invalid quote provenance %s/%s", m.ID, q.Key)
				}
				quotes[q.Key] = true
				if err := store.ValidatePriceTable(q.Table); err != nil {
					t.Fatalf("%s/%s: %v", m.ID, q.Key, err)
				}
			}
		}
	}
	if len(models) != 28 || len(providers) != 13 {
		t.Fatalf("selected model coverage lost: %d models/%d providers", len(models), len(providers))
	}
	for _, id := range []string{"gpt-6.1-sol", "gpt-5.6-terra", "kimi-k3", "kimi-k2.7-code", "MiniMax-M3", "muse-spark-1.3", "mistral-large-4", "step-5-preview", "step-3.7-flash"} {
		if !models[id] {
			t.Fatalf("missing exact current API id %s", id)
		}
	}
	for _, id := range []string{"mistral-medium-3-5", "mistral-small-2603", "muse-spark-1.2", "muse-spark-1.3-contributor", "glm-5.3-flashx", "kimi-k2.6", "MiniMax-M2.7", "MiniMax-M2.7-highspeed", "doubao-seed-2-1-pro-260915", "doubao-seed-2-1-lite-260915", "doubao-seed-2-1-turbo-260628"} {
		if models[id] {
			t.Fatalf("out-of-scope or superseded selected model %s", id)
		}
	}
}

// Opt-in live acceptance reads only public metadata, never requests generation.
func TestLivePublicMetadata(t *testing.T) {
	if os.Getenv("TOROUTER_CATALOG_LIVE") != "1" {
		t.Skip("set TOROUTER_CATALOG_LIVE=1 for public metadata acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	items, err := FetchOpenRouter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("empty live directory")
	}
	t.Logf("OpenRouter directory: %d models", len(items))
	offerings, err := FetchOpenRouterEndpoints(ctx, "openai/gpt-6.1-sol")
	if err != nil {
		t.Fatal(err)
	}
	if len(offerings) == 0 {
		t.Fatal("no live offerings")
	}
	for _, o := range offerings {
		if o.SourceKind != "hosted" || strings.TrimSpace(o.Key) == "" {
			t.Fatal("live scope identity lost")
		}
	}
	t.Logf("targeted OpenRouter GPT-6.1 Sol: %d offerings", len(offerings))
	items, err = FetchLiteLLM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("empty LiteLLM dictionary")
	}
	for _, m := range items {
		if m.ID == "gpt-6.1-sol" && m.ContextLength != nil {
			t.Fatal("live legacy max_tokens interpreted as total window")
		}
	}
	t.Logf("LiteLLM chat/completion metadata: %d models", len(items))
}
