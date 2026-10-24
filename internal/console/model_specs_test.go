package console

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"torouter/internal/auth"
	"torouter/internal/gateway"
	"torouter/internal/store"
)

func TestModelSpecsDedicatedSaveAndConnectionIsolation(t *testing.T) {
	s, h := newTestCatalogConsole(t)
	defer s.Close()
	c := catalogHTTPChannel(t, s, "one")
	c.Models = append(c.Models, "two")
	c.ModelConfigs = map[string]store.ModelConfig{"two": {ContextWindow: ptr(int64(99))}}
	if err := s.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	prices := []store.Price{{ChannelID: c.ID, Model: "one", Selection: store.PriceSelection{Mode: "manual", Table: &store.PriceTable{Currency: "USD", UnitTokens: 1000, Rates: store.PriceRates{Input: ptr(0.0)}}}}}
	if err := s.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	prices, _ = s.GetPrices()
	original, _ := s.Channel(c.ID)
	row := store.ModelSpec{ChannelID: c.ID, Model: "one", Specs: store.ModelConfig{Streaming: ptr(false), MaxOutputTokens: ptr(int64(0)), InputModalities: ptr([]string{})}, CatalogModelRef: &store.CatalogModelRef{Source: "builtin", ID: "gpt-4o"}}
	code, obj, _ := call(t, h, "PUT", "/model-specs", catalogJSON(t, row))
	if code != 200 {
		t.Fatal(code, obj)
	}
	saved, _ := s.Channel(c.ID)
	if saved.APIKey != original.APIKey || !reflect.DeepEqual(saved.Endpoints, original.Endpoints) || saved.Generation != original.Generation || !reflect.DeepEqual(saved.ModelConfigs["two"], original.ModelConfigs["two"]) {
		t.Fatal("spec save changed connection or other model")
	}
	gotPrices, _ := s.GetPrices()
	if !reflect.DeepEqual(gotPrices, prices) {
		t.Fatal("spec save changed prices", gotPrices)
	}
	original.Name = "Renamed connection"
	original.ModelConfigs = nil
	original.ModelCatalogRefs = nil
	if err := s.SaveChannelConfig(&original); err != nil {
		t.Fatal(err)
	}
	saved, _ = s.Channel(c.ID)
	if !reflect.DeepEqual(saved.ModelConfigs["one"], row.Specs) || !reflect.DeepEqual(saved.ModelConfigs["two"], c.ModelConfigs["two"]) {
		t.Fatal("connection save cleared dedicated specs")
	}
	if saved.ModelCatalogRefs["one"].ID != "gpt-4o" {
		t.Fatal("connection save cleared model reference")
	}
	code, _, list := call(t, h, "GET", "/model-specs", "")
	if code != 200 || len(list) != 2 {
		t.Fatal(code, list)
	}
	raw := catalogJSON(t, list)
	if strings.Contains(raw, "api_key") || strings.Contains(raw, "credential-stays") || strings.Contains(raw, "base_url") {
		t.Fatal("spec list leaked connection data")
	}
	code, _, _ = call(t, h, "PUT", "/model-specs", `{"channel_id":1,"model":"one","specs":{"defaults":{"max_output_tokens":8}}}`)
	if code != 400 {
		t.Fatal("object defaults accepted", code)
	}
	code, _, _ = call(t, h, "PUT", "/model-specs", `{"channel_id":1,"model":"missing","specs":{}}`)
	if code != 400 {
		t.Fatal("unlisted model accepted", code)
	}
}
func TestConsoleAndClientPublicSpecsShareProjection(t *testing.T) {
	s, h := newTestCatalogConsole(t)
	defer s.Close()
	c := catalogHTTPChannel(t, s, "one")
	if err := s.SaveModelSpecs(store.ModelSpec{ChannelID: c.ID, Model: "one", Specs: store.ModelConfig{Streaming: ptr(false), MaxOutputTokens: ptr(int64(0)), InputModalities: ptr([]string{})}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "one", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	management := httptest.NewRecorder()
	h.ServeHTTP(management, httptest.NewRequest("GET", "/models/specs", nil))
	client := httptest.NewRecorder()
	clientRequest := httptest.NewRequest("GET", "/v1/models/specs", nil)
	clientRequest = clientRequest.WithContext(auth.WithClientContext(clientRequest.Context(), store.ClientKey{Scope: store.DefaultClientKeyScope()}))
	gateway.New(s).ServeHTTP(client, clientRequest)
	var a, b any
	_ = json.Unmarshal(management.Body.Bytes(), &a)
	_ = json.Unmarshal(client.Body.Bytes(), &b)
	if management.Code != 200 || client.Code != 200 || !reflect.DeepEqual(a, b) {
		t.Fatal("public projections differ", management.Code, client.Code)
	}
	projection := a.(map[string]any)["data"].([]any)[0].(map[string]any)
	if projection["streaming"] != false || projection["max_output_tokens"] != float64(0) || projection["context_length"] != nil || len(projection["modalities"].(map[string]any)["input"].([]any)) != 0 {
		t.Fatal("zero, false, empty or unknown projected incorrectly", projection)
	}
	binding := projection["bindings"].([]any)[0].(map[string]any)
	if len(binding["defaults"].(map[string]any)) != 0 || len(binding["inapplicable"].(map[string]any)) != 0 {
		t.Fatal("disabled defaults projected as active", binding)
	}
	_, _, models := call(t, h, "GET", "/models", "")
	if len(models) != 1 {
		t.Fatal(models)
	}
	public := models[0].(map[string]any)
	if _, exists := public["config"]; exists {
		t.Fatal("public model still exposes editable capabilities")
	}
	if _, exists := public["published_specs"]; exists {
		t.Fatal("public declaration still exposed")
	}
}
