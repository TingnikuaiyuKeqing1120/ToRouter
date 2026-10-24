package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/jsonschema-go/jsonschema"
	"torouter/internal/management"
	"torouter/internal/store"
)

func mcpCoverageFixture(t *testing.T) (chi.Routes, []management.Operation) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Include host-conditional routes without calling any hook or upstream.
	h := New(s, "http://127.0.0.1:49123", Hooks{Shutdown: func() {}, Restart: func() {}}).(consoleHandler)
	t.Cleanup(func() { ReleaseOAuthListeners(h) })
	routes, ok := h.Handler.(chi.Routes)
	if !ok {
		t.Fatal("console handler no longer exposes mounted chi routes")
	}
	return routes, h.management.Operations()
}

func TestMCPCoverageRoutes(t *testing.T) {
	routes, ops := mcpCoverageFixture(t)
	for _, issue := range managementRouteIssues(routes, ops, managementRouteContracts()) {
		t.Error(issue)
	}
}

func TestMCPCoverageDetectsNewRouteAndMissingCapability(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/example", func(http.ResponseWriter, *http.Request) {})
	contracts := []managementRouteCoverage{{Route: "GET /example", Capabilities: []string{"example.read"}}}
	ops := []management.Operation{{Capability: "example.read"}}
	if issues := managementRouteIssues(r, ops, contracts); len(issues) != 0 {
		t.Fatalf("valid route: %v", issues)
	}
	r.Post("/new-business-action", func(http.ResponseWriter, *http.Request) {})
	if issues := managementRouteIssues(r, ops, contracts); !slices.Contains(issues, "unregistered mounted route: POST /new-business-action") {
		t.Fatalf("new route went unnoticed: %v", issues)
	}
	if issues := managementRouteIssues(r, nil, contracts); !slices.Contains(issues, "GET /example: missing MCP capability example.read") {
		t.Fatalf("removed capability went unnoticed: %v", issues)
	}
	if issues := managementRouteIssues(r, ops, []managementRouteCoverage{{Route: "GET /example", Exception: "not_implemented", Reason: "later"}}); !slices.Contains(issues, "invalid route exception: GET /example") {
		t.Fatal("unimplemented work can masquerade as an exception")
	}
}

func coverageJSONFields(typ reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}

