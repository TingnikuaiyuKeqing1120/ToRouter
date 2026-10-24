package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"
	"torouter/internal/store"
)

// AdminRoutes must be mounted under the existing authenticated console router.
// These routes intentionally do not accept MCP tokens or expose signing tools.
func (s *Server) AdminRoutes(r chi.Router) {
	r.Get("/mcp/tools", func(w http.ResponseWriter, r *http.Request) {
		// Read the same registry as tools/list. The console catalogue contains
		// descriptions only: browsing must never invoke a tool or read a secret.
		type toolSummary struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Capability  string `json:"capability"`
			Permission  string `json:"permission"`
			ReadOnly    bool   `json:"read_only"`
		}
		tools := make([]toolSummary, 0, len(s.operations))
		for _, op := range s.operations {
			tools = append(tools, toolSummary{op.Name, op.Description, op.Capability, op.Permission, op.ReadOnly})
		}
		adminJSON(w, http.StatusOK, tools)
	})
	r.Get("/mcp/status", func(w http.ResponseWriter, r *http.Request) {
		connections, err := s.store.ListManagementConnections()
		if err != nil {
			adminFailure(w, err)
			return
		}
		enabled := false
		caps := map[string]bool{}
		for _, c := range connections {
			enabled = enabled || c.Enabled
		}
		for _, op := range s.operations {
			caps[op.Capability] = true
		}
		status := map[string]any{"origin": s.origin, "endpoint": s.origin + "/mcp", "enabled": enabled, "connections": connections, "connection_count": len(connections), "tool_count": len(s.operations), "capability_count": len(caps), "permissions": []string{Read, Manage, Maintain, Sensitive}}
		if executable, err := os.Executable(); err == nil {
			status["cli_path"] = executable
		}
		adminJSON(w, 200, status)
	})
	r.Get("/mcp/connections", func(w http.ResponseWriter, r *http.Request) {
		connections, err := s.store.ListManagementConnections()
		if err != nil {
			adminFailure(w, err)
			return
		}
		adminJSON(w, 200, connections)
	})
	r.Post("/mcp/connections", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		}
		if !adminDecode(w, r, &body) {
			return
		}
		if _, err := store.ValidateManagementPermissions(body.Permissions); err != nil {
			adminJSON(w, 400, map[string]string{"error": "Invalid permissions"})
			return
		}
		c, token, err := s.store.CreateManagementConnection(body.Name, body.Permissions)
		if err != nil {
			adminJSON(w, 400, map[string]string{"error": "Cannot create authorization; use a name of 1..100 bytes and valid permissions"})
			return
		}
		adminJSON(w, 201, map[string]any{"connection": c, "token": token})
	})
	r.Patch("/mcp/connections/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := adminID(w, r)
		if !ok {
			return
		}
		var body store.ManagementPatch
		if !adminDecode(w, r, &body) {
			return
		}
		if body.Permissions != nil {
			if _, err := store.ValidateManagementPermissions(*body.Permissions); err != nil {
				adminJSON(w, 400, map[string]string{"error": "Invalid permissions"})
				return
			}
		}
		if body.Name != nil && (*body.Name == "" || len(*body.Name) > 100) {
			adminJSON(w, 400, map[string]string{"error": "Invalid name"})
			return
		}
		c, err := s.store.UpdateManagementConnection(id, body)
		if err != nil {
			adminFailure(w, err)
			return
		}
		adminJSON(w, 200, c)
	})
	r.Delete("/mcp/connections/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := adminID(w, r)
		if !ok {
			return
		}
		if err := s.store.DeleteManagementConnection(id); err != nil {
			adminFailure(w, err)
			return
		}
		w.WriteHeader(204)
	})
	r.Post("/mcp/connections/{id}/rotate", func(w http.ResponseWriter, r *http.Request) {
		id, ok := adminID(w, r)
		if !ok {
			return
		}
		c, token, err := s.store.RotateManagementConnection(id)
		if err != nil {
			adminFailure(w, err)
			return
		}
		adminJSON(w, 200, map[string]any{"connection": c, "token": token})
	})
}

func adminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func adminFailure(w http.ResponseWriter, err error) {
	status := 500
	message := "Management operation failed"
	if errors.Is(err, store.ErrNotFound) {
		status = 404
		message = "Management authorization not found"
	}
	adminJSON(w, status, map[string]string{"error": message})
}
func adminID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		adminJSON(w, 400, map[string]string{"error": "Invalid authorization ID"})
		return 0, false
	}
	return id, true
}
func adminDecode(w http.ResponseWriter, r *http.Request, value any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		adminJSON(w, 400, map[string]string{"error": "Invalid request body"})
		return false
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		adminJSON(w, 400, map[string]string{"error": "Expected one JSON object"})
		return false
	}
	return true
}
