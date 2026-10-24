package claude

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf16"

	"github.com/google/uuid"
	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"torouter/internal/accessguard"
	"torouter/internal/nativeoauth"
	"torouter/internal/store"
)

const maxRequestBody = 32 << 20

func newDeviceID() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

// object rejects duplicate members at the boundaries we rewrite. Raw messages,
// tool schemas and signed thinking remain the caller's logical content.
func object(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("Claude request field must be a JSON object")
	}
	result := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid Claude JSON object")
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("invalid Claude JSON key")
		}
		if _, ok := result[name]; ok {
			return nil, errors.New("duplicate Claude JSON object member")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("invalid Claude JSON value")
		}
		result[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, errors.New("invalid Claude JSON object end")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("invalid trailing Claude JSON")
	}
	return result, nil
}

func rawString(raw json.RawMessage) string {
	var result string
	_ = json.Unmarshal(raw, &result)
	return result
}
func encoded(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func prepareBody(raw []byte, headers http.Header, credential store.OAuthCredential) ([]byte, string, error) {
	if !validDeviceID(credential.DeviceID) || credential.Account == "" {
		return nil, "", errors.New("Claude credential identity is incomplete; authorize again")
	}
	body, err := object(raw)
	if err != nil {
		return nil, "", err
	}
	metadata := make(map[string]json.RawMessage)
	if value, present := body["metadata"]; present {
		metadata, err = object(value)
		if err != nil {
			return nil, "", err
		}
	}
	identity := make(map[string]json.RawMessage)
	if existing := rawString(metadata["user_id"]); strings.HasPrefix(strings.TrimSpace(existing), "{") {
		identity, err = object([]byte(existing))
		if err != nil {
			return nil, "", err
		}
	}
	session := strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id"))
	if session == "" {
		session = rawString(identity["session_id"])
	}
	if session == "" {
		session = headers.Get("Session-Id")
	}
	if session == "" {
		// The first conversation turn stays stable through tool continuations;
		// callers can disambiguate identical roots with their session header.
		var messages []json.RawMessage
		_ = json.Unmarshal(body["messages"], &messages)
		if len(messages) > 0 {
			session = string(messages[0])
		}
	}
	if parsed, err := uuid.Parse(session); err == nil {
		session = parsed.String()
	} else if session != "" {
		session = uuid.NewSHA1(uuid.NameSpaceOID, []byte("to-router\x00claude\x00"+session)).String()
	} else {
		session = uuid.NewString()
	}
	identity["device_id"] = encoded(credential.DeviceID)
	identity["account_uuid"] = encoded(credential.Account)
	identity["session_id"] = encoded(session)
	metadata["user_id"] = encoded(string(encoded(identity)))
	body["metadata"] = encoded(metadata)
	var system []json.RawMessage
	if value, present := body["system"]; present {
		if len(bytes.TrimSpace(value)) > 0 && bytes.TrimSpace(value)[0] == '"' {
			system = []json.RawMessage{encoded(map[string]string{"type": "text", "text": rawString(value)})}
		} else if json.Unmarshal(value, &system) != nil {
			return nil, "", errors.New("invalid Claude system prompt")
		}
	}
	// Keep incoming native billing attribution (and its continuity tags), while
	// regenerating CCH only after all body edits. Other system blocks survive.
	billing := ""
	if len(system) > 0 {
		var block map[string]json.RawMessage
		_ = json.Unmarshal(system[0], &block)
		text := rawString(block["text"])
		if strings.HasPrefix(text, "x-anthropic-billing-header:") {
			billing = text
			system = system[1:]
		}
	}
	if billing == "" {
		billing = "x-anthropic-billing-header: cc_version=" + profileVersion + "." + buildFingerprint(firstUserText(body), profileVersion) + "; cc_entrypoint=cli; cch=00000;"
	}
	if !strings.Contains(billing, "cch=") {
		at := strings.Index(billing, "cc_entrypoint=")
		if at >= 0 {
			if end := strings.IndexByte(billing[at:], ';'); end >= 0 {
				pos := at + end + 1
				billing = billing[:pos] + " cch=00000;" + billing[pos:]
			}
		}
	}
	blocks := []json.RawMessage{encoded(map[string]string{"type": "text", "text": billing})}
	const agent = "You are Claude Code, Anthropic's official CLI for Claude."
	hasAgent := false
	for _, block := range system {
		var value map[string]json.RawMessage
		_ = json.Unmarshal(block, &value)
		if rawString(value["text"]) == agent {
			hasAgent = true
		}
	}
	if !hasAgent {
		blocks = append(blocks, encoded(map[string]string{"type": "text", "text": agent}))
	}
	body["system"] = encoded(append(blocks, system...))
	result := encoded(body)
	result, err = signBody(result)
	return result, session, err
}

func firstUserText(body map[string]json.RawMessage) string {
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(body["messages"], &messages)
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		if text := rawString(message.Content); text != "" {
			return text
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(message.Content, &blocks)
		for _, block := range blocks {
			if block.Type == "text" {
				return block.Text
			}
		}
		return ""
	}
	return ""
}

// CPA/Claude Code samples UTF-16 positions, not Unicode rune positions.
func buildFingerprint(text, version string) string {
	units := utf16.Encode([]rune(text))
	var sampled [3]uint16
	for i, index := range [...]int{4, 7, 20} {
		sampled[i] = '0'
		if index < len(units) {
			sampled[i] = units[index]
		}
	}
	sum := sha256.Sum256([]byte("59cf53e54c78" + string(utf16.Decode(sampled[:])) + version))
	return hex.EncodeToString(sum[:])[:3]
}

func signBody(body []byte) ([]byte, error) {
	// Locate only system[0].text, never a user's literal cch marker.
	decoder := json.NewDecoder(bytes.NewReader(body))
	_, _ = decoder.Token()
	offset := -1
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid Claude signing body")
		}
		start := int(decoder.InputOffset())
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return nil, errors.New("invalid Claude signing body")
		}
		if token == "system" {
			colon := bytes.IndexByte(body[start:], ':')
			if colon < 0 {
				break
			}
			start += colon + 1
			for start < len(body) && (body[start] == ' ' || body[start] == '\n' || body[start] == '\r' || body[start] == '\t') {
				start++
			}
			var blocks []json.RawMessage
			_ = json.Unmarshal(raw, &blocks)
			if len(blocks) == 0 {
				break
			}
			var block map[string]json.RawMessage
			_ = json.Unmarshal(blocks[0], &block)
			if !strings.HasPrefix(rawString(block["text"]), "x-anthropic-billing-header:") {
				break
			}
			// Marshaled text cannot contain an unescaped quote inside the value.
			textRaw := block["text"]
			at := bytes.Index(raw, textRaw)
			marker := bytes.Index(textRaw, []byte("cch="))
			if at >= 0 && marker >= 0 {
				offset = start + at + marker + 4
			}
			break
		}
	}
	if offset < 0 || offset+5 >= len(body) || body[offset+5] != ';' {
		return nil, errors.New("Claude billing CCH placeholder is missing or invalid")
	}
	result := bytes.Clone(body)
	copy(result[offset:offset+5], "00000")
	normalized, err := normalizeClaudeCCHInput(result)
	if err != nil {
		return nil, errors.New("invalid Claude signing JSON")
	}
	hash := xxHash64.New(0x4D659218E32A3268)
	_, _ = hash.Write(normalized)
	copy(result[offset:offset+5], fmt.Sprintf("%05x", hash.Sum64()&0xFFFFF))
	return result, nil
}

