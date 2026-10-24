package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"torouter/internal/health"
	"torouter/internal/store"
)

func TestStatusRoutesTargetOnlyConfiguredCredential(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := store.Channel{Name: "DeepSeek", Enabled: true, BaseURL: "https://api.deepseek.com", Protocol: "openai", APIKey: "synthetic-only", Models: []string{"up"}}
	if err = s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Channel(c.ID)
	cred := c.Credentials[0]
	var reset statusTarget
	resetCalls, refreshCalls := 0, 0
	refreshErr := errors.New("余额查询失败，请检查网络")
	h := New(s, "http://127.0.0.1:17878", Hooks{
		RuntimeStatus: func() health.StatusSnapshot {
			return health.StatusSnapshot{Rows: []health.StatusRow{{ChannelID: c.ID, CredentialID: cred.ID, Red: true}}, ProblemCount: 1}
		},
		ResetStatus: func(id int64, credential, model string) error {
			resetCalls++
			reset = statusTarget{id, credential, model}
			return nil
		},
		RefreshQuota: func(ctx context.Context, actual store.Channel, member store.CredentialItem) error {
			refreshCalls++
			if actual.ID != c.ID || member.ID != cred.ID || member.APIKey != "synthetic-only" {
				t.Error("quota refresh did not resolve the selected configured member")
			}
			return refreshErr
		},
	})
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s %s = %d %s", method, path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "synthetic-only") {
			t.Fatal("status endpoint leaked credential material")
		}
		return w
	}
	w := request("GET", "/status", "", 200)
	if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"problem_count":1`) {
		t.Fatal("status snapshot is missing its live count/no-store contract")
	}
	for _, path := range []string{"/status/reset", "/status/refresh-quota"} {
		request("POST", path, `{}`, 400)
		request("POST", path, fmt.Sprintf(`{"channel_id":%d,"credential_id":"missing"}`, c.ID), 404)
		request("POST", path, `{"channel_id":999,"credential_id":"missing"}`, 404)
	}
	if resetCalls != 0 || refreshCalls != 0 {
		t.Fatal("invalid targets reached a mutation/query hook")
	}
	member := fmt.Sprintf(`{"channel_id":%d,"credential_id":%q}`, c.ID, cred.ID)
	request("POST", "/status/reset", member, 200)
	if reset != (statusTarget{ChannelID: c.ID, CredentialID: cred.ID}) {
		t.Fatalf("reset target = %+v", reset)
	}
	model := fmt.Sprintf(`{"channel_id":%d,"credential_id":%q,"model":"up"}`, c.ID, cred.ID)
	request("POST", "/status/reset", model, 200)
	if reset.Model != "up" {
		t.Fatal("model-level restore broadened to the credential")
	}
	request("POST", "/status/refresh-quota", model, 400)
	request("POST", "/status/refresh-quota", member, 502)
	refreshErr = nil
	request("POST", "/status/refresh-quota", member, 200)
	if resetCalls != 2 || refreshCalls != 2 {
		t.Fatalf("reset=%d refresh=%d", resetCalls, refreshCalls)
	}
	current, _ := s.Channel(c.ID)
	if !current.Enabled || !current.Credentials[0].Enabled || current.Credentials[0].APIKey != cred.APIKey {
		t.Fatal("runtime action altered the saved channel/credential")
	}
}

func TestUnsentLogDoesNotEnterUsageAPI(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, row := range []store.Usage{
		{Model: "attempted", Status: 200, InputTokens: 4, OutputTokens: 2},
		{Model: "not-attempted", Status: 503, NotSent: true, InputTokens: 100, OutputTokens: 100},
	} {
		if err = s.AddUsage(row); err != nil {
			t.Fatal(err)
		}
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/logs", nil))
	var logs []store.Usage
	if err = json.Unmarshal(w.Body.Bytes(), &logs); err != nil || w.Code != 200 || len(logs) != 2 {
		t.Fatalf("logs must include both records: %d %s %v", w.Code, w.Body.String(), err)
	}
	if !logs[0].NotSent || logs[0].InputTokens != 0 || logs[0].ActualCost != nil {
		t.Fatal("not-sent log lost its classification or carries fabricated usage")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/usage", nil))
	var totals map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &totals); err != nil || w.Code != 200 {
		t.Fatalf("usage: %d %s %v", w.Code, w.Body.String(), err)
	}
	if totals["requests"] != float64(1) || totals["input_tokens"] != float64(4) || totals["output_tokens"] != float64(2) || totals["failures"] != float64(0) || strings.Contains(w.Body.String(), "not-attempted") {
		t.Fatalf("unsent request entered the usage projection: %s", w.Body.String())
	}
}
