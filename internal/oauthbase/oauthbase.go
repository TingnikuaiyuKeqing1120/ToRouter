// Package oauthbase holds the OAuth plumbing the three ChatGPT-family tracks
// share: the loopback callback listener, the PKCE pair, the state/nonce
// randomness and the pasted-callback parser.
//
// It exists because the tracks differ exactly where it matters — authorize
// endpoint, client id, scope set, which host the redirect may name — and
// nowhere else. Codex pins its redirect to http://localhost:<port> while SIWC
// pins its to http://127.0.0.1:<port> (the two refs are explicit and opposite
// about it), so the host is a parameter here and a constant in each track.
package oauthbase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CallbackPath is the path every track's redirect lands on.
const CallbackPath = "/auth/callback"

// CallbackResult is what the loopback redirect — or the pasted fallback —
// delivers.
type CallbackResult struct {
	Code     string
	State    string
	ClientID string
	// Scope is the granted scope set when the callback carries one. Only some
	// issuers echo it, which is why the consumer treats it as evidence rather
	// than as the answer.
	Scope       string
	Error       string
	Description string
}

// PKCE is one S256 pair. Verifier never leaves the process.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE mints a verifier and its S256 challenge (RFC 7636).
func NewPKCE() (PKCE, error) {
	verifier, err := RandomToken(48)
	if err != nil {
		return PKCE{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// RandomToken returns n crypto-random bytes, base64url encoded: the shape
// state, nonce and verifiers are made of.
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CallbackListener is the loopback half of a flow: it binds the requested host
// on an ephemeral port and serves the one pinned path an issuer is allowed to
// redirect to.
type CallbackListener struct {
	RedirectURI string
	server      *http.Server
	result      chan CallbackResult
}

// ListenCallbackOn binds the loopback listener on host. The port is chosen by
// the kernel, which is what keeps two concurrent authorizations from colliding.
//
// host is "127.0.0.1" or "localhost": the former binds the literal address, the
// latter binds 127.0.0.1 and falls back to the IPv6 loopback, because a browser
// resolving "localhost" may well arrive on ::1 (the Codex track pins localhost
// and its listener binds 127.0.0.1 with the dual-stack fallback — see
// docs/protocol-refs/oauth-openai-codex.md).
//
// When ports are named they are tried in order and the first free one wins:
// both ChatGPT-family tracks pin 1455 (the port their CLI registers with the
// issuer) and fall back to 1457, because a redirect the issuer allow-lists must
// land on the port the flow was built with. An empty list keeps the kernel's
// choice. When every bind fails the error names each attempted address and its
// actual failure category, so access restrictions are not reported as use.
func ListenCallbackOn(host string, ports ...int) (*CallbackListener, error) {
	return ListenCallbackOnPath(host, CallbackPath, ports...)
}

// ListenCallbackOnPath keeps each native provider's registered redirect path
// exact. Existing tracks continue to use /auth/callback through the wrapper.
func ListenCallbackOnPath(host, path string, ports ...int) (*CallbackListener, error) {
	if !validCallbackPath(path) {
		return nil, errors.New("invalid callback path")
	}
	listener, port, err := listenLoopback(host, ports...)
	if err != nil {
		return nil, err
	}
	l := &CallbackListener{
		RedirectURI: fmt.Sprintf("http://%s:%d%s", host, port, path),
		// IdleTimeout so a browser that connected and wandered off cannot hold
		// the port: the listener's job is one redirect, and the flow's own
		// window decides when the port is released.
		server: &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second},
		result: make(chan CallbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, l.callback)
	l.server.Handler = mux
	go func() { _ = l.server.Serve(listener) }()
	return l, nil
}

// listenLoopback binds the loopback interface for host and reports the port it
// bound. ports are tried in order; an empty list means "any free port".
func listenLoopback(host string, ports ...int) (net.Listener, int, error) {
	return listenLoopbackWith(net.Listen, host, ports...)
}

// The listener function is passed explicitly so failure cases can be tested
// without changing process-wide state or requiring reserved system ports.
func listenLoopbackWith(listen func(string, string) (net.Listener, error), host string, ports ...int) (net.Listener, int, error) {
	if len(ports) == 0 {
		ports = []int{0}
	}
	hosts := []string{host}
	if host == "localhost" {
		hosts = []string{"127.0.0.1", "::1"}
	}
	failure := &callbackBindError{host: host, ports: ports}
	for _, port := range ports {
		for _, bindHost := range hosts {
			address := net.JoinHostPort(bindHost, strconv.Itoa(port))
			listener, err := listen("tcp", address)
			if err == nil {
				// A zero request is whatever port the kernel chose.
				return listener, portOf(listener), nil
			}
			failure.attempts = append(failure.attempts, callbackBindAttempt{address: address, err: err})
		}
	}
	return nil, 0, failure
}

type callbackBindAttempt struct {
	address string
	err     error
}

type callbackBindError struct {
	host     string
	ports    []int
	attempts []callbackBindAttempt
}

func (e *callbackBindError) Error() string {
	details := make([]string, 0, len(e.attempts))
	var occupied, denied, other bool
	for _, attempt := range e.attempts {
		reason := "绑定失败"
		switch {
		case callbackErrnoIs(attempt.err, syscall.EADDRINUSE, 10048):
			reason, occupied = "地址已被占用", true
		case callbackErrnoIs(attempt.err, syscall.EACCES, 10013), errors.Is(attempt.err, syscall.EPERM):
			reason, denied = "访问被拒绝", true
		case callbackErrnoIs(attempt.err, syscall.EAFNOSUPPORT, 10047), callbackErrnoIs(attempt.err, syscall.EPROTONOSUPPORT, 10043):
			reason, other = "地址族或协议不受支持", true
		case callbackErrnoIs(attempt.err, syscall.EADDRNOTAVAIL, 10049):
			reason, other = "回环地址不可用", true
		default:
			other = true
		}
		// Never include arbitrary wrapped error text: it can contain paths or
		// credentials. The original errors remain available through Unwrap.
		var errno syscall.Errno
		if errors.As(attempt.err, &errno) {
			reason += fmt.Sprintf("（系统错误 %d）", uintptr(errno))
		}
		details = append(details, attempt.address+"："+reason)
	}
	tips := make([]string, 0, 3)
	if occupied {
		tips = append(tips, "请完成或取消其他授权，并检查占用端口的程序后重试")
	}
	if denied {
		tips = append(tips, "访问被拒绝可能与系统端口保留或安全策略有关；请检查端口保留及安全软件策略后重试")
	}
	if other {
		tips = append(tips, "请检查本机回环网络及系统网络配置后重试")
	}
	return fmt.Sprintf("授权回调监听失败（%s，端口 %s）：%s。%s。", e.host, joinPorts(e.ports), strings.Join(details, "；"), strings.Join(tips, "；"))
}

func (e *callbackBindError) Unwrap() []error {
	errs := make([]error, 0, len(e.attempts))
	for _, attempt := range e.attempts {
		errs = append(errs, attempt.err)
	}
	return errs
}

// Windows socket errors are Winsock codes, distinct from Go's portable
// syscall constants. Do not interpret those numbers as Winsock on other OSes.
func callbackErrnoIs(err error, portable syscall.Errno, winsock syscall.Errno) bool {
	return errors.Is(err, portable) || runtime.GOOS == "windows" && errors.Is(err, winsock)
}

func joinPorts(ports []int) string {
	names := make([]string, 0, len(ports))
	for _, port := range ports {
		names = append(names, strconv.Itoa(port))
	}
	return strings.Join(names, "/")
}

func portOf(l net.Listener) int {
	if addr, ok := l.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// Wait blocks until the browser's redirect arrives, the context expires, or the
// listener is closed. A timeout is not a failure of the flow: the paste fallback
// stays available for as long as the flow lives.
func (l *CallbackListener) Wait(ctx context.Context) (CallbackResult, error) {
	select {
	case result := <-l.result:
		return result, nil
	case <-ctx.Done():
		return CallbackResult{}, ctx.Err()
	}
}

// Close stops the listener and releases the port.
func (l *CallbackListener) Close() { _ = l.server.Close() }

// Result exposes the listener's channel without blocking, so a caller that owns
// its own pacing can consume the redirect once and otherwise get on with its
// interval. Exactly one value is ever delivered: the channel is buffered, and a
// second redirect from the same flow has nowhere to land.
func (l *CallbackListener) Result() <-chan CallbackResult { return l.result }

// callback answers the browser. The state check happens at the caller, which
// owns the state it issued; the page only says whether a code arrived.
func (l *CallbackListener) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	result := CallbackResult{Code: q.Get("code"), State: q.Get("state"), ClientID: q.Get("client_id"), Scope: q.Get("scope"), Error: q.Get("error"), Description: q.Get("error_description")}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if result.Error != "" {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "<html><body><p>授权未完成："+html.EscapeString(result.Error)+"。</p><p>可以关闭此页面，在 to-router 控制台重试。</p></body></html>")
		return
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "<html><body><p>授权完成，可以关闭此页面并返回 to-router 控制台。</p></body></html>")
	select {
	case l.result <- result:
	default:
	}
}

// ParseCallbackURLOn reads a callback the user pasted back, the way a tray user
// has to when the browser cannot reach the loopback listener. It accepts the
// full URL or a bare query string, and refuses anything that is not the pinned
// path on the flow's own host: a pasted link aimed somewhere else is a link
// someone else crafted.
func ParseCallbackURLOn(raw, host string) (CallbackResult, error) {
	return ParseCallbackURLOnPath(raw, host, CallbackPath)
}

func validCallbackPath(path string) bool {
	parsed, err := url.Parse(path)
	return err == nil && strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") && !strings.Contains(path, "..") && parsed.Path == path && parsed.RawQuery == "" && parsed.Fragment == ""
}

func ParseCallbackURLOnPath(raw, host, path string) (CallbackResult, error) {
	if !validCallbackPath(path) {
		return CallbackResult{}, errors.New("invalid callback path")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return CallbackResult{}, errors.New("empty callback")
	}
	if !strings.Contains(raw, "://") {
		// A bare query is what a clipboard paste usually yields; it belongs to
		// the pinned callback path on the flow's host.
		if !strings.HasPrefix(raw, "?") && !strings.HasPrefix(raw, "&") {
			raw = "?" + raw
		}
		raw = "http://" + host + path + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return CallbackResult{}, err
	}
	if parsed.Scheme != "http" || parsed.Hostname() != host || parsed.Path != path || parsed.User != nil || parsed.Fragment != "" {
		return CallbackResult{}, fmt.Errorf("callback must be http://%s<port>%s", host, path)
	}
	q := parsed.Query()
	result := CallbackResult{Code: q.Get("code"), State: q.Get("state"), ClientID: q.Get("client_id"), Scope: q.Get("scope"), Error: q.Get("error"), Description: q.Get("error_description")}
	if result.Code == "" && result.Error == "" {
		return CallbackResult{}, errors.New("callback carries neither a code nor an error")
	}
	return result, nil
}
