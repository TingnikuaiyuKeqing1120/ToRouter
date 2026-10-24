package configbackup

import (
	"context"
	"strings"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestConfigBackupCryptoRoundtrip(t *testing.T) {
	ctx := context.Background()
	payload := &FullConfigPayload{
		Version:    1,
		ExportedAt: time.Now().UTC().Truncate(time.Second),
		Channels: []store.Channel{
			{
				ID:      1,
				Name:    "Test Channel",
				Enabled: true,
				Models:  []string{"model-a", "model-b"},
				Credentials: []store.CredentialItem{
					{
						ID:      "cred-1",
						APIKey:  "sk-test-secret-12345678",
						Enabled: true,
					},
				},
			},
		},
		DownstreamModels: []store.DownstreamModel{
			{
				ID:          "gpt-4o",
				DisplayName: "GPT-4o",
				Enabled:     true,
				Bindings: []store.Binding{
					{ChannelID: 1, Model: "model-a", Priority: 1, Enabled: true},
				},
			},
		},
		Settings: store.Settings{
			RetentionDays:          30,
			MaxAttempts:            3,
			RetriesPerChannel:      2,
			FailureCooldownSeconds: 10,
		},
	}

	password := "correct-horse-battery-staple-123"
	encrypted, err := Encrypt(ctx, payload, password)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if !IsEncryptedEnvelope([]byte(encrypted)) {
		t.Fatal("IsEncryptedEnvelope returned false for encrypted payload")
	}

	// Decrypt with correct password
	restored, err := Decrypt(ctx, []byte(encrypted), password)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if restored.Version != 1 {
		t.Errorf("expected version 1, got %d", restored.Version)
	}
	if len(restored.Channels) != 1 || restored.Channels[0].Name != "Test Channel" {
		t.Errorf("channel mismatch: %+v", restored.Channels)
	}
	if len(restored.DownstreamModels) != 1 || restored.DownstreamModels[0].ID != "gpt-4o" {
		t.Errorf("model mismatch: %+v", restored.DownstreamModels)
	}

	// Decrypt with wrong password
	_, err = Decrypt(ctx, []byte(encrypted), "wrong-password-123456789")
	if err == nil {
		t.Fatal("expected error with wrong password, got nil")
	}

	// Short password
	_, err = Encrypt(ctx, payload, "short")
	if err != ErrPassword {
		t.Fatalf("expected ErrPassword for short password, got %v", err)
	}

	// Tampered ciphertext
	tampered := strings.Replace(encrypted, "a", "b", 1)
	_, err = Decrypt(ctx, []byte(tampered), password)
	if err == nil {
		t.Fatal("expected error with tampered ciphertext, got nil")
	}

	// Payload exceeding limit rejected
	hugePayload := &FullConfigPayload{
		Version: 1,
		Channels: []store.Channel{
			{Name: strings.Repeat("x", maxPlainBytes+100)},
		},
	}
	_, err = Encrypt(ctx, hugePayload, password)
	if err != ErrLimit {
		t.Fatalf("expected ErrLimit for huge payload, got %v", err)
	}

	// Input exceeding MaxFileBytes rejected by Decrypt
	hugeRaw := make([]byte, MaxFileBytes+10)
	_, err = Decrypt(ctx, hugeRaw, password)
	if err != ErrLimit {
		t.Fatalf("expected ErrLimit for huge raw file, got %v", err)
	}
}
