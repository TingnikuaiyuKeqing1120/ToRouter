package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"torouter/internal/store"
)

func newTestCatalogConsole(t *testing.T) (*store.Store, *chi.Mux) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}

	r := chi.NewRouter()
	catalogRoutes(r, st)
	modelRoutes(r, st)
	return st, r
}

func TestCatalogModelsEndpoint(t *testing.T) {
	st, r := newTestCatalogConsole(t)
	defer st.Close()

	// 1. GET /catalog/models (all)
	req := httptest.NewRequest(http.MethodGet, "/catalog/models?source=builtin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var items []store.CatalogItem
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected items in builtin catalog")
	}

	// 2. GET /catalog/models with query
	reqQ := httptest.NewRequest(http.MethodGet, "/catalog/models?source=builtin&query=deepseek", nil)
	wQ := httptest.NewRecorder()
	r.ServeHTTP(wQ, reqQ)
	var dsItems []store.CatalogItem
	_ = json.Unmarshal(wQ.Body.Bytes(), &dsItems)
	if len(dsItems) < 2 {
		t.Fatalf("expected at least 2 deepseek models, got %d", len(dsItems))
	}
}

func TestCatalogSourcesEndpoint(t *testing.T) {
	st, r := newTestCatalogConsole(t)
	defer st.Close()

	req := httptest.NewRequest(http.MethodGet, "/catalog/sources", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var sources []store.CatalogSourceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &sources); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sources) < 3 {
		t.Fatalf("expected at least 3 sources, got %d", len(sources))
	}
}

func TestCatalogFetchInvalidSource(t *testing.T) {
	st, r := newTestCatalogConsole(t)
	defer st.Close()

	req := httptest.NewRequest(http.MethodPost, "/catalog/fetch", strings.NewReader(`{"source":"unsupported_xyz"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for unsupported source, got %d", w.Code)
	}
}

func TestCatalogPrefillEndpoint(t *testing.T) {
	st, r := newTestCatalogConsole(t)
	defer st.Close()
	c := catalogHTTPChannel(t, st, "claude-3-7-sonnet")
	item := catalogHTTPItem("claude-3-7-sonnet", 64000)
	if err := st.SaveExternalCatalog("openrouter", []store.CatalogItem{item}); err != nil {
		t.Fatal(err)
	}
	c.ModelConfigs = map[string]store.ModelConfig{item.ID: {ContextWindow: ptr(int64(32000)), OutputLimit: ptr(int64(0)), InputModalities: ptr([]string{})}}
	if err := st.SaveChannel(c); err != nil {
		t.Fatal(err)
	}
	m := store.DownstreamModel{ID: item.ID, Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: item.ID, Enabled: true}}}
	if err := st.SaveDownstreamModel(m); err != nil {
		t.Fatal(err)
	}
	req := store.PrefillRequest{Targets: []store.PrefillTarget{{Kind: "upstream", ChannelID: c.ID, Model: m.ID, Ref: &store.CatalogModelRef{Source: "openrouter", ID: item.ID}}}, Groups: []string{"capabilities", "price"}}
	preview := catalogHTTPPreview(t, r, req)
	if preview.Updated != 1 || len(preview.Patches) != 1 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	before, err := st.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	if before[0].ModelConfigs[item.ID].MaxOutputTokens != nil {
		t.Fatal("preview saved model early")
	}
	code, _, _ := call(t, r, http.MethodPost, "/catalog/prefill/apply", catalogJSON(t, req))
	if code != 200 {
		t.Fatal("apply", code)
	}
	models, err := st.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ModelConfigs[item.ID].MaxOutputTokens == nil || *models[0].ModelConfigs[item.ID].MaxOutputTokens != 64000 {
		t.Fatalf("expected one saved model with MaxOutputTokens=64000, got %+v", models)
	}
	if *models[0].ModelConfigs[item.ID].ContextWindow != 32000 || *models[0].ModelConfigs[item.ID].OutputLimit != 0 || len(*models[0].ModelConfigs[item.ID].InputModalities) != 0 {
		t.Fatalf("manual/zero/empty overwritten or declaration invented: %+v", models[0])
	}
	prices, err := st.GetPrices()
	if err != nil || len(prices) != 1 || prices[0].Selection.Mode != "catalog" {
		t.Fatalf("associated price: %+v %v", prices, err)
	}
}
