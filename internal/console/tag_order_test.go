package console

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"torouter/internal/store"
)

func TestConsoleTagReorder(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveTagGroup(store.TagGroup{ID: "custom", Name: "Custom", Type: store.TagGroupTypeUser, SortOrder: 44}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"custom:a", "custom:b"} {
		if err := s.SaveTag(store.Tag{ID: id, GroupID: "custom", Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	request := func(path, body string, want int) {
		t.Helper()
		r := httptest.NewRequest("PUT", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d: %s", path, body, w.Code, want, w.Body.String())
		}
	}
	for _, body := range []string{`{}`, `{"order":null}`, `{"order":[]}`, `{"order":["custom","custom","billing","privacy"]}`, `{"order":["custom","ghost","billing","privacy"]}`, `{"order":"invalid"}`} {
		request("/tag-groups/reorder", body, 400)
	}
	for _, body := range []string{`{}`, `{"group_id":"custom"}`, `{"group_id":"custom","order":[]}`, `{"group_id":"missing","order":[]}`, `{"group_id":"custom","order":["custom:a","custom:a"]}`, `{"group_id":"custom","order":["custom:a","billing:free"]}`} {
		request("/tags/reorder", body, 400)
	}
	request("/tags/reorder", `{"group_id":"custom","order":["custom:b","custom:a","custom:unknown"]}`, 200)
	request("/tag-groups/reorder", `{"order":["custom","privacy","billing","modality"]}`, 200)
	groups, _ := s.ListTagGroups()
	if groups[0].ID != "custom" {
		t.Fatal("group route failed to save order")
	}
	tags, _ := s.ListTags()
	var custom []string
	for _, tag := range tags {
		if tag.GroupID == "custom" {
			custom = append(custom, tag.ID)
		}
	}
	got, _ := json.Marshal(custom)
	if string(got) != `["custom:b","custom:a","custom:unknown"]` {
		t.Fatalf("tag route order: %s", got)
	}
}
