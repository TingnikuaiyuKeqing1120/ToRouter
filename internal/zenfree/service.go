// Package zenfree implements public OpenCode Zen Free HTTP access. It does not
// read a native login, manage an OAuth token or execute an agent.
package zenfree

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/store"
	"torouter/internal/streamreply"
)

const BaseURL = "https://opencode.ai/inference/v1"
const maxBody = 16 << 20

type Endpoints struct{ APIBase, CatalogueURL, ProtocolsURL string }
type Service struct {
	Store     *store.Store
	client    *http.Client
	endpoints Endpoints
	mu        sync.Mutex
	sessions  map[[32]byte]string
}

func NewService(st *store.Store, client *http.Client, ep Endpoints) *Service {
	if ep.APIBase == "" {
		ep.APIBase = BaseURL
	}
	if ep.CatalogueURL == "" {
		ep.CatalogueURL = "https://models.opencode.ai/api.json"
	}
	if ep.ProtocolsURL == "" {
		ep.ProtocolsURL = "https://raw.githubusercontent.com/anomalyco/opencode/dev/services/www/src/docs/content/console/models.mdx"
	}
	return &Service{Store: st, client: client, endpoints: ep, sessions: map[[32]byte]string{}}
}

// Connect creates the public access channel only. Model discovery is explicit.
func (s *Service) Connect(ctx context.Context) (store.Channel, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := accessguard.Check(ctx); err != nil {
		return store.Channel{}, false, err
	}
	rows, err := s.Store.ListChannels()
	if err != nil {
		return store.Channel{}, false, err
	}
	for _, row := range rows {
		if row.CredentialType == store.CredentialZenFree {
			return row, true, nil
		}
	}
	c := store.Channel{Name: "OpenCode Zen Free", Enabled: true, CredentialType: store.CredentialZenFree, Tags: []string{"billing:free"},
		ModelFormats: map[string][]string{}, Credentials: []store.CredentialItem{{ID: "public", Label: "公共访问", APIKey: "public", Enabled: true}},
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIChatCompletions, BaseURL: s.endpoints.APIBase},
			{Format: store.FormatOpenAIResponses, BaseURL: s.endpoints.APIBase},
			{Format: store.FormatAnthropicMessages, BaseURL: s.endpoints.APIBase},
		}}
	if err := accessguard.Check(ctx); err != nil {
		return store.Channel{}, false, err
	}
	err = s.Store.SaveChannel(&c)
	return c, false, err
}

func (s *Service) Bearer(ctx context.Context, c store.Channel) (string, error) {
	if err := accessguard.Check(ctx); err != nil {
		return "", err
	}
	if c.CredentialType != store.CredentialZenFree || c.PrimaryKey() != "public" {
		return "", errors.New("Zen Free requires its public access channel")
	}
	return "public", nil
}

type catalogueModel struct {
	Name           string                           `json:"name"`
	Status         string                           `json:"status"`
	Lifecycle      string                           `json:"lifecycle"`
	Deprecated     bool                             `json:"deprecated"`
	DeprecatedAt   json.RawMessage                  `json:"deprecated_at"`
	RetirementDate json.RawMessage                  `json:"retirement_date"`
	Cost           struct{ Input, Output *float64 } `json:"cost"`
	Provider       struct {
		NPM string `json:"npm"`
	} `json:"provider"`
}
type catalogueProvider struct {
	NPM    string                    `json:"npm"`
	Models map[string]catalogueModel `json:"models"`
}

var protocolTable = regexp.MustCompile("\\|[^|]+\\|\\s*`?([^|`\\s]+)`?\\s*\\|\\s*`[^`]+/v1/(chat/completions|responses|messages|systemone)`")
var pathFormats = map[string]string{"/chat/completions": store.FormatOpenAIChatCompletions, "/responses": store.FormatOpenAIResponses, "/messages": store.FormatAnthropicMessages}

func protocol(npm string) string {
	switch {
	case strings.Contains(npm, "anthropic"):
		return store.FormatAnthropicMessages
	case npm == "@ai-sdk/openai":
		return store.FormatOpenAIResponses
	case strings.Contains(npm, "openai-compatible"):
		return store.FormatOpenAIChatCompletions
	default:
		return ""
	}
}
func (s *Service) read(ctx context.Context, address string, public bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, errors.New("invalid catalogue address")
	}
	if public {
		req.Header.Set("Authorization", "Bearer public")
		req.Header.Set("x-opencode-client", "cli")
	}
	resp, err := s.do(s.client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Zen Free catalogue returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if len(b) > maxBody {
		return nil, errors.New("Zen Free catalogue is too large")
	}
	return b, err
}

