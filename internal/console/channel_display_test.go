package console

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"torouter/internal/store"
)

func TestChannelDisplayAPI(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := store.Channel{Name: "A", Protocol: "openai", BaseURL: "https://example.com", APIKey: "private-channel-secret", Models: []string{"model"}, Enabled: true}
	b := a
	b.Name = "B"
	for _, c := range []*store.Channel{&a, &b} {
		if err = s.SaveChannel(c); err != nil {
			t.Fatal(err)
		}
	}
	h := New(s, "http://127.0.0.1:17878", Hooks{})
	request := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/channels/display", strings.NewReader(body)))
		if strings.Contains(w.Body.String(), a.APIKey) {
			t.Fatal("display API exposed secret")
		}
		return w
	}
	read := func(w *httptest.ResponseRecorder) store.ChannelDisplay {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("display API: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("display API permits caching")
		}
		var v store.ChannelDisplay
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if v.Order == nil || v.Icons == nil {
			t.Fatalf("null display preferences: %s", w.Body.String())
		}
		return v
	}
	if v := read(request(http.MethodGet, "")); v.SortMode != "name" || len(v.Order)+len(v.Icons) != 0 {
		t.Fatal(v)
	}
	read(request(http.MethodPut, fmt.Sprintf(`{"order":[%d],"icon":{"channel_id":%d,"value":"openai"}}`, b.ID, a.ID)))
	// PUT responds with the full preferences after either kind of patch.
	updated := read(request(http.MethodPut, fmt.Sprintf(`{"order":[%d,%d]}`, a.ID, b.ID)))
	if !reflect.DeepEqual(updated.Order, []int64{a.ID, b.ID}) || updated.Icons[strconv.FormatInt(a.ID, 10)] != "openai" {
		t.Fatalf("order patch lost icon: %#v", updated)
	}
	var pngBytes bytes.Buffer
	if err = png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 96, 96))); err != nil {
		t.Fatal(err)
	}
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	updated = read(request(http.MethodPut, fmt.Sprintf(`{"icon":{"channel_id":%d,"value":%q}}`, b.ID, imageURL)))
	if updated.Icons[strconv.FormatInt(b.ID, 10)] != imageURL || len(updated.Order) != 2 {
		t.Fatalf("PNG patch lost order: %#v", updated)
	}
	want := updated
	for _, mode := range []string{"manual", "name", "manual"} {
		updated = read(request(http.MethodPut, fmt.Sprintf(`{"sort_mode":%q}`, mode)))
		want.SortMode = mode
		if !reflect.DeepEqual(updated, want) {
			t.Fatalf("mode switch discarded order or icon: %#v", updated)
		}
	}
	for name, body := range map[string]string{
		"invalid sort mode":    `{"sort_mode":"invalid"}`,
		"null sort mode":       `{"sort_mode":null}`,
		"number sort mode":     `{"sort_mode":1}`,
		"empty sort mode":      `{"sort_mode":""}`,
		"valid mode bad order": `{"sort_mode":"name","order":[999]}`,
		"empty":                `{}`,
		"null":                 `null`,
		"null order":           `{"order":null}`,
		"null icon":            `{"icon":null}`,
		"object order":         `{"order":{}}`,
		"string ID":            `{"order":["1"]}`,
		"fractional ID":        `{"order":[1.5]}`,
		"overflow ID":          `{"order":[9223372036854775808]}`,
		"zero ID":              `{"order":[0]}`,
		"negative ID":          `{"order":[-1]}`,
		"unknown ID":           `{"order":[999999]}`,
		"duplicate ID":         fmt.Sprintf(`{"order":[%d,%d]}`, a.ID, a.ID),
		"unknown field":        `{"priority":5}`,
		"unknown nested field": fmt.Sprintf(`{"icon":{"channel_id":%d,"value":"generic","url":"https://example.com"}}`, a.ID),
		"missing value":        fmt.Sprintf(`{"icon":{"channel_id":%d}}`, a.ID),
		"null value":           fmt.Sprintf(`{"icon":{"channel_id":%d,"value":null}}`, a.ID),
		"missing channel":      `{"icon":{"value":"generic"}}`,
		"unknown icon channel": `{"order":[],"icon":{"channel_id":999999,"value":"generic"}}`,
		"unknown builtin":      fmt.Sprintf(`{"order":[],"icon":{"channel_id":%d,"value":"unknown"}}`, a.ID),
		"external URL":         fmt.Sprintf(`{"icon":{"channel_id":%d,"value":"https://example.com/a.png"}}`, a.ID),
		"SVG":                  fmt.Sprintf(`{"icon":{"channel_id":%d,"value":"data:image/svg+xml;base64,PHN2Zy8+"}}`, a.ID),
		"broken PNG":           fmt.Sprintf(`{"icon":{"channel_id":%d,"value":"data:image/png;base64,YmFk"}}`, a.ID),
		"oversized body":       fmt.Sprintf(`{"icon":{"channel_id":%d,"value":"`, a.ID) + strings.Repeat("x", 1<<20) + `"}}`,
		"trailing JSON":        `{"order":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := request(http.MethodPut, body)
			if w.Code != 400 {
				t.Fatalf("accepted invalid patch: %d %s", w.Code, w.Body.String())
			}
			if got := read(request(http.MethodGet, "")); !reflect.DeepEqual(got, want) {
				t.Fatalf("invalid request changed preferences: %#v", got)
			}
		})
	}
	updated = read(request(http.MethodPut, fmt.Sprintf(`{"order":[],"icon":{"channel_id":%d,"value":""}}`, a.ID)))
	if len(updated.Order) != 0 || len(updated.Icons) != 1 || updated.Icons[strconv.FormatInt(b.ID, 10)] != imageURL {
		t.Fatalf("reset lost other icon: %#v", updated)
	}
	// The existing DELETE path also cleans metadata.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/channels/%d", b.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete channel: %d %s", w.Code, w.Body.String())
	}
	if got := read(request(http.MethodGet, "")); len(got.Order)+len(got.Icons) != 0 {
		t.Fatalf("delete left preferences: %#v", got)
	}
}
