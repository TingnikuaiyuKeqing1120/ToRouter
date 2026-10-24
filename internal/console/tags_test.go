package console

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestConsole_TagRoutes(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	handler := New(st, "http://127.0.0.1:17878", Hooks{})

	// 1. GET /tag-groups 验证预设组
	req := httptest.NewRequest("GET", "/tag-groups", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /tag-groups expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var groups []tagGroupWithTags
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("unmarshal groups: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("expected 3 builtin groups, got %d", len(groups))
	}

	// 2. POST /tag-groups 创建自定义组
	body := `{"id": "scenario", "name": "业务场景", "description": "场景标签"}`
	req = httptest.NewRequest("POST", "/tag-groups", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("POST /tag-groups expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. POST /tags 创建自定义标签
	tagBody := `{"id": "scenario:test", "group_id": "scenario", "name": "测试标签", "color": "emerald"}`
	req = httptest.NewRequest("POST", "/tags", strings.NewReader(tagBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("POST /tags expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. 将该标签挂到下游模型上
	ch := store.Channel{
		Name:      "Ch1",
		APIKey:    "sk-test",
		Models:    []string{"m1"},
		Enabled:   true,
		Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "http://example.com"}},
	}
	if err := st.SaveChannel(&ch); err != nil {
		t.Fatalf("SaveChannel: %v", err)
	}
	dm := store.DownstreamModel{
		ID:      "dm-test",
		Enabled: true,
		Tags:    []string{"scenario:test"},
		Bindings: []store.Binding{
			{ChannelID: ch.ID, Model: "m1", Priority: 1, Enabled: true},
		},
	}
	if err := st.SaveDownstreamModel(dm); err != nil {
		t.Fatalf("SaveDownstreamModel: %v", err)
	}

	// 5. 尝试删除该标签 (未加 force=true)，应返回 409 Conflict
	req = httptest.NewRequest("DELETE", "/tags/scenario:test", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("DELETE /tags/scenario:test expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}
	var conflictResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &conflictResp); err != nil {
		t.Fatalf("unmarshal conflict response: %v", err)
	}
	if conflictResp["needs_confirm"] != true {
		t.Fatalf("expected needs_confirm: true in response")
	}

	// 6. 带 force=true 强制级联解绑并删除
	req = httptest.NewRequest("DELETE", "/tags/scenario:test?force=true", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("DELETE /tags/scenario:test?force=true expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 7. 验证下游模型的标签已经被级联解绑
	savedDM, err := st.ListDownstreamModels()
	if err != nil {
		t.Fatalf("ListDownstreamModels: %v", err)
	}
	if len(savedDM[0].Tags) != 1 || savedDM[0].Tags[0] != "scenario:unknown" {
		t.Fatalf("expected removed group value to become unknown, got %v", savedDM[0].Tags)
	}
}
