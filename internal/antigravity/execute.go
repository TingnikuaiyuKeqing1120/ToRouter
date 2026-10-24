package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"torouter/internal/nativeoauth"
	"torouter/internal/store"
)

// Execute accepts the standard Gemini URL/body prepared by the gateway. The
// envelope is a transport concern; no schema, model, reasoning or retry policy
// belongs here. Non-success HTTP status and vendor error reasons survive.
func (d *Driver) Execute(client *http.Client, original *http.Request, c store.OAuthCredential) (*http.Response, error) {
	if original == nil || original.URL == nil || original.Body == nil {
		return nil, fmt.Errorf("%w: antigravity requires a Gemini request body and URL", nativeoauth.ErrUnsupportedRequest)
	}
	defer original.Body.Close()
	if c.AccessToken == "" || strings.TrimSpace(c.ProjectID) == "" {
		return nil, errors.New("antigravity: missing project or access token; authorize again")
	}
	modelAction := strings.Split(original.URL.Path, "/models/")
	if len(modelAction) != 2 {
		return nil, fmt.Errorf("%w: antigravity requires a Gemini models URL", nativeoauth.ErrUnsupportedRequest)
	}
	model, action, ok := strings.Cut(modelAction[1], ":")
	if !ok || model == "" || strings.ContainsAny(model, "/\\?#") || action != "generateContent" && action != "streamGenerateContent" {
		return nil, fmt.Errorf("%w: antigravity requires a Gemini generateContent or streamGenerateContent model action", nativeoauth.ErrUnsupportedRequest)
	}
	b, err := readBounded(original.Body, maxInferenceBody)
	if err != nil {
		return nil, err
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(b, &request) != nil || request == nil {
		return nil, fmt.Errorf("%w: antigravity requires a Gemini JSON object", nativeoauth.ErrUnsupportedRequest)
	}
	if raw := request["sessionId"]; len(raw) == 0 || string(raw) == `""` || string(raw) == "null" {
		request["sessionId"], _ = json.Marshal(sessionID(request))
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	typeOfRequest := "agent"
	requestID := "agent-" + id.String()
	if strings.Contains(model, "image") {
		typeOfRequest = "image_gen"
		requestID = "image_gen/" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "/" + id.String() + "/12"
	}
	b, err = json.Marshal(map[string]any{"model": model, "project": c.ProjectID, "userAgent": "antigravity", "requestType": typeOfRequest, "requestId": requestID, "request": request})
	if err != nil {
		return nil, errors.New("antigravity: invalid request envelope")
	}
	endpoint := strings.TrimRight(d.endpoints.APIBase, "/") + "/v1internal:" + action
	stream := action == "streamGenerateContent"
	if stream {
		endpoint += "?alt=sse"
	}
	req, err := http.NewRequestWithContext(original.Context(), http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("antigravity: invalid API endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if client == nil {
		client = d.client
	}
	resp, err := do(d.profileClient(client, c), req)
	if err != nil {
		return nil, networkError(req.Context(), "inference", err)
	}
	secrets := []string{c.AccessToken, c.RefreshToken, c.Account, c.Subject, c.ProjectID}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := readBounded(resp.Body, maxControlBody)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		replaceBody(resp, safeJSON(body, secrets...))
		return resp, nil
	}
	if stream {
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Body.Close()
			return nil, errors.New("antigravity: expected upstream SSE response")
		}
		resp.Body = newEventBody(original.Context(), resp.Body, secrets)
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		resp.Header.Del("Content-Encoding")
		return resp, nil
	}
	body, err := readBounded(resp.Body, maxInferenceBody)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	body, err = unwrap(body, secrets)
	if err != nil {
		return nil, err
	}
	replaceBody(resp, body)
	return resp, nil
}

func replaceBody(resp *http.Response, body []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	resp.Header.Del("Content-Encoding")
}

func unwrap(body []byte, secrets []string) ([]byte, error) {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil || env == nil {
		return nil, errors.New("antigravity: invalid response envelope")
	}
	// Error is a genuine upstream failure, including errors delivered after
	// HTTP 200. Keep it in-band so the gateway can observe the actual failure.
	if raw := env["error"]; len(raw) > 0 && string(raw) != "null" {
		return safeJSON(body, secrets...), nil
	}
	raw, exists := env["response"]
	if !exists || len(raw) == 0 || string(raw) == "null" || raw[0] != '{' {
		return nil, errors.New("antigravity: missing response in envelope")
	}
	return raw, nil
}

// eventBody parses SSE records independently of network reads. It is lazy and
// has no producer goroutine: Close immediately closes the original response;
// cancellation also unblocks a body read, even with a custom RoundTripper.
type eventBody struct {
	ctx      context.Context
	reader   *bufio.Reader
	upstream io.ReadCloser
	pending  []byte
	secrets  []string
	stop     func() bool
	once     sync.Once
}

func newEventBody(ctx context.Context, body io.ReadCloser, secrets []string) *eventBody {
	b := &eventBody{ctx: ctx, reader: bufio.NewReader(body), upstream: body, secrets: secrets}
	b.stop = context.AfterFunc(ctx, func() { _ = body.Close() })
	return b
}
func (b *eventBody) Close() error {
	var err error
	b.once.Do(func() { b.stop(); err = b.upstream.Close() })
	return err
}
func (b *eventBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	for len(b.pending) == 0 {
		record, err := b.record()
		if err != nil {
			b.Close()
			if b.ctx.Err() != nil {
				return 0, b.ctx.Err()
			}
			return 0, err
		}
		b.pending = record
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *eventBody) record() ([]byte, error) {
	var lines [][]byte
	var data [][]byte
	total := 0
	for {
		var line []byte
		for {
			piece, err := b.reader.ReadSlice('\n')
			total += len(piece)
			if total > maxInferenceBody {
				return nil, errors.New("antigravity: SSE event exceeds size limit")
			}
			line = append(line, piece...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				if err == io.EOF && (len(lines) > 0 || len(data) > 0 || len(line) > 0) {
					return nil, io.ErrUnexpectedEOF
				}
				if err == io.EOF {
					return nil, io.EOF
				}
				return nil, errors.New("antigravity: SSE response read failed")
			}
			break
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			break
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			value := line[5:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			data = append(data, value)
		} else {
			lines = append(lines, line)
		}
	}
	var out bytes.Buffer
	for _, line := range lines {
		out.Write(line)
		out.WriteByte('\n')
	}
	if len(data) > 0 {
		payload := bytes.Join(data, []byte("\n"))
		if !bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			var err error
			payload, err = unwrap(payload, b.secrets)
			if err != nil {
				return nil, err
			}
		}
		// RawMessage can contain newlines; each line remains an SSE data field.
		for _, line := range bytes.Split(payload, []byte("\n")) {
			out.WriteString("data: ")
			out.Write(line)
			out.WriteByte('\n')
		}
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}
