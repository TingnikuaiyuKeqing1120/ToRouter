package pricing

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestExchangeSourceFormatsAndFallbackOrder(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/ecb":
			w.WriteHeader(503)
		case "/frankfurter":
			fmt.Fprint(w, `{"base":"USD","quote":"CNY","date":"2026-10-02","rate":7}`)
		case "/exchangerate_api":
			fmt.Fprint(w, `{"result":"success","base_code":"USD","time_last_update_unix":1791072152,"rates":{"CNY":6.72}}`)
		}
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := NewExchangeService(s)
	for id := range svc.Endpoints {
		svc.Endpoints[id] = server.URL + "/" + id
	}
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }
	v, err := svc.Get(context.Background())
	if err != nil || v.Rate != 7 || v.SourceID != "frankfurter" || v.Error != "" || !reflect.DeepEqual(paths, []string{"/ecb", "/frankfurter"}) {
		t.Fatal(v, err, paths)
	}
	s.SaveExchangeSources([]string{"exchangerate_api", "ecb"})
	paths = nil
	v, err = svc.Refresh(context.Background())
	if err != nil || v.Rate != 6.72 || v.SourceURL != "https://www.exchangerate-api.com" || !reflect.DeepEqual(paths, []string{"/exchangerate_api"}) {
		t.Fatal(v, err, paths)
	}
	rate, date, err := parseExchange("ecb", []byte(`<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref"><Cube><Cube time="2026-10-02"><Cube currency="USD" rate="1.1225"/><Cube currency="CNY" rate="7.5259"/></Cube></Cube></gesmes:Envelope>`))
	if err != nil || date != "2026-10-02" || math.Abs(rate-7.5259/1.1225) > 1e-12 {
		t.Fatal(rate, date, err)
	}
}

func TestExchangeManualRetryAfterFailureAndSuccess(t *testing.T) {
	calls := 0
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if fail {
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
	svc := NewExchangeService(s)
	svc.Endpoints["frankfurter"] = server.URL
	for i := 0; i < 2; i++ {
		v, err := svc.Get(context.Background())
		if err != nil || v.Available || v.Error == "" {
			t.Fatal(v, err)
		}
	}
	if calls != 1 {
		t.Fatal("automatic retry bypassed daily cache")
	}
	fail = false
	v, err := svc.Refresh(context.Background())
	if err != nil || !v.Available || calls != 2 {
		t.Fatal(v, err, calls)
	}
	v, err = svc.Refresh(context.Background())
	if err != nil || v.Stale || calls != 3 {
		t.Fatal("manual success was cached", v, err, calls)
	}
	fail = true
	v, err = svc.Refresh(context.Background())
	if err != nil || !v.Available || !v.Stale || v.Rate != 7.1 || v.SourceID != "frankfurter" || calls != 4 {
		t.Fatal("failed refresh lost old value", v, err, calls)
	}
}

func TestExchangeConcurrentManualRefreshSharesAttempt(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		fmt.Fprint(w, `{"base":"USD","quote":"CNY","date":"2026-10-02","rate":7.1}`)
	}))
	defer server.Close()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SaveExchangeSources([]string{"frankfurter"})
	svc := NewExchangeService(s)
	svc.Endpoints["frankfurter"] = server.URL
	results := make(chan store.ExchangeRate, 8)
	for i := 0; i < 8; i++ {
		go func() {
			v, err := svc.Refresh(context.Background())
			if err != nil {
				t.Error(err)
			}
			results <- v
		}()
	}
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	var checked string
	for i := 0; i < 8; i++ {
		v := <-results
		if checked == "" {
			checked = v.CheckedAt
		}
		if v.CheckedAt != checked {
			t.Fatal("concurrent manual calls did not share attempt")
		}
	}
}
