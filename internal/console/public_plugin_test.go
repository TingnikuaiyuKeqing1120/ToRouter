package console

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"torouter/internal/plugins"
	"torouter/internal/store"
	"torouter/internal/zenfree"
)

func TestPublicPluginConnectCatalogueAndDisable(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			io.WriteString(w, `{"data":[{"id":"chat"},{"id":"response"}]}`)
		case "/catalogue":
			io.WriteString(w, `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{"chat":{"cost":{"input":0,"output":0}},"response":{"cost":{"input":0,"output":0},"provider":{"npm":"@ai-sdk/openai"}}}}}`)
		case "/docs":
			w.WriteHeader(503)
		default:
			t.Error("unexpected upstream request")
			w.WriteHeader(400)
		}
	}))
	defer up.Close()
	_, st := probeStore(t)
	manager, err := plugins.NewWithZenFreeService(st, zenfree.NewService(st, up.Client(), zenfree.Endpoints{APIBase: up.URL, CatalogueURL: up.URL + "/catalogue", ProtocolsURL: up.URL + "/docs"}))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithPlugins(st, "http://127.0.0.1:17878", Hooks{}, manager)
	pluginHTTP(t, handler, "POST", "/plugins/opencode-zen-free/connect", "", 503)
	consolePluginToggle(t, handler, "opencode-zen-free", true)
	var first struct {
		ChannelID int64 `json:"channel_id"`
		Linked    bool  `json:"already_linked"`
	}
	_ = json.Unmarshal(pluginHTTP(t, handler, "POST", "/plugins/opencode-zen-free/connect", "", 200).Body.Bytes(), &first)
	if first.ChannelID == 0 || first.Linked {
		t.Fatal(first)
	}
	var again struct {
		ChannelID int64 `json:"channel_id"`
		Linked    bool  `json:"already_linked"`
	}
	_ = json.Unmarshal(pluginHTTP(t, handler, "POST", "/plugins/opencode-zen-free/connect", "", 200).Body.Bytes(), &again)
	if again.ChannelID != first.ChannelID || !again.Linked {
		t.Fatal("duplicate public channel")
	}
	var visible []map[string]json.RawMessage
	_ = json.Unmarshal(pluginHTTP(t, handler, "GET", "/channels?credential_type=zen_free", "", 200).Body.Bytes(), &visible)
	if len(visible) != 1 || string(visible[0]["models"]) != "[]" || string(visible[0]["available_models"]) != "[]" {
		t.Fatal("empty model inventories must be arrays in public DTOs")
	}
	path := "/channels/" + strconv.FormatInt(first.ChannelID, 10) + "/oauth/models"
	pluginHTTP(t, handler, "POST", path, "", 200)
	c, err := st.Channel(first.ChannelID)
	if err != nil || c.OAuth != nil || len(c.Models) != 0 || len(c.ModelFormats["response"]) != 1 || c.ModelFormats["response"][0] != store.FormatOpenAIResponses || len(c.AvailableModels) != 2 {
		t.Fatal(c.ModelFormats, err)
	}
	consolePluginToggle(t, handler, "opencode-zen-free", false)
	pluginHTTP(t, handler, "POST", path, "", 503)
	pluginHTTP(t, handler, "GET", "/channels?credential_type=zen_free", "", 200)
}
