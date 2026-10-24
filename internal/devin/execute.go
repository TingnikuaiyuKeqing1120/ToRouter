package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"torouter/internal/nativeoauth"
	"torouter/internal/store"
)

var _ nativeoauth.Driver = (*Driver)(nil)

// Execute translates one full-context Responses request into one private RPC.
// It never retries inference, invokes a CLI, or changes the client's tools.
func (d *Driver) Execute(client *http.Client, original *http.Request, oauth store.OAuthCredential) (*http.Response, error) {
	if original == nil || original.Body == nil {
		return nil, unsupported(errors.New("requires a Responses body"))
	}
	defer original.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(original.Body, maxWire+1))
	if err != nil || len(raw) > maxWire {
		return nil, unsupported(errors.New("request exceeds the readable size limit"))
	}
	input, err := parseRequest(raw, original.Header, oauth)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(original.Context(), http.MethodPost, strings.TrimRight(d.endpoints.ServerURL, "/")+chatPath, bytes.NewReader(input.Wire))
	if err != nil {
		return nil, unsupported(errors.New("invalid RPC endpoint"))
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Accept-Encoding", "gzip")
	req.Header.Set("Authorization", "Basic "+oauth.AccessToken)
	if client == nil {
		client = d.client
	}
	resp, err := d.do(client, req)
	if err != nil {
		return nil, err
	}
	secrets := []string{oauth.AccessToken, oauth.Account, oauth.Subject}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxWire+1))
		resp.Body.Close()
		if readErr != nil || len(raw) > maxWire {
			return nil, errors.New("Devin error response could not be read")
		}
		return errorResponse(resp, rpcFailure(raw, resp.StatusCode, secrets)), nil
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "application/connect+proto") {
		resp.Body.Close()
		return nil, errors.New("Devin returned an unexpected RPC response type")
	}
	body := &responseBody{
		ctx: original.Context(), upstream: resp.Body, input: input, secrets: secrets,
		id: "resp_" + randomID(), created: time.Now().Unix(), textIndex: -1, thoughtIndex: -1,
		lastTool: -1, tools: map[string]int{}, output: []jsonObject{},
	}
	body.stop = context.AfterFunc(body.ctx, func() { _ = body.upstream.Close() })
	// Before returning HTTP 200, inspect the first frame so a genuine immediate
	// rejection remains an HTTP error and can use the gateway's existing policy.
	flags, data, err := readFrame(body.upstream)
	if err != nil {
		body.Close()
		return nil, errors.New("Devin RPC ended before its first frame")
	}
	if flags&2 != 0 {
		body.Close()
		failure := rpcFailure(data, 200, secrets)
		if failure == nil {
			return nil, errors.New("Devin RPC ended without a model terminal")
		}
		return errorResponse(resp, failure), nil
	}
	body.first = data
	if input.Streaming {
		body.event("response.created", jsonObject{"response": body.response("in_progress")})
		body.event("response.in_progress", jsonObject{"response": body.response("in_progress")})
		resp.Body = body
		resp.Header = http.Header{"Content-Type": {"text/event-stream"}}
		resp.ContentLength = -1
		return resp, nil
	}
	defer body.Close()
	for !body.done {
		if err = body.step(); err != nil {
			return nil, err // possibly executed; never turn it into a local rejection
		}
		body.pending.Reset()
	}
	raw, err = json.Marshal(body.response(body.status()))
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	resp.ContentLength = int64(len(raw))
	return resp, nil
}

type rpcError struct {
	status        int
	code, message string
}

func (e *rpcError) Error() string { return "Devin RPC: " + e.message }

var privateEmail = regexp.MustCompile(`[A-Za-z0-9.!#$%&'*+/=?^_\x60{|}~-]+@[A-Za-z0-9.-]+`)
var privateBearer = regexp.MustCompile(`(?i)(?:bearer\s+|(?:access_token|refresh_token|session_token|authorization)\s*[=:]\s*)[^\s,;"<>]+`)

