package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func edgeFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), name))
	if err != nil {
		t.Fatalf("missing edge input: %s", name)
	}
	return string(data)
}

func TestEdgeAllowlist(t *testing.T) {
	template := edgeFile(t, "deploy/edge/default.conf.template")
	// These expressions are the complete public method/path contract. Named
	// locations can only be reached through try_files, after Basic auth.
	want := map[string]string{
		"^(GET|HEAD):/ui/.*$":                               "@edge_default",
		"^(GET|HEAD):/v1/entities/[^/]+$":                   "@edge_default",
		"^(GET|HEAD):/v1/documents$":                        "@edge_default",
		"^(GET|HEAD|DELETE):/v1/documents/[^/]+$":           "@edge_default",
		"^(GET|HEAD):/v1/documents/[^/]+/(status|content)$": "@edge_default",
		"^(GET|HEAD):/v1/agent/status$":                     "@edge_default",
		"^POST:/v1/(search|traverse)$":                      "@edge_default",
		"^POST:/v1/ingest$":                                 "@edge_ingest",
		"^POST:/v1/agent/ask$":                              "@edge_ask",
		"^POST:/v1/agent/ask/stream$":                       "@edge_stream",
		"^(GET|POST|DELETE):/mcp$":                          "@edge_mcp",
		"^(GET|HEAD):/$":                                    "@edge_root",
	}
	got := map[string]string{}
	routes := regexp.MustCompile(`(?m)^\s*"~([^"\n]+)"\s+(@edge_\w+);`).FindAllStringSubmatch(template, -1)
	for _, route := range routes {
		got[route[1]] = route[2]
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("method/path allowlist differs from public contract")
	}
	if !strings.Contains(template, `map "$request_method:$uri" $edge_route`) || !strings.Contains(template, "default @edge_not_found;") || !strings.Contains(template, "try_files /__edge_missing $edge_route;") {
		t.Error("allowlist must dispatch only after access/auth")
	}
	app := edgeFile(t, "internal/webui/assets/app.js")
	// Reconstruct literal path expressions, including suffixes after an ID.
	// A new UI call such as documents/<id>/new-action must fail this contract.
	calls := regexp.MustCompile(`(?:api|fetch)\(\s*("/v1/[^"\n]*"(?:\s*\+\s*(?:encodeURIComponent\([^)]*\)|"[^"\n]*"))*)`)
	parts := regexp.MustCompile(`"([^"\n]*)"|encodeURIComponent\([^)]*\)`)
	for _, call := range calls.FindAllStringSubmatch(app, -1) {
		var path strings.Builder
		for _, part := range parts.FindAllStringSubmatch(call[1], -1) {
			if strings.HasPrefix(part[0], "encodeURIComponent(") {
				path.WriteString("fixture-id")
			} else {
				path.WriteString(part[1])
			}
		}
		route, _, _ := strings.Cut(path.String(), "?")
		covered := false
		for pattern := range want {
			re := regexp.MustCompile(pattern)
			if re.MatchString("GET:"+route) || re.MatchString("POST:"+route) || re.MatchString("DELETE:"+route) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("UI route has no edge entry: %s", route)
		}
	}

	// No unlisted location is permitted to introduce an upstream.
	if strings.Count(template, "proxy_pass ") != 5 {
		t.Error("expected exactly five named proxy groups")
	}
	if strings.Contains(template, "/v1/cypher") || strings.Contains(template, "/metrics") || strings.Contains(template, "/readyz") {
		t.Error("operator routes must not be in template")
	}
}