// Models requires live listing plus explicit price/protocol evidence. Missing
// evidence is not guessed from a suffix, and a failed fetch preserves the Store.
func (s *Service) Models(ctx context.Context) ([]nativeoauth.Model, error) {
	raw, err := s.read(ctx, strings.TrimRight(s.endpoints.APIBase, "/")+"/models", true)
	if err != nil {
		return nil, err
	}
	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &listed) != nil || listed.Data == nil {
		return nil, errors.New("Zen Free model list has no data array")
	}
	raw, err = s.read(ctx, s.endpoints.CatalogueURL, false)
	if err != nil {
		return nil, err
	}
	var catalogue struct {
		OpenCode catalogueProvider `json:"opencode"`
	}
	if json.Unmarshal(raw, &catalogue) != nil || catalogue.OpenCode.Models == nil {
		return nil, errors.New("OpenCode catalogue has no provider models")
	}
	overrides := map[string]string{}
	// Documentation refines protocols when readable. The catalogue's explicit
	// SDK is sufficient evidence if the optional docs are temporarily unavailable.
	if docs, e := s.read(ctx, s.endpoints.ProtocolsURL, false); e == nil {
		for _, m := range protocolTable.FindAllStringSubmatch(string(docs), -1) {
			overrides[m[1]] = pathFormats["/"+m[2]]
		}
	}
	if err = accessguard.Check(ctx); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	out := []nativeoauth.Model{}
	seen := map[string]bool{}
	for _, item := range listed.Data {
		id := item.ID
		m, ok := catalogue.OpenCode.Models[id]
		if !ok || seen[id] || id == "" || m.Deprecated || m.Status == "retired" || m.Status == "deprecated" || m.Status == "disabled" || m.Lifecycle == "retired" || m.Lifecycle == "deprecated" || m.Lifecycle == "disabled" || nonnull(m.DeprecatedAt) || nonnull(m.RetirementDate) {
			continue
		}
		if m.Cost.Input == nil || m.Cost.Output == nil || *m.Cost.Input != 0 || *m.Cost.Output != 0 {
			continue
		}
		npm := m.Provider.NPM
		if npm == "" {
			npm = catalogue.OpenCode.NPM
		}
		format := protocol(npm)
		if override, exists := overrides[id]; exists {
			format = override
		}
		if format == "" {
			continue
		}
		seen[id] = true
		out = append(out, nativeoauth.Model{ID: id, Name: m.Name, Format: format})
	}
	return out, nil
}
func nonnull(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func (s *Service) do(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return accessguard.Do(&copy, req)
}

func (s *Service) Execute(client *http.Client, req *http.Request, c store.Channel) (*http.Response, error) {
	if _, err := s.Bearer(req.Context(), c); err != nil {
		return nil, err
	}
	fail := func(message string) (*http.Response, error) {
		return nil, fmt.Errorf("%w: Zen Free %s", nativeoauth.ErrUnsupportedRequest, message)
	}
	base, err := url.Parse(s.endpoints.APIBase)
	if err != nil {
		return fail("invalid API address")
	}
	if req.Method != "POST" || req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
		return fail("unsupported API destination")
	}
	suffix := strings.TrimPrefix(req.URL.Path, strings.TrimRight(base.Path, "/"))
	format := pathFormats[suffix]
	if format == "" {
		return fail("unsupported endpoint")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
	req.Body.Close()
	if err != nil || len(raw) > maxBody {
		return fail("invalid request body")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return fail("expected a JSON object")
	}
	var model string
	_ = json.Unmarshal(body["model"], &model)
	if !slices.Contains(c.ModelOutboundFormats(model), format) {
		return fail("model protocol is unknown or changed; refresh the model catalogue")
	}
	var streaming bool
	if raw, exists := body["stream"]; exists && json.Unmarshal(raw, &streaming) != nil {
		return fail("stream must be a boolean")
	}
	// Zen requires the native agent's core tool set. Do not invent executable
	// client tools or silently rename calls just to get an HTTP success.
	if !coreToolsPresent(body["tools"], format) {
		return fail("requires client-declared bash/edit/glob/grep/read tools; use a client supplying these tools or another upstream")
	}
	body["stream"] = json.RawMessage("true")
	if format == store.FormatOpenAIChatCompletions {
		options := map[string]json.RawMessage{}
		if raw, ok := body["stream_options"]; ok && string(raw) != "null" && json.Unmarshal(raw, &options) != nil {
			return fail("invalid stream_options")
		}
		if options == nil {
			options = map[string]json.RawMessage{}
		}
		options["include_usage"] = json.RawMessage("true")
		body["stream_options"], _ = json.Marshal(options)
	}
	wire, _ := json.Marshal(body)
	out, err := http.NewRequestWithContext(req.Context(), "POST", strings.TrimRight(s.endpoints.APIBase, "/")+suffix, bytes.NewReader(wire))
	if err != nil {
		return fail("invalid API address")
	}
	session := s.session(req.Header, body)
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("Accept", "text/event-stream")
	version := "4.0.23"
	if format == store.FormatOpenAIResponses {
		version = "4.0.40"
	}
	if format == store.FormatAnthropicMessages {
		version = "4.0.46"
	}
	out.Header.Set("User-Agent", "opencode/1.19.0 ai-sdk/provider-utils/"+version+" runtime/bun/1.3.14")
	out.Header.Set("x-opencode-client", "cli")
	out.Header.Set("x-opencode-project", "global")
	out.Header.Set("x-opencode-session", session)
	out.Header.Set("x-opencode-session-id", session)
	out.Header.Set("x-opencode-request", newID("msg", false))
	if format == store.FormatAnthropicMessages {
		out.Header.Set("x-api-key", "public")
		out.Header.Set("anthropic-version", "2023-06-01")
		if beta := req.Header.Get("anthropic-beta"); beta != "" {
			out.Header.Set("anthropic-beta", beta)
		}
	} else {
		out.Header.Set("Authorization", "Bearer public")
	}
	resp, err := s.do(client, out)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || streaming {
		return resp, nil
	}
	defer resp.Body.Close()
	assembled, err := streamreply.Assemble(resp.Body, resp.Header.Get("Content-Type"), format)
	if err != nil {
		return nil, fmt.Errorf("Zen Free response already started but could not be assembled: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(assembled))
	resp.ContentLength = int64(len(assembled))
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return resp, nil
}

func coreToolsPresent(raw json.RawMessage, format string) bool {
	var tools []struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return false
	}
	names := map[string]bool{}
	for _, t := range tools {
		n := t.Name
		if format == store.FormatOpenAIChatCompletions {
			n = t.Function.Name
		}
		names[n] = true
	}
	for _, n := range []string{"bash", "edit", "glob", "grep", "read"} {
		if !names[n] {
			return false
		}
	}
	return true
}

// ProbeTools belongs to the console's explicit test client only. Real gateway
// requests keep their own tool declarations and never receive these stand-ins.
func ProbeTools(format string) []any {
	out := []any{}
	for _, t := range []struct{ name, param string }{{"bash", "command"}, {"edit", "filePath"}, {"glob", "pattern"}, {"grep", "pattern"}, {"read", "filePath"}} {
		schema := map[string]any{"type": "object", "properties": map[string]any{t.param: map[string]any{"type": "string"}}, "additionalProperties": false}
		def := map[string]any{"name": t.name, "description": "Disabled in this connectivity test."}
		switch format {
		case store.FormatOpenAIChatCompletions:
			def["parameters"] = schema
			out = append(out, map[string]any{"type": "function", "function": def})
		case store.FormatAnthropicMessages:
			def["input_schema"] = schema
			out = append(out, def)
		default:
			def["type"] = "function"
			def["parameters"] = schema
			out = append(out, def)
		}
	}
	return out
}
func newID(prefix string, descending bool) string {
	value := uint64(time.Now().UnixMilli())<<12 | 1
	if descending {
		value = ^value
	}
	value &= 0xffffffffffff
	var random [14]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(err)
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for i, b := range random {
		random[i] = alphabet[int(b)%len(alphabet)]
	}
	return fmt.Sprintf("%s_%012x%s", prefix, value, string(random[:]))
}

var nativeSession = regexp.MustCompile(`^ses_[0-9a-f]{9}ff[0-9a-f][0-9A-Za-z]{14}$`)

func (s *Service) session(headers http.Header, body map[string]json.RawMessage) string {
	signal := ""
	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id", "conversation-id"} {
		if signal = headers.Get(name); signal != "" {
			break
		}
	}
	if nativeSession.MatchString(signal) {
		return signal
	}
	if signal == "" {
		var messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		raw := body["messages"]
		if raw == nil {
			raw = body["input"]
		}
		if json.Unmarshal(raw, &messages) == nil {
			for _, m := range messages {
				if m.Role == "user" {
					signal = string(m.Content)
					break
				}
			}
		} else {
			signal = string(raw)
		}
	}
	if signal == "" {
		return newID("ses", true)
	}
	key := sha256.Sum256([]byte(signal))
	s.mu.Lock()
	defer s.mu.Unlock()
	if value := s.sessions[key]; value != "" {
		return value
	}
	if len(s.sessions) >= 1000 {
		for k := range s.sessions {
			delete(s.sessions, k)
			break
		}
	}
	value := newID("ses", true)
	s.sessions[key] = value
	return value
}
