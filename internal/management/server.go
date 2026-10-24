// Package management adapts registered domain operations to the independent
// local MCP management boundary. It never creates a store or scheduler.
package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"torouter/internal/store"
)

const (
	Read      = "read"
	Manage    = "manage"
	Maintain  = "maintain"
	Sensitive = "sensitive"
)

type Operation struct {
	Name        string                                                         `json:"name"`
	Description string                                                         `json:"description"`
	Capability  string                                                         `json:"capability"`
	Permission  string                                                         `json:"permission"`
	InputSchema map[string]any                                                 `json:"input_schema"`
	ReadOnly    bool                                                           `json:"read_only"`
	Destructive bool                                                           `json:"destructive"`
	External    bool                                                           `json:"external"`
	Handler     func(context.Context, Principal, json.RawMessage) (any, error) `json:"-"`
}

// OperationOutcome optionally reports the domain outcome independently of a
// transport/authorization error. Partial completed facts must remain in the
// durable receipt even when the caller can no longer receive that receipt.
type OperationOutcome interface{ OperationStatus() string }

type Principal struct {
	ID          int64    `json:"id"`
	Permissions []string `json:"permissions"`
	store       *store.Store
	token       string
	audit       *auditTarget
}

func (p Principal) String() string {
	return fmt.Sprintf("management principal %d permissions=%v", p.ID, p.Permissions)
}
func (p Principal) GoString() string { return p.String() }

func (p Principal) Has(permission string) bool {
	for _, v := range p.Permissions {
		if v == permission {
			return true
		}
	}
	return false
}

// Require checks the original token again, including rotation, disablement and
// deletion. Domain handlers must call it immediately before committing writes
// or sending external requests when work continues after the entry check.
func (p Principal) Require(ctx context.Context, permission string) (result error) {
	if permission == Sensitive && p.store != nil {
		defer func() {
			a := p.audit
			if a == nil {
				a = &auditTarget{operation: "sensitive_access"}
			}
			outcome := "authorized"
			if result != nil {
				outcome = "denied"
			}
			if err := p.store.RecordManagementAudit(context.WithoutCancel(ctx), p.ID, a.operation, outcome, a.targets, a.fields); err != nil && result == nil {
				result = NewError("audit_unavailable", "Sensitive access is unavailable because its audit record could not be saved.")
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.store == nil {
		return NewError("forbidden", "Management authorization is unavailable.")
	}
	c, err := p.store.AuthenticateManagementConnection(p.token)
	if err != nil || c.ID != p.ID {
		return NewError("forbidden", "Management authorization was revoked; use the console to reconnect.")
	}
	for _, v := range c.Permissions {
		if v == permission {
			return nil
		}
	}
	return NewError("forbidden", "Missing permission: "+permission+". Update authorization in the console.")
}

// Error is safe to return to a client; unknown implementation errors are never
// reflected because upstream/SQL errors may contain private request material.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string            { return e.Code + ": " + e.Message }
func NewError(code, message string) error { return &Error{Code: code, Message: message} }
func publicError(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return NewError("operation_failed", "Operation failed; inspect the console and current state before retrying.")
}

type Server struct {
	store      *store.Store
	origin     string
	operations []Operation
}

func New(st *store.Store, origin string, operations []Operation) *Server {
	s := &Server{store: st, origin: strings.TrimRight(origin, "/")}
	seen := map[string]bool{}
	for _, op := range operations {
		if op.Name == "" || seen[op.Name] || op.Handler == nil {
			panic("management: invalid or duplicate operation")
		}
		if _, err := store.ValidateManagementPermissions([]string{op.Permission}); err != nil {
			panic("management: operation needs explicit permission")
		}
		seen[op.Name] = true
		// Copy the schema: adding request_id must not mutate the caller's registry.
		raw, err := json.Marshal(op.InputSchema)
		if err != nil {
			panic("management: invalid schema")
		}
		if err = json.Unmarshal(raw, &op.InputSchema); err != nil || op.InputSchema == nil {
			panic("management: missing input schema")
		}
		if op.InputSchema["type"] != "object" {
			panic("management: input schema must be object")
		}
		if !op.ReadOnly {
			addRequestID(op.InputSchema)
		}
		s.operations = append(s.operations, op)
	}
	sort.Slice(s.operations, func(i, j int) bool { return s.operations[i].Name < s.operations[j].Name })
	return s
}

func addRequestID(schema map[string]any) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		schema["properties"] = props
	}
	props["request_id"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Unique write ID. Reuse only for identical arguments; query the receipt after disconnect or unknown outcome."}
	required, _ := schema["required"].([]any)
	for _, v := range required {
		if v == "request_id" {
			return
		}
	}
	schema["required"] = append(required, "request_id")
}

// Operations returns the canonical deterministic registry used by tools/help.
func (s *Server) Operations() []Operation { return append([]Operation(nil), s.operations...) }

func (s *Server) sdkServer(p Principal) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "to-router", Version: "1"}, &mcp.ServerOptions{Instructions: "Search before selecting exact IDs. Unknown values are not zero. Read versions before patching. Maintenance uses an explicit preview. After disconnect, conflict or unknown outcome, inspect the receipt and current state before retrying. Authorization roots are managed only in the user console."})
	s.addResources(server, p)
	for _, op := range s.operations {
		if !p.Has(op.Permission) {
			continue
		}
		destructive, external := op.Destructive, op.External
		mcp.AddTool(server, &mcp.Tool{Name: op.Name, Description: op.Description, InputSchema: op.InputSchema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: op.ReadOnly, DestructiveHint: &destructive, OpenWorldHint: &external, IdempotentHint: !op.ReadOnly}}, func(ctx context.Context, _ *mcp.CallToolRequest, input json.RawMessage) (*mcp.CallToolResult, any, error) {
			out, err := s.invoke(ctx, p, op, input)
			if err != nil {
				return nil, nil, publicError(err)
			}
			return nil, out, nil
		})
	}
	return server
}