func TestEdgePolicy(t *testing.T) {
	template := edgeFile(t, "deploy/edge/default.conf.template")
	for _, directive := range []string{
		"absolute_redirect off;", "auth_basic_user_file /etc/nginx/edge-auth/htpasswd;",
		"ssl_protocols TLSv1.2 TLSv1.3;", "server_tokens off;", "max-age=${EDGE_HSTS_MAX_AGE}",
		"X-Content-Type-Options nosniff always;", "Referrer-Policy no-referrer always;", "X-Frame-Options DENY always;", "frame-ancestors 'none'",
		"resolver 127.0.0.11 valid=10s;", "set $gkg_upstream http://gkg:8080;",
		`proxy_set_header Authorization "Bearer ${GKG_API_KEY}";`,
		"client_max_body_size 1m;", "client_max_body_size 21037056;", "client_max_body_size 4m;",
		"proxy_read_timeout ${EDGE_INGEST_TIMEOUT_S}s;", "proxy_read_timeout ${EDGE_ASK_TIMEOUT_S}s;", "proxy_read_timeout 3600s;",
		"limit_req_zone $binary_remote_addr zone=edge:10m rate=20r/s;", "limit_req zone=edge burst=80 nodelay;",
		"$http_sec_fetch_site", "$http_origin", "$scheme://$http_host", "cross-site", "same-site", "POST|DELETE",
	} {
		if !strings.Contains(template, directive) {
			t.Errorf("edge policy missing %s", directive)
		}
	}
	for _, name := range []string{"stream", "mcp"} {
		body := regexp.MustCompile(`(?s)location @edge_` + name + ` \{(.*?)\n    \}`).FindString(template)
		for _, directive := range []string{"proxy_buffering off;", "X-Accel-Buffering no always;", "X-Frame-Options DENY always;"} {
			if !strings.Contains(body, directive) {
				t.Errorf("%s missing %s", name, directive)
			}
		}
	}
	if strings.Count(template, `proxy_set_header Authorization "Bearer ${GKG_API_KEY}";`) != 1 {
		t.Error("all proxies must inherit one server-level bearer replacement")
	}
	if strings.Count(template, "proxy_buffering off;") != 2 || strings.Count(template, "X-Accel-Buffering no always;") != 2 {
		t.Error("ask stream and MCP must disable buffering")
	}
	if strings.Contains(template, "modsecurity on;") {
		t.Error("image already enables ModSecurity at http scope")
	}
	for _, path := range []string{"location = /healthz", "location ^~ /.well-known/acme-challenge/"} {
		start := strings.Index(template, path)
		if start < 0 {
			t.Fatalf("missing unauthenticated location %s", path)
		}
		body := template[start:]
		end := strings.Index(body, "\n    }")
		if end < 0 {
			t.Fatal("unterminated location")
		}
		body = body[:end]
		if !strings.Contains(body, "auth_basic off;") {
			t.Error("public probe/challenge must disable inherited auth")
		}
	}
	if !strings.Contains(template, "location @edge_not_found { return 404; }") {
		t.Error("unlisted routes must terminate locally")
	}
}

func TestEdgeValidateEnv(t *testing.T) {
	script := filepath.Join(repoRoot(t), "deploy/edge/15-validate-env.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatal("missing pre-envsubst validation")
	}
	for _, test := range []struct {
		key, name string
		ok        bool
	}{
		{"synthetic-test-key", "localhost", true},
		{strings.Repeat("A", 256), "edge.example", true},
		{"short", "localhost", false}, {strings.Repeat("A", 257), "localhost", false},
		{"synthetic$key-for-test", "localhost", false}, {"synthetic;key-for-test", "localhost", false},
		{"synthetic\nkey-for-test", "localhost", false}, {"synthetic\"key-for-test", "localhost", false},
		{"synthetic-test-key", "invalid;name", false}, {"synthetic-test-key", "", false},
		{"synthetic-test-key", "-bad.example", false},
	} {
		cmd := exec.Command("sh", script)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GKG_API_KEY=" + test.key, "SERVER_NAME=" + test.name}
		out, err := cmd.CombinedOutput()
		if (err == nil) != test.ok {
			t.Error("environment safety validation returned wrong status")
		}
		if strings.Contains(string(out), test.key) {
			t.Error("validation exposed a key")
		}
		if !test.ok && !strings.Contains(string(out), "GKG_API_KEY") && !strings.Contains(string(out), "SERVER_NAME") {
			t.Error("validation must name rejected variable")
		}
	}
}
