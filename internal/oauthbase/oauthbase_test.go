package oauthbase

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The listener binds the host the flow pins and answers the one callback path.
// The Codex track names localhost (with the IPv6 fallback), SIWC names
// 127.0.0.1: the plumbing is the same and only the host differs.
func TestListenCallbackOn(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost"} {
		listener, err := ListenCallbackOn(host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if !strings.HasPrefix(listener.RedirectURI, "http://"+host+":") || !strings.HasSuffix(listener.RedirectURI, CallbackPath) {
			t.Fatalf("%s: redirect %q", host, listener.RedirectURI)
		}
		waited := make(chan CallbackResult, 1)
		go func() {
			result, err := listener.Wait(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			waited <- result
		}()
		resp, err := http.Get(listener.RedirectURI + "?code=code-x&state=st-x")
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: callback page %d", host, resp.StatusCode)
		}
		select {
		case result := <-waited:
			if result.Code != "code-x" || result.State != "st-x" {
				t.Fatalf("%s: %+v", host, result)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no callback delivered", host)
		}
		listener.Close()
		// The port is released once the flow finishes with the listener.
		port := listener.RedirectURI[len("http://"+host+":"):]
		port = strings.TrimSuffix(port, CallbackPath)
		again, err := net.Listen("tcp", net.JoinHostPort(host, port))
		if err != nil {
			t.Fatalf("%s: port %s still held: %v", host, port, err)
		}
		again.Close()
	}
}

// The pinned ports are tried in order: the first free one serves, the next is
// the fallback, and an all-taken pair is an error that names the ports so the
// operator knows what to free. The pair is chosen at run time because
// `go test ./...` runs the packages in parallel and the production pair may be
// held by another package's OAuth test.
func TestListenCallbackPinnedPorts(t *testing.T) {
	first_port, second_port := freePortPair(t)
	first, err := ListenCallbackOn("127.0.0.1", first_port, second_port)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !strings.HasPrefix(first.RedirectURI, "http://127.0.0.1:"+strconv.Itoa(first_port)) {
		t.Fatalf("first redirect %q", first.RedirectURI)
	}
	// The fallback: the first port is held by the flow above.
	second, err := ListenCallbackOn("127.0.0.1", first_port, second_port)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !strings.HasPrefix(second.RedirectURI, "http://127.0.0.1:"+strconv.Itoa(second_port)) {
		t.Fatalf("second redirect %q", second.RedirectURI)
	}
	// localhost falls back to the IPv6 loopback when the IPv4 port is taken, so
	// the same port number still serves the other address family.
	onLocalhost, err := ListenCallbackOn("localhost", first_port)
	if err != nil {
		t.Fatalf("localhost: %v", err)
	}
	if !strings.HasPrefix(onLocalhost.RedirectURI, "http://localhost:"+strconv.Itoa(first_port)) {
		t.Fatalf("localhost redirect %q", onLocalhost.RedirectURI)
	}
	// Both named ports held on every family: the error names them.
	if _, err := ListenCallbackOn("127.0.0.1", first_port, second_port); err == nil || !strings.Contains(err.Error(), strconv.Itoa(first_port)) || !strings.Contains(err.Error(), strconv.Itoa(second_port)) {
		t.Fatalf("all-taken error %v", err)
	}
	onLocalhost.Close()
	second.Close()
	first.Close()
	// The ports are released with the listeners.
	for _, port := range []int{first_port, second_port} {
		if listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err != nil {
			t.Fatalf("port %d still held: %v", port, err)
		} else {
			listener.Close()
		}
	}
}

func TestListenCallbackOccupiedPortDiagnostic(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	listener, err := ListenCallbackOn("127.0.0.1", port)
	if listener != nil || err == nil {
		if listener != nil {
			listener.Close()
		}
		t.Fatalf("occupied port: listener=%v err=%v", listener, err)
	}
	for _, want := range []string{occupied.Addr().String(), "地址已被占用", "检查占用端口的程序"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if !callbackErrnoIs(err, syscall.EADDRINUSE, 10048) {
		t.Errorf("occupied socket cause not preserved: %v", err)
	}
	if strings.Contains(err.Error(), "系统端口保留") {
		t.Fatalf("occupied socket incorrectly diagnosed as access restriction: %v", err)
	}
}

func TestLoopbackBindFailureDiagnostics(t *testing.T) {
	cases := []struct {
		name   string
		cause  error
		want   string
		advice string
	}{
		{"occupied", syscall.EADDRINUSE, "地址已被占用", "检查占用端口的程序"},
		{"access denied", syscall.EACCES, "访问被拒绝", "可能与系统端口保留或安全策略有关"},
		{"operation denied", syscall.EPERM, "访问被拒绝", "检查端口保留及安全软件策略"},
		{"unsupported family", syscall.EAFNOSUPPORT, "地址族或协议不受支持", "检查本机回环网络"},
		{"unsupported protocol", syscall.EPROTONOSUPPORT, "地址族或协议不受支持", "检查本机回环网络"},
		{"unavailable address", syscall.EADDRNOTAVAIL, "回环地址不可用", "检查本机回环网络"},
		{"other errno", syscall.EINVAL, "绑定失败", "检查本机回环网络"},
		{"unknown", errors.New("synthetic secret must not appear"), "绑定失败", "检查本机回环网络"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, []struct {
			name   string
			cause  error
			want   string
			advice string
		}{
			{"winsock occupied", syscall.Errno(10048), "地址已被占用", "检查占用端口的程序"},
			{"winsock denied", syscall.Errno(10013), "访问被拒绝", "可能与系统端口保留或安全策略有关"},
			{"winsock unsupported family", syscall.Errno(10047), "地址族或协议不受支持", "检查本机回环网络"},
			{"winsock unsupported protocol", syscall.Errno(10043), "地址族或协议不受支持", "检查本机回环网络"},
			{"winsock unavailable address", syscall.Errno(10049), "回环地址不可用", "检查本机回环网络"},
		}...)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var addresses []string
			listen := func(network, address string) (net.Listener, error) {
				if network != "tcp" {
					t.Fatalf("unexpected network %q", network)
				}
				addresses = append(addresses, address)
				return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", fmt.Errorf("synthetic secret: %w", tc.cause))}
			}
			listener, port, err := listenLoopbackWith(listen, "localhost", 1455, 1457)
			if listener != nil || port != 0 || err == nil {
				t.Fatalf("listener=%v port=%d err=%v", listener, port, err)
			}
			wantAddresses := []string{"127.0.0.1:1455", "[::1]:1455", "127.0.0.1:1457", "[::1]:1457"}
			if strings.Join(addresses, ",") != strings.Join(wantAddresses, ",") {
				t.Fatalf("attempt order %v, want %v", addresses, wantAddresses)
			}
			for _, want := range append(wantAddresses, "localhost", "1455/1457", tc.want, tc.advice) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("lost original error: %v", err)
			}
			var errno syscall.Errno
			if errors.As(tc.cause, &errno) && !strings.Contains(err.Error(), fmt.Sprintf("系统错误 %d", uintptr(errno))) {
				t.Errorf("system error number missing: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic secret") {
				t.Fatalf("raw error leaked: %v", err)
			}
			if tc.want != "地址已被占用" && strings.Contains(err.Error(), "地址已被占用") {
				t.Fatalf("incorrectly claimed address in use: %v", err)
			}
		})
	}
}

