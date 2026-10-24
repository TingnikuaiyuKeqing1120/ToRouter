package store

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func displayTestChannel(t *testing.T, s *Store, name string) Channel {
	t.Helper()
	contextWindow := int64(65536)
	c := Channel{Name: name, APIKey: "display-test-secret-" + name, Protocol: "openai", BaseURL: "https://example.com", Models: []string{"model"}, ModelConfigs: map[string]ModelConfig{"model": {ContextWindow: &contextWindow}}, Enabled: true}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func displayPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			im.SetNRGBA(x, y, color.NRGBA{R: byte(x), G: byte(y), B: 100, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func displayPNGURL(raw []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func displayPrefs(t *testing.T, s *Store) ChannelDisplay {
	t.Helper()
	v, err := s.GetChannelDisplay()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestChannelDisplayPersistenceAndIsolation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	a := displayTestChannel(t, s, "A")
	b := displayTestChannel(t, s, "B")
	model := DownstreamModel{ID: "public", Enabled: true, Bindings: []Binding{{ChannelID: b.ID, Model: "model", Priority: 7, Enabled: true}, {ChannelID: a.ID, Model: "model", Priority: 2, Enabled: true}}}
	if err = s.SaveDownstreamModel(model); err != nil {
		t.Fatal(err)
	}
	// Compare stored JSON byte-for-byte: credentials, specs, price tables and
	// continuation generations must not be rewritten by console preferences.
	snapshot := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{"SELECT data FROM channels ORDER BY id", "SELECT data FROM downstream_models ORDER BY id", "SELECT data FROM settings ORDER BY id"} {
			rows, e := s.db.Query(query)
			if e != nil {
				t.Fatal(e)
			}
			for rows.Next() {
				var raw string
				if e = rows.Scan(&raw); e != nil {
					t.Fatal(e)
				}
				result = append(result, raw)
			}
			if e = rows.Err(); e != nil {
				t.Fatal(e)
			}
			rows.Close()
		}
		return result
	}
	before := snapshot()
	if got := displayPrefs(t, s); got.Order == nil || got.Icons == nil || len(got.Order)+len(got.Icons) != 0 {
		t.Fatalf("nonempty/default-null preferences: %#v", got)
	}
	order := []int64{b.ID}
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{Order: &order}); err != nil {
		t.Fatal(err)
	}
	icon := displayPNGURL(displayPNG(t, 96, 96))
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{Icon: &ChannelDisplayIcon{ChannelID: a.ID, Value: icon}}); err != nil {
		t.Fatal(err)
	}
	want := ChannelDisplay{SortMode: "name", Order: order, Icons: map[string]string{strconv.FormatInt(a.ID, 10): icon}}
	if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("patch lost another preference: %#v", got)
	}
	order = []int64{a.ID, b.ID}
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{Order: &order}); err != nil {
		t.Fatal(err)
	}
	want.Order = order
	if got := snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatal("display patch rewrote channel, bindings or settings")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("preferences did not persist: %#v", got)
	}
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{Order: &[]int64{}, Icon: &ChannelDisplayIcon{ChannelID: a.ID}}); err != nil {
		t.Fatal(err)
	}
	if got := displayPrefs(t, s); len(got.Order)+len(got.Icons) != 0 || got.Order == nil || got.Icons == nil {
		t.Fatalf("reset did not restore empty preferences: %#v", got)
	}
}

func TestChannelDisplayInvalidPatchIsAtomic(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := displayTestChannel(t, s, "A")
	b := displayTestChannel(t, s, "B")
	initial := []int64{a.ID}
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{Order: &initial, Icon: &ChannelDisplayIcon{ChannelID: b.ID, Value: "deepseek"}}); err != nil {
		t.Fatal(err)
	}
	want := displayPrefs(t, s)
	for name, patch := range map[string]ChannelDisplayPatch{
		"empty":                                 {},
		"duplicate":                             {Order: &[]int64{b.ID, b.ID}},
		"zero":                                  {Order: &[]int64{0}},
		"negative":                              {Order: &[]int64{-1}},
		"unknown":                               {Order: &[]int64{999}},
		"bad order plus valid icon":             {Order: &[]int64{999}, Icon: &ChannelDisplayIcon{ChannelID: a.ID, Value: "openai"}},
		"valid order plus missing icon channel": {Order: &[]int64{b.ID}, Icon: &ChannelDisplayIcon{ChannelID: 999, Value: "openai"}},
		"valid order plus bad icon":             {Order: &[]int64{b.ID}, Icon: &ChannelDisplayIcon{ChannelID: a.ID, Value: "https://example.com/icon.png"}},
		"valid order plus unknown icon":         {Order: &[]int64{b.ID}, Icon: &ChannelDisplayIcon{ChannelID: a.ID, Value: "not-a-provider"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.PatchChannelDisplay(patch); !errors.Is(err, ErrInvalidChannelDisplay) {
				t.Fatalf("want validation error, got %v", err)
			}
			if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
				t.Fatalf("invalid patch changed preferences: %#v", got)
			}
		})
	}
}

