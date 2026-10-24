package management

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunStdioFromEnvironment is a transport-only bridge. It does not resolve the
// data directory, open a database, start the gateway, or print a startup banner.
func RunStdioFromEnvironment(ctx context.Context) error {
	return BridgeStdio(ctx, os.Getenv("TOROUTER_MCP_URL"), os.Getenv("TOROUTER_MCP_TOKEN"), &mcp.StdioTransport{})
}

// BridgeStdio relays SDK connections without reimplementing JSON-RPC or
// maintaining a second copy of tools/resources/prompts. All authorization and
// domain work stays in the single running HTTP gateway.
func BridgeStdio(ctx context.Context, endpoint, token string, local mcp.Transport) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !loopbackHost(u.Hostname()) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/mcp" {
		return errors.New("TOROUTER_MCP_URL must be the running gateway's local /mcp URL; update it after a port change")
	}
	if !strings.HasPrefix(token, "tr_mcp_") || strings.ContainsAny(token, "\r\n\t ") {
		return errors.New("TOROUTER_MCP_TOKEN must contain a management token created in the console")
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	defer base.CloseIdleConnections()
	client := &http.Client{Transport: &managementTransport{base: base, origin: u.Scheme + "://" + u.Host, token: token}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("management redirects are disabled") }}
	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	remote, err := (&mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}).Connect(bridgeCtx)
	if err != nil {
		return errors.New("cannot connect to the local MCP gateway; verify its actual address and authorization in the console")
	}
	defer remote.Close()
	stdio, err := local.Connect(bridgeCtx)
	if err != nil {
		return errors.New("cannot open MCP standard input/output")
	}
	defer stdio.Close()
	finished := make(chan error, 2)
	pump := func(from, to mcp.Connection) {
		for {
			msg, readErr := from.Read(bridgeCtx)
			if readErr != nil {
				finished <- readErr
				return
			}
			if writeErr := to.Write(bridgeCtx, msg); writeErr != nil {
				finished <- writeErr
				return
			}
		}
	}
	go pump(stdio, remote)
	go pump(remote, stdio)
	select {
	case <-ctx.Done():
		return nil
	case err := <-finished:
		if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			return nil
		}
		return errors.New("MCP connection ended; verify the gateway and reconnect. For writes inspect the receipt before retrying")
	}
}

type managementTransport struct {
	base          http.RoundTripper
	origin, token string
}

func (t *managementTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != t.origin {
		return nil, errors.New("MCP credential destination changed")
	}
	clone := r.Clone(r.Context())
	clone.Header = clone.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}
