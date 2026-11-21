package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/docs"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"go.yaml.in/yaml/v4"
)

func TestOpenAPIProductMutationBodyTooLarge(t *testing.T) {
	type mediaType struct {
		Schema struct {
			Ref  string `yaml:"$ref"`
			Type string
		}
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]mediaType
			}
		}
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"put", "delete"} {
		content := spec.Paths["/v1/products"][method].Responses["413"].Content
		if got := content["text/html"].Schema; got.Type != "string" || got.Ref != "" {
			t.Errorf("%s 413 should document the edge's HTML body", method)
		}
		jsonBody, hasJSON := content["application/json"]
		if method == "put" && jsonBody.Schema.Ref != "#/components/schemas/Error" {
			t.Error("PUT 413 should document the API's JSON Error body")
		}
		if method == "delete" && hasJSON {
			t.Error("DELETE 413 should only document the edge's HTML body")
		}
	}
}

func TestOpenAPIProductMutationStatuses(t *testing.T) {
	type property struct{ Enum []string }
	type schema struct{ Properties map[string]property }
	var spec struct {
		Components struct {
			Schemas   map[string]schema
			Responses map[string]struct {
				Content map[string]struct{ Schema schema }
			}
		}
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	applied, refused := graphProductMutationStatuses(t)
	success := spec.Components.Schemas["ProductMutationResult"].Properties["status"].Enum
	conflict := spec.Components.Schemas["ProductMutationConflict"].Properties["error"].Enum
	unavailable := spec.Components.Responses["ProductMutationUnavailable"].Content["application/json"].Schema.Properties["error"].Enum
	for status := range applied {
		rr := httptest.NewRecorder()
		writeProductMutation(rr, graph.ProductMutationResult{Status: status}, nil)
		if rr.Code != http.StatusOK || decode[graph.ProductMutationResult](t, rr).Status != status || !slices.Contains(success, status) {
			t.Errorf("applied status %q: got %d %s, OpenAPI enum %v", status, rr.Code, rr.Body, success)
		}
	}
	for status := range refused {
		rr := httptest.NewRecorder()
		writeProductMutation(rr, graph.ProductMutationResult{}, &graph.ProductMutationError{Status: status})
		var enum []string
		switch rr.Code {
		case http.StatusConflict:
			enum = conflict
		case http.StatusServiceUnavailable:
			enum = unavailable
		}
		if !slices.Contains(enum, status) || decode[errorBody](t, rr).Error != status {
			t.Errorf("graph error %q: got %d %s, OpenAPI enum for that response %v", status, rr.Code, rr.Body, enum)
		}
	}
}

// Read status literals from the graph source so a new outcome requires
// neither an update to a redundant test list nor a live database fixture.
func graphProductMutationStatuses(t *testing.T) (applied, refused map[string]bool) {
	t.Helper()
	source, err := parser.ParseFile(token.NewFileSet(), "../graph/product_push.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	applied, refused = map[string]bool{}, map[string]bool{}
	for node := range ast.Preorder(source) {
		switch node := node.(type) {
		case *ast.ReturnStmt:
			// The ordering, upsert, and remove helpers return ("status", nil).
			if len(node.Results) != 2 {
				continue
			}
			value, literal := node.Results[0].(*ast.BasicLit)
			errResult, ident := node.Results[1].(*ast.Ident)
			if literal && value.Kind == token.STRING && ident && errResult.Name == "nil" {
				status, _ := strconv.Unquote(value.Value)
				if status != "" {
					applied[status] = true
				}
			}
		case *ast.CompositeLit:
			typ, ok := node.Type.(*ast.Ident)
			if !ok || typ.Name != "ProductMutationError" {
				continue
			}
			for _, field := range node.Elts {
				pair, ok := field.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := pair.Key.(*ast.Ident)
				if !ok || key.Name != "Status" {
					continue
				}
				value, ok := pair.Value.(*ast.BasicLit)
				if !ok || value.Kind != token.STRING {
					t.Fatal("expected a ProductMutationError status literal")
				}
				status, _ := strconv.Unquote(value.Value)
				refused[status] = true
			}
		}
	}
	if len(applied) == 0 || len(refused) == 0 {
		t.Fatal("no product mutation statuses found in graph source")
	}
	return applied, refused
}

func TestOpenAPIProductStoreWatermark(t *testing.T) {
	type property struct {
		Type, Format string
		Nullable     bool
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]property
				Required   []string
			}
		}
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	schema := spec.Components.Schemas["ProductStoreWatermark"]
	for _, field := range reflect.VisibleFields(reflect.TypeFor[graph.ProductStoreWatermark]()) {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("ProductStoreWatermark.%s missing from OpenAPI", name)
		}
	}
	for name, want := range map[string]property{
		"materialized_activation_seq": {"integer", "int64", true},
		"pushed_at":                   {"string", "date-time", true},
	} {
		if got := schema.Properties[name]; got != want || slices.Contains(schema.Required, name) {
			t.Errorf("ProductStoreWatermark.%s = %+v, want optional %+v", name, got, want)
		}
	}
}