// Only a structured vendor message is exposed, bounded and with credentials
// removed. Arbitrary HTML/protobuf or a body echo never becomes diagnostic text.
func rpcFailure(raw []byte, status int, secrets []string) *rpcError {
	var value struct {
		Code, Message string
		Error         *struct{ Code, Message string }
	}
	valid := json.Unmarshal(raw, &value) == nil
	if valid && value.Error != nil {
		value.Code, value.Message = value.Error.Code, value.Error.Message
	}
	if status == 200 && valid && value.Error == nil && value.Code == "" {
		return nil
	}
	if !valid && status >= 400 {
		return &rpcError{status, "upstream_error", http.StatusText(status)}
	}
	if !valid || status == 200 && value.Code == "" {
		return &rpcError{502, "invalid_rpc_terminal", "invalid upstream terminal"}
	}
	switch value.Code {
	case "unauthenticated":
		status = 401
	case "permission_denied":
		status = 403
	case "resource_exhausted":
		status = 429
	case "invalid_argument", "out_of_range":
		status = 400
	case "unavailable":
		status = 503
	default:
		if status < 400 {
			status = 502
		}
	}
	for _, secret := range secrets {
		if secret != "" {
			value.Message = strings.ReplaceAll(value.Message, secret, "[redacted]")
			value.Code = strings.ReplaceAll(value.Code, secret, "[redacted]")
		}
	}
	value.Message = privateEmail.ReplaceAllString(value.Message, "[redacted]")
	value.Message = privateBearer.ReplaceAllString(value.Message, "[redacted]")
	if len(value.Message) > 1024 {
		value.Message = string([]rune(value.Message)[:min(256, len([]rune(value.Message)))])
	}
	if len(value.Code) > 80 {
		value.Code = "upstream_error"
	}
	if value.Message == "" {
		value.Message = http.StatusText(status)
	}
	return &rpcError{status, value.Code, value.Message}
}
func errorResponse(resp *http.Response, failure *rpcError) *http.Response {
	raw, _ := json.Marshal(jsonObject{"error": jsonObject{"type": "upstream_error", "code": failure.code, "message": failure.message}})
	resp.StatusCode, resp.Status = failure.status, http.StatusText(failure.status)
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	return resp
}

type jsonObject map[string]any
type outputState struct {
	text      string
	partial   []byte
	signature signatureState
}

// Lazy conversion provides backpressure with no producer goroutine. A Close or
// context cancellation closes the actual RPC body, including during first read.
type responseBody struct {
	ctx                               context.Context
	upstream                          io.ReadCloser
	stop                              func() bool
	once                              sync.Once
	input                             request
	secrets                           []string
	id                                string
	created                           int64
	sequence, consumed                int
	first                             []byte
	pending                           bytes.Buffer
	output                            []jsonObject
	states                            []outputState
	textIndex, thoughtIndex, lastTool int
	tools                             map[string]int
	usage                             jsonObject
	counts                            [4]uint64
	hasCount                          [4]bool
	reason                            uint64
	done                              bool
}

