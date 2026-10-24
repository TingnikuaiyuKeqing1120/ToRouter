package configbackup

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"torouter/internal/accessguard"
)

const (
	MaxFileBytes  = 12 << 20 // 12 MB
	maxPlainBytes = 7 << 20  // 7 MB
	FileFormat    = "torouter-full-config"
	FileAAD       = "torouter-full-config/v1/argon2id-64m-t3-p2/aes-256-gcm"
	CurrentVersion = 1
)

var (
	ErrFile     = errors.New("backup file or password is invalid")
	ErrPassword = errors.New("password must contain 12 to 256 characters")
	ErrLimit    = errors.New("backup file exceeds size limit")
	ErrVersion  = errors.New("unsupported backup version")
)

type envelope struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

var derivations = make(chan struct{}, 2)

func strictJSON(raw []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return ErrFile
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return ErrFile
	}
	return nil
}

func validPassword(password string) bool {
	n := utf8.RuneCountInString(password)
	return utf8.ValidString(password) && n >= 12 && n <= 256 && len(password) <= 1024
}

func derive(ctx context.Context, password string, salt []byte) ([]byte, error) {
	if err := accessguard.Check(ctx); err != nil {
		return nil, err
	}
	select {
	case derivations <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-derivations }()
	secret := []byte(password)
	defer clear(secret)
	key := argon2.IDKey(secret, salt, 3, 64*1024, 2, 32)
	if err := accessguard.Check(ctx); err != nil {
		clear(key)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

func IsEncryptedEnvelope(raw []byte) bool {
	var probe struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.Format == FileFormat
}

func Encrypt(ctx context.Context, p *FullConfigPayload, password string) (string, error) {
	if !validPassword(password) {
		return "", ErrPassword
	}
	p.Version = CurrentVersion
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if len(plain) > maxPlainBytes {
		return "", ErrLimit
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	key, err := derive(ctx, password, salt)
	if err != nil {
		return "", err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, []byte(FileAAD))
	env := envelope{
		Format:     FileFormat,
		Version:    CurrentVersion,
		KDF:        "argon2id-64m-t3-p2",
		Salt:       base64.RawURLEncoding.EncodeToString(salt),
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	if len(out) > MaxFileBytes {
		return "", ErrLimit
	}
	return string(out), nil
}

func Decrypt(ctx context.Context, raw []byte, password string) (*FullConfigPayload, error) {
	if len(raw) > MaxFileBytes {
		return nil, ErrLimit
	}
	if !validPassword(password) {
		return nil, ErrPassword
	}
	var env envelope
	if err := strictJSON(raw, &env); err != nil {
		return nil, err
	}
	if env.Format != FileFormat || env.Version != CurrentVersion || env.KDF != "argon2id-64m-t3-p2" {
		return nil, ErrFile
	}
	salt, err := base64.RawURLEncoding.DecodeString(env.Salt)
	if err != nil || len(salt) != 16 {
		return nil, ErrFile
	}
	nonce, err := base64.RawURLEncoding.DecodeString(env.Nonce)
	if err != nil || len(nonce) != 12 {
		return nil, ErrFile
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(env.Ciphertext)
	if err != nil || len(ciphertext) < 16 {
		return nil, ErrFile
	}
	key, err := derive(ctx, password, salt)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(FileAAD))
	if err != nil {
		return nil, ErrFile
	}
	defer clear(plain)
	if len(plain) > maxPlainBytes {
		return nil, ErrLimit
	}
	var payload FullConfigPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, ErrFile
	}
	if payload.Version != CurrentVersion {
		return nil, ErrVersion
	}
	return &payload, nil
}
