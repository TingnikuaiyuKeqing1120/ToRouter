// Run with go run ./scripts/release-manifest. This tool never creates keys.
package main

import (
	"bytes"
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
	publicKey := flag.String("public-key", "", "base64 Ed25519 public key injected into the release executable")
	checkKey := flag.Bool("check-key", false, "only validate that -key matches -public-key; write no release files")
	output := flag.String("output", "", "new signed manifest file")
	osName := flag.String("os", runtime.GOOS, "release OS")
	arch := flag.String("arch", runtime.GOARCH, "release architecture")
	flag.Parse()
	if *checkKey && (*keyPath == "" || *publicKey == "") {
		return fmt.Errorf("key check requires -key and -public-key")
	}
	if !*checkKey && (*exe == "" || *version == "" || *url == "" || *keyPath == "" || *output == "") {
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
	// Derive from the seed rather than trusting the public half stored in the
	// private key. A damaged/mixed key must not pass preflight or sign a release.
	derived := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	if !bytes.Equal(key, derived) {
		return fmt.Errorf("signing key has inconsistent seed and public key")
	}
	verificationKey := derived.Public().(ed25519.PublicKey)
	if *publicKey != "" {
		decoded, err := base64.StdEncoding.DecodeString(*publicKey)
		if err != nil || len(decoded) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(decoded) != *publicKey {
			return fmt.Errorf("verification key must contain base64 Ed25519 public key")
		}
		verificationKey = ed25519.PublicKey(decoded)
		if !bytes.Equal(verificationKey, derived.Public().(ed25519.PublicKey)) {
			return fmt.Errorf("verification public key does not match signing private key")
		}
	}
	if *checkKey {
		return nil
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
	if _, err = updater.VerifyManifest(b, verificationKey); err != nil {
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