func (s *Server) Handler() http.Handler {
	transport := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return s.sdkServer(r.Context().Value(principalKey{}).(Principal)) }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r, s.origin) {
			http.Error(w, "Local management endpoint only", http.StatusForbidden)
			return
		}
		if r.URL.RawQuery != "" {
			http.Error(w, "Credentials and parameters belong in protocol headers/body", http.StatusBadRequest)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(r.Header.Values("Authorization")) != 1 {
			ok = false
		}
		if !ok || strings.TrimSpace(token) != token || token == "" {
			http.Error(w, "Management bearer token required", http.StatusUnauthorized)
			return
		}
		c, err := s.store.AuthenticateManagementConnection(token)
		if err != nil {
			http.Error(w, "Invalid management authorization", http.StatusUnauthorized)
			return
		}
		p := Principal{ID: c.ID, Permissions: c.Permissions, store: s.store, token: token}
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024*1024)
		queue := &afterResponseQueue{}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		ctx = context.WithValue(ctx, afterResponseKey{}, queue)
		outcome := &responseOutcome{ResponseWriter: w}
		transport.ServeHTTP(outcome, r.WithContext(ctx))
		if !queue.pending() {
			return
		}
		if outcome.err != nil || outcome.written == 0 || outcome.status < 200 || outcome.status >= 300 || r.Context().Err() != nil {
			return
		}
		if err := http.NewResponseController(outcome).Flush(); err != nil {
			return
		}
		// A shutdown hook may wait for active handlers to drain; execute outside
		// this handler after the response is on the wire, without timer guesses.
		go queue.run(context.WithoutCancel(r.Context()))
	})
}

type principalKey struct{}

func loopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}
func localRequest(r *http.Request, origin string) bool {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !loopbackHost(remote) {
		return false
	}
	expected, err := url.Parse(origin)
	if err != nil || expected.Host == "" {
		return false
	}
	hostURL, err := url.Parse("http://" + r.Host)
	if err != nil || !loopbackHost(hostURL.Hostname()) || !strings.EqualFold(r.Host, expected.Host) {
		return false
	}
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		if len(origins) != 1 || origins[0] != origin {
			return false
		}
	}
	return true
}