func TestDocsServesEmbeddedAssets(t *testing.T) {
	h := NewServer(config.Config{}, nil, nil).Handler()
	for _, tc := range []struct {
		path, contentType string
		code              int
	}{
		{"/docs", "text/html", 301},
		{"/docs/", "text/html", 200},
		{"/docs/index.html", "", 301},
		{"/docs/swagger-ui.css", "text/css", 200},
		{"/docs/swagger-ui-bundle.js", "text/javascript", 200},
		{"/docs/swagger-initializer.js", "text/javascript", 200},
		{"/docs/openapi.yaml", "application/yaml", 200},
		{"/docs/nope", "text/plain", 404},
		{"/docs/LICENSE", "text/plain", 404},
		{"/docs/VERSION", "text/plain", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rr := do(t, h, http.MethodGet, tc.path, nil, nil)
			if rr.Code != tc.code {
				t.Fatalf("code = %d, want %d", rr.Code, tc.code)
			}
			if tc.contentType != "" && !strings.HasPrefix(rr.Header().Get("Content-Type"), tc.contentType) {
				t.Errorf("Content-Type = %q, want %s", rr.Header().Get("Content-Type"), tc.contentType)
			}
			const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
			if got := rr.Header().Get("Content-Security-Policy"); got != csp {
				t.Errorf("CSP = %q, want %q", got, csp)
			}
			if tc.path == "/docs" && rr.Header().Get("Location") != "/docs/" {
				t.Errorf("Location = %q", rr.Header().Get("Location"))
			}
			if tc.path == "/docs/" && !strings.Contains(rr.Body.String(), `id="swagger-ui"`) {
				t.Error("missing Swagger UI root")
			}
		})
	}
	rr := do(t, h, http.MethodGet, "/docs/openapi.yaml", nil, nil)
	spec, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rr.Body.Bytes(), spec) {
		t.Error("served spec differs from docs/openapi.yaml")
	}
	rr = do(t, h, http.MethodHead, "/docs/openapi.yaml", nil, nil)
	if rr.Code != 200 || rr.Body.Len() != 0 {
		t.Errorf("HEAD spec: code=%d bytes=%d", rr.Code, rr.Body.Len())
	}
}

func TestDocsBearerPolicy(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	s := NewServer(cfg, nil, nil)
	h := s.Handler()
	for _, path := range []string{"/docs", "/docs/", "/docs/index.html", "/docs/swagger-ui.css", "/docs/swagger-ui-bundle.js", "/docs/swagger-initializer.js", "/docs/openapi.yaml"} {
		if !authExempt(path) {
			t.Errorf("%s must be bearer exempt", path)
		}
		rr := do(t, h, http.MethodGet, path, nil, nil)
		if rr.Code != 200 && rr.Code != 301 {
			t.Errorf("%s without bearer: %d", path, rr.Code)
		}
	}
	for _, path := range []string{"/docs/nope", "/docs/openapi.yaml/extra", "/docs/swagger-ui.css/extra", "/docs/LICENSE", "/docs/VERSION", "/docs2", "/v1/documents", "/v1/agent/status", "/v1/agent/ask", "/mcp"} {
		if authExempt(path) {
			t.Errorf("%s must not be bearer exempt", path)
		}
		rr := do(t, h, http.MethodGet, path, nil, nil)
		if rr.Code != 401 {
			t.Errorf("%s without bearer: %d, want 401", path, rr.Code)
		}
		if strings.HasPrefix(path, "/docs/") && rr.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s unauthorized response missing CSP", path)
		}
	}
	// Every registered REST data route retains the bearer requirement.
	for _, pattern := range s.RoutePatterns() {
		method, path, _ := strings.Cut(pattern, " ")
		if !strings.HasPrefix(path, "/v1/") {
			continue
		}
		path = strings.ReplaceAll(path, "{id}", "fixture-id")
		rr := do(t, h, method, path, nil, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without bearer: %d, want 401", method, path, rr.Code)
		}
	}
	rr := do(t, h, http.MethodGet, "/docs/nope", nil, map[string]string{"Authorization": "Bearer sekret"})
	if rr.Code != 404 {
		t.Errorf("unknown docs path with bearer: %d, want 404", rr.Code)
	}
}
