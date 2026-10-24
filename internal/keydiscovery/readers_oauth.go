package keydiscovery

import (
	"context"
	"encoding/json"

	"torouter/internal/store"
)

const gatewayManagerOAuthSQL = "SELECT t.access_token,t.refresh_token,t.id_token,a.chatgpt_account_id,a.workspace_id,a.status FROM tokens t JOIN accounts a ON a.id=t.account_id"

// Only the language server's declared plaintext token files are read. This does
// not inspect VS Code SecretStorage, system keychains, or the standalone CLI.
func readCopilot(ctx context.Context, r Resources) ([]Candidate, []Diagnostic, error) {
	var out []Candidate
	var notes []Diagnostic
	for _, file := range []string{"apps.json", "hosts.json"} {
		raw, exists, code, err := fileRead(ctx, r, file)
		if err != nil {
			return nil, nil, err
		}
		if code != "" {
			notes = append(notes, diagnostic(code))
			continue
		}
		if !exists {
			continue
		}
		items, ds, err := readNativeMaterial(ctx, store.CredentialCopilotOAuth, raw)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, items...)
		notes = append(notes, ds...)
	}
	return out, notes, nil
}

func readGatewayOAuth(ctx context.Context, r Resources, source string) ([]Candidate, []Diagnostic, error) {
	var out []Candidate
	var ds []Diagnostic
	appendRaw := func(kind string, raw []byte) {
		cs, notes, _ := readNativeMaterial(ctx, kind, raw)
		out = append(out, cs...)
		ds = append(ds, notes...)
	}
	switch source {
	case "source-magpie":
		raw, exists, code, err := fileRead(ctx, r, "logins.json")
		if err != nil {
			return nil, nil, err
		}
		if code != "" {
			return fileProblem(code)
		}
		if !exists {
			return nil, nil, nil
		}
		var entries []struct {
			Agent   string          `json:"agent"`
			Auth    json.RawMessage `json:"auth"`
			Project string          `json:"project"`
		}
		if json.Unmarshal(raw, &entries) != nil {
			return fileProblem("invalid_format")
		}
		for _, entry := range entries {
			kind := ""
			switch entry.Agent {
			case "codex":
				kind = store.CredentialCodexOAuth
			case "claude":
				kind = store.CredentialClaudeOAuth
			case "antigravity":
				kind = store.CredentialAntigravityOAuth
			case "copilot":
				kind = store.CredentialCopilotOAuth
			default:
				continue
			}
			before := len(out)
			appendRaw(kind, entry.Auth)
			for i := before; i < len(out); i++ {
				if out[i].OAuth != nil && entry.Project != "" {
					out[i].OAuth.OAuth.ProjectID = entry.Project
				}
			}
		}
	case "source-codex-manager":
		rows, err := r.Query(ctx, "codexmanager.db", gatewayManagerOAuthSQL)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, nil
		}
		for _, row := range rows {
			if len(row) != 6 {
				ds = append(ds, diagnostic("invalid_format"))
				continue
			}
			account := row[3]
			if account == "" {
				account = row[4]
			}
			payload := map[string]any{"tokens": map[string]any{"access_token": row[0], "refresh_token": row[1], "id_token": row[2], "account_id": account}}
			raw, _ := json.Marshal(payload)
			before := len(out)
			appendRaw(store.CredentialCodexOAuth, raw)
			for i := before; i < len(out); i++ {
				if row[5] == "disabled" || row[5] == "inactive" {
					out[i].OAuth.Enabled = false
					out[i].Disabled = true
				}
			}
		}
	case "source-ccswitch":
		for _, file := range []string{"codex_oauth_auth.json", "copilot_auth.json"} {
			raw, exists, code, err := fileRead(ctx, r, file)
			if err != nil {
				return nil, nil, err
			}
			if code != "" {
				ds = append(ds, diagnostic(code))
				continue
			}
			if !exists {
				continue
			}
			doc, ok := fileJSON(raw)
			if !ok {
				ds = append(ds, diagnostic("invalid_format"))
				continue
			}
			accounts, ok := fileObject(doc["accounts"])
			if !ok {
				ds = append(ds, diagnostic("incomplete"))
				continue
			}
			for _, id := range fileKeys(accounts) {
				entry, ok := fileObject(accounts[id])
				if !ok {
					continue
				}
				kind := store.CredentialCodexOAuth
				if file == "codex_oauth_auth.json" {
					copy := map[string]any{}
					for name, value := range entry {
						copy[name] = value
					}
					copy["account_id"] = entry["chatgpt_account_id"]
					entry = copy
				}
				if file == "copilot_auth.json" {
					kind = store.CredentialCopilotOAuth
					entry = map[string]any{"oauth_token": entry["github_token"]}
				}
				bytes, _ := json.Marshal(entry)
				appendRaw(kind, bytes)
			}
		}
	}
	return out, ds, nil
}
