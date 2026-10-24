package store

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func newTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open store failed: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFullConfigExportAndRestoreCrossStore(t *testing.T) {
	tmpSrc := t.TempDir()
	tmpDst := t.TempDir()

	srcStore := newTestStore(t, tmpSrc)
	dstStore := newTestStore(t, tmpDst)

	// Verify different master.key
	srcKey, _ := os.ReadFile(filepath.Join(tmpSrc, "master.key"))
	dstKey, _ := os.ReadFile(filepath.Join(tmpDst, "master.key"))
	if string(srcKey) == string(dstKey) {
		t.Fatal("expected distinct master.key between src and dst stores")
	}

	// 1. Populate Source Store
	srcChan := Channel{
		Name:    "Source OpenAI",
		Enabled: true,
		Models:  []string{"gpt-4o", "o3-mini"},
		Endpoints: []Endpoint{
			{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"},
		},
		Credentials: []CredentialItem{
			{
				ID:      "cred-src-1",
				APIKey:  "sk-src-secret-12345678",
				Enabled: true,
			},
		},
		DispatchMode: "round_robin",
	}
	if err := srcStore.SaveChannel(&srcChan); err != nil {
		t.Fatalf("SaveChannel on src failed: %v", err)
	}

	warmupCfg := DefaultCredentialOperationsConfig()
	warmupCfg.Warmup.Enabled = true
	warmupCfg.Warmup.Mode = "reset"
	warmupCfg.Warmup.Model = "gpt-4o"
	codexChan := Channel{
		Name:           "Source Codex OAuth",
		Enabled:        true,
		CredentialType: CredentialCodexOAuth,
		Models:         []string{"gpt-4o"},
		Endpoints: []Endpoint{
			{Format: FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api"},
		},
		Credentials: []CredentialItem{
			{
				ID:         "cred-codex-1",
				OAuth:      &OAuthCredential{Provider: "codex", ClientID: "synthetic-client-id", AccessToken: "synthetic-token", RefreshToken: "synthetic-refresh"},
				Enabled:    true,
				Operations: &warmupCfg,
			},
		},
		DispatchMode: "round_robin",
	}
	if err := srcStore.SaveChannel(&codexChan); err != nil {
		t.Fatalf("SaveChannel codex on src failed: %v", err)
	}

	// Save downstream model on src
	err := srcStore.SaveDownstreamModel(DownstreamModel{
		ID:          "openai-4o",
		DisplayName: "OpenAI 4o",
		Enabled:     true,
		Bindings: []Binding{
			{ChannelID: srcChan.ID, Model: "gpt-4o", Priority: 1, Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("SaveDownstreamModel on src failed: %v", err)
	}

	// Save client key on src
	_, clientToken, err := srcStore.CreateClientKey("Test Client")
	if err != nil {
		t.Fatalf("CreateClientKey on src failed: %v", err)
	}

	// Save plan price on src
	planAmount := 20.0
	firstDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := srcStore.db.Exec("INSERT INTO plan_prices(channel_id, default_amount, currency, first_payment_at, timezone, active_from) VALUES(?, ?, 'USD', ?, 'UTC', ?)",
		srcChan.ID, planAmount, firstDate, time.Now().UnixMilli()); err != nil {
		t.Fatalf("Insert plan_prices on src failed: %v", err)
	}

	// Save channel-specific price in Settings on src
	srcSettings, err := srcStore.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings on src failed: %v", err)
	}
	srcSettings.Prices = []Price{
		{
			ChannelID:   srcChan.ID,
			Model:       "gpt-4o",
			DisplayName: "Source 4o Channel Price",
			Origin:      "manual",
			Selection:   PriceSelection{Mode: "manual", Table: &PriceTable{Currency: "USD"}},
		},
	}
	if err := srcStore.SaveSettings(srcSettings); err != nil {
		t.Fatalf("SaveSettings on src failed: %v", err)
	}

	// Save channel display on src
	_, _ = srcStore.PatchChannelDisplay(ChannelDisplayPatch{
		Order: &[]int64{srcChan.ID},
		Icon:  &ChannelDisplayIcon{ChannelID: srcChan.ID, Value: "openai"},
	})

	// 2. Populate Target Store with existing data and history
	dstOldChan := Channel{
		Name:    "Target Old Channel",
		Enabled: true,
		Models:  []string{"legacy-model"},
		Endpoints: []Endpoint{
			{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.legacy.com/v1"},
		},
		Credentials: []CredentialItem{
			{ID: "dst-old-cred", APIKey: "sk-dst-old-key", Enabled: true},
		},
	}
	if err := dstStore.SaveChannel(&dstOldChan); err != nil {
		t.Fatalf("SaveChannel on dst failed: %v", err)
	}

	// Target has existing admin key
	dstSettings, err := dstStore.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings on dst failed: %v", err)
	}
	targetAdminKey := dstSettings.GatewayKey
	if targetAdminKey == "" {
		t.Fatal("target store has empty GatewayKey")
	}

	// Target has existing usage_logs referencing channel_id 1 and 2
	_, err = dstStore.db.Exec("INSERT INTO usage_logs(time, channel_id, model, status, data) VALUES(?, ?, 'legacy-model', 200, '{}')",
		time.Now().UnixMilli(), dstOldChan.ID)
	if err != nil {
		t.Fatalf("Insert usage_logs failed: %v", err)
	}
	// Also insert usage with a higher channel_id (e.g. 10) to test ID collision avoidance
	_, err = dstStore.db.Exec("INSERT INTO usage_logs(time, channel_id, model, status, data) VALUES(?, 10, 'ancient-model', 200, '{}')",
		time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("Insert usage_logs high channel_id failed: %v", err)
	}

	// Target has historical plan charges
	_, err = dstStore.db.Exec("INSERT INTO plan_charges(channel_id, month, channel_name, amount, currency, paid_at) VALUES(?, '2026-01', 'Target Old Channel', 15.0, 'USD', ?)",
		dstOldChan.ID, time.Now().Unix())
	if err != nil {
		t.Fatalf("Insert plan_charges failed: %v", err)
	}

	// 3. Export from Source Store
	exported, err := srcStore.ExportFullConfig()
	if err != nil {
		t.Fatalf("ExportFullConfig failed: %v", err)
	}

	// Assertions on exported config
	if exported.Settings.GatewayKey != "" {
		t.Fatal("exported config must NOT include GatewayKey")
	}
	if len(exported.Channels) != 2 {
		t.Fatalf("expected 2 exported channels, got %d", len(exported.Channels))
	}
	if exported.Channels[0].Credentials[0].APIKey != "sk-src-secret-12345678" {
		t.Fatalf("expected plaintext API key in export, got %s", exported.Channels[0].Credentials[0].APIKey)
	}
	if len(exported.ClientKeys) != 1 || exported.ClientKeys[0].Token != clientToken {
		t.Fatalf("expected decrypted client key token in export, got %+v", exported.ClientKeys)
	}
	if len(exported.PlanPrices) != 1 {
		t.Fatalf("expected 1 plan price in export, got %d", len(exported.PlanPrices))
	}

	summary := exported.Summary()
	if summary.ChannelsCount != 2 || summary.DownstreamModelsCount != 1 || summary.ClientKeysCount != 1 {
		t.Fatalf("summary counts mismatch: %+v", summary)
	}

	// 4. Restore into Target Store
	beforeRestoreTimeMilli := time.Now().UnixMilli()
	err = dstStore.RestoreFullConfig(exported)
	if err != nil {
		t.Fatalf("RestoreFullConfig on dst failed: %v", err)
	}

	// 5. Verify Target Store State After Restore

	// Target Admin Key MUST be preserved
	dstSettingsAfter, err := dstStore.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings on dst after restore failed: %v", err)
	}
	if dstSettingsAfter.GatewayKey != targetAdminKey {
		t.Fatalf("target GatewayKey was overwritten! before=%s, after=%s", targetAdminKey, dstSettingsAfter.GatewayKey)
	}

	// Restored channels must have new IDs greater than max historical channel_id (10)
	dstChannels, err := dstStore.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels on dst failed: %v", err)
	}
	if len(dstChannels) != 2 {
		t.Fatalf("expected 2 restored channels, got %d", len(dstChannels))
	}
	restoredChan := dstChannels[0]
	if restoredChan.ID <= 10 {
		t.Fatalf("expected restored channel ID > 10 to avoid collision with history, got %d", restoredChan.ID)
	}
	if restoredChan.Name != "Source OpenAI" {
		t.Errorf("expected channel name 'Source OpenAI', got '%s'", restoredChan.Name)
	}
	if len(restoredChan.Credentials) != 1 || restoredChan.Credentials[0].APIKey != "sk-src-secret-12345678" {
		t.Errorf("channel credentials did not decrypt properly on dst: %+v", restoredChan.Credentials)
	}

	// Restored Settings Prices must have ChannelID remapped to restoredChan.ID
	if len(dstSettingsAfter.Prices) != 1 {
		t.Fatalf("expected 1 price in Settings, got %d", len(dstSettingsAfter.Prices))
	}
	if dstSettingsAfter.Prices[0].ChannelID != restoredChan.ID {
		t.Fatalf("expected Settings.Prices[0].ChannelID remapped to %d, got %d", restoredChan.ID, dstSettingsAfter.Prices[0].ChannelID)
	}

	// Restored downstream models must have binding ChannelID remapped to restoredChan.ID
	dstModels, err := dstStore.ListDownstreamModels()
	if err != nil {
		t.Fatalf("ListDownstreamModels on dst failed: %v", err)
	}
	if len(dstModels) != 1 {
		t.Fatalf("expected 1 restored model, got %d", len(dstModels))
	}
	restoredModel := dstModels[0]
	if len(restoredModel.Bindings) != 1 || restoredModel.Bindings[0].ChannelID != restoredChan.ID {
		t.Fatalf("expected binding ChannelID to be remapped to %d, got %+v", restoredChan.ID, restoredModel.Bindings)
	}

	// Restored Client Key can authenticate with original clientToken
	authKey, err := dstStore.AuthenticateClientKey(clientToken)
	if err != nil {
		t.Fatalf("AuthenticateClientKey with restored token failed: %v", err)
	}
	if authKey.Name != "Test Client" {
		t.Errorf("expected client key name 'Test Client', got '%s'", authKey.Name)
	}
	if authKey.ID <= 0 {
		t.Errorf("expected positive client key ID, got %d", authKey.ID)
	}

	// Restored Plan Price has remapped ChannelID and active_from >= beforeRestoreTimeMilli
	var restoredPlanChanID int64
	var activeFrom int64
	err = dstStore.db.QueryRow("SELECT channel_id, active_from FROM plan_prices WHERE channel_id=?", restoredChan.ID).Scan(&restoredPlanChanID, &activeFrom)
	if err != nil {
		t.Fatalf("restored plan price not found for channel %d: %v", restoredChan.ID, err)
	}
	if activeFrom < beforeRestoreTimeMilli {
		t.Errorf("expected active_from >= %d (milliseconds), got %d", beforeRestoreTimeMilli, activeFrom)
	}

	// Tag channel as billing:plan and accrue plan charges: MUST NOT accrue historical charges prior to restore time
	restoredChan.Tags = []string{"billing:plan"}
	if err := dstStore.SaveChannel(&restoredChan); err != nil {
		t.Fatalf("SaveChannel billing:plan failed: %v", err)
	}
	if err := dstStore.AccruePlanCharges(time.Now()); err != nil {
		t.Fatalf("AccruePlanCharges failed: %v", err)
	}
	var postAccrueCharges int
	_ = dstStore.db.QueryRow("SELECT count(*) FROM plan_charges WHERE channel_id=?", restoredChan.ID).Scan(&postAccrueCharges)
	if postAccrueCharges > 0 {
		t.Fatalf("expected 0 historical plan charges accrued for restored channel, got %d", postAccrueCharges)
	}

	// Restored Channel Display has remapped IDs
	var rawDisplay string
	err = dstStore.db.QueryRow("SELECT value FROM meta WHERE key='channel_display'").Scan(&rawDisplay)
	if err != nil {
		t.Fatalf("channel_display not found on dst: %v", err)
	}
	chanIDStr := strconv.FormatInt(restoredChan.ID, 10)
	if !containsSubstring(rawDisplay, chanIDStr) {
		t.Fatalf("channel_display does not contain remapped channel id %s: %s", chanIDStr, rawDisplay)
	}

	// Restored Credential Operations is on restored codex channel's credential
	var restoredCodexChan Channel
	for _, ch := range dstChannels {
		if ch.CredentialType == CredentialCodexOAuth {
			restoredCodexChan = ch
		}
	}
	if len(restoredCodexChan.Credentials) == 0 || restoredCodexChan.Credentials[0].Operations == nil {
		t.Fatalf("CredentialOperations not found on restored codex channel: %+v", restoredCodexChan.Credentials)
	}
	opConfig := *restoredCodexChan.Credentials[0].Operations
	if !opConfig.Warmup.Enabled || opConfig.Warmup.Model != "gpt-4o" {
		t.Errorf("credential operations not restored properly: %+v", opConfig)
	}

	// Verify pre-restore snapshot was generated in dstStore.dir
	snapshotFile := dstStore.LastPreRestoreSnapshot()
	if snapshotFile == "" {
		t.Fatal("expected LastPreRestoreSnapshot to return snapshot filename")
	}
	if _, err := os.Stat(filepath.Join(tmpDst, snapshotFile)); err != nil {
		t.Fatalf("snapshot file %s not found on disk: %v", snapshotFile, err)
	}

	// Target historical usage_logs are UNTOUCHED
	var usageCount int
	err = dstStore.db.QueryRow("SELECT count(*) FROM usage_logs").Scan(&usageCount)
	if err != nil || usageCount != 2 {
		t.Fatalf("expected 2 historical usage_logs intact, got %d (err: %v)", usageCount, err)
	}

	// Target historical plan_charges are UNTOUCHED
	var chargeCount int
	err = dstStore.db.QueryRow("SELECT count(*) FROM plan_charges").Scan(&chargeCount)
	if err != nil || chargeCount != 1 {
		t.Fatalf("expected 1 historical plan_charge intact, got %d (err: %v)", chargeCount, err)
	}
}

func TestFullConfigRestoreRollbackOnError(t *testing.T) {
	tmpDst := t.TempDir()
	dstStore := newTestStore(t, tmpDst)

	initialChan := Channel{
		Name:    "Initial Channel",
		Enabled: true,
		Models:  []string{"initial-model"},
		Endpoints: []Endpoint{
			{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.initial.com/v1"},
		},
		Credentials: []CredentialItem{
			{ID: "initial-cred", APIKey: "sk-initial-key", Enabled: true},
		},
	}
	if err := dstStore.SaveChannel(&initialChan); err != nil {
		t.Fatalf("SaveChannel failed: %v", err)
	}

	// Prepare invalid config that fails during restore (e.g. invalid version)
	err := dstStore.RestoreFullConfig(&FullConfig{Version: 999})
	if err == nil {
		t.Fatal("expected error with unsupported version, got nil")
	}

	// Verify initial channel is still present
	channels, err := dstStore.ListChannels()
	if err != nil || len(channels) != 1 || channels[0].Name != "Initial Channel" {
		t.Fatalf("target was corrupted after failed restore: %+v", channels)
	}

	// Inject trigger failure on insert to test transaction rollback
	if _, err := dstStore.db.Exec("CREATE TRIGGER abort_restore BEFORE INSERT ON channels BEGIN SELECT RAISE(ABORT, 'injected'); END;"); err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	validCfg := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		DownstreamModels: []DownstreamModel{},
		Settings: Settings{
			RetentionDays: 30,
			MaxAttempts:   3,
		},
		Channels: []Channel{
			{
				ID:      100,
				Name:    "New Channel",
				Enabled: true,
				Models:  []string{"new-model"},
				Endpoints: []Endpoint{
					{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.new.com/v1"},
				},
				Credentials: []CredentialItem{
					{ID: "new-cred", APIKey: "sk-new-key", Enabled: true},
				},
			},
		},
	}
	err = dstStore.RestoreFullConfig(validCfg)
	if err == nil {
		t.Fatal("expected restore to fail due to trigger, got nil")
	}

	// Drop trigger and verify original channel is STILL present
	if _, err := dstStore.db.Exec("DROP TRIGGER abort_restore"); err != nil {
		t.Fatalf("drop trigger failed: %v", err)
	}
	channels, err = dstStore.ListChannels()
	if err != nil || len(channels) != 1 || channels[0].Name != "Initial Channel" {
		t.Fatalf("target was not rolled back after failed restore: %+v", channels)
	}
}

func TestSourceChannelIDRemapAndValidation(t *testing.T) {
	// 1. Invalid SourceChannelID fails Validate
	invalidCfg := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		DownstreamModels: []DownstreamModel{},
		Settings: Settings{
			RetentionDays: 30,
			MaxAttempts:   3,
		},
		Channels: []Channel{
			{
				ID:             1,
				Name:           "Channel 1",
				CredentialType: CredentialAPIKey,
				Endpoints:      []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"}},
				Credentials: []CredentialItem{
					{ID: "c1", SourceChannelID: 999, Enabled: true}, // 999 does not exist
				},
			},
		},
	}
	if err := invalidCfg.Validate(); err == nil {
		t.Fatal("expected Validate to fail on non-existent SourceChannelID, got nil")
	}

	// 2. Valid SourceChannelID remapping cross-restore
	tmpDst := t.TempDir()
	dstStore := newTestStore(t, tmpDst)
	existingChan := Channel{
		Name:      "Existing Target",
		APIKey:    "sk-target-key",
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.target.com/v1"}},
	}
	if err := dstStore.SaveChannel(&existingChan); err != nil {
		t.Fatal(err)
	}

	validCfg := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		DownstreamModels: []DownstreamModel{},
		Settings: Settings{
			RetentionDays: 30,
			MaxAttempts:   3,
		},
		Channels: []Channel{
			{
				ID:             10,
				Name:           "Borrowing Channel",
				CredentialType: CredentialAPIKey,
				Endpoints:      []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"}},
				Credentials: []CredentialItem{
					{ID: "borrowed-c", SourceChannelID: 20, Enabled: true},
				},
			},
			{
				ID:             20,
				Name:           "Source Channel",
				CredentialType: CredentialAPIKey,
				Endpoints:      []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"}},
				Credentials: []CredentialItem{
					{ID: "actual-c", APIKey: "sk-real", Enabled: true},
				},
			},
		},
	}
	if err := validCfg.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if err := dstStore.RestoreFullConfig(validCfg); err != nil {
		t.Fatalf("RestoreFullConfig failed: %v", err)
	}

	channels, err := dstStore.ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected 2 restored channels, got %d", len(channels))
	}
	var borrowing, source *Channel
	for i := range channels {
		if channels[i].Name == "Borrowing Channel" {
			borrowing = &channels[i]
		} else if channels[i].Name == "Source Channel" {
			source = &channels[i]
		}
	}
	if borrowing == nil || source == nil {
		t.Fatalf("could not find both restored channels: %+v", channels)
	}
	if len(borrowing.Credentials) != 1 {
		t.Fatalf("expected 1 credential in borrowing channel, got %d", len(borrowing.Credentials))
	}
	if borrowing.Credentials[0].SourceChannelID != source.ID {
		t.Fatalf("SourceChannelID not remapped to new source channel ID: got %d, want %d",
			borrowing.Credentials[0].SourceChannelID, source.ID)
	}
}

