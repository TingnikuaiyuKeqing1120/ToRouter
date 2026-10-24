package console

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
	"torouter/internal/configbackup"
	"torouter/internal/store"
)

const backupTestPassword = "synthetic-backup-password-123"

func TestConfigBackupHTTPExportPreviewRestore(t *testing.T) {
	source := newDiscoveryConsoleFixture(t)
	destination := newDiscoveryConsoleFixture(t)

	// Populate channel on source
	srcChan := store.Channel{
		Name:    "Source Channel for Backup",
		Enabled: true,
		Models:  []string{"backup-model-1"},
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"},
		},
		Credentials: []store.CredentialItem{
			{
				ID:      "cred-backup-1",
				APIKey:  "sk-backup-secret-key-12345",
				Enabled: true,
			},
		},
	}
	if err := source.store.SaveChannel(&srcChan); err != nil {
		t.Fatalf("SaveChannel on source failed: %v", err)
	}

	// 1. Export with short password -> 400
	source.local(t, "POST", "/config-backup/export", map[string]any{
		"password": "short",
	}, 400)

	// 2. Export with valid password -> 200
	exportOut := source.local(t, "POST", "/config-backup/export", map[string]any{
		"password": backupTestPassword,
	}, 200)

	var exportResp struct {
		File     string `json:"file"`
		Filename string `json:"filename"`
	}
	discoveryDecode(t, exportOut, &exportResp)
	if exportResp.File == "" || !strings.HasSuffix(exportResp.Filename, ".json") {
		t.Fatalf("export did not return valid encrypted file: %+v", exportResp)
	}

	// 3. Preview with wrong password -> 400
	destination.local(t, "POST", "/config-backup/preview", map[string]any{
		"file":     exportResp.File,
		"password": "wrong-password-12345678",
	}, 400)

	// 4. Preview with empty file -> 400
	destination.local(t, "POST", "/config-backup/preview", map[string]any{
		"file":     "",
		"password": backupTestPassword,
	}, 400)

	// 5. Preview with correct password -> 200
	previewOut := destination.local(t, "POST", "/config-backup/preview", map[string]any{
		"file":     exportResp.File,
		"password": backupTestPassword,
	}, 200)

	var previewResp struct {
		Summary store.FullConfigSummary `json:"summary"`
	}
	discoveryDecode(t, previewOut, &previewResp)
	if previewResp.Summary.ChannelsCount != 1 {
		t.Fatalf("preview summary expected 1 channel, got %d", previewResp.Summary.ChannelsCount)
	}

	// 6. Restore without confirm -> 400
	destination.local(t, "POST", "/config-backup/restore", map[string]any{
		"file":     exportResp.File,
		"password": backupTestPassword,
		"confirm":  false,
	}, 400)

	// 7. Restore with confirm -> 200
	restoreOut := destination.local(t, "POST", "/config-backup/restore", map[string]any{
		"file":     exportResp.File,
		"password": backupTestPassword,
		"confirm":  true,
	}, 200)

	var restoreResp struct {
		OK              bool   `json:"ok"`
		RestartRequired bool   `json:"restart_required"`
		Snapshot        string `json:"snapshot"`
	}
	discoveryDecode(t, restoreOut, &restoreResp)
	if !restoreResp.OK || !restoreResp.RestartRequired || restoreResp.Snapshot == "" {
		t.Fatalf("unexpected restore response: %+v", restoreResp)
	}

	// 8. Verify destination store has restored channel with decrypted secret
	dstChannels, err := destination.store.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels on destination failed: %v", err)
	}
	if len(dstChannels) != 1 {
		t.Fatalf("expected 1 restored channel on destination, got %d", len(dstChannels))
	}
	if dstChannels[0].Name != "Source Channel for Backup" {
		t.Errorf("expected channel name 'Source Channel for Backup', got '%s'", dstChannels[0].Name)
	}
	if len(dstChannels[0].Credentials) != 1 || dstChannels[0].Credentials[0].APIKey != "sk-backup-secret-key-12345" {
		t.Errorf("channel credentials did not restore properly: %+v", dstChannels[0].Credentials)
	}

	// 9. Query rollback status via GET /config-backup/rollback
	rollbackStatusOut := destination.local(t, "GET", "/config-backup/rollback", nil, 200)
	var rollbackStatus struct {
		Available bool   `json:"available"`
		Snapshot  string `json:"snapshot"`
	}
	discoveryDecode(t, rollbackStatusOut, &rollbackStatus)
	if !rollbackStatus.Available || rollbackStatus.Snapshot != restoreResp.Snapshot {
		t.Fatalf("expected available snapshot %q, got %+v", restoreResp.Snapshot, rollbackStatus)
	}

	// 10. Rollback without specifying snapshot name -> 200
	rollbackOut := destination.local(t, "POST", "/config-backup/rollback", map[string]any{
		"confirm": true,
	}, 200)
	var rollbackResp struct {
		OK               bool   `json:"ok"`
		RestartRequired  bool   `json:"restart_required"`
		RestoredSnapshot string `json:"restored_snapshot"`
	}
	discoveryDecode(t, rollbackOut, &rollbackResp)
	if !rollbackResp.OK || rollbackResp.RestoredSnapshot != restoreResp.Snapshot {
		t.Fatalf("unexpected rollback response: %+v", rollbackResp)
	}
}

