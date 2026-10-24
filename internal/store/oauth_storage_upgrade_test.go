package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOAuthStartupReadsAbsentOptionalSecrets(t *testing.T) {
	for _, kind := range []string{CredentialCopilotOAuth, CredentialChatGPTOAuth, CredentialCodexOAuth, CredentialClaudeOAuth, CredentialAntigravityOAuth, CredentialDevinOAuth} {
		for _, shape := range []string{"pool", "top-level"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				dir := t.TempDir()
				st, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				item := oauthTransferTestItem(t, kind, "storage-fixture")
				// JSON drops process-local monotonic clock data from timestamps.
				expectedJSON, err := json.Marshal(item.OAuth)
				if err != nil {
					t.Fatal(err)
				}
				var expected OAuthCredential
				if err = json.Unmarshal(expectedJSON, &expected); err != nil {
					t.Fatal(err)
				}
				c := Channel{Name: "Synthetic upgrade", CredentialType: kind, Enabled: true,
					Endpoints: item.Endpoints, Credentials: []CredentialItem{{ID: "cred_default", Label: "Synthetic credential", Enabled: true, OAuth: item.OAuth}}}
				if err = st.SaveChannel(&c); err != nil {
					t.Fatal(err)
				}
				var raw string
				if err = st.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var row map[string]any
				if err = json.Unmarshal([]byte(raw), &row); err != nil {
					t.Fatal(err)
				}
				oauth := row["credentials"].([]any)[0].(map[string]any)["oauth"].(map[string]any)
				// Before native plugins none of these existed; the intermediate
				// native release had the three IDs but no plugin_state.
				delete(oauth, "plugin_state")
				if !IsNativeOAuth(kind) {
					delete(oauth, "organization_id")
					oauth["device_id"] = ""
					oauth["project_id"] = nil
				}
				if shape == "top-level" {
					row["oauth"] = oauth
					delete(row, "credentials")
				}
				oldRow, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = st.db.Exec("UPDATE channels SET data=? WHERE id=?", string(oldRow), c.ID); err != nil {
					t.Fatal(err)
				}
				if err = st.Close(); err != nil {
					t.Fatal(err)
				}
				keyBefore, err := os.ReadFile(filepath.Join(dir, "master.key"))
				if err != nil {
					t.Fatal(err)
				}
				st, err = Open(dir)
				if err != nil {
					t.Fatal("existing OAuth data must open without replacing its master key:", err)
				}
				defer st.Close()
				got, err := st.Channel(c.ID)
				if err != nil || len(got.Credentials) != 1 || !reflect.DeepEqual(got.Credentials[0].OAuth, &expected) {
					t.Fatalf("upgrade changed credential material: %v", err)
				}
				var unchanged string
				if err = st.db.QueryRow("SELECT data FROM channels WHERE id=?", c.ID).Scan(&unchanged); err != nil || unchanged != string(oldRow) {
					t.Fatalf("opening must not rewrite the stored channel: %v", err)
				}
				// Ordinary editing must continue to preserve the complete token
				// set, then survive another close/open.
				got.Name = "Edited synthetic name"
				if err = st.SaveChannelConfig(&got); err != nil {
					t.Fatal(err)
				}
				if err = st.Close(); err != nil {
					t.Fatal(err)
				}
				st, err = Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				again, err := st.Channel(c.ID)
				if err != nil || again.Name != got.Name || !reflect.DeepEqual(again.Credentials[0].OAuth, &expected) {
					t.Fatalf("save/restart changed credential material: %v", err)
				}
				keyAfter, err := os.ReadFile(filepath.Join(dir, "master.key"))
				if err != nil || !bytes.Equal(keyBefore, keyAfter) {
					t.Fatalf("master key changed: %v", err)
				}
			})
		}
	}
}

func TestOAuthOptionalSecretsStillAuthenticate(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oauth := OAuthCredential{RefreshToken: "synthetic-refresh", AccessToken: "synthetic-access",
		OrganizationID: "synthetic-org", DeviceID: "synthetic-device", ProjectID: "synthetic-project", PluginState: "synthetic-state"}
	sealed, err := sealOAuthFields(st, oauth)
	if err != nil {
		t.Fatal(err)
	}
	// Absence is permitted only for optional fields. Required encrypted slots
	// and every nonempty ciphertext must still fail closed when damaged.
	for _, name := range []string{"github_token", "refresh_token", "id_token", "access_token", "organization_id", "device_id", "project_id", "plugin_state"} {
		for _, damage := range []string{"truncated", "authentication", "wrong-key"} {
			t.Run(name+"/"+damage, func(t *testing.T) {
				raw, _ := json.Marshal(sealed)
				var fields map[string]any
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				value := fields[name].(string)
				switch damage {
				case "truncated":
					value = "AA=="
				case "authentication":
					ciphertext, err := base64.StdEncoding.DecodeString(value)
					if err != nil {
						t.Fatal(err)
					}
					ciphertext[len(ciphertext)-1] ^= 1
					value = base64.StdEncoding.EncodeToString(ciphertext)
				case "wrong-key":
					other, err := Open(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					defer other.Close()
					value, err = other.seal("synthetic-wrong-key")
					if err != nil {
						t.Fatal(err)
					}
				}
				fields[name] = value
				raw, _ = json.Marshal(fields)
				var damaged OAuthCredential
				if err := json.Unmarshal(raw, &damaged); err != nil {
					t.Fatal(err)
				}
				if _, err := openOAuthFields(st, damaged); err == nil {
					t.Fatal("damaged ciphertext was accepted")
				}
			})
		}
	}
	for _, name := range []string{"github_token", "refresh_token", "id_token", "access_token"} {
		t.Run("required/"+name, func(t *testing.T) {
			raw, _ := json.Marshal(sealed)
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			delete(fields, name)
			raw, _ = json.Marshal(fields)
			var missing OAuthCredential
			_ = json.Unmarshal(raw, &missing)
			if _, err := openOAuthFields(st, missing); err == nil {
				t.Fatal("missing required encrypted slot was accepted")
			}
		})
	}
}
