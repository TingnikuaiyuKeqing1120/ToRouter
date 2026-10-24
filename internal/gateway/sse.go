package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"torouter/internal/gateway/convert"
	"torouter/internal/store"
)

// Observing bytes must never change the wire representation of native streams.
// format is the upstream endpoint's full format name, not the wire family: the
// two OpenAI facades report usage with different schemas (T024).
type observer struct {
	reader             io.Reader
	format             string
	usage              *store.Usage
	hasUsage, complete bool
	line, event        []byte
	pending            error
	responseTerminal   string
	incompleteReason   string
	responseBody       []byte
	onOutput           func(int64)
	onTextUnits        func(int64)
}

func newObserver(r io.Reader, format string, u *store.Usage) *observer {
	return &observer{reader: r, format: format, usage: u}
}
func (o *observer) Read(p []byte) (int, error) {
	if o.pending != nil {
		return 0, o.pending
	}
	n, err := o.reader.Read(p)
	for _, b := range p[:n] {
		if b == '\n' {
			if e := o.processLine(); e != nil {
				o.pending = e
				break
			}
			o.line = o.line[:0]
		} else {
			o.line = append(o.line, b)
			if len(o.line) > 8<<20 {
				o.pending = errors.New("SSE line exceeds 8 MiB")
				break
			}
		}
	}
	if err == io.EOF && (len(o.line) > 0 || len(o.event) > 0) {
		o.pending = errors.New("truncated SSE event")
	}
	if n > 0 {
		return n, nil
	}
	if o.pending != nil {
		return 0, o.pending
	}
	return n, err
}
func (o *observer) processLine() error {
	line := bytes.TrimSuffix(o.line, []byte("\r"))
	if len(line) == 0 {
		if len(o.event) == 0 {
			return nil
		}
		data := o.event
		o.event = nil
		return o.observe(data)
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		d := line[5:]
		if len(d) > 0 && d[0] == ' ' {
			d = d[1:]
		}
		if len(o.event) > 0 {
			o.event = append(o.event, '\n')
		}
		o.event = append(o.event, d...)
		if len(o.event) > 8<<20 {
			return errors.New("SSE event exceeds 8 MiB")
		}
	}
	return nil
}
func (o *observer) observe(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		// Chat's sentinel is not a Responses terminal event. Some native
		// endpoints append it after their typed terminal, so leave it intact.
		if o.format != store.FormatOpenAIResponses {
			o.complete = true
		}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return errors.New("invalid SSE JSON payload")
	}
	if raw, ok := obj["error"]; ok && string(raw) != "null" && o.format != store.FormatOpenAIResponses {
		return errors.New("upstream stream error")
	}
	// Text precedes usage: a count reported in the same event calibrates all
	// text seen through that event. This observation never enters billing.
	if o.onTextUnits != nil {
		if units := companionTextUnits(obj, o.format); units > 0 {
			o.onTextUnits(units)
		}
	}
	in, out, cr, cw, ok := convert.ParseUsage(data, o.format)
	if ok {
		o.hasUsage = true
		o.usage.InputTokens = max(o.usage.InputTokens, in)
		o.usage.OutputTokens = max(o.usage.OutputTokens, out)
		o.usage.CacheReadTokens = max(o.usage.CacheReadTokens, cr)
		o.usage.CacheWriteTokens = max(o.usage.CacheWriteTokens, cw)
		if o.onOutput != nil {
			if output, reported := convert.ReportedOutputTokens(data, o.format); reported {
				o.onOutput(output)
			}
		}
	}
	var eventType string
	_ = json.Unmarshal(obj["type"], &eventType)
	if o.format == store.FormatOpenAIResponses {
		switch eventType {
		case "response.completed", "response.incomplete", "response.failed", "error":
			o.complete = true
			o.responseTerminal = eventType
			o.responseBody = obj["response"]
			if eventType == "response.incomplete" {
				o.incompleteReason = incompleteReason(obj["response"])
			}
		}
		if raw, ok := obj["error"]; ok && string(raw) != "null" {
			o.complete = true
			o.responseTerminal = "error"
		}
		// Read native terminal events through unchanged. The gateway grades
		// completed, incomplete and failed separately once the wire is drained.
		return nil
	}
	if eventType == "message_stop" {
		o.complete = true
	}
	if eventType == "response.failed" || eventType == "response.incomplete" || eventType == "error" {
		return errors.New("upstream stream error")
	}
	if familyOf(o.format) == "gemini" {
		var candidates []struct {
			FinishReason string `json:"finishReason"`
		}
		_ = json.Unmarshal(obj["candidates"], &candidates)
		for _, c := range candidates {
			if c.FinishReason != "" {
				o.complete = true
			}
		}
	}
	return nil
}

// Persist only the documented reason codes. An unknown upstream string can
// contain request content, so it is not copied into diagnostics or logs.
func incompleteReason(response []byte) string {
	var shape struct {
		Details struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	_ = json.Unmarshal(response, &shape)
	switch shape.Details.Reason {
	case "max_output_tokens", "content_filter":
		return shape.Details.Reason
	default:
		return "unknown"
	}
}
