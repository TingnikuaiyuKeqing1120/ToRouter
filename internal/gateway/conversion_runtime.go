package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"torouter/internal/gateway/convert"
	route "torouter/internal/router"
	"torouter/internal/store"
)

const requestIDHeader = "X-To-Router-Request-ID"

// Scope is derived from authenticated identity, never a client-supplied session
// or header. A stable, domain-separated signing key lets inline attachments
// survive a restart without persisting conversations or another secret.
func (g *Gateway) conversionOptions(settings store.Settings, clientID int64, publicModel string, a route.Attempt, credentialID string) convert.Options {
	g.reasoningOnce.Do(func() { g.reasoningBridge = convert.NewReasoningBridge(512, 24*time.Hour) })
	signing := sha256.Sum256([]byte("to-router/reasoning-attachment/v1\x00" + settings.GatewayKey))
	sourceBytes, _ := json.Marshal([]any{a.Channel.ID, a.Channel.Generation, a.Endpoint.Format, a.Endpoint.BaseURL, a.Model, credentialID})
	source := sha256.Sum256(sourceBytes)
	return convert.Options{
		AllowReasoningDrop: settings.ReasoningPolicy == store.ReasoningAvailability,
		Scope:              fmt.Sprintf("client:%d/model:%s", clientID, publicModel),
		Source:             hex.EncodeToString(source[:]), SigningKey: signing[:], Bridge: g.reasoningBridge,
	}
}

func conversionFailure(err error, phase string) *ConversionDiagnostic {
	code := "conversion_unsupported"
	switch {
	case errors.Is(err, convert.ErrReasoningState):
		code = "reasoning_state_unavailable"
	case errors.Is(err, convert.ErrContinuationState):
		code = "reasoning_not_representable"
	case err != nil:
		// Only classify known schema categories; never copy arbitrary error text
		// (which can contain upstream output) into a diagnostic.
		message := err.Error()
		switch {
		case strings.Contains(message, "previous_response_id"), strings.Contains(message, "conversation"), strings.Contains(message, "item_reference"), strings.Contains(message, "compaction"), strings.Contains(message, "cachedContent"):
			code = "context_unavailable"
		case strings.Contains(message, "unmatched tool"), strings.Contains(message, "unknown tool call"), strings.Contains(message, "tool result without"), strings.Contains(message, "duplicate tool"):
			code = "tool_context_mismatch"
		case errors.Is(err, convert.ErrMalformed):
			code = "conversion_invalid_payload"
		}
	}
	d := newConversionDiagnostic(code, phase)
	if err != nil {
		for _, field := range []string{"previous_response_id", "conversation", "item_reference", "compaction", "cachedContent", "response_format", "json_schema", "strict", "stop_sequence", "parallel_tool_calls", "reasoning_effort", "thinking", "encrypted_content", "tool_choice", "image_url", "input_image", "inlineData", "fileData"} {
			if strings.Contains(err.Error(), field) {
				d.Facts = append(d.Facts, "受影响的字段或内容类别："+field+"。")
			}
		}
	}
	return d
}

func setUsageDiagnostic(u *store.Usage, d *ConversionDiagnostic) {
	if d == nil {
		return
	}
	copy := *d
	copy.RequestID = u.RequestID
	copy.Facts = append([]string{}, d.Facts...)
	if u.Inbound != "" && u.Outbound != "" {
		copy.Facts = append(copy.Facts, "本次格式："+u.Inbound+" → "+u.Outbound+"。")
	}
	u.Diagnostic, _ = json.Marshal(copy)
}

func usageDiagnostic(u store.Usage) *ConversionDiagnostic {
	if len(u.Diagnostic) == 0 {
		return nil
	}
	var d ConversionDiagnostic
	if json.Unmarshal(u.Diagnostic, &d) != nil {
		return nil
	}
	return &d
}

func diagnosticRecovery(rec *recovery, d *ConversionDiagnostic) *recovery {
	if d == nil {
		return rec
	}
	if rec == nil {
		rec = &recovery{Code: d.Code}
	} else {
		c := *rec
		rec = &c
	}
	rec.Diagnostic = d
	return rec
}

func diagnosticForRecovery(rec *recovery) *ConversionDiagnostic {
	if rec == nil {
		return nil
	}
	if rec.Diagnostic != nil {
		return rec.Diagnostic
	}
	switch rec.Code {
	case "state_affinity_conflict", "state_domain_changed", "state_affinity_unavailable", "state_affinity_unknown", "siwc_server_state_unsupported":
		return newConversionDiagnostic("context_unavailable", "routing")
	case "conversion_refused":
		return newConversionDiagnostic("conversion_unsupported", "response_conversion")
	case "upstream_may_have_processed":
		return newConversionDiagnostic("execution_unconfirmed", "transport")
	}
	return nil
}

func completeUsageDiagnostic(u *store.Usage) {
	if len(u.Diagnostic) > 0 {
		return
	}
	if u.Error != "" && u.Status != 499 {
		code, phase := "upstream_rejected", "upstream_response"
		if strings.Contains(u.Error, "stream") || strings.Contains(u.Error, "processed") || strings.Contains(u.Error, "incomplete") || strings.Contains(u.Error, "invalid JSON") {
			code, phase = "execution_unconfirmed", "transport"
		}
		setUsageDiagnostic(u, newConversionDiagnostic(code, phase))
		return
	}
	actualDrops := strings.ReplaceAll(u.Degraded, "thought_signature_bypass", "")
	if actualDrops != "" && (strings.Contains(actualDrops, "reasoning") || strings.Contains(actualDrops, "thinking") || strings.Contains(actualDrops, "thought")) {
		d := newConversionDiagnostic("reasoning_state_dropped", "response_conversion")
		d.ActionsTaken = []string{"已按可用优先策略移除日志中列出的思考材料。"}
		setUsageDiagnostic(u, d)
	} else if strings.Contains(u.Degraded, "cache_control_removed") {
		setUsageDiagnostic(u, newConversionDiagnostic("cache_hint_removed", "request_conversion"))
	} else if strings.Contains(u.Degraded, "thought_signature_bypass") {
		setUsageDiagnostic(u, newConversionDiagnostic("reasoning_signature_bypass", "request_conversion"))
	}
}

func writeDegradationHeader(w http.ResponseWriter, u store.Usage) {
	if u.Degraded != "" {
		w.Header().Set("X-To-Router-Degraded", u.Degraded)
	}
}

// Same structured diagnostic is emitted whether the client is still awaiting
// HTTP headers or has already received part of an SSE response.
func writeStreamDiagnostic(w io.Writer, format string, d *ConversionDiagnostic) {
	errorBody := map[string]any{"type": "gateway_error", "message": d.Message, "diagnostic": d}
	var value any = map[string]any{"error": errorBody}
	event := ""
	switch format {
	case store.FormatAnthropicMessages:
		event = "error"
		value = map[string]any{"type": "error", "error": errorBody}
	case store.FormatOpenAIResponses:
		event = "response.failed"
		value = map[string]any{"type": event, "response": map[string]any{"object": "response", "status": "failed", "error": errorBody}}
	}
	b, _ := json.Marshal(value)
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func exposeDiagnostic(w http.ResponseWriter, rec *recovery, message string) (*recovery, string) {
	d := diagnosticForRecovery(rec)
	if d == nil {
		return rec, message
	}
	copy := *d
	copy.RequestID = w.Header().Get(requestIDHeader)
	return diagnosticRecovery(rec, &copy), copy.Message + " " + message
}