func (d *Driver) Execute(client *http.Client, req *http.Request, credential store.OAuthCredential) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Body == nil {
		return nil, errors.New("invalid Claude Messages request")
	}
	if err := accessguard.Check(req.Context()); err != nil {
		return nil, err
	}
	if credential.AccessToken == "" {
		return nil, errors.New("Claude access token is missing")
	}
	base, err := url.Parse(d.endpoints.APIBase)
	if err != nil {
		return nil, errors.New("invalid Claude Messages endpoint")
	}
	if req.URL.Scheme != base.Scheme || !strings.EqualFold(req.URL.Host, base.Host) {
		return nil, errors.New("Claude credential may only target its registered origin")
	}
	if req.Method != http.MethodPost || req.URL.Path != "/v1/messages" {
		return nil, fmt.Errorf("%w: Claude subscription supports POST /v1/messages", nativeoauth.ErrUnsupportedRequest)
	}
	if !validDeviceID(credential.DeviceID) || credential.Account == "" {
		return nil, errors.New("Claude credential identity is incomplete; authorize again")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody+1))
	if err != nil {
		return nil, safeError(req.Context(), "request reading", err)
	}
	if len(raw) > maxRequestBody {
		return nil, fmt.Errorf("%w: Claude request exceeds size limit", nativeoauth.ErrUnsupportedRequest)
	}
	body, session, err := prepareBody(raw, req.Header, credential)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", nativeoauth.ErrUnsupportedRequest, err)
	}
	upstream := req.Clone(req.Context())
	upstream.Header = req.Header.Clone()
	upstream.Body = io.NopCloser(bytes.NewReader(body))
	upstream.ContentLength = int64(len(body))
	upstream.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	upstream.Header.Del("X-Api-Key")
	upstream.Header.Del("Proxy-Authorization")
	applyProfileHeaders(upstream.Header, credential.AccessToken, session)
	upstream.Header.Set("Anthropic-Beta", requestBetas(body, strings.Join(req.Header.Values("Anthropic-Beta"), ",")))
	if client == nil {
		client = d.client
	}
	client, err = d.profileClient(client, true)
	if err != nil {
		return nil, err
	}
	response, err := accessguard.Do(client, upstream)
	if err != nil {
		return nil, safeError(req.Context(), "Messages request", err)
	}
	decoded, err := decodedBody(response)
	if err != nil {
		response.Body.Close()
		return nil, safeError(req.Context(), "Messages response decoding", err)
	}
	response.Body = decoded
	// Do not consume/rewrite errors or SSE: the host observes true status,
	// truncation, tool/think blocks, and terminal events for its own policy.
	if response.Header.Get("Content-Encoding") != "" {
		response.Header.Del("Content-Encoding")
		response.Header.Del("Content-Length")
		response.ContentLength = -1
	}
	return response, nil
}
