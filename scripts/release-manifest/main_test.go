package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"torouter/internal/updater"
)

func command(t *testing.T, args ...string) error {
	t.Helper()
	oldFlags, oldArgs := flag.CommandLine, os.Args
	flag.CommandLine = flag.NewFlagSet("release-manifest", flag.ContinueOnError)
	os.Args = append([]string{"release-manifest"}, args...)
	defer func() { flag.CommandLine, os.Args = oldFlags, oldArgs }()
	return run()
}

func fixtureKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic-private.key")
	if err = os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0600); err != nil {
		t.Fatal(err)
	}
	return pub, key, path
}

func TestCheckKeyMatchesPublicKey(t *testing.T) {
	pub, _, path := fixtureKey(t)
	if err := command(t, "-check-key", "-key", path, "-public-key", base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
}

func TestCheckKeyRejectsDifferentValidPublicKey(t *testing.T) {
	_, _, path := fixtureKey(t)
	other, _, _ := fixtureKey(t)
	if err := command(t, "-check-key", "-key", path, "-public-key", base64.StdEncoding.EncodeToString(other)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected explicit key mismatch, got %v", err)
	}
}

func TestRejectsInconsistentPrivateKey(t *testing.T) {
	pub, key, path := fixtureKey(t)
	key[0] ^= 1 // Keep the stored public half, alter the seed.
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command(t, "-check-key", "-key", path, "-public-key", base64.StdEncoding.EncodeToString(pub)); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("expected inconsistent private key error, got %v", err)
	}
}

func TestSignAndVerifyWithInjectedPublicKey(t *testing.T) {
	pub, _, path := fixtureKey(t)
	dir := t.TempDir()
	exe, manifest := filepath.Join(dir, "fixture.exe"), filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(exe, []byte("synthetic executable"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-exe", exe, "-version", "1.2.3", "-url", "https://example.invalid/fixture.exe", "-key", path, "-output", manifest}
	other, _, _ := fixtureKey(t)
	if err := command(t, append(args, "-public-key", base64.StdEncoding.EncodeToString(other))...); err == nil {
		t.Fatal("wrong injected public key accepted")
	}
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatal("mismatched key wrote a manifest")
	}
	if err := command(t, append(args, "-public-key", base64.StdEncoding.EncodeToString(pub))...); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = updater.VerifyManifest(data, pub); err != nil {
		t.Fatal(err)
	}
}

func TestExistingSigningFlagsRemainUsable(t *testing.T) {
	pub, _, path := fixtureKey(t)
	dir := t.TempDir()
	exe, manifest := filepath.Join(dir, "fixture.exe"), filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(exe, []byte("synthetic executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command(t, "-exe", exe, "-version", "1.2.3", "-url", "https://example.invalid/fixture.exe", "-key", path, "-output", manifest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = updater.VerifyManifest(data, pub); err != nil {
		t.Fatal(err)
	}
}
