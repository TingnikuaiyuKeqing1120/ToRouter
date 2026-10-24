package claude

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	stdtls "crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/proxy"
)

// Transport profiles are shared across requests so keep-alive and TLS session
// resumption survive token rotation. Explicit custom RoundTrippers stay caller
// owned (e.g. a synthetic fixture); no hidden fallback retry is performed.
type profileKey struct {
	transport *http.Transport
	inference bool
}

var profileTransports sync.Map

func (d *Driver) profileClient(client *http.Client, inference bool) (*http.Client, error) {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	base := http.DefaultTransport
	if client.Transport != nil {
		base = client.Transport
	}
	if transport, ok := base.(*http.Transport); ok {
		if c := transport.TLSClientConfig; c != nil {
			// A caller-supplied verifier or client certificate is a security
			// contract. Refuse unsupported hooks rather than silently dropping it.
			if c.VerifyConnection != nil || c.GetClientCertificate != nil || len(c.Certificates) != 0 || len(c.EncryptedClientHelloConfigList) != 0 || len(c.CurvePreferences) != 0 || len(c.CipherSuites) != 0 || c.MinVersion > stdtls.VersionTLS12 || (c.MaxVersion != 0 && c.MaxVersion < stdtls.VersionTLS13) {
				return nil, errors.New("Claude TLS profile cannot preserve the supplied custom TLS policy")
			}
		}
		key := profileKey{transport, inference}
		if value, exists := profileTransports.Load(key); exists {
			copy.Transport = value.(*http.Transport)
		} else {
			profile := newProfileTransport(transport, inference)
			actual, loaded := profileTransports.LoadOrStore(key, profile)
			if loaded {
				profile.CloseIdleConnections()
			}
			copy.Transport = actual.(*http.Transport)
		}
	}
	// Token-bearing redirects are not an OAuth endpoint-discovery mechanism.
	// Return the actual 3xx so the caller sees failure without credential leaks.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy, nil
}

func newProfileTransport(base *http.Transport, inference bool) *http.Transport {
	transport := base.Clone()
	proxyFor := base.Proxy
	transport.Proxy = func(request *http.Request) (*url.URL, error) {
		if request.URL.Scheme == "http" && proxyFor != nil {
			return proxyFor(request)
		}
		return nil, nil
	}
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *stdtls.Conn) http.RoundTripper{}
	sessionCache := tls.NewLRUClientSessionCache(8)
	dial := base.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid Claude TLS address")
		}
		var proxyURL *url.URL
		if proxyFor != nil {
			proxyURL, err = proxyFor((&http.Request{URL: &url.URL{Scheme: "https", Host: address}}).WithContext(ctx))
			if err != nil {
				return nil, errors.New("Claude proxy configuration failed")
			}
		}
		conn, err := dialProfile(ctx, network, address, proxyURL, dial)
		if err != nil {
			return nil, err
		}
		config := &tls.Config{ServerName: host, ClientSessionCache: sessionCache, OmitEmptyPsk: true, PreferSkipResumptionOnNilExtension: true}
		if base.TLSClientConfig != nil {
			config.RootCAs = base.TLSClientConfig.RootCAs
			config.InsecureSkipVerify = base.TLSClientConfig.InsecureSkipVerify
			config.VerifyPeerCertificate = base.TLSClientConfig.VerifyPeerCertificate
			if base.TLSClientConfig.ServerName != "" {
				config.ServerName = base.TLSClientConfig.ServerName
			}
		}
		tlsConn := tls.UClient(conn, config, tls.HelloCustom)
		spec := claudeOAuthTLSClientHelloSpec()
		if inference {
			spec = claudeCodeTLSClientHelloSpec()
		}
		if err = tlsConn.ApplyPreset(spec); err != nil {
			conn.Close()
			return nil, errors.New("Claude TLS profile setup failed")
		}
		handshakeCtx := ctx
		if base.TLSHandshakeTimeout > 0 {
			var cancel context.CancelFunc
			handshakeCtx, cancel = context.WithTimeout(ctx, base.TLSHandshakeTimeout)
			defer cancel()
		}
		if err = tlsConn.HandshakeContext(handshakeCtx); err != nil {
			tlsConn.Close()
			return nil, &wireError{operation: "TLS handshake", cause: err}
		}
		return NewOrderedRequestConn(tlsConn, func(method, target string) []string {
			if inference {
				return messagesHeaderOrder
			}
			if method == http.MethodGet {
				return inspectHeaderOrder
			}
			return tokenHeaderOrder
		}), nil
	}
	// Plain HTTP is reserved for explicit fixture endpoints and retains the
	// supplied transport's dialer/proxy semantics without a TLS fingerprint.
	transport.DialContext = dial
	return transport
}