func coverageSchemaNode(root, node map[string]any, path []string) (map[string]any, error) {
	if ref, ok := node["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("nonlocal schema reference %s", ref)
		}
		var target any = root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			object, ok := target.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid reference %s", ref)
			}
			target = object[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
		}
		var ok bool
		node, ok = target.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("missing reference %s", ref)
		}
		return coverageSchemaNode(root, node, path)
	}
	if len(path) == 0 {
		return node, nil
	}
	part := strings.TrimSuffix(path[0], "[]")
	props, _ := node["properties"].(map[string]any)
	if next, ok := props[part].(map[string]any); ok {
		if strings.HasSuffix(path[0], "[]") {
			resolved, err := coverageSchemaNode(root, next, nil)
			if err != nil {
				return nil, err
			}
			next, ok = resolved["items"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s has no array items", part)
			}
		}
		return coverageSchemaNode(root, next, path[1:])
	}
	for _, union := range []string{"anyOf", "oneOf", "allOf"} {
		if variants, ok := node[union].([]any); ok {
			for _, variant := range variants {
				if object, ok := variant.(map[string]any); ok {
					if found, err := coverageSchemaNode(root, object, path); err == nil {
						return found, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("missing schema field %s", strings.Join(path, "."))
}

func managementFieldIssues(types map[string]reflect.Type, ops []management.Operation, contracts []managementFieldCoverage) []string {
	var issues []string
	byCap := map[string]management.Operation{}
	for _, op := range ops {
		byCap[op.Capability] = op
	}
	registered := map[string]bool{}
	for _, row := range contracts {
		key := row.Type + "." + row.Field
		if registered[key] {
			issues = append(issues, "duplicate field contract: "+key)
		}
		registered[key] = true
		typ, ok := types[row.Type]
		if !ok {
			issues = append(issues, "field type no longer reachable: "+key)
			continue
		}
		if _, ok := coverageJSONFields(typ)[row.Field]; !ok {
			issues = append(issues, "field no longer exists: "+key)
		}
		if !slices.Contains([]string{"writable", "readonly", "authorization_root", "user_authorization", "dedicated"}, row.Access) || row.Reason == "" {
			issues = append(issues, "invalid field disposition: "+key)
		}
		if row.Access == "authorization_root" {
			if row.Capability != "" {
				issues = append(issues, "authorization root cannot name an MCP write/read capability: "+key)
			}
			continue
		}
		op, ok := byCap[row.Capability]
		if !ok {
			issues = append(issues, "field needs implemented capability: "+key+" -> "+row.Capability)
			continue
		}
		if row.SchemaParent != "" {
			parent, err := coverageSchemaNode(op.InputSchema, op.InputSchema, strings.Split(row.SchemaParent, "."))
			if err != nil {
				issues = append(issues, fmt.Sprintf("%s -> %s: %v", key, op.Name, err))
				continue
			}
			name := row.InputField
			if name == "" {
				name = row.Field
			}
			if _, err := coverageSchemaNode(op.InputSchema, parent, []string{name}); err != nil {
				issues = append(issues, fmt.Sprintf("%s missing writable schema %s.%s", key, op.Name, row.SchemaParent+"."+name))
			}
		}
	}
	for name, typ := range types {
		for field := range coverageJSONFields(typ) {
			if !registered[name+"."+field] {
				issues = append(issues, "unregistered domain field: "+name+"."+field)
			}
		}
	}
	slices.Sort(issues)
	return issues
}

func TestMCPCoverageFields(t *testing.T) {
	_, ops := mcpCoverageFixture(t)
	for _, issue := range managementFieldIssues(managementCoverageTypes(), ops, managementFieldContracts()) {
		t.Error(issue)
	}
}

func TestMCPCoverageDetectsNewField(t *testing.T) {
	typ := reflect.TypeOf(struct {
		Existing         bool `json:"existing"`
		NewBusinessField bool `json:"new_business_field"`
	}{})
	issues := managementFieldIssues(map[string]reflect.Type{"Example": typ}, []management.Operation{{Capability: "example.read"}}, []managementFieldCoverage{{Type: "Example", Field: "existing", Access: "readonly", Capability: "example.read", Reason: "derived"}})
	if !slices.Contains(issues, "unregistered domain field: Example.new_business_field") {
		t.Fatalf("new field went unnoticed: %v", issues)
	}
	issues = managementFieldIssues(map[string]reflect.Type{"Example": typ}, []management.Operation{{Name: "example_write", Capability: "example.write", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"patch": map[string]any{"type": "object", "properties": map[string]any{}}}}}}, []managementFieldCoverage{{Type: "Example", Field: "existing", Access: "writable", Capability: "example.write", SchemaParent: "patch", Reason: "explicit patch"}})
	if !slices.Contains(issues, "Example.existing missing writable schema example_write.patch.existing") {
		t.Fatalf("a registered field absent from the real patch schema went unnoticed: %v", issues)
	}
}

func TestMCPCoveragePatchWhitelists(t *testing.T) {
	for _, tc := range []struct{ domain, path, function, target string }{
		{"Channel", "management_config.go", "configOperations", "c"},
		{"Settings", filepath.Join("..", "store", "entity_patch.go"), "PatchRuntimeSettings", "v"},
		{"DownstreamModel", filepath.Join("..", "store", "entity_patch.go"), "PatchPublicModels", "m"},
	} {
		t.Run(tc.domain, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), tc.path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			actual := map[string]bool{}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != tc.function {
					continue
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || len(call.Args) < 3 {
						return true
					}
					name := ""
					switch fun := call.Fun.(type) {
					case *ast.Ident:
						name = fun.Name
					case *ast.SelectorExpr:
						name = fun.Sel.Name
					}
					if name != "MergeFields" {
						return true
					}
					target := call.Args[0]
					if ptr, ok := target.(*ast.UnaryExpr); ok {
						target = ptr.X
					}
					id, ok := target.(*ast.Ident)
					if !ok || id.Name != tc.target {
						return true
					}
					for _, arg := range call.Args[2:] {
						literal, ok := arg.(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							t.Error("patch whitelist must remain explicit")
							continue
						}
						field, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal(err)
						}
						actual[field] = true
					}
					return true
				})
			}
			if len(actual) == 0 {
				t.Fatalf("cannot locate %s.%s real patch whitelist", tc.path, tc.function)
			}
			registered := map[string]managementFieldCoverage{}
			for _, row := range managementFieldContracts() {
				if row.Type != tc.domain {
					continue
				}
				registered[row.Field] = row
				if row.Access == "writable" && !actual[row.Field] {
					t.Errorf("%s.%s is declared writable but the real patch rejects it", tc.domain, row.Field)
				}
			}
			for field := range actual {
				rule, ok := registered[field]
				if !ok || rule.Access == "readonly" || rule.Access == "authorization_root" || rule.Access == "user_authorization" {
					t.Errorf("real patch permits unregistered/nonwritable %s.%s", tc.domain, field)
				}
			}
		})
	}
}

