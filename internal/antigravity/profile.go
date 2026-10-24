package antigravity

import (
	"crypto/sha256"
	"crypto/tls"
	"net/http"
	"strings"
	"sync"
	"time"

	"torouter/internal/store"
)

// Go Hub profile: HTTP/pooling from CLIProxyAPI e7d4c7316fca's executor,
// UA from d946c4fba6fc's misc/antigravity_version.go. Release and changelist
// must be updated together. See SOURCE.md for evidence and custom TLS limits.
const (
	clientVersion          = "2.24.0"
	hubPlatform            = "os_type=darwin; arch=arm64"
	hubChangelist          = "1000335682"
	nodeAPIClient          = "google-api-nodejs-client/10.3.0"
	googAPIClient          = "gl-node/22.21.1"
	transportCacheCapacity = 64
	transportIdleTimeout   = 30 * time.Second
)

func userAgent() string {
	return "antigravity/hub/" + clientVersion + " (aidev_client; " + hubPlatform + "; cl=" + hubChangelist + ")"
}

type transportKey struct {
	identity [32]byte
	base     *http.Transport
}

type transportEntry struct {
	transport *http.Transport
	used      uint64
}

// The driver's pools retain no raw account IDs or tokens. The source transport
// pointer scopes proxy/dial/certificate policy; callers replace transports when
// changing their configuration, as net/http requires after first use.
type transportCache struct {
	mu      sync.Mutex
	clock   uint64
	entries map[transportKey]transportEntry
}

func credentialScope(c store.OAuthCredential) [32]byte {
	for _, field := range []struct{ name, value string }{
		{"subject", c.Subject}, {"account", c.Account}, {"refresh", c.RefreshToken}, {"access", c.AccessToken},
	} {
		if value := strings.TrimSpace(field.value); value != "" {
			return sha256.Sum256([]byte(field.name + ":" + c.ClientID + ":" + value))
		}
	}
	return sha256.Sum256([]byte("unauthenticated"))
}

func (d *Driver) profileClient(client *http.Client, c store.OAuthCredential) *http.Client {
	if client == nil {
		client = d.client
	}
	copy := *client
	roundTripper := client.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	base, ok := roundTripper.(*http.Transport)
	if ok && base == nil {
		base, ok = http.DefaultTransport.(*http.Transport)
	}
	// An opaque RoundTripper or TLS dialer owns its protocol and connection
	// semantics. Do not unwrap or replace it to claim a profile we cannot enforce.
	if !ok || base == nil || base.DialTLSContext != nil || base.DialTLS != nil {
		return &copy
	}
	key := transportKey{identity: credentialScope(c), base: base}
	d.transports.mu.Lock()
	defer d.transports.mu.Unlock()
	d.transports.clock++
	if entry, found := d.transports.entries[key]; found {
		entry.used = d.transports.clock
		d.transports.entries[key] = entry
		copy.Transport = entry.transport
		return &copy
	}
	if d.transports.entries == nil {
		d.transports.entries = make(map[transportKey]transportEntry)
	}
	if len(d.transports.entries) >= transportCacheCapacity {
		var oldest transportKey
		var used uint64 = ^uint64(0)
		for k, entry := range d.transports.entries {
			if entry.used < used {
				oldest, used = k, entry.used
			}
		}
		d.transports.entries[oldest].transport.CloseIdleConnections()
		delete(d.transports.entries, oldest)
	}
	transport := base.Clone()
	transport.ForceAttemptHTTP2 = false
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = new(tls.Config)
	}
	transport.TLSClientConfig.NextProtos = nil
	if transport.IdleConnTimeout == 0 || transport.IdleConnTimeout > transportIdleTimeout {
		transport.IdleConnTimeout = transportIdleTimeout
	}
	if transport.MaxIdleConnsPerHost == 0 || transport.MaxIdleConnsPerHost > 2 {
		transport.MaxIdleConnsPerHost = 2
	}
	if transport.MaxIdleConns == 0 || transport.MaxIdleConns > 8 {
		transport.MaxIdleConns = 8
	}
	d.transports.entries[key] = transportEntry{transport: transport, used: d.transports.clock}
	copy.Transport = transport
	return &copy
}