func (b *responseBody) Close() error {
	var err error
	b.once.Do(func() { b.stop(); err = b.upstream.Close() })
	return err
}
func (b *responseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	for b.pending.Len() == 0 {
		if b.done {
			b.Close()
			return 0, io.EOF
		}
		if err := b.step(); err != nil {
			b.Close()
			return 0, err
		}
	}
	return b.pending.Read(p)
}
func (b *responseBody) event(name string, value jsonObject) {
	if !b.input.Streaming {
		return
	}
	value["type"], value["sequence_number"] = name, b.sequence
	b.sequence++
	raw, _ := json.Marshal(value)
	fmt.Fprintf(&b.pending, "event: %s\ndata: %s\n\n", name, raw)
}
func (b *responseBody) response(status string) jsonObject {
	result := jsonObject{"id": b.id, "object": "response", "created_at": b.created, "model": b.input.Model,
		"status": status, "output": b.output, "usage": b.usage, "store": false, "error": nil, "incomplete_details": nil}
	if status == "incomplete" {
		result["incomplete_details"] = jsonObject{"reason": "max_output_tokens"}
	}
	return result
}
func (b *responseBody) status() string {
	if b.reason == 3 {
		return "incomplete"
	}
	return "completed"
}
func (b *responseBody) add(item jsonObject) int {
	i := len(b.output)
	item["id"], item["status"] = fmt.Sprintf("item_%s_%d", b.id, i), "in_progress"
	b.output = append(b.output, item)
	b.states = append(b.states, outputState{})
	b.event("response.output_item.added", jsonObject{"output_index": i, "item": item})
	return i
}
func (b *responseBody) ensureText(thought bool) int {
	index := &b.textIndex
	if thought {
		index = &b.thoughtIndex
	}
	if *index >= 0 && *index == len(b.output)-1 {
		return *index
	}
	item := jsonObject{"type": "message", "role": "assistant", "content": []any{}}
	if thought {
		item = jsonObject{"type": "reasoning", "summary": []any{}}
	}
	*index = b.add(item)
	name, position, kind := "response.content_part.added", "content_index", "output_text"
	if thought {
		name, position, kind = "response.reasoning_summary_part.added", "summary_index", "summary_text"
	}
	part := jsonObject{"type": kind, "text": ""}
	if !thought {
		part["annotations"] = []any{}
	}
	b.event(name, jsonObject{"output_index": *index, "item_id": item["id"], position: 0, "part": part})
	return *index
}
func (b *responseBody) appendText(index int, data []byte, name string, position string) error {
	s := &b.states[index]
	s.partial = append(s.partial, data...)
	n := 0
	for n < len(s.partial) {
		if !utf8.FullRune(s.partial[n:]) {
			break
		}
		r, size := utf8.DecodeRune(s.partial[n:])
		if r == utf8.RuneError && size == 1 {
			return errors.New("Devin returned invalid UTF-8")
		}
		n += size
	}
	value := string(s.partial[:n])
	s.partial = append(s.partial[:0], s.partial[n:]...)
	s.text += value
	if value != "" {
		event := jsonObject{"output_index": index, "item_id": b.output[index]["id"], "delta": value}
		if position != "" {
			event[position] = 0
		}
		b.event(name, event)
	}
	return nil
}
func (b *responseBody) step() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	var flags byte
	data := b.first
	b.first = nil
	if data == nil {
		var err error
		flags, data, err = readFrame(b.upstream)
		if err != nil {
			return errors.New("Devin RPC was truncated before its terminal")
		}
	}
	b.consumed += len(data)
	if b.consumed > maxWire {
		return errors.New("Devin RPC response exceeds size limit")
	}
	if flags&2 != 0 {
		if failure := rpcFailure(data, 200, b.secrets); failure != nil {
			return failure
		}
		return b.finish()
	}
	values, err := fields(data)
	if err != nil {
		return err
	}
	for _, f := range values {
		switch f.number {
		case 3, 6, 7, 9, 10, 21:
			if f.kind != 2 {
				return errors.New("Devin returned a malformed semantic RPC field")
			}
		case 5:
			if f.kind != 0 {
				return errors.New("Devin returned a malformed model terminal")
			}
		}
		switch {
		case f.number == 5 && f.kind == 0:
			if f.n != 0 {
				if b.reason != 0 && b.reason != f.n {
					return errors.New("Devin returned conflicting terminals")
				}
				b.reason = f.n
			}
		case f.number == 3 && f.kind == 2 && len(f.data) > 0:
			if err = b.appendText(b.ensureText(false), f.data, "response.output_text.delta", "content_index"); err != nil {
				return err
			}
		case f.number == 9 && f.kind == 2 && len(f.data) > 0:
			if err = b.appendText(b.ensureText(true), f.data, "response.reasoning_summary_text.delta", "summary_index"); err != nil {
				return err
			}
		case f.number == 10 && f.kind == 2:
			i := b.thoughtIndex
			if i < 0 {
				i = b.ensureText(true)
			}
			b.states[i].signature.Data = append(b.states[i].signature.Data, f.data...)
		case f.number == 21 && f.kind == 2:
			if !utf8.Valid(f.data) {
				return errors.New("Devin returned an invalid signature type")
			}
			i := b.thoughtIndex
			if i < 0 {
				i = b.ensureText(true)
			}
			old := b.states[i].signature.Type
			if old != "" && old != string(f.data) {
				return errors.New("Devin returned conflicting reasoning signature types")
			}
			b.states[i].signature.Type = string(f.data)
		case f.number == 6 && f.kind == 2:
			if err = b.tool(f.data); err != nil {
				return err
			}
		case f.number == 7 && f.kind == 2:
			if err = b.readUsage(f.data); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *responseBody) tool(raw []byte) error {
	values, err := fields(raw)
	if err != nil {
		return err
	}
	id, name := "", ""
	var args []byte
	for _, f := range values {
		switch {
		case f.kind == 2 && f.number == 1:
			id = string(f.data)
		case f.kind == 2 && f.number == 2:
			name = string(f.data)
		case f.kind == 2 && f.number == 3:
			args = append(args, f.data...)
		case f.kind == 2 && (f.number == 4 || f.number == 5) && len(f.data) > 0:
			return errors.New("Devin returned invalid tool arguments")
		case f.kind == 0 && f.number == 6 && f.n != 0:
			return errors.New("Devin returned an unsupported custom tool")
		}
	}
	i := b.lastTool
	if id != "" {
		var found bool
		i, found = b.tools[id]
		if !found {
			if name == "" || !b.input.Tools[name] || !utf8.ValidString(id) || !utf8.ValidString(name) {
				return errors.New("Devin returned an unidentified or undeclared tool")
			}
			i = b.add(jsonObject{"type": "function_call", "call_id": outID(id), "name": name, "arguments": ""})
			b.tools[id] = i
		} else if name != "" && name != b.output[i]["name"] {
			return errors.New("Devin changed a tool name")
		}
	} else if name != "" {
		// Inventing a call ID would make the next request impossible to pair.
		return errors.New("Devin returned a tool without its call ID")
	}
	if i < 0 {
		return errors.New("Devin returned tool arguments without a call")
	}
	b.lastTool = i
	return b.appendText(i, args, "response.function_call_arguments.delta", "")
}
func (b *responseBody) readUsage(raw []byte) error {
	values, err := fields(raw)
	if err != nil {
		return err
	}
	for _, f := range values {
		if f.kind == 0 && f.number >= 2 && f.number <= 5 {
			if f.n > 1<<40 {
				return errors.New("Devin returned invalid usage")
			}
			b.counts[f.number-2] = f.n
			b.hasCount[f.number-2] = true
		}
		if f.kind == 0 && f.number == 6 && f.n >= 400 {
			return errors.New("Devin reported a failed model request")
		}
	}
	if b.hasCount[0] || b.hasCount[1] || b.hasCount[2] || b.hasCount[3] {
		// Keep explicitly reported counters across frames. An absent counter
		// is not an estimate of zero and a later status-only frame erases none.
		b.usage = jsonObject{}
		input := b.counts[0] + b.counts[2] + b.counts[3]
		if b.hasCount[0] {
			b.usage["input_tokens"] = input
		}
		if b.hasCount[1] {
			b.usage["output_tokens"] = b.counts[1]
		}
		if b.hasCount[0] && b.hasCount[1] {
			b.usage["total_tokens"] = input + b.counts[1]
		}
		details := jsonObject{}
		if b.hasCount[2] {
			details["cache_write_tokens"] = b.counts[2]
		}
		if b.hasCount[3] {
			details["cached_tokens"] = b.counts[3]
		}
		if len(details) > 0 {
			b.usage["input_tokens_details"] = details
		}
	}
	return nil
}
func (b *responseBody) finish() error {
	switch b.reason {
	case 2, 3, 4, 10:
	default:
		return errors.New("Devin RPC has no supported model terminal")
	}
	if b.reason == 10 && len(b.tools) == 0 || len(b.tools) > 0 && b.reason != 10 && b.reason != 3 {
		return errors.New("Devin tool output contradicts the model terminal")
	}
	for i, item := range b.output {
		s := b.states[i]
		if len(s.partial) > 0 {
			return errors.New("Devin returned an incomplete UTF-8 fragment")
		}
		item["status"] = b.status()
		switch item["type"] {
		case "message", "reasoning":
			thought := item["type"] == "reasoning"
			name, position, kind, property := "response.output_text", "content_index", "output_text", "content"
			if thought {
				name, position, kind, property = "response.reasoning_summary_text", "summary_index", "summary_text", "summary"
			}
			part := jsonObject{"type": kind, "text": s.text}
			if !thought {
				part["annotations"] = []any{}
			}
			item[property] = []any{part}
			if len(s.signature.Data) > 0 {
				item["encrypted_content"] = encodeSignature(s.signature)
			}
			b.event(name+".done", jsonObject{"output_index": i, "item_id": item["id"], position: 0, "text": s.text})
			partEvent := "response.content_part.done"
			if thought {
				partEvent = "response.reasoning_summary_part.done"
			}
			b.event(partEvent, jsonObject{"output_index": i, "item_id": item["id"], position: 0, "part": part})
		case "function_call":
			if b.reason != 3 {
				var args map[string]json.RawMessage
				if json.Unmarshal([]byte(s.text), &args) != nil || args == nil {
					return errors.New("Devin returned incomplete or non-object tool arguments")
				}
			}
			item["arguments"] = s.text
			b.event("response.function_call_arguments.done", jsonObject{"output_index": i, "item_id": item["id"], "arguments": s.text})
		}
		b.event("response.output_item.done", jsonObject{"output_index": i, "item": item})
	}
	b.done = true
	b.event("response."+b.status(), jsonObject{"response": b.response(b.status())})
	return nil
}