func dialProfile(ctx context.Context, network, address string, proxyURL *url.URL, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	if proxyURL == nil {
		return dial(ctx, network, address)
	}
	if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
		var auth *proxy.Auth
		if proxyURL.User != nil {
			password, _ := proxyURL.User.Password()
			auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
		}
		proxyAddress := proxyURL.Host
		if proxyURL.Port() == "" {
			proxyAddress = net.JoinHostPort(proxyURL.Hostname(), "1080")
		}
		dialer, err := proxy.SOCKS5("tcp", proxyAddress, auth, contextProxyDialer{dial})
		if err != nil {
			return nil, errors.New("Claude SOCKS proxy setup failed")
		}
		return dialer.(proxy.ContextDialer).DialContext(ctx, network, address)
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, errors.New("unsupported Claude proxy scheme")
	}
	proxyAddress := proxyURL.Host
	if proxyURL.Port() == "" {
		port := "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
		proxyAddress = net.JoinHostPort(proxyURL.Hostname(), port)
	}
	conn, err := dial(ctx, network, proxyAddress)
	if err != nil {
		return nil, err
	}
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	if proxyURL.Scheme == "https" {
		secure := stdtls.Client(conn, &stdtls.Config{ServerName: proxyURL.Hostname(), MinVersion: stdtls.VersionTLS12})
		if err := secure.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = secure
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username()+":"+password)))
	}
	if err = request.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("Claude CONNECT proxy returned HTTP %d", response.StatusCode)
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type contextProxyDialer struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d contextProxyDialer) Dial(network, address string) (net.Conn, error) {
	return d.dial(context.Background(), network, address)
}
func (d contextProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

var tokenHeaderOrder = []string{"Accept", "Content-Type", "User-Agent", "Content-Length", "Accept-Encoding", "Host", "Connection"}
var inspectHeaderOrder = []string{"Accept", "Content-Type", "Authorization", "Cache-Control", "User-Agent", "Accept-Encoding", "Host", "Connection"}
var messagesHeaderOrder = []string{"Accept", "Authorization", "Content-Type", "User-Agent", "X-Claude-Code-Session-Id", "X-Stainless-Arch", "X-Stainless-Lang", "X-Stainless-OS", "X-Stainless-Package-Version", "X-Stainless-Retry-Count", "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Timeout", "anthropic-beta", "anthropic-dangerous-direct-browser-access", "anthropic-version", "x-app", "x-client-request-id", "Connection", "Host", "Accept-Encoding", "Content-Length"}

type responseReader struct {
	io.Reader
	body         io.ReadCloser
	closeDecoder func() error
}

func (r *responseReader) Close() error {
	if r.closeDecoder != nil {
		_ = r.closeDecoder()
	}
	return r.body.Close()
}

func decodedBody(response *http.Response) (io.ReadCloser, error) {
	result := &responseReader{Reader: response.Body, body: response.Body}
	switch strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return response.Body, nil
	case "gzip":
		decoder, err := gzip.NewReader(response.Body)
		if err != nil {
			return nil, err
		}
		result.Reader = decoder
		result.closeDecoder = decoder.Close
	case "deflate":
		buffer := bufio.NewReader(response.Body)
		prefix, err := buffer.Peek(2)
		if err != nil {
			return nil, err
		}
		var decoder io.ReadCloser
		if prefix[0]&15 == 8 && (int(prefix[0])*256+int(prefix[1]))%31 == 0 {
			decoder, err = zlib.NewReader(buffer)
			if err != nil {
				return nil, err
			}
		} else {
			decoder = flate.NewReader(buffer)
		}
		result.Reader = decoder
		result.closeDecoder = decoder.Close
	case "br":
		result.Reader = brotli.NewReader(response.Body)
	case "zstd":
		decoder, err := zstd.NewReader(response.Body, zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		result.Reader = decoder
		result.closeDecoder = func() error { decoder.Close(); return nil }
	default:
		return nil, errors.New("unsupported Claude response encoding")
	}
	return result, nil
}