func TestLoopbackBindMixedFailures(t *testing.T) {
	causes := []error{syscall.EACCES, syscall.EADDRINUSE, syscall.EAFNOSUPPORT, errors.New("synthetic private detail")}
	next := 0
	listen := func(_, _ string) (net.Listener, error) {
		err := causes[next]
		next++
		return nil, err
	}
	_, _, err := listenLoopbackWith(listen, "localhost", 1455, 1457)
	if err == nil {
		t.Fatal("expected binding failure")
	}
	for i, want := range []string{"127.0.0.1:1455：访问被拒绝", "[::1]:1455：地址已被占用", "127.0.0.1:1457：地址族或协议不受支持", "[::1]:1457：绑定失败"} {
		if !strings.Contains(err.Error(), want) || !errors.Is(err, causes[i]) {
			t.Errorf("attempt %d missing reason/cause: %v", i, err)
		}
	}
}

func TestLoopbackBindFallbackAfterDenied(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	var addresses []string
	listen := func(_, address string) (net.Listener, error) {
		addresses = append(addresses, address)
		if len(addresses) < 3 {
			return nil, syscall.EACCES
		}
		return probe, nil
	}
	listener, port, err := listenLoopbackWith(listen, "localhost", 1455, 1457)
	if err != nil || listener != probe || port != probe.Addr().(*net.TCPAddr).Port {
		t.Fatalf("listener=%v port=%d err=%v", listener, port, err)
	}
	if strings.Join(addresses, ",") != "127.0.0.1:1455,[::1]:1455,127.0.0.1:1457" {
		t.Fatalf("unexpected fallback sequence %v", addresses)
	}
}

