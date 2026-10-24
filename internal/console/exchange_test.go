package console

import (
	"fmt"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"torouter/internal/pricing"
	"torouter/internal/store"
)

func TestExchangeRoutesManualRefreshAndSources(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"base":"USD","quote":"CNY","date":"2026-10-02","rate":7.1}`)
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SaveExchangeSources([]string{"frankfurter"})
	svc := pricing.NewExchangeService(s)
	svc.Endpoints["frankfurter"] = server.URL
	router := chi.NewRouter()
	exchangeRoutesWithService(router, s, svc)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	first := call("GET", "/exchange-rate", "")
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"available":false`) {
		t.Fatal(first.Code, first.Body)
	}
	call("GET", "/exchange-rate", "")
	if calls != 1 {
		t.Fatal("automatic cache missed")
	}
	retried := call("POST", "/exchange-rate", "")
	if retried.Code != 200 || calls != 2 || !strings.Contains(retried.Body.String(), `"available":true`) {
		t.Fatal(retried.Code, retried.Body, calls)
	}
	got := call("PUT", "/exchange-rate/sources", `{"sources":["exchangerate_api","ecb"]}`)
	if got.Code != 200 {
		t.Fatal(got.Code, got.Body)
	}
	got = call("GET", "/exchange-rate/sources", "")
	if !strings.Contains(got.Body.String(), `"sources":["exchangerate_api","ecb"]`) || !strings.Contains(got.Body.String(), `"available"`) {
		t.Fatal(got.Body)
	}
	invalid := call("PUT", "/exchange-rate/sources", `{"sources":["ecb","ecb"]}`)
	if invalid.Code != 400 {
		t.Fatal(invalid.Code)
	}
}
