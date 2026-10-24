// Package updater verifies releases and coordinates replacement of one executable.
package updater

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const MaxManifestSize = 256 << 10
const MaxArtifactSize int64 = 512 << 20

type Release struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Notes   string `json:"notes"`
}

// SignedManifest preserves the exact bytes signed by the publisher.
type SignedManifest struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

func ParsePublicKey(value string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid update verification key")
	}
	return ed25519.PublicKey(b), nil
}

func SignManifest(release Release, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid signing key")
	}
	payload, err := json.Marshal(release)
	if err != nil {
		return nil, err
	}
	return json.Marshal(SignedManifest{payload, base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
}

func decodeStrict(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}

func VerifyManifest(data []byte, key ed25519.PublicKey) (Release, error) {
	var envelope SignedManifest
	var release Release
	if len(data) > MaxManifestSize {
		return release, errors.New("update manifest too large")
	}
	if err := decodeStrict(data, &envelope); err != nil {
		return release, fmt.Errorf("invalid update manifest: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, envelope.Payload, sig) {
		return release, errors.New("update signature verification failed")
	}
	if err := decodeStrict(envelope.Payload, &release); err != nil {
		return release, fmt.Errorf("invalid signed release: %w", err)
	}
	if _, err := versionParts(release.Version); err != nil {
		return release, err
	}
	hash, err := hex.DecodeString(release.SHA256)
	if err != nil || len(hash) != 32 || release.Size <= 0 || release.Size > MaxArtifactSize || release.OS == "" || release.Arch == "" {
		return release, errors.New("invalid release artifact metadata")
	}
	return release, nil
}

func validateURL(raw string, allowLoopback bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid update URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if allowLoopback && u.Scheme == "http" && ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("update URL requires HTTPS")
}

// Release versions are stable numeric triples. Pre-release builds must use an
// explicit separate feed before that policy can be extended.
func versionParts(version string) ([3]uint64, error) {
	var result [3]uint64
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 {
		return result, errors.New("release version must be a numeric major.minor.patch")
	}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return result, errors.New("invalid release version")
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return result, errors.New("invalid release version")
			}
		}
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return result, errors.New("invalid release version")
		}
		result[i] = n
	}
	return result, nil
}

func newer(candidate, current string) (bool, error) {
	a, err := versionParts(candidate)
	if err != nil {
		return false, err
	}
	b, err := versionParts(current)
	if err != nil {
		return false, err
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i], nil
		}
	}
	return false, nil
}
