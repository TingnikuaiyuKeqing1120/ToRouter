// Package accessguard carries an optional caller authorization check through
// existing domain operations. Calls without a guard retain their old behavior.
package accessguard

import (
	"context"
	"errors"
	"net/http"
)

type key struct{}
type denial struct{ cause error }

func (e *denial) Error() string { return e.cause.Error() }
func (e *denial) Unwrap() error { return e.cause }

func With(ctx context.Context, check func() error) context.Context {
	return context.WithValue(ctx, key{}, check)
}
func Present(ctx context.Context) bool {
	check, _ := ctx.Value(key{}).(func() error)
	return check != nil
}
func Check(ctx context.Context) error {
	check, _ := ctx.Value(key{}).(func() error)
	if check == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := check(); err != nil {
		return &denial{err}
	}
	return nil
}
func Denied(err error) bool { var denied *denial; return errors.As(err, &denied) }

type guardedTransport struct{ base http.RoundTripper }

func (t guardedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := Check(r.Context()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(r)
}
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return guardedTransport{base}
}

// Do copies only the client, preserving its redirect policy, timeout and jar.
// The per-RoundTrip gate therefore also covers every followed redirect. No
// global client is mutated, including clients shared with normal inference.
func Do(client *http.Client, r *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if !Present(r.Context()) {
		return client.Do(r)
	}
	copy := *client
	copy.Transport = Transport(client.Transport)
	return copy.Do(r)
}