func TestChannelDisplayBuiltinCatalogPersistence(t *testing.T) {
	// Keep the backend's persisted IDs aligned with the actual frontend catalog.
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "assets", "provider-icons", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Icons []struct {
			ID string `json:"id"`
		} `json:"icons"`
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Icons) != 41 {
		t.Fatalf("want 41 additional builtins, got %d", len(catalog.Icons))
	}
	ids := []string{"generic", "openai", "anthropic", "deepseek", "moonshot", "zhipu", "minimax", "stepfun", "nvidia", "openrouter", "xiaomi"}
	for _, icon := range catalog.Icons {
		ids = append(ids, icon.ID)
	}
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	want := map[string]string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("empty or duplicate builtin ID: %q", id)
		}
		seen[id] = true
		channel := displayTestChannel(t, s, "Icon "+id)
		if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{Icon: &ChannelDisplayIcon{ChannelID: channel.ID, Value: id}}); err != nil {
			t.Fatalf("cannot save builtin %q: %v", id, err)
		}
		want[strconv.FormatInt(channel.ID, 10)] = id
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := displayPrefs(t, s).Icons; !reflect.DeepEqual(got, want) {
		t.Fatalf("builtin IDs did not persist after reopen: %#v", got)
	}
}

func TestChannelDisplayPNGValidation(t *testing.T) {
	valid := displayPNG(t, 128, 128)
	// A valid ancillary chunk proves the decoded-byte boundary independently
	// of the image dimensions and the encoded input length.
	chunk := make([]byte, maxChannelIconBytes-len(valid))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(chunk)-12))
	copy(chunk[4:8], "teST")
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	exactLimit := append(append(append([]byte{}, valid[:len(valid)-12]...), chunk...), valid[len(valid)-12:]...)
	// A structurally valid PNG header with zero width must fail before decode.
	zero := append([]byte{}, valid...)
	binary.BigEndian.PutUint32(zero[16:20], 0)
	binary.BigEndian.PutUint32(zero[29:33], crc32.ChecksumIEEE(zero[12:29]))
	for name, value := range map[string]string{
		"external":                "https://example.com/icon.png",
		"unknown builtin":         "builtin:openai",
		"unknown ID":              "not-a-provider",
		"SVG":                     "data:image/svg+xml;base64,PHN2Zy8+",
		"JPEG":                    "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(valid),
		"malformed base64":        "data:image/png;base64,%%%",
		"not PNG":                 displayPNGURL([]byte("not PNG")),
		"truncated PNG":           displayPNGURL(valid[:40]),
		"corrupt PNG":             displayPNGURL(append(append([]byte{}, valid[:40]...), []byte("corrupt")...)),
		"zero width":              displayPNGURL(zero),
		"too wide":                displayPNGURL(displayPNG(t, 129, 1)),
		"too tall":                displayPNGURL(displayPNG(t, 1, 129)),
		"too many bytes":          displayPNGURL(append(append([]byte{}, valid...), bytes.Repeat([]byte{0}, maxChannelIconBytes+1-len(valid))...)),
		"oversized encoded input": "data:image/png;base64," + strings.Repeat("A", 100000),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateChannelIcon(value); !errors.Is(err, ErrInvalidChannelDisplay) {
				t.Fatalf("accepted invalid icon: %v", err)
			}
		})
	}
	for _, value := range []string{"", "generic", "openai", "anthropic", "deepseek", "moonshot", "zhipu", "minimax", "stepfun", "nvidia", "openrouter", "xiaomi", displayPNGURL(displayPNG(t, 1, 1)), displayPNGURL(valid), displayPNGURL(exactLimit)} {
		if err := validateChannelIcon(value); err != nil {
			t.Fatalf("valid icon rejected: %v", err)
		}
	}
}

