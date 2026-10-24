package pricing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"torouter/internal/store"
)

func TestExchangeDailyConcurrentFailureAndRestart(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"base":"USD","quote":"CNY","date":"2026-10-02","rate":7.12}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 15, 59, 0, 0, time.UTC) // October 4 in Shanghai.
	svc := NewExchangeService(st)
	svc.Endpoints["frankfurter"] = server.URL
	st.SaveExchangeSources([]string{"frankfurter"})
	svc.Now = func() time.Time { return now }
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := svc.Get(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			if !v.Available || v.Stale || v.Rate != 7.12 || v.AttemptedOn != "2026-10-04" {
				t.Errorf("bad result: %+v", v)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent fetch count", calls.Load())
	}
	st.Close()
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc.Store = st
	if _, err := svc.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("restart repeated daily fetch")
	}
	fail.Store(true)
	now = now.Add(time.Minute) // New Shanghai day, same UTC date.
	v, err := svc.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !v.Available || !v.Stale || v.Rate != 7.12 || v.Error == "" || v.AttemptedOn != "2026-10-05" {
		t.Fatalf("failed refresh lost old rate: %+v", v)
	}
	st.Close()
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc.Store = st
	v, err = svc.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !v.Stale || v.Error == "" {
		t.Fatal("failed attempt not persisted", calls.Load(), v)
	}
}

func TestExchangeRejectsInvalidResponsesAndCachesUnavailable(t *testing.T) {
	for _, body := range []string{
		`{"base":"USD","quote":"CNY","date":"2026-10-04","rate":0}`,
		`{"base":"CNY","quote":"USD","date":"2026-10-04","rate":7}`,
		`{"base":"USD","quote":"CNY","date":"2026-99-99","rate":7}`,
		`{"base":"USD","quote":"CNY","date":"2026-10-06","rate":7}`,
		`{"base":"USD","quote":"CNY","date":"2026-10-04","rate":1e999}`,
		strings.Repeat("x", 4097),
	} {
		t.Run(fmt.Sprint(len(body), body[:8]), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, body) }))
			defer server.Close()
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			svc := NewExchangeService(st)
			svc.Endpoints["frankfurter"] = server.URL
			st.SaveExchangeSources([]string{"frankfurter"})
			svc.Now = func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }
			for i := 0; i < 2; i++ {
				v, err := svc.Get(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if v.Available || !v.Stale || v.Error == "" || v.Rate != 0 {
					t.Fatal(v)
				}
			}
			if calls != 1 {
				t.Fatal("retried failed day", calls)
			}
		})
	}
}

func TestExchangeAutoRetryAfterCooldownAndMaxRetries(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(500)
			return
		}
		fmt.Fprint(w, `{"base":"USD","quote":"CNY","date":"2026-10-05","rate":7.15}`)
	}))
	defer server.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	currentTime := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	svc := NewExchangeService(st)
	svc.Endpoints["frankfurter"] = server.URL
	st.SaveExchangeSources([]string{"frankfurter"})
	svc.Now = func() time.Time { return currentTime }

	// 1. 首次尝试失败 (calls == 1, retries == 0)
	v, err := svc.Get(context.Background())
	if err != nil || v.Error == "" || calls.Load() != 1 {
		t.Fatalf("first attempt should fail: v=%+v, calls=%d, err=%v", v, calls.Load(), err)
	}

	// 2. 5秒后重试（在10秒冷却期内，不应该调用网络，calls 仍为 1）
	currentTime = currentTime.Add(5 * time.Second)
	v, err = svc.Get(context.Background())
	if err != nil || v.Error == "" || calls.Load() != 1 {
		t.Fatalf("call during 10s cooldown should not retry: v=%+v, calls=%d, err=%v", v, calls.Load(), err)
	}

	// 3. 连续失败重试，验证重试次数递增直至上限 10 次
	for expectedRetries := 1; expectedRetries <= 10; expectedRetries++ {
		currentTime = currentTime.Add(11 * time.Second)
		v, err = svc.Get(context.Background())
		if err != nil || v.Error == "" {
			t.Fatalf("retry %d expected failure: v=%+v, err=%v", expectedRetries, v, err)
		}
		if int(calls.Load()) != expectedRetries+1 {
			t.Fatalf("retry %d expected calls %d, got %d", expectedRetries, expectedRetries+1, calls.Load())
		}
		if v.Retries != expectedRetries {
			t.Fatalf("expected retries=%d, got %d", expectedRetries, v.Retries)
		}
	}

	// 此时 retries 已达到 10 (calls=11)。再次推进 11 秒后调用，已达当日上限，不应再调用网络
	currentTime = currentTime.Add(11 * time.Second)
	v, err = svc.Get(context.Background())
	if err != nil || v.Error == "" || calls.Load() != 11 {
		t.Fatalf("call exceeding max retries should not retry: calls=%d, v=%+v", calls.Load(), v)
	}

	// 4. 手动强制刷新不受上限限制，网络恢复后成功
	fail.Store(false)
	v, err = svc.Refresh(context.Background())
	if err != nil || !v.Available || v.Stale || v.Error != "" || v.Rate != 7.15 || calls.Load() != 12 {
		t.Fatalf("force refresh should succeed: v=%+v, calls=%d, err=%v", v, calls.Load(), err)
	}

	// 5. 成功后，即使再过 20 秒，当日也不再重复发起网络请求 (calls 保持 12)
	currentTime = currentTime.Add(20 * time.Second)
	v, err = svc.Get(context.Background())
	if err != nil || !v.Available || v.Stale || v.Rate != 7.15 || calls.Load() != 12 {
		t.Fatalf("cached success should not refetch: v=%+v, calls=%d, err=%v", v, calls.Load(), err)
	}
}

func TestExchangeContextDecoupledFromCaller(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, `{"base":"USD","quote":"CNY","date":"2026-10-05","rate":7.18}`)
	}))
	defer server.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := NewExchangeService(st)
	svc.Endpoints["frankfurter"] = server.URL
	st.SaveExchangeSources([]string{"frankfurter"})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC) }

	// 创建一个已被取消的 context 模拟前端客户端在未完成时断开连接
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// 调用 Get，底层网络请求应解耦并在 independent context 中顺利完成
	v, err := svc.Get(canceledCtx)
	if err != nil || !v.Available || v.Stale || v.Rate != 7.18 || v.Error != "" {
		t.Fatalf("decoupled fetch should succeed even if caller context was canceled: v=%+v, err=%v", v, err)
	}
}
