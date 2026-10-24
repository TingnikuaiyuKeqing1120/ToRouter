// Run with go run ./scripts/release-manifest. This tool never creates keys.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"torouter/internal/updater"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	exe := flag.String("exe", "", "release executable")
	version := flag.String("version", "", "numeric release version")
	url := flag.String("url", "", "HTTPS artifact URL")
	notes := flag.String("notes", "", "UTF-8 release notes file")
	keyPath := flag.String("key", "", "private file containing a base64 Ed25519 64-byte private key")
	output := flag.String("output", "", "new signed manifest file")
	osName := flag.String("os", runtime.GOOS, "release OS")
	arch := flag.String("arch", runtime.GOARCH, "release architecture")
	flag.Parse()
	if *exe == "" || *version == "" || *url == "" || *keyPath == "" || *output == "" {
		return fmt.Errorf("required flags: -exe -version -url -key -output")
	}
	keyBytes, err := os.ReadFile(*keyPath)
	if err != nil {
		return fmt.Errorf("read signing key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyBytes)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing key must contain base64 Ed25519 private key")
	}
	info, err := os.Stat(*exe)
	if err != nil {
		return err
	}
	hash, err := updater.HashFile(*exe)
	if err != nil {
		return err
	}
	var text []byte
	if *notes != "" {
		text, err = os.ReadFile(*notes)
		if err != nil {
			return err
		}
	}
	release := updater.Release{Version: *version, OS: *osName, Arch: *arch, URL: *url, SHA256: hash, Size: info.Size(), Notes: string(text)}
	b, err := updater.SignManifest(release, ed25519.PrivateKey(key))
	if err != nil {
		return err
	}
	if _, err = updater.VerifyManifest(b, ed25519.PrivateKey(key).Public().(ed25519.PublicKey)); err != nil {
		return err
	}
	f, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
