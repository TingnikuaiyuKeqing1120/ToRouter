package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestPublicModelTagsNormalizeCRUDImportAndProjection(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	if err := s.SaveTagGroup(store.TagGroup{ID: "scenario", Name: "Scenario", Type: store.TagGroupTypeUser}); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []store.Tag{
		{ID: "scenario:daily", GroupID: "scenario", Name: "Daily"},
		{ID: "scenario:channel", GroupID: "scenario", Name: "Channel"},
		{ID: "scenario:upstream", GroupID: "scenario", Name: "Upstream"},
		{ID: "looks-custom", GroupID: "privacy", Name: "Builtin group"},
	} {
		if err := s.SaveTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	input, output := []string{"image"}, []string{"text"}
	c := store.Channel{Name: "provider", APIKey: "test-key", Enabled: true, Models: []string{"native"},
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "http://localhost"}},
		Tags:      []string{"billing:free", "scenario:channel"}, ModelTags: map[string][]string{"native": {"privacy:zdr", "scenario:upstream", "modality:input:audio"}},
		ModelConfigs: map[string]store.ModelConfig{"native": {InputModalities: &input, OutputModalities: &output}},
	}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	m := store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "native", Enabled: true}},
		Tags: []string{" scenario:daily ", "scenario:daily", "missing", "billing:free", "privacy:zdr", "looks-custom", "modality:input:audio"}, EffectiveTags: []string{"billing:free"}}
	stringsFromJSON := func(v any) []string {
		t.Helper()
		var result []string
		raw, _ := json.Marshal(v)
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertOwn := func(obj map[string]any) {
		t.Helper()
		if got := stringsFromJSON(obj["tags"]); !slices.Equal(got, []string{"scenario:daily"}) {
			t.Fatalf("raw public tags = %v", got)
		}
		if obj["effective_tags"] != nil {
			t.Fatalf("write echoed untrusted effective tags: %v", obj)
		}
	}
	for _, request := range []struct{ method, path, id string }{{"POST", "/models", "public"}, {"PUT", "/models/public", "public"}, {"POST", "/models/public/rename", "renamed"}} {
		m.ID = request.id
		body, _ := json.Marshal(m)
		code, obj, _ := call(t, h, request.method, request.path, string(body))
		if code != 200 {
			t.Fatalf("%s %s = %d: %v", request.method, request.path, code, obj)
		}
		assertOwn(obj)
	}
	m.ID = "imported"
	body, _ := json.Marshal(map[string]any{"mode": "overwrite", "models": []store.DownstreamModel{m}})
	if code, obj, _ := call(t, h, "POST", "/models/import", string(body)); code != 200 {
		t.Fatalf("import = %d: %v", code, obj)
	}
	code, _, models := call(t, h, "GET", "/models", "")
	if code != 200 || len(models) != 1 {
		t.Fatalf("list = %d: %v", code, models)
	}
	model := models[0].(map[string]any)
	if got := stringsFromJSON(model["tags"]); !slices.Equal(got, []string{"scenario:daily"}) {
		t.Fatalf("projected own tags = %v", got)
	}
	want := []string{"modality:input:image", "modality:output:text", "scenario:daily"}
	if got := stringsFromJSON(model["effective_tags"]); !slices.Equal(got, want) {
		t.Fatalf("effective tags = %v", got)
	}
	for _, raw := range model["effective_tag_details"].([]any) {
		detail := raw.(map[string]any)
		for _, source := range stringsFromJSON(detail["sources"]) {
			if strings.HasPrefix(source, "渠道:") || strings.HasPrefix(source, "上游模型:") {
				t.Fatalf("inherited tag source: %v", detail)
			}
		}
	}
	// Existing stored selections stop appearing when their actual group changes.
	if err := s.SaveTag(store.Tag{ID: "scenario:daily", GroupID: "privacy", Name: "Reassigned"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveModelSpecs(store.ModelSpec{ChannelID: c.ID, Model: "native", Specs: store.ModelConfig{}}); err != nil {
		t.Fatal(err)
	}
	code, _, models = call(t, h, "GET", "/models", "")
	if code != 200 || len(models) != 1 {
		t.Fatalf("list after metadata change = %d: %v", code, models)
	}
	model = models[0].(map[string]any)
	if len(stringsFromJSON(model["tags"])) != 0 || len(stringsFromJSON(model["effective_tags"])) != 0 {
		t.Fatalf("invalid old selection or unknown modality remained visible: %v", model)
	}
}

func ptr[T any](v T) *T { return &v }

func call(t *testing.T, h interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, method, path, body string) (int, map[string]any, []any) {
	t.Helper()
	w := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	h.ServeHTTP(w, r)
	var obj map[string]any
	var list []any
	_ = json.Unmarshal(w.Body.Bytes(), &obj)
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	return w.Code, obj, list
}

func TestModelsCRUDAndEffectiveSources(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	c := store.Channel{Name: "prov", Protocol: "openai", BaseURL: "http://localhost:9999", APIKey: "upstream-secret", Models: []string{"real"}, AvailableModels: []string{"off"}, Enabled: true}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(store.Settings{GatewayKey: "gateway-key-1234", RetentionDays: 30, MaxAttempts: 3, RequestDefaultsEnabled: true, RequestDefaults: store.ModelDefaults{MaxOutputTokens: ptr(int64(8192))}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveModelSpecs(store.ModelSpec{ChannelID: c.ID, Model: "real", Specs: store.ModelConfig{MaxOutputTokens: ptr(int64(0))}}); err != nil {
		t.Fatal(err)
	}
	code, _, _ := call(t, h, "POST", "/models", `{"id":"public","enabled":true,"fallback":true,"bindings":[{"channel_id":1,"model":"real","priority":0,"enabled":true}]}`)
	if code != 200 {
		t.Fatal("create failed", code)
	}
	if code, _, _ = call(t, h, "POST", "/models", `{"id":"bad","enabled":true,"bindings":[{"channel_id":1,"model":"ghost","priority":0,"enabled":true}]}`); code != 400 {
		t.Fatal("accepted unknown upstream model", code)
	}
	code, _, list := call(t, h, "GET", "/models", "")
	if code != 200 || len(list) != 1 {
		t.Fatalf("list failed %d %v", code, list)
	}
	parsed := []map[string]any{}
	raw, _ := json.Marshal(list)
	_ = json.Unmarshal(raw, &parsed)
	first := parsed[0]["bindings"].([]any)[0].(map[string]any)
	effective := first["effective"].(map[string]any)
	sources := effective["sources"].(map[string]any)
	if sources["max_output_tokens"] != "上游模型" {
		t.Fatalf("explicit zero should come from the concrete upstream model: %v", sources)
	}
	if _, ok := effective["config"]; !ok {
		t.Fatal("effective config missing")
	}
	if code, _, _ = call(t, h, "PUT", "/models/public", `{"id":"public","enabled":true,"bindings":[{"channel_id":1,"model":"real","priority":0,"enabled":true}]}`); code != 200 {
		t.Fatal("update failed", code)
	}
	if code, _, _ = call(t, h, "PUT", "/models/public", `{"id":"other","enabled":true,"bindings":[{"channel_id":1,"model":"real","priority":0,"enabled":true}]}`); code != 400 {
		t.Fatal("accepted mismatched ID")
	}
	if code, _, _ = call(t, h, "DELETE", "/models/public", ""); code != 200 {
		t.Fatal("delete failed", code)
	}
	if code, _, _ = call(t, h, "DELETE", "/models/public", ""); code != 404 {
		t.Fatal("second delete should 404", code)
	}
}

func TestDownstreamOnlyRoute(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	// A channel exists but no downstream model binds it.
	c := store.Channel{Name: "prov", Protocol: "openai", BaseURL: "http://localhost:9999", APIKey: "upstream-secret", Models: []string{"real"}, Enabled: true}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	// /v1/models belongs to the gateway, but its visibility rule is the same:
	// nothing is exposed without an enabled downstream model.
	rows, _ := s.ListDownstreamModels()
	if len(rows) != 0 {
		t.Fatal("expected empty model table")
	}
	_ = h
}

// Rule 2, downstream side: a channel fetch that drops a model from the
// catalogue leaves the models bound to it valid (the channel still has it
// enabled), and the owner hears about it when they re-enable the public model —
// without the save being refused. Rule 4: an enabled public model can be edited
// and deleted by hand just like a disabled one.
func TestReenabledModelReportsCatalogueGap(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	// "retired" is still enabled on the channel (a fetch must not take an
	// enabled model away) but the catalogue no longer lists it.
	c := store.Channel{Name: "prov", Protocol: "openai", BaseURL: "http://localhost:9999", APIKey: "upstream-secret", Enabled: true,
		Models: []string{"live", "retired"}, AvailableModels: []string{"live"}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	bindings := `"bindings":[{"channel_id":1,"model":"live","priority":0,"enabled":true},{"channel_id":1,"model":"retired","priority":1,"enabled":true}]`
	if code, _, _ := call(t, h, "POST", "/models", `{"id":"public","enabled":true,"fallback":true,`+bindings+`}`); code != 200 {
		t.Fatal("create failed", code)
	}
	// While enabled nothing is reported; the binding is still valid.
	if code, _, _ := call(t, h, "PUT", "/models/public", `{"id":"public","enabled":true,"fallback":true,`+bindings+`}`); code != 200 {
		t.Fatal("re-save an enabled model")
	}
	// Disable, then edit the disabled model: allowed, and silent.
	if code, _, _ := call(t, h, "PUT", "/models/public", `{"id":"public","enabled":false,"fallback":true,`+bindings+`}`); code != 200 {
		t.Fatal("disable failed", code)
	}
	if code, obj, _ := call(t, h, "PUT", "/models/public", `{"id":"public","enabled":false,"fallback":false,`+bindings+`}`); code != 200 {
		t.Fatal("edit a disabled model")
	} else if obj["notice"] != nil {
		t.Fatalf("editing a disabled model reported a notice: %v", obj["notice"])
	}
	// Re-enable: the save goes through and the answer names the model the
	// catalogue no longer lists, without declaring it gone for good.
	code, obj, _ := call(t, h, "PUT", "/models/public", `{"id":"public","enabled":true,"fallback":false,`+bindings+`}`)
	if code != 200 {
		t.Fatal("re-enable was refused:", code)
	}
	notice, _ := obj["notice"].(string)
	if !strings.Contains(notice, "retired") || !strings.Contains(notice, "可能") {
		t.Fatalf("notice = %q", notice)
	}
	if _, ok := obj["model"]; !ok {
		t.Fatalf("notice answer lost the model shape: %v", obj)
	}
	// A model whose bindings are all still catalogued re-enables silently.
	other := store.Channel{Name: "other", Protocol: "openai", BaseURL: "http://localhost:9998", APIKey: "k", Enabled: true, Models: []string{"m"}, AvailableModels: []string{"m"}}
	if err := s.SaveChannel(&other); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := call(t, h, "POST", "/models", `{"id":"clean","enabled":false,"bindings":[{"channel_id":2,"model":"m","priority":0,"enabled":true}]}`); code != 200 {
		t.Fatal("create failed", code)
	}
	if code, obj, _ := call(t, h, "PUT", "/models/clean", `{"id":"clean","enabled":true,"bindings":[{"channel_id":2,"model":"m","priority":0,"enabled":true}]}`); code != 200 || obj["notice"] != nil {
		t.Fatalf("catalogued binding reported a notice: %d %v", code, obj["notice"])
	}
	// Rule 4: deleting the model is allowed while it is enabled.
	if code, _, _ := call(t, h, "DELETE", "/models/public", ""); code != 200 {
		t.Fatal("delete an enabled model", code)
	}
}

func TestModelsReorderAndBatchEndpoints(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:17878", Hooks{})

	c := store.Channel{Name: "prov", Protocol: "openai", BaseURL: "http://localhost:9999", APIKey: "k", Models: []string{"real"}, Enabled: true}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}

	call(t, h, "POST", "/models", `{"id":"m1","enabled":true,"bindings":[{"channel_id":1,"model":"real","priority":0,"enabled":true}]}`)
	call(t, h, "POST", "/models", `{"id":"m2","enabled":true,"bindings":[{"channel_id":1,"model":"real","priority":0,"enabled":true}]}`)

	// 1. Verify sort_order in GET /models
	code, _, list := call(t, h, "GET", "/models", "")
	if code != 200 || len(list) != 2 {
		t.Fatalf("list failed: %d %v", code, list)
	}
	first := list[0].(map[string]any)
	if _, ok := first["sort_order"]; !ok {
		t.Fatal("expected sort_order in GET /models response")
	}

	// 2. PUT /models/reorder with object body
	code, obj, _ := call(t, h, "PUT", "/models/reorder", `{"order":["m2","m1"]}`)
	if code != 200 || obj["ok"] != true {
		t.Fatalf("reorder failed: %d %v", code, obj)
	}
	_, _, list = call(t, h, "GET", "/models", "")
	if list[0].(map[string]any)["id"] != "m2" || list[1].(map[string]any)["id"] != "m1" {
		t.Fatalf("unexpected order after reorder: %v", list)
	}

	// 3. PUT /models/reorder with raw array body
	code, obj, _ = call(t, h, "PUT", "/models/reorder", `["m1","m2"]`)
	if code != 200 || obj["ok"] != true {
		t.Fatalf("raw array reorder failed: %d %v", code, obj)
	}
	_, _, list = call(t, h, "GET", "/models", "")
	if list[0].(map[string]any)["id"] != "m1" || list[1].(map[string]any)["id"] != "m2" {
		t.Fatalf("unexpected order after array reorder: %v", list)
	}

	// 4. PUT /models/batch/status
	code, obj, _ = call(t, h, "PUT", "/models/batch/status", `{"ids":["m1","m2"],"enabled":false}`)
	if code != 200 || obj["ok"] != true {
		t.Fatalf("batch status failed: %d %v", code, obj)
	}
	_, _, list = call(t, h, "GET", "/models", "")
	if list[0].(map[string]any)["enabled"] != false || list[1].(map[string]any)["enabled"] != false {
		t.Fatalf("batch status did not disable models: %v", list)
	}
	code, obj, _ = call(t, h, "PUT", "/models/batch/status", `{"ids":["m1"],"enabled":true}`)
	if code != 200 || obj["ok"] != true {
		t.Fatalf("batch status enable failed: %d %v", code, obj)
	}
	_, _, list = call(t, h, "GET", "/models", "")
	if list[0].(map[string]any)["enabled"] != true || list[1].(map[string]any)["enabled"] != false {
		t.Fatalf("batch status did not enable m1: %v", list)
	}

	// 5. POST /models/batch-delete
	code, obj, _ = call(t, h, "POST", "/models/batch-delete", `{"ids":["m1"]}`)
	if code != 200 || obj["ok"] != true {
		t.Fatalf("batch delete failed: %d %v", code, obj)
	}
	_, _, list = call(t, h, "GET", "/models", "")
	if len(list) != 1 || list[0].(map[string]any)["id"] != "m2" {
		t.Fatalf("batch delete did not remove m1: %v", list)
	}

	// 6. POST /models/import (invalid mode)
	code, _, _ = call(t, h, "POST", "/models/import", `{"mode":"invalid","models":[]}`)
	if code != 400 {
		t.Fatalf("expected 400 for invalid mode, got %d", code)
	}

	// 7. POST /models/import (merge mode)
	importMerge := `{"mode":"merge","models":[{"id":"m3","enabled":true,"bindings":[{"channel_id":1,"model":"real","priority":0,"enabled":true}]}]}`
	code, obj, _ = call(t, h, "POST", "/models/import", importMerge)
	if code != 200 || obj["ok"] != true || obj["count"] != float64(1) {
		t.Fatalf("import merge failed: %d %v", code, obj)
	}
	_, _, list = call(t, h, "GET", "/models", "")
	if len(list) != 2 {
		t.Fatalf("expected 2 models after merge, got %d", len(list))
	}

	// 8. POST /models/import (overwrite mode)
	importOverwrite := `{"mode":"overwrite","models":[{"id":"m4","enabled":true,"bindings":[{"channel_id":1,"model":"real","priority":0,"enabled":true}]}]}`
	code, obj, _ = call(t, h, "POST", "/models/import", importOverwrite)
	if code != 200 || obj["ok"] != true || obj["count"] != float64(1) {
		t.Fatalf("import overwrite failed: %d %v", code, obj)
	}
	_, _, list = call(t, h, "GET", "/models", "")
	if len(list) != 1 || list[0].(map[string]any)["id"] != "m4" {
		t.Fatalf("expected only m4 after overwrite, got %v", list)
	}

	// 9. POST /models/import (invalid model)
	importBad := `{"mode":"merge","models":[{"id":"bad","enabled":true,"bindings":[{"channel_id":9999,"model":"real","priority":0,"enabled":true}]}]}`
	code, _, _ = call(t, h, "POST", "/models/import", importBad)
	if code != 400 {
		t.Fatalf("expected 400 for invalid import, got %d", code)
	}
}
