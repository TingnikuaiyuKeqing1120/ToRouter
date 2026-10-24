//go:build !windows

package cliview

// There is no portable API for raising another terminal's window.
func claimCLIView(string) (func(), bool, error) { return func() {}, false, nil }
