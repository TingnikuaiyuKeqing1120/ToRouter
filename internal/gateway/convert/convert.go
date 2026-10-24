// Package convert implements strict cross-protocol conversion for LLM
// gateway traffic between Chat Completions, Responses, Anthropic Messages and
// Gemini generateContent, via a shared intermediate representation (IR).
//
// Public surface:
//
//	Request(body []byte, from, to, model string) ([]byte, error)
//	Response(body []byte, from, to, model string) ([]byte, error)
//	Stream(r io.Reader, w io.Writer, from, to, model string) error
//	ParseUsage(body []byte, protocol string) (in, out, cr, cw int64, ok bool)
//
// Supported protocol names: "openai", "responses", "anthropic", "gemini".
// All entry points also accept the full endpoint format names, mirroring
// store's values (the gateway tests assert the two stay equal): a caller that
// knows which endpoint it called must not have a Responses usage body read
// with the Chat Completions schema — they are different endpoints with
// different schemas.
// Same-protocol conversion is handled elsewhere and is rejected here.
//
// Model parameter semantics: for Request, `model` is the upstream model name
// to place in the outgoing request. For Response and Stream, `model` is the
// model name originally requested by the client, used when re-emitting.
package convert

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Errors returned by this package.
var (
	// ErrSameProtocol is returned when from == to; same-protocol handling
	// lives elsewhere in the gateway.
	ErrSameProtocol = errors.New("convert: same-protocol conversion is handled elsewhere")
	// ErrUnknownProtocol is returned for a protocol name other than
	// openai / responses / anthropic / gemini.
	ErrUnknownProtocol = errors.New("convert: unknown protocol")
	// ErrTruncated is returned when a stream ends without its proper
	// terminator (missing [DONE] / message_stop) or with an unterminated,
	// unparseable trailing event.
	ErrTruncated = errors.New("convert: truncated stream")
	// ErrLineLimit is returned when a single SSE line exceeds the 8 MiB bound.
	ErrLineLimit = errors.New("convert: SSE line exceeds 8MiB limit")
	// ErrMalformed is returned for syntactically invalid JSON payloads.
	ErrMalformed = errors.New("convert: malformed payload")
	// ErrUnsupported is returned when the payload contains content or fields
	// this package refuses to drop silently (images, reasoning signatures,
	// response_format, etc.). Wrapped messages name the offending field.
	ErrUnsupported = errors.New("convert: unsupported field")
	// ErrContinuationState marks reasoning that cannot be preserved in the
	// requested format. It wraps ErrUnsupported at conversion boundaries;
	// authorized reasoning drop can resolve this, unlike missing bridge state.
	ErrContinuationState = errors.New("convert: reasoning state cannot be represented")
)

// thoughtSignatureBypass is the provider-documented dummy value that skips
// Gemini 3's thought-signature validation. Google documents it for exactly the
// case of transferring a trace from a model that produced no signatures, i.e.
// cross-protocol conversion, while discouraging hand-built function calls.
const thoughtSignatureBypass = "skip_thought_signature_validator"

// Protocol names. Chat and Responses are independent wire contracts.
const (
	OpenAI    = "openai"
	Responses = "responses"
	Anthropic = "anthropic"
	Gemini    = "gemini"
	// Endpoint format names, equal to store.Format*. Accepted by usage parsing
	// in place of the families above.
	OpenAIChatCompletions = "openai_chat_completions"
	OpenAIResponses       = "openai_responses"
	AnthropicMessages     = "anthropic_messages"
	GeminiGenerateContent = "gemini_generate_content"

	maxLineLen = 8 << 20 // 8 MiB bounded SSE line
)

func checkProtocols(from, to string) error {
	for _, p := range []string{from, to} {
		switch p {
		case OpenAI, Responses, Anthropic, Gemini:
		default:
			return fmt.Errorf("%w: %q", ErrUnknownProtocol, p)
		}
	}
	if from == to {
		return ErrSameProtocol
	}
	return nil
}

// Request converts a client request body from protocol `from` into a request
// body for protocol `to`. `model` is the upstream model name to target.
func Request(body []byte, from, to, model string) ([]byte, error) {
	out, _, err := RequestWithInfo(body, from, to, model)
	return out, err
}

// RequestWithInfo is Request plus the lossy adaptations applied on the way
// out, so the gateway can record them the same way it records stripped
// reasoning on the way back, including cache hints and signature bypasses.
func RequestWithInfo(body []byte, from, to, model string) ([]byte, []string, error) {
	return RequestWithOptions(body, from, to, model, Options{})
}
func RequestWithOptions(body []byte, from, to, model string, opts Options) ([]byte, []string, error) {
	from, to = normalizeProtocol(from), normalizeProtocol(to)
	if err := checkProtocols(from, to); err != nil {
		return nil, nil, err
	}
	body, cacheDrops, err := stripCacheHints(body, from)
	if err != nil {
		return nil, nil, err
	}
	req, err := decodeRequest(body, from)
	if err != nil {
		return nil, nil, err
	}
	drops, err := prepareRequestReasoning(req, to, opts)
	if err != nil {
		return nil, nil, err
	}
	out, adapted, err := encodeRequest(req, to, model)
	return out, uniqueKinds(append(append(cacheDrops, drops...), adapted...)), err
}

