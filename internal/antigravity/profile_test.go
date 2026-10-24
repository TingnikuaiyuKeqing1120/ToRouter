package antigravity

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/store"
)

type wireObservation struct{ remote, proto, alpn, ua, apiClient, auth string }

func closeProfilePools(d *Driver) {
	d.transports.mu.Lock()
	defer d.transports.mu.Unlock()
	for _, entry := range d.transports.entries {
		entry.transport.CloseIdleConnections()
	}
}

func executeFixture(t *testing.T, d *Driver, client *http.Client, c store.OAuthCredential) {
	t.Helper()
	resp, err := d.Execute(client, request(t, context.Background(), false, `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`), c)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
}

func TestProfileTLSWireAndAccountReuseAcrossRefresh(t *testing.T) {
	observations := make(chan wireObservation, 16)
	hellos := make(chan []string, 16)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observations <- wireObservation{r.RemoteAddr, r.Proto, r.TLS.NegotiatedProtocol, r.UserAgent(), r.Header.Get("X-Goog-Api-Client"), r.Header.Get("Authorization")}
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
		case "/v1internal:fetchAvailableModels":
			fmt.Fprint(w, `{"models":{}}`)
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"allowedTiers":[{"id":"fixture","isDefault":true}]}`)
		case "/v1internal:onboardUser":
			fmt.Fprint(w, `{"done":true,"response":{"cloudaicompanionProject":"fixture-project"}}`)
		default:
			fmt.Fprint(w, `{"response":{"candidates":[]}}`)
		}
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		hellos <- append([]string(nil), hello.SupportedProtos...)
		return nil, nil
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := srv.Client()
	base := client.Transport.(*http.Transport)
	var verified atomic.Int32
	base.TLSClientConfig.VerifyConnection = func(tls.ConnectionState) error {
		verified.Add(1)
		return nil
	}
	base.ForceAttemptHTTP2 = true
	base.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	base.Protocols = new(http.Protocols)
	base.Protocols.SetHTTP1(true)
	base.Protocols.SetHTTP2(true)
	d := NewDriver(client, Endpoints{TokenURL: srv.URL + "/token", APIBase: srv.URL, LoadBase: srv.URL})
	t.Cleanup(func() { closeProfilePools(d) })
	c := fixtureCredential()
	executeFixture(t, d, nil, c)
	first := <-observations
	executeFixture(t, d, nil, c)
	second := <-observations
	rotated, err := d.Refresh(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	refresh := <-observations
	executeFixture(t, d, nil, rotated)
	afterRefresh := <-observations
	if _, err := d.Models(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}
	models := <-observations
	if _, err := d.project(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}
	load, onboard := <-observations, <-observations
	for _, got := range []wireObservation{second, refresh, afterRefresh, models, load, onboard} {
		if got.remote != first.remote {
			t.Fatalf("same account lost connection reuse: %q != %q", got.remote, first.remote)
		}
	}
	if afterRefresh.auth != "Bearer rotated-access" {
		t.Fatal("inference did not use the refreshed access token")
	}
	other := rotated
	other.Subject, other.Account = "other-subject", "other-account"
	executeFixture(t, d, nil, other)
	isolated := <-observations
	if isolated.remote == first.remote {
		t.Fatal("different accounts shared a connection")
	}
	otherClient := *client
	otherClient.Transport = base.Clone()
	executeFixture(t, d, &otherClient, rotated)
	otherBase := <-observations
	if otherBase.remote == first.remote || otherBase.remote == isolated.remote {
		t.Fatal("different base transports shared a connection")
	}
	for _, got := range []wireObservation{first, second, refresh, afterRefresh, models, load, onboard, isolated, otherBase} {
		if got.proto != "HTTP/1.1" || got.alpn != "" {
			t.Fatalf("wire protocol = %s / %q", got.proto, got.alpn)
		}
	}
	for _, got := range []wireObservation{first, afterRefresh, models, load, isolated, otherBase} {
		if got.ua != "antigravity/hub/2.24.0 (aidev_client; os_type=darwin; arch=arm64; cl=1000335682)" {
			t.Fatalf("wire UA = %q", got.ua)
		}
	}
	if onboard.ua != "antigravity/hub/2.24.0 (aidev_client; os_type=darwin; arch=arm64; cl=1000335682) google-api-nodejs-client/10.3.0" || onboard.apiClient != "gl-node/22.21.1" {
		t.Fatalf("onboarding software headers = %+v", onboard)
	}
	for i := 0; i < 3; i++ {
		if protocols := <-hellos; len(protocols) != 0 {
			t.Fatalf("ClientHello advertised ALPN: %q", protocols)
		}
	}
	if base.TLSClientConfig.NextProtos[0] != "h2" || !base.Protocols.HTTP2() {
		t.Fatal("profile mutated caller TLS/HTTP configuration")
	}
	if verified.Load() != 3 {
		t.Fatalf("caller certificate verification hook ran %d times, want 3 handshakes", verified.Load())
	}
}

func TestProfileRetainsDynamicProxyRouting(t *testing.T) {
	type proxyHit struct{ proxy, remote string }
	hits := make(chan proxyHit, 4)
	newProxy := func(name string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if r.URL.Host != "synthetic-upstream.invalid" {
				t.Errorf("proxy did not receive the absolute upstream URL: %s", r.URL)
			}
			hits <- proxyHit{name, r.RemoteAddr}
			fmt.Fprint(w, `{"response":{"candidates":[]}}`)
		}))
		t.Cleanup(server.Close)
		return server
	}
	one, two := newProxy("one"), newProxy("two")
	oneURL, _ := url.Parse(one.URL)
	twoURL, _ := url.Parse(two.URL)
	var selected atomic.Bool
	base := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		if selected.Load() {
			return twoURL, nil
		}
		return oneURL, nil
	}}
	d := NewDriver(&http.Client{Transport: base}, Endpoints{APIBase: "http://synthetic-upstream.invalid"})
	t.Cleanup(func() { closeProfilePools(d) })
	c := fixtureCredential()
	executeFixture(t, d, nil, c)
	first := <-hits
	selected.Store(true)
	executeFixture(t, d, nil, c)
	second := <-hits
	selected.Store(false)
	executeFixture(t, d, nil, c)
	third := <-hits
	if first.proxy != "one" || second.proxy != "two" || third.proxy != "one" || first.remote != third.remote || second.remote == first.remote {
		t.Fatalf("proxy routing/reuse changed: %+v %+v %+v", first, second, third)
	}
}

func TestProfileProxyTLSVerificationAndCancellation(t *testing.T) {
	started := make(chan struct{}, 2)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer wait" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `{"response":{"candidates":[]}}`)
	}))
	t.Cleanup(srv.Close)
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Error("TLS request did not use proxy CONNECT")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		connects.Add(1)
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		downstream, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer downstream.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		go func() {
			_, _ = io.Copy(upstream, buffered)
			_ = upstream.Close()
		}()
		_, _ = io.Copy(downstream, upstream)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	client := srv.Client()
	base := client.Transport.(*http.Transport)
	base.Proxy = http.ProxyURL(proxyURL)
	base.IdleConnTimeout = time.Second // An explicit shorter limit survives.
	d := NewDriver(client, Endpoints{APIBase: srv.URL})
	t.Cleanup(func() { closeProfilePools(d) })
	c := fixtureCredential()
	executeFixture(t, d, nil, c)
	executeFixture(t, d, nil, c)
	if connects.Load() != 1 {
		t.Fatalf("proxy connections = %d, expected reuse", connects.Load())
	}
	if d.profileClient(client, c).Transport.(*http.Transport).IdleConnTimeout != time.Second {
		t.Fatal("explicit shorter idle timeout changed")
	}
	untrusted := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	if _, err := d.Execute(untrusted, request(t, context.Background(), false, `{}`), c); err == nil {
		t.Fatal("accepted an untrusted TLS certificate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.AccessToken = "wait"
	result := make(chan error, 1)
	go func() {
		_, err := d.Execute(nil, request(t, ctx, false, `{}`), c)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach TLS server")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not stop the request")
	}
	short := *client
	short.Timeout = 30 * time.Millisecond
	before := time.Now()
	if _, err := d.Execute(&short, request(t, context.Background(), false, `{}`), c); err == nil || time.Since(before) > time.Second {
		t.Fatalf("client timeout was not retained: %v", err)
	}
}

func TestProfileCacheEvictionClosesIdleConnection(t *testing.T) {
	closed := make(chan struct{}, 100)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"response":{"candidates":[]}}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	d := NewDriver(srv.Client(), Endpoints{APIBase: srv.URL})
	t.Cleanup(func() { closeProfilePools(d) })
	c := fixtureCredential()
	executeFixture(t, d, nil, c)
	// Fill the bounded cache without opening 64 unnecessary connections.
	for i := 0; i < transportCacheCapacity; i++ {
		other := c
		other.Subject = fmt.Sprintf("synthetic-%d", i)
		d.profileClient(nil, other)
	}
	if len(d.transports.entries) != transportCacheCapacity {
		t.Fatalf("cache size = %d", len(d.transports.entries))
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("eviction did not close its idle connection")
	}
}

type profileRoundTripper func(*http.Request) (*http.Response, error)

func (f profileRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProfilePreservesCallerOwnedTransports(t *testing.T) {
	var hits int
	rt := profileRoundTripper(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"response":{"candidates":[]}}`))}, nil
	})
	client := &http.Client{Transport: rt, Timeout: time.Second}
	d := NewDriver(client, Endpoints{})
	executeFixture(t, d, nil, fixtureCredential())
	if hits != 1 || len(d.transports.entries) != 0 || d.profileClient(client, fixtureCredential()).Timeout != time.Second {
		t.Fatal("custom RoundTripper or client semantics changed")
	}
	// A custom TLS dialer owns the handshake: its deliberate ALPN must survive.
	observed := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.TLS.NegotiatedProtocol
		fmt.Fprint(w, `{"response":{"candidates":[]}}`)
	}))
	t.Cleanup(srv.Close)
	base := srv.Client().Transport.(*http.Transport).Clone()
	config := base.TLSClientConfig.Clone()
	config.NextProtos = []string{"http/1.1"}
	base.DialTLSContext = (&tls.Dialer{Config: config}).DialContext
	custom := &http.Client{Transport: base}
	t.Cleanup(base.CloseIdleConnections)
	d = NewDriver(custom, Endpoints{APIBase: srv.URL})
	executeFixture(t, d, nil, fixtureCredential())
	if <-observed != "http/1.1" || d.profileClient(custom, fixtureCredential()).Transport != base || len(d.transports.entries) != 0 {
		t.Fatal("custom TLS dialer was replaced or incorrectly profiled")
	}
}
