package siwc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
	"torouter/internal/accessguard"
)

// IDClaims is the verified subset of the id_token. Subject is the account
// identity the gateway pins a session to.
type IDClaims struct {
	Subject string
	Email   string
	Name    string
	Issuer  string
	Expiry  time.Time
}

// VerifyIDToken checks an id_token against the issuer's JWKS and the nonce this
// flow issued. RS256 only: an alg the verifier did not choose is the classic
// way to make a verifier accept a token signed with a key the attacker holds.
//
// The audience is required to contain the client the flow was issued: a token
// without one is not one this flow can claim.
func VerifyIDToken(ctx context.Context, client *http.Client, keys keySource, issuer, idToken, expectedNonce, expectedClientID string) (IDClaims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return IDClaims{}, errors.New("id_token is not a JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return IDClaims{}, fmt.Errorf("id_token header is not base64url: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return IDClaims{}, err
	}
	if header.Alg != "RS256" {
		return IDClaims{}, fmt.Errorf("id_token alg %q is not RS256", header.Alg)
	}
	key, err := keys.Key(ctx, header.Kid)
	if err != nil {
		return IDClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return IDClaims{}, fmt.Errorf("id_token signature is not base64url: %w", err)
	}
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return IDClaims{}, fmt.Errorf("id_token signature does not verify: %w", err)
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return IDClaims{}, fmt.Errorf("id_token claims are not base64url: %w", err)
	}
	var claims struct {
		Issuer   string `json:"iss"`
		Subject  string `json:"sub"`
		Audience any    `json:"aud"`
		Expiry   int64  `json:"exp"`
		Nonce    string `json:"nonce"`
		Email    string `json:"email"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		return IDClaims{}, err
	}
	// 60 seconds of clock skew: the issuer's clock and ours are different
	// machines, and a token that expired a moment ago is not an attack.
	if claims.Expiry == 0 || time.Now().Add(60*time.Second).After(time.Unix(claims.Expiry, 0)) {
		return IDClaims{}, errors.New("id_token has expired")
	}
	if claims.Issuer != issuer {
		return IDClaims{}, fmt.Errorf("id_token issuer %q is not %q", claims.Issuer, issuer)
	}
	if claims.Nonce == "" || claims.Nonce != expectedNonce {
		return IDClaims{}, errors.New("id_token nonce does not match this flow")
	}
	// The audience is required and must contain the client the flow issued: the
	// official guidance names issuer, audience, expiration and nonce as the
	// claims to verify, and a token with no aud is not one this flow can claim.
	audiences := audienceList(claims.Audience)
	if len(audiences) == 0 {
		return IDClaims{}, errors.New("id_token carries no audience")
	}
	if !containsString(audiences, expectedClientID) {
		return IDClaims{}, fmt.Errorf("id_token audience %v does not contain the client %q", audiences, expectedClientID)
	}
	if claims.Subject == "" {
		return IDClaims{}, errors.New("id_token carries no subject")
	}
	return IDClaims{Subject: claims.Subject, Email: claims.Email, Name: claims.Name, Issuer: claims.Issuer, Expiry: time.Unix(claims.Expiry, 0).UTC()}, nil
}

// audienceList normalizes aud, which OIDC allows to be a string or an array.
func audienceList(v any) []string {
	switch value := v.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []string{value}
	case []any:
		out := []string{}
		for _, entry := range value {
			if s, ok := entry.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// keySource resolves a JWKS kid to a public key. It is an interface so tests
// can serve a fixed key without standing up an HTTP server for the JWKS alone.
type keySource interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// jwksCache fetches the issuer's key set once and reuses it, refetching when a
// kid it has never seen shows up — which is how a key rotation is survived
// without a restart.
type jwksCache struct {
	url     string
	client  *http.Client
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// jwksTTL is how long a fetched key set is trusted. A rotation publishes new
// kids; an unknown kid refetches anyway, so this is a bound on staleness, not
// on correctness.
const jwksTTL = time.Hour

func newJWKSCache(url string, client *http.Client) *jwksCache {
	return &jwksCache{url: url, client: client, keys: map[string]*rsa.PublicKey{}}
}

func (c *jwksCache) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.keys[kid]; ok {
		return key, nil
	}
	// An unknown kid means the issuer rotated its keys, so the fetch bypasses
	// the TTL: honouring it would refuse every token signed with the new key
	// until the hour ran out, which is a rotation seen as an outage.
	if err := c.refetchLocked(ctx, true); err != nil {
		return nil, err
	}
	key, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("issuer published no key for kid %q", kid)
	}
	return key, nil
}

func (c *jwksCache) refetchLocked(ctx context.Context, force bool) error {
	if !force && !c.fetched.IsZero() && time.Since(c.fetched) < jwksTTL {
		return errors.New("issuer key set is cached and does not contain the requested kid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := accessguard.Do(c.client, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks returned HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Keys []struct {
			Kid string   `json:"kid"`
			Alg string   `json:"alg"`
			Kty string   `json:"kty"`
			N   string   `json:"n"`
			E   string   `json:"e"`
			X5C []string `json:"x5c"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, entry := range reply.Keys {
		if entry.Kid == "" {
			continue
		}
		// Only RSA keys can serve RS256. An issuer that publishes EC keys
		// alongside would otherwise make a kid ambiguous — and an entry we
		// cannot use must not shadow one we can.
		if entry.Kty != "" && entry.Kty != "RSA" {
			continue
		}
		key, err := rsaKeyFromJWK(entry.N, entry.E, entry.X5C)
		if err != nil {
			continue
		}
		keys[entry.Kid] = key
	}
	if len(keys) == 0 {
		return errors.New("jwks contained no usable rsa key")
	}
	c.keys = keys
	c.fetched = time.Now()
	return nil
}

// rsaKeyFromJWK builds the public key from the modulus and exponent, falling
// back to the x5c certificate when the numbers are absent.
func rsaKeyFromJWK(n, e string, x5c []string) (*rsa.PublicKey, error) {
	if n != "" && e != "" {
		modulus, err := base64.RawURLEncoding.DecodeString(n)
		if err != nil {
			return nil, err
		}
		exponent, err := base64.RawURLEncoding.DecodeString(e)
		if err != nil {
			return nil, err
		}
		public := &rsa.PublicKey{E: int(new(big.Int).SetBytes(exponent).Int64())}
		public.N = new(big.Int).SetBytes(modulus)
		if public.E <= 0 || public.N.Sign() <= 0 {
			return nil, errors.New("jwks entry carries an unusable modulus or exponent")
		}
		return public, nil
	}
	if len(x5c) > 0 {
		der, err := base64.StdEncoding.DecodeString(x5c[0])
		if err != nil {
			return nil, err
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		if key, ok := certificate.PublicKey.(*rsa.PublicKey); ok {
			return key, nil
		}
	}
	return nil, errors.New("jwks entry carries neither n/e nor x5c")
}