// Response converts a complete (non-streaming) response body from the
// upstream protocol `from` into a response body for the client protocol `to`.
// `model` is the model name requested by the client.
func Response(body []byte, from, to, model string) ([]byte, error) {
	out, _, err := ResponseWithInfo(body, from, to, model)
	return out, err
}

// ResponseWithInfo is Response plus the list of reasoning carriers that were
// stripped because `to` cannot express them (see irResponse.Dropped). The
// gateway uses it to report the loss; an empty list means nothing was dropped.
func ResponseWithInfo(body []byte, from, to, model string) ([]byte, []string, error) {
	return ResponseWithOptions(body, from, to, model, Options{})
}
func ResponseWithOptions(body []byte, from, to, model string, opts Options) ([]byte, []string, error) {
	from, to = normalizeProtocol(from), normalizeProtocol(to)
	if err := checkProtocols(from, to); err != nil {
		return nil, nil, err
	}
	resp, err := decodeResponse(body, from)
	if err != nil {
		return nil, nil, err
	}
	parts, drops, err := prepareResponseReasoning(resp.Parts, to, opts)
	if err != nil {
		return nil, nil, err
	}
	resp.Parts = parts
	resp.Dropped = append(resp.Dropped, drops...)
	out, err := encodeResponse(resp, to, model)
	if err != nil {
		return nil, nil, err
	}
	return out, resp.Dropped, nil
}

// Stream converts a streaming response (SSE for all protocols; Gemini uses
// streamGenerateContent?alt=sse) read from r into the target protocol written to
// w. Data is flushed after every emitted event when w implements Flush().
// Malformed or truncated upstream streams are reported as errors; a
// successful end is never synthesized on unexpected EOF.
func Stream(r io.Reader, w io.Writer, from, to, model string) error {
	_, err := StreamWithInfo(r, w, from, to, model)
	return err
}

// StreamWithInfo is Stream plus the reasoning carriers stripped during the
// conversion. When the list is non-empty and nothing was written yet, a
// comment line reporting the loss is emitted first, because response headers
// were already sent by the time the first reasoning block is seen.
func StreamWithInfo(r io.Reader, w io.Writer, from, to, model string) ([]string, error) {
	return StreamWithOptions(r, w, from, to, model, Options{})
}
func StreamWithOptions(r io.Reader, w io.Writer, from, to, model string, opts Options) ([]string, error) {
	from, to = normalizeProtocol(from), normalizeProtocol(to)
	if err := checkProtocols(from, to); err != nil {
		return nil, err
	}
	emitter, err := newEmitter(to, w, model)
	if err != nil {
		return nil, err
	}
	emitter.opts = opts
	switch from {
	case Responses:
		err = streamResponses(r, emitter)
	case OpenAI:
		err = streamOpenAI(r, emitter)
	case Anthropic:
		err = streamAnthropic(r, emitter)
	case Gemini:
		err = streamGemini(r, emitter)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProtocol, from)
	}
	if err != nil {
		return emitter.dropped, err
	}
	if len(emitter.dropped) > 0 && !emitter.started {
		if cErr := emitter.comment("x-to-router-degraded: reasoning stripped (" + strings.Join(emitter.dropped, ",") + ")"); cErr != nil {
			return emitter.dropped, cErr
		}
	}
	return emitter.dropped, nil
}

// ParseUsage extracts token usage from a complete response body or a single
// streaming event JSON payload. `protocol` is a wire family (openai / anthropic
// / gemini) or the endpoint's full format name; the full name is what tells the
// two OpenAI facades apart, since only Responses reports input_tokens /
// output_tokens. Input is normalized so cached input tokens
// are NOT double counted: for openai and gemini, input = total prompt tokens
// minus cached tokens; anthropic input_tokens already excludes caches and is
// returned as-is. ok is false when no usage information is present.
func ParseUsage(body []byte, protocol string) (input, output, cacheRead, cacheWrite int64, ok bool) {
	switch protocol {
	case OpenAI, OpenAIChatCompletions:
		return parseOpenAIUsage(body)
	case OpenAIResponses, Responses:
		return parseResponsesUsage(body)
	case Anthropic, AnthropicMessages:
		return parseAnthropicUsage(body)
	case Gemini, GeminiGenerateContent:
		return parseGeminiUsage(body)
	}
	return 0, 0, 0, 0, false
}