func encryptRawBackup(t *testing.T, plain []byte, password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	raw, err := json.Marshal(map[string]any{
		"format":     configbackup.FileFormat,
		"version":    1,
		"kdf":        "argon2id-64m-t3-p2",
		"salt":       encode(salt),
		"nonce":      encode(nonce),
		"ciphertext": encode(gcm.Seal(nil, nonce, plain, []byte(configbackup.FileAAD))),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func storeFingerprint(t *testing.T, s *store.Store) string {
	c, err := s.ExportFullConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.ExportedAt = time.Time{}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConfigBackupHTTPRequiredStructureValidation(t *testing.T) {
	source := newDiscoveryConsoleFixture(t)
	destination := newDiscoveryConsoleFixture(t)

	// Set destination initial settings and channel so we can check fingerprint
	dstChan := store.Channel{
		Name:    "Destination Initial Channel",
		Enabled: true,
		APIKey:  "sk-dest-key-12345678",
		Models:  []string{"dest-model"},
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://dest.example.com/v1"},
		},
	}
	if err := destination.store.SaveChannel(&dstChan); err != nil {
		t.Fatal(err)
	}
	if err := destination.store.SaveDownstreamModel(store.DownstreamModel{
		ID:      "dest-public-model",
		Enabled: true,
		Bindings: []store.Binding{
			{ChannelID: dstChan.ID, Model: "dest-model", Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	dstSettings, err := destination.store.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	dstSettings.MaxAttempts = 7
	if err := destination.store.SaveSettings(dstSettings); err != nil {
		t.Fatal(err)
	}

	// Populate source store
	srcChan := store.Channel{
		Name:    "Source Channel",
		Enabled: true,
		Models:  []string{"src-model"},
		Endpoints: []store.Endpoint{
			{Format: store.FormatOpenAIChatCompletions, BaseURL: "https://src.example.com/v1"},
		},
		Credentials: []store.CredentialItem{
			{ID: "c1", APIKey: "sk-src", Enabled: true},
		},
	}
	if err := source.store.SaveChannel(&srcChan); err != nil {
		t.Fatal(err)
	}
	if err := source.store.SaveDownstreamModel(store.DownstreamModel{
		ID:      "src-public-model",
		Enabled: true,
		Bindings: []store.Binding{
			{ChannelID: srcChan.ID, Model: "src-model", Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := source.store.ExportFullConfig()
	if err != nil {
		t.Fatal(err)
	}
	originalJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	initialFingerprint := storeFingerprint(t, destination.store)

	for _, field := range []string{"channels", "downstream_models", "settings"} {
		for _, form := range []string{"missing", "null"} {
			t.Run(field+"_"+form, func(t *testing.T) {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(originalJSON, &obj); err != nil {
					t.Fatal(err)
				}
				if form == "missing" {
					delete(obj, field)
				} else {
					obj[field] = json.RawMessage("null")
				}
				raw, err := json.Marshal(obj)
				if err != nil {
					t.Fatal(err)
				}
				encrypted := encryptRawBackup(t, raw, backupTestPassword)

				// Preview -> 400
				destination.local(t, "POST", "/config-backup/preview", map[string]any{
					"file":     encrypted,
					"password": backupTestPassword,
				}, 400)

				// Restore -> 400
				destination.local(t, "POST", "/config-backup/restore", map[string]any{
					"file":     encrypted,
					"password": backupTestPassword,
					"confirm":  true,
				}, 400)

				// Destination fingerprint must be unchanged
				currentFingerprint := storeFingerprint(t, destination.store)
				if currentFingerprint != initialFingerprint {
					t.Fatalf("destination store was modified on rejected %s_%s", field, form)
				}
			})
		}
	}

	// Legal empty store export/preview/restore succeeds
	emptySource := newDiscoveryConsoleFixture(t)
	emptyExportOut := emptySource.local(t, "POST", "/config-backup/export", map[string]any{
		"password": backupTestPassword,
	}, 200)
	var emptyExportResp struct {
		File string `json:"file"`
	}
	discoveryDecode(t, emptyExportOut, &emptyExportResp)

	destination.local(t, "POST", "/config-backup/preview", map[string]any{
		"file":     emptyExportResp.File,
		"password": backupTestPassword,
	}, 200)

	destination.local(t, "POST", "/config-backup/restore", map[string]any{
		"file":     emptyExportResp.File,
		"password": backupTestPassword,
		"confirm":  true,
	}, 200)
}
