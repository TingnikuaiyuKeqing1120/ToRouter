package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"torouter/internal/store"
)

type auditTarget struct {
	operation string
	targets   map[string]any
	fields    []string
}

// Only fixed identifier/field selectors enter this record. Never persist raw
// arguments, free text, credential inputs, callback URLs, or returned values.
func safeAuditTarget(operation string, input json.RawMessage) *auditTarget {
	a := &auditTarget{operation: operation, targets: map[string]any{}, fields: []string{}}
	var args map[string]json.RawMessage
	_ = json.Unmarshal(input, &args)
	for _, name := range []string{"id", "channel_id", "client_key_id", "connection_id", "usage_id"} {
		var number int64
		if json.Unmarshal(args[name], &number) == nil && number > 0 {
			a.targets[name] = number
		}
	}
	for _, name := range []string{"credential_id", "source_credential_id"} {
		var id string
		if json.Unmarshal(args[name], &id) == nil && id != "" {
			if safeAuditID(id, "cred_") {
				a.targets[name] = id
			} else {
				digest := sha256.Sum256([]byte(id))
				a.targets[name+"_sha256"] = hex.EncodeToString(digest[:])
			}
		}
	}
	if operation == "read_export" {
		var id string
		if json.Unmarshal(args["id"], &id) == nil && safeAuditID(id, "export_") {
			a.targets["export_id"] = id
		}
	}
	switch operation {
	case "read_credential_key":
		a.fields = []string{"api_key"}
	case "read_credential_identity":
		a.fields = []string{"oauth_identity"}
	case "read_client_key", "read_client_api_key":
		a.fields = []string{"client_key"}
	case "read_usage_log_sensitive":
		a.fields = []string{"private_details"}
	case "read_export":
		a.fields = []string{"export_content"}
	case "export_oauth_credentials", "preview_oauth_import", "get_oauth_import_preview":
		a.fields = []string{"oauth_transfer"}
	default:
		for _, name := range []string{"field", "fields"} {
			var fields []string
			if name == "field" {
				var field string
				if json.Unmarshal(args[name], &field) == nil {
					fields = []string{field}
				}
			} else {
				_ = json.Unmarshal(args[name], &fields)
			}
			for _, field := range fields {
				switch field {
				case "api_key", "client_key", "oauth_identity", "private_details", "export_content", "credential_destination":
					a.fields = append(a.fields, field)
				}
			}
		}
	}
	return a
}

func safeAuditID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) > 100 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// Audit exposes only the calling authorization's checks; it cannot select a
// different connection, unlike the store method intended for console owners.
func (s *Server) Audit(ctx context.Context, p Principal, beforeID int64, limit int) (store.ManagementAuditPage, error) {
	if err := p.Require(ctx, Read); err != nil {
		return store.ManagementAuditPage{}, err
	}
	return s.store.ListManagementAudit(ctx, p.ID, beforeID, limit)
}