func TestLoopbackBindAutomaticPortFailure(t *testing.T) {
	listen := func(_, address string) (net.Listener, error) {
		if address != "127.0.0.1:0" {
			t.Fatalf("automatic port address %q", address)
		}
		return nil, syscall.EACCES
	}
	_, _, err := listenLoopbackWith(listen, "127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:0：访问被拒绝") || strings.Contains(err.Error(), "地址已被占用") {
		t.Fatalf("automatic port failure: %v", err)
	}
}

// freePortPair asks the kernel for two adjacent free ports and releases them, so
// a test can pin ports no other process holds.
func freePortPair(t *testing.T) (int, int) {
	t.Helper()
	take := func() int {
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := probe.Addr().(*net.TCPAddr).Port
		probe.Close()
		return port
	}
	first := take()
	second := take()
	for second == first {
		second = take()
	}
	return first, second
}

// A pasted callback is accepted only for the flow's own host and the pinned
// path: a link aimed anywhere else is a link someone else crafted.
func TestParseCallbackURLOn(t *testing.T) {
	good := "http://localhost:1455/auth/callback?code=c&state=s"
	if result, err := ParseCallbackURLOn(good, "localhost"); err != nil || result.Code != "c" {
		t.Fatalf("%v %+v", err, result)
	}
	if result, err := ParseCallbackURLOn("http://127.0.0.1:1455/auth/callback?code=c&state=s", "127.0.0.1"); err != nil || result.Code != "c" {
		t.Fatalf("%v %+v", err, result)
	}
	// The two tracks' hosts are not interchangeable.
	if _, err := ParseCallbackURLOn(good, "127.0.0.1"); err == nil {
		t.Fatal("localhost accepted for a 127.0.0.1 flow")
	}
	if _, err := ParseCallbackURLOn("http://127.0.0.1:1455/auth/callback?code=c&state=s", "localhost"); err == nil {
		t.Fatal("127.0.0.1 accepted for a localhost flow")
	}
	// Bare query strings belong to the flow's own path.
	if result, err := ParseCallbackURLOn("?code=c2&state=s2", "localhost"); err != nil || result.Code != "c2" {
		t.Fatalf("bare query: %v %+v", err, result)
	}
	// The error branch travels, and everything else is refused.
	if result, err := ParseCallbackURLOn("http://127.0.0.1:1/auth/callback?error=access_denied", "127.0.0.1"); err != nil || result.Error != "access_denied" {
		t.Fatalf("error branch: %v %+v", err, result)
	}
	for _, bad := range []string{
		"http://localhost:1455/other?code=c",
		"https://127.0.0.1:1455/auth/callback?code=c",
		"http://evil.test/auth/callback?code=c",
		"http://127.0.0.1:1455/auth/callback",
		"",
	} {
		if _, err := ParseCallbackURLOn(bad, "127.0.0.1"); err == nil {
			t.Fatalf("callback %q accepted", bad)
		}
	}
}

// PKCE is an S256 pair over a fresh verifier, and every pair is new.
func TestNewPKCE(t *testing.T) {
	first, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Verifier) < 40 {
		t.Fatalf("verifier too short: %q", first.Verifier)
	}
	sum := sha256.Sum256([]byte(first.Verifier))
	if challenge := base64.RawURLEncoding.EncodeToString(sum[:]); first.Challenge != challenge {
		t.Fatalf("challenge is not S256 of the verifier")
	}
	if strings.Contains(first.Challenge, "=") || strings.Contains(first.Challenge, "+") || strings.Contains(first.Challenge, "/") {
		t.Fatalf("challenge is not base64url: %q", first.Challenge)
	}
	second, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if second.Verifier == first.Verifier || second.Challenge == first.Challenge {
		t.Fatal("pkce pair reused")
	}
}

// RandomToken is fresh, base64url and long enough for state and nonce.
func TestRandomToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		token, err := RandomToken(24)
		if err != nil {
			t.Fatal(err)
		}
		if len(token) < 30 {
			t.Fatalf("token %q", token)
		}
		if _, err := base64.RawURLEncoding.DecodeString(token); err != nil {
			t.Fatalf("not base64url: %q", token)
		}
		if seen[token] {
			t.Fatal("token reused")
		}
		seen[token] = true
	}
	// A zero-length request is still a valid (empty) token, not an error.
	if _, err := RandomToken(0); err != nil {
		t.Fatal(err)
	}
}
