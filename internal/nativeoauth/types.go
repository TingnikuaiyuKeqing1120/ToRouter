// Package nativeoauth shares the credential lifecycle of native subscription
// adapters. Drivers own vendor wire formats; the gateway owns routing,
// cross-protocol conversion, accounting and retry decisions.
package nativeoauth

import (
	"context"
	"errors"
	"net/http"

	"torouter/internal/oauthbase"
	"torouter/internal/store"
)

// Drivers wrap this only before sending when a logical request cannot be
// represented without losing client context. A transport error after sending
// must keep the gateway's conservative, possibly-executed classification.
var ErrUnsupportedRequest = errors.New("native provider cannot preserve this request")

type Definition struct {
	ID             string
	Name           string
	CredentialType string
	Provider       string
	CallbackHost   string
	CallbackPath   string
	CallbackPorts  []int
	Endpoints      []store.Endpoint
}

// Authorization lives only in the existing console flow book, never in a
// public channel or a second credential database.
type Authorization struct {
	URL         string
	State       string
	Verifier    string
	RedirectURI string
}

type ConsentTarget struct {
	ChannelID    int64
	CredentialID string
	Identity     string
	Generation   int64
}

type Model struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Format string `json:"format"`
}

type Driver interface {
	Definition() Definition
	Begin(context.Context, string) (Authorization, error)
	Exchange(context.Context, Authorization, oauthbase.CallbackResult) (store.OAuthCredential, error)
	Refresh(context.Context, store.OAuthCredential) (store.OAuthCredential, error)
	Models(context.Context, store.OAuthCredential) ([]Model, error)
	// Execute receives a request in the declared logical protocol, with its
	// guarded context. It may wrap/unwrap vendor transport, but must return
	// the same logical protocol and preserve genuine failure/terminal facts.
	// No internal inference retries, implicit CLI execution or source-file reads.
	Execute(*http.Client, *http.Request, store.OAuthCredential) (*http.Response, error)
}