func TestChannelDisplayConcurrentPatchesAndDelete(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := displayTestChannel(t, s, "A")
	b := displayTestChannel(t, s, "B")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, e := s.PatchChannelDisplay(ChannelDisplayPatch{Order: &[]int64{b.ID, a.ID}})
		errs <- e
	}()
	go func() {
		defer wg.Done()
		_, e := s.PatchChannelDisplay(ChannelDisplayPatch{Icon: &ChannelDisplayIcon{ChannelID: a.ID, Value: "openai"}})
		errs <- e
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{Icon: &ChannelDisplayIcon{ChannelID: b.ID, Value: "deepseek"}}); err != nil {
		t.Fatal(err)
	}
	if got := displayPrefs(t, s); !reflect.DeepEqual(got.Order, []int64{b.ID, a.ID}) || got.Icons[strconv.FormatInt(a.ID, 10)] != "openai" {
		t.Fatalf("concurrent patches lost a preference: %#v", got)
	}
	// A failed metadata write must roll back the channel deletion as well.
	if _, err = s.db.Exec("CREATE TRIGGER reject_display_update BEFORE UPDATE ON meta WHEN NEW.key='channel_display' BEGIN SELECT RAISE(ABORT, 'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteChannel(a.ID); err == nil {
		t.Fatal("accepted failed metadata cleanup")
	}
	if _, err = s.Channel(a.ID); err != nil {
		t.Fatal("failed cleanup deleted channel", err)
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_display_update"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteChannel(a.ID); err != nil {
		t.Fatal(err)
	}
	want := ChannelDisplay{SortMode: "name", Order: []int64{b.ID}, Icons: map[string]string{strconv.FormatInt(b.ID, 10): "deepseek"}}
	if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("delete did not preserve other channel preferences: %#v", got)
	}
	var raw string
	if err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", channelDisplayKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored ChannelDisplay
	if err = json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("deleted preferences remain stored: %#v", stored)
	}
	if err = s.DeleteChannel(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete: %v", err)
	}
}

func TestChannelDisplayModeSwitchPreservesManualOrder(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	a, b := displayTestChannel(t, s, "A"), displayTestChannel(t, s, "B")
	name, manual := "name", "manual"
	order := []int64{b.ID, a.ID}
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{SortMode: &manual, Order: &order, Icon: &ChannelDisplayIcon{ChannelID: a.ID, Value: "openai"}}); err != nil {
		t.Fatal(err)
	}
	want := displayPrefs(t, s)
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{SortMode: &name}); err != nil {
		t.Fatal(err)
	}
	want.SortMode = name
	if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("mode switch discarded preferences: %#v", got)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("name mode or manual order did not persist: %#v", got)
	}
	if _, err = s.PatchChannelDisplay(ChannelDisplayPatch{SortMode: &manual}); err != nil {
		t.Fatal(err)
	}
	want.SortMode = manual
	if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("manual order was not restored: %#v", got)
	}
	invalid := "invalid"
	for _, patch := range []ChannelDisplayPatch{
		{SortMode: &invalid, Order: &[]int64{a.ID, b.ID}},
		{SortMode: &name, Order: &[]int64{999}},
	} {
		if _, err = s.PatchChannelDisplay(patch); !errors.Is(err, ErrInvalidChannelDisplay) {
			t.Fatalf("invalid patch: %v", err)
		}
		if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
			t.Fatalf("invalid patch changed mode: %#v", got)
		}
	}
}

func TestChannelDisplayInitializesExistingSortMode(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		t.Run(strconv.FormatBool(ordered), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			a := displayTestChannel(t, s, "A")
			order := []int64{}
			mode := "name"
			if ordered {
				order, mode = []int64{a.ID}, "manual"
			}
			icons := map[string]string{strconv.FormatInt(a.ID, 10): "deepseek"}
			raw, err := json.Marshal(map[string]any{"order": order, "icons": icons})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?)", channelDisplayKey, string(raw)); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			want := ChannelDisplay{SortMode: mode, Order: order, Icons: icons}
			if got := displayPrefs(t, s); !reflect.DeepEqual(got, want) {
				t.Fatalf("initialization changed old preferences: %#v", got)
			}
			var stored string
			if err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", channelDisplayKey).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			var persisted ChannelDisplay
			if err = json.Unmarshal([]byte(stored), &persisted); err != nil || !reflect.DeepEqual(persisted, want) {
				t.Fatalf("mode was not written at initialization: %s %v", stored, err)
			}
		})
	}
}