func TestMCPCoverageModelSpecificationWhitelist(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "store", "entity_patch.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "PatchModelSpecification" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok || name.Name != "MergeFields" {
				return true
			}
			for _, arg := range call.Args[2:] {
				literal, ok := arg.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Error("model specification whitelist must stay explicit")
					continue
				}
				field, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				actual[field] = true
			}
			return true
		})
	}
	if len(actual) == 0 {
		t.Fatal("cannot locate the real ModelConfig patch whitelist")
	}
	fields := coverageJSONFields(reflect.TypeOf(store.ModelConfig{}))
	for field := range fields {
		if !actual[field] {
			t.Errorf("ModelConfig.%s is not patchable through the actual store whitelist", field)
		}
	}
	for field := range actual {
		if _, exists := fields[field]; !exists {
			t.Errorf("obsolete ModelConfig patch field %s", field)
		}
	}
}

func TestMCPCoverageToolSchemasAndHelpExamples(t *testing.T) {
	_, ops := mcpCoverageFixture(t)
	byName := map[string]management.Operation{}
	for _, op := range ops {
		byName[op.Name] = op
		if op.Capability == "" || op.Description == "" || !slices.Contains([]string{management.Read, management.Manage, management.Maintain, management.Sensitive}, op.Permission) {
			t.Errorf("incomplete metadata for %s", op.Name)
		}
		if op.Capability == "usage.amount.apply" && !op.Destructive {
			t.Errorf("history overwrite lacks destructive annotation: %s", op.Name)
		}
		if slices.Contains([]string{"upstream.discover", "upstream.probe", "upstream.oauth_discover", "oauth.start", "oauth.status", "catalog.fetch", "catalog.offerings.fetch", "exchange.refresh", "quota.refresh", "credential.operation.refresh_reset_cards", "credential.operation.warmup_credential", "credential.operation.use_reset_card"}, op.Capability) && !op.External {
			t.Errorf("external request capability lacks external annotation: %s", op.Name)
		}
		encoded, err := json.Marshal(op.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err = json.Unmarshal(encoded, &schema); err != nil {
			t.Errorf("%s: invalid schema: %v", op.Name, err)
		} else if _, err = schema.Resolve(nil); err != nil {
			t.Errorf("%s: unresolved schema: %v", op.Name, err)
		}
		if !op.ReadOnly {
			required, _ := op.InputSchema["required"].([]any)
			if !slices.Contains(required, any("request_id")) {
				t.Errorf("write tool lacks request_id: %s", op.Name)
			}
		}
	}
	help, ok := byName["get_help"]
	if !ok {
		t.Fatal("get_help not registered")
	}
	principal := management.Principal{Permissions: []string{management.Read, management.Manage, management.Maintain, management.Sensitive}}
	exemplified := map[string]bool{}
	for _, example := range managementHelpExamples() {
		t.Run(example.Tool, func(t *testing.T) {
			if _, ok := byName[example.Tool]; !ok {
				t.Fatal("example names an absent tool")
			}
			exemplified[example.Tool] = true
			raw, _ := json.Marshal(map[string]string{"tool": example.Tool})
			out, err := help.Handler(context.Background(), principal, raw)
			if err != nil {
				t.Fatal(err)
			}
			ref, ok := out.(map[string]any)
			if !ok || ref["name"] != example.Tool {
				t.Fatal("get_help did not return the current tool reference")
			}
			encoded, err := json.Marshal(ref["input_schema"])
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err = json.Unmarshal(encoded, &schema); err != nil {
				t.Fatal(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = resolved.Validate(example.Arguments); err != nil {
				t.Errorf("example no longer validates against get_help schema: %v", err)
			}
		})
	}
	// Every actual tool cited by the in-product examples must have a parameter
	// example checked above, rather than testing only that its name exists.
	for _, name := range regexp.MustCompile(`[a-z][a-z_]+`).FindAllString(managementHelp["examples"], -1) {
		if _, exists := byName[name]; exists && !exemplified[name] {
			t.Errorf("get_help examples cite %s without a validated input example", name)
		}
	}
}

func TestMCPCoverageReference(t *testing.T) {
	_, ops := mcpCoverageFixture(t)
	actual, err := managementReferenceJSON(ops)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "docs", "generated", "mcp-tools.json")
	if os.Getenv("TOROUTER_UPDATE_MCP_REFERENCE") == "1" {
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, actual, 0644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read MCP reference: %v; run scripts/check-mcp.ps1 -UpdateReference", err)
	}
	// Normalize checkout line endings, but keep the content/format comparison
	// exact so schema or annotation changes require an intentional regeneration.
	if !bytes.Equal(bytes.ReplaceAll(expected, []byte("\r\n"), []byte("\n")), actual) {
		t.Fatal("MCP reference differs from the actual registry; run scripts/check-mcp.ps1 -UpdateReference and inspect the generated diff")
	}
}
