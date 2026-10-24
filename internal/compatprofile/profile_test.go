package compatprofile

import (
	"encoding/json"
	"testing"
)

func stateFixture(p Profile) map[string]any {
	return map[string]any{"type": "oauth", "uid": "synthetic-account", "realUserID": "synthetic-account",
		"access": "synthetic-access", "refresh": "synthetic-refresh", "expires": 1900000000000,
		"sessionID": "12345678-1234-4123-8123-123456789012", "unknown": map[string]any{"keep": "opaque"}}
}

func TestStateIdentityAndFullOpaqueRetention(t *testing.T) {
	for _, p := range Profiles() {
		t.Run(p.ID, func(t *testing.T) {
			value := stateFixture(p)
			raw, _ := json.Marshal(value)
			canonical, owner, err := p.State(string(raw))
			if err != nil || owner == "" {
				t.Fatalf("valid state: %v", err)
			}
			var retained map[string]any
			if json.Unmarshal([]byte(canonical), &retained) != nil || retained["unknown"].(map[string]any)["keep"] != "opaque" {
				t.Fatal("opaque state was dropped")
			}
			value["access"], value["refresh"], value["expires"] = "rotated-access", "rotated-refresh", 1900000005000
			raw, _ = json.Marshal(value)
			_, rotatedOwner, err := p.State(string(raw))
			if err != nil || rotatedOwner != owner {
				t.Fatal("rotation changed owner")
			}
			if p.Package == "workbuddy" {
				value["enterpriseId"] = "synthetic-team"
				raw, _ = json.Marshal(value)
				_, enterpriseOwner, err := p.State(string(raw))
				if err != nil || enterpriseOwner == owner {
					t.Fatal("enterprise account collapsed into personal identity")
				}
			}
			for _, invalid := range []map[string]any{
				{"refresh": ""}, {"expires": 0}, {"vpc": true}, {"deviceChat": true, "uid": ""},
			} {
				bad := stateFixture(p)
				for k, v := range invalid {
					bad[k] = v
				}
				raw, _ = json.Marshal(bad)
				if _, _, err := p.State(string(raw)); err == nil {
					t.Fatalf("invalid state accepted: %v", invalid)
				}
			}
		})
	}
}

func TestRegionalAuthorizationAndExternalStateBranches(t *testing.T) {
	for _, p := range Profiles() {
		path := "/login"
		if p.Package == "qoder" {
			path = "/device/selectAccounts"
		}
		if !p.AuthorizationURL(p.Web + path + "?code=synthetic") {
			t.Fatalf("%s rejected its site", p.ID)
		}
		for _, bad := range []string{"https://example.invalid/login", p.Web + ".example.invalid/login", p.Web + "/login#secret", "http://" + p.Web[8:] + path} {
			if p.AuthorizationURL(bad) {
				t.Fatalf("%s accepted unrelated browser URL", p.ID)
			}
		}
		if p.Package == "qoder" {
			continue
		}
		for _, field := range []string{"sessionID", "uid"} {
			value := stateFixture(p)
			delete(value, field)
			raw, _ := json.Marshal(value)
			if _, _, err := p.State(string(raw)); err == nil {
				t.Fatalf("%s accepted missing %s", p.ID, field)
			}
		}
		if p.Package == "workbuddy" {
			for _, invalid := range []map[string]any{{"source": "desktop"}, {"domain": "https://private.invalid/path"}, {"enterpriseId": 42}} {
				value := stateFixture(p)
				for k, v := range invalid {
					value[k] = v
				}
				raw, _ := json.Marshal(value)
				if _, _, err := p.State(string(raw)); err == nil {
					t.Fatal("external login branch or malformed account accepted")
				}
			}
		}
	}
}