func TestTargetPricesNotLeakedOnRestore(t *testing.T) {
	tmpDst := t.TempDir()
	dstStore := newTestStore(t, tmpDst)

	// Create channel and settings with custom prices in target store
	targetChan := Channel{
		Name:      "Target Channel",
		APIKey:    "sk-target-key",
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.target.com/v1"}},
	}
	if err := dstStore.SaveChannel(&targetChan); err != nil {
		t.Fatal(err)
	}
	if err := dstStore.SaveSettings(Settings{
		GatewayKey:    "initial-gateway-key-16ch",
		RetentionDays: 30,
		MaxAttempts:   3,
		Prices: []Price{
			{ChannelID: targetChan.ID, Model: "target-model"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Restore backup that has NO custom prices (Prices is nil/empty)
	backup := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		DownstreamModels: []DownstreamModel{},
		Channels: []Channel{
			{
				ID:             1,
				Name:           "Restored Channel",
				CredentialType: CredentialAPIKey,
				Endpoints:      []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.restored.com/v1"}},
				Credentials: []CredentialItem{
					{ID: "c1", APIKey: "sk-restored", Enabled: true},
				},
			},
		},
		Settings: Settings{
			RetentionDays: 60,
			MaxAttempts:   3,
			Prices:        nil,
		},
	}
	if err := dstStore.RestoreFullConfig(backup); err != nil {
		t.Fatalf("RestoreFullConfig failed: %v", err)
	}

	st, err := dstStore.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Prices) != 0 {
		t.Fatalf("expected 0 custom prices after restore, but target prices leaked: %+v", st.Prices)
	}
	if st.RetentionDays != 30 {
		t.Fatalf("expected target RetentionDays=30 preserved (local log retention policy), got %d", st.RetentionDays)
	}
}

func TestRestorePreservesTargetRetentionAndHistoryOnReopen(t *testing.T) {
	tmpDst := t.TempDir()
	dstStore := newTestStore(t, tmpDst)

	// Target has retention of 3650 days and usage log from 45 days ago
	if err := dstStore.SaveSettings(Settings{
		GatewayKey:    "target-admin-key-16ch",
		RetentionDays: 3650,
		MaxAttempts:   3,
	}); err != nil {
		t.Fatal(err)
	}
	pastUsage := Usage{
		Time:        time.Now().AddDate(0, 0, -45),
		ChannelID:   1,
		ChannelName: "Target channel",
		Model:       "m",
		Status:      200,
	}
	if err := dstStore.AddUsage(pastUsage); err != nil {
		t.Fatal(err)
	}

	beforeLogs, err := dstStore.ListUsage(UsageFilter{})
	if err != nil || len(beforeLogs) != 1 {
		t.Fatalf("expected 1 initial usage log, got %d, err: %v", len(beforeLogs), err)
	}

	// Backup with shorter RetentionDays (30 days)
	backup := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		DownstreamModels: []DownstreamModel{},
		Channels: []Channel{
			{
				ID:             1,
				Name:           "Backup Channel",
				CredentialType: CredentialAPIKey,
				Endpoints:      []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.test.com/v1"}},
				Credentials: []CredentialItem{
					{ID: "c1", APIKey: "sk-test", Enabled: true},
				},
			},
		},
		Settings: Settings{
			RetentionDays: 30,
			MaxAttempts:   3,
		},
	}

	if err := dstStore.RestoreFullConfig(backup); err != nil {
		t.Fatalf("RestoreFullConfig failed: %v", err)
	}

	// Reopen store to trigger store.Open() cleanup routines
	if err := dstStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := Open(tmpDst)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer reopenedStore.Close()

	afterLogs, err := reopenedStore.ListUsage(UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterLogs) != 1 {
		t.Fatalf("R1 regression: target history was wiped on store reopen! expected 1 log, got %d", len(afterLogs))
	}
}

func TestPreRestoreSnapshotEncryptionAndRollback(t *testing.T) {
	tmpDir := t.TempDir()
	st := newTestStore(t, tmpDir)

	ch := Channel{
		Name:    "Snapshot Target Channel",
		Enabled: true,
		Models:  []string{"snapshot-model"},
		Endpoints: []Endpoint{
			{Format: FormatOpenAIChatCompletions, BaseURL: "https://api.openai.com/v1"},
		},
		Credentials: []CredentialItem{
			{ID: "snap-cred", APIKey: "mock-secret-snapshot-api-key", Enabled: true},
		},
	}
	if err := st.SaveChannel(&ch); err != nil {
		t.Fatalf("SaveChannel failed: %v", err)
	}
	_, clientToken, err := st.CreateClientKey("Snapshot Client")
	if err != nil {
		t.Fatalf("CreateClientKey failed: %v", err)
	}

	snapName, err := st.SavePreRestoreSnapshot()
	if err != nil {
		t.Fatalf("SavePreRestoreSnapshot failed: %v", err)
	}
	if snapName == "" {
		t.Fatal("expected non-empty snapshot filename")
	}

	// 1. Verify file on disk does NOT contain secrets in plaintext
	raw, err := os.ReadFile(filepath.Join(tmpDir, snapName))
	if err != nil {
		t.Fatalf("read snapshot file failed: %v", err)
	}
	if containsSubstring(string(raw), "mock-secret-snapshot-api-key") {
		t.Fatal("pre-restore snapshot leaked plaintext API key!")
	}
	if containsSubstring(string(raw), clientToken) {
		t.Fatal("pre-restore snapshot leaked plaintext client key token!")
	}

	// 2. ReadPreRestoreSnapshot decrypts successfully
	readCfg, err := st.ReadPreRestoreSnapshot(snapName)
	if err != nil {
		t.Fatalf("ReadPreRestoreSnapshot failed: %v", err)
	}
	if err := readCfg.Validate(); err != nil {
		t.Fatalf("ReadPreRestoreSnapshot failed Validate: %v", err)
	}
	if len(readCfg.Channels) != 1 || readCfg.Channels[0].Credentials[0].APIKey != "mock-secret-snapshot-api-key" {
		t.Fatalf("ReadPreRestoreSnapshot did not unseal API key properly: %+v", readCfg.Channels)
	}
	if len(readCfg.ClientKeys) != 1 || readCfg.ClientKeys[0].Token != clientToken {
		t.Fatalf("ReadPreRestoreSnapshot did not unseal client token properly: %+v", readCfg.ClientKeys)
	}

	// 3. Mutate store (delete channel)
	channels, err := st.ListChannels()
	if err != nil || len(channels) != 1 {
		t.Fatalf("unexpected channels: %+v", channels)
	}
	if err := st.DeleteChannel(channels[0].ID); err != nil {
		t.Fatalf("DeleteChannel failed: %v", err)
	}
	rem, _ := st.ListChannels()
	if len(rem) != 0 {
		t.Fatalf("expected 0 channels after deletion, got %d", len(rem))
	}

	// 4. Rollback via RestoreSnapshot
	if err := st.RestoreSnapshot(snapName); err != nil {
		t.Fatalf("RestoreSnapshot failed: %v", err)
	}
	restored, err := st.ListChannels()
	if err != nil || len(restored) != 1 {
		t.Fatalf("expected 1 restored channel after rollback, got %d (err: %v)", len(restored), err)
	}
	if restored[0].Name != "Snapshot Target Channel" {
		t.Errorf("expected channel name 'Snapshot Target Channel', got %q", restored[0].Name)
	}
	if restored[0].Credentials[0].APIKey != "mock-secret-snapshot-api-key" {
		t.Errorf("restored channel credential mismatch: %+v", restored[0].Credentials)
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && len(sub) > 0 && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestScopeCanonicalizationRestoreAndRollback(t *testing.T) {
	st := newTestStore(t, t.TempDir())
	ch := Channel{
		Name:      "Initial Channel",
		APIKey:    "initial-key",
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://initial.example/v1"}},
	}
	if err := st.SaveChannel(&ch); err != nil {
		t.Fatal(err)
	}

	snapName, err := st.SavePreRestoreSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Incoming config has un-canonicalized scope
	incoming := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		DownstreamModels: []DownstreamModel{},
		Settings: Settings{
			RetentionDays: 30,
			MaxAttempts:   3,
		},
		Channels: []Channel{
			{
				ID:        1,
				Name:      "Restored Channel",
				APIKey:    "restored-key",
				Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://restored.example/v1"}},
			},
		},
		ClientKeys: []ClientKeyRecord{
			{
				Name:      "test-key",
				Token:     "test-token-1234567890",
				CreatedAt: time.Now().UTC(),
				Scope:     ClientKeyScope{ModelMode: "all"}, // missing model_ids and tag_rules
			},
		},
	}

	if err := incoming.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if err := st.RestoreFullConfig(incoming); err != nil {
		t.Fatalf("RestoreFullConfig failed: %v", err)
	}

	keys, err := st.ListClientKeys()
	if err != nil {
		t.Fatalf("ListClientKeys failed after restore: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].Scope.ModelMode != "all" || keys[0].Scope.ModelIDs == nil || keys[0].Scope.TagRules == nil {
		t.Fatalf("scope was not canonicalized: %+v", keys[0].Scope)
	}

	// Rollback to initial snapshot succeeds
	if err := st.RestoreSnapshot(snapName); err != nil {
		t.Fatalf("RestoreSnapshot failed: %v", err)
	}
	chans, err := st.ListChannels()
	if err != nil || len(chans) != 1 || chans[0].Name != "Initial Channel" {
		t.Fatalf("rollback failed to restore initial state: %+v (err: %v)", chans, err)
	}
}

func TestMissingChannelsCollectionRejected(t *testing.T) {
	sparse := &FullConfig{
		Version:    1,
		ExportedAt: time.Now().UTC(),
	}
	if err := sparse.Validate(); err == nil || !containsSubstring(err.Error(), "missing channels collection") {
		t.Fatalf("expected missing channels collection error, got %v", err)
	}
}

func TestMissingDownstreamModelsCollectionRejected(t *testing.T) {
	sparse := &FullConfig{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Channels:   []Channel{},
	}
	if err := sparse.Validate(); err == nil || !containsSubstring(err.Error(), "missing downstream_models collection") {
		t.Fatalf("expected missing downstream_models collection error, got %v", err)
	}
}

func TestMissingSettingsRejected(t *testing.T) {
	sparse := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		Channels:         []Channel{},
		DownstreamModels: []DownstreamModel{},
	}
	if err := sparse.Validate(); err == nil || !containsSubstring(err.Error(), "missing settings object") {
		t.Fatalf("expected missing settings object error, got %v", err)
	}
}

func TestInvalidSettingsMaxAttemptsRejected(t *testing.T) {
	sparse := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		Channels:         []Channel{},
		DownstreamModels: []DownstreamModel{},
		Settings: Settings{
			RetentionDays: 30,
			MaxAttempts:   0,
		},
	}
	if err := sparse.Validate(); err == nil || !containsSubstring(err.Error(), "max_attempts") {
		t.Fatalf("expected max_attempts validation error, got %v", err)
	}
}

func TestLegalEmptyConfigPasses(t *testing.T) {
	empty := &FullConfig{
		Version:          1,
		ExportedAt:       time.Now().UTC(),
		Channels:         []Channel{},
		DownstreamModels: []DownstreamModel{},
		Settings: Settings{
			RetentionDays: 30,
			MaxAttempts:   3,
		},
	}
	if err := empty.Validate(); err != nil {
		t.Fatalf("expected legal empty config to validate, got %v", err)
	}
}

func TestRollbackDiscoversSnapshotAfterStoreReopen(t *testing.T) {
	dir := t.TempDir()
	st1 := newTestStore(t, dir)
	ch := Channel{
		Name:      "Before Snapshot",
		APIKey:    "k",
		Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.com/v1"}},
	}
	if err := st1.SaveChannel(&ch); err != nil {
		t.Fatal(err)
	}
	snap, err := st1.SavePreRestoreSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen store in new instance
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	discovered := st2.LastPreRestoreSnapshot()
	if discovered != snap {
		t.Fatalf("expected discovered snapshot %q, got %q", snap, discovered)
	}

	// Rollback using discovered snapshot succeeds
	if err := st2.RestoreSnapshot(discovered); err != nil {
		t.Fatalf("RestoreSnapshot failed: %v", err)
	}
}
