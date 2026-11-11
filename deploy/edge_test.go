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
		"^(GET|HEAD):/ui(/.*)?$":                            "@edge_default",
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

func TestEdgeEncodedPathGuard(t *testing.T) {
	template := edgeFile(t, "deploy/edge/default.conf.template")
	guard := regexp.MustCompile(`(?s)map \$request_uri \$edge_encoded_path \{\s*default 0;\s*"~\*([^"\n]+)" 1;\s*\}`).FindStringSubmatch(template)
	if len(guard) != 2 {
		t.Fatal("raw request path needs a case-insensitive encoded separator/dot guard")
	}
	pattern, err := regexp.Compile("(?i)" + guard[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1%2Fdocuments", "/v1/documents/%2e%2e/metrics", "/ui/%2E%2E/metrics", "/v1/documents/x%5Cy", "/v1%2fagent%2fstatus", "/v1/documents/x%5cy"} {
		if !pattern.MatchString(path) {
			t.Errorf("encoded path bypass: %s", path)
		}
	}
	for _, path := range []string{"/v1/documents/abcdef012345", "/v1/documents?x=a%2Fb", "/v1/documents?x=%2e%5C", "/ui/app.js", "/v1/documents?x=/v1%2fdocuments"} {
		if pattern.MatchString(path) {
			t.Errorf("legitimate path or query rejected: %s", path)
		}
	}
	// A server rewrite check runs before try_files or any named proxy group.
	servers := strings.Split(template, "server {\n")[1:]
	for _, server := range servers {
		check := strings.Index(server, "if ($edge_encoded_path) { return 400; }")
		if check < 0 || check > strings.Index(server, "location ") {
			t.Error("encoded paths must be rejected at server scope before dispatch")
		}
	}
}

func TestEdgePolicy(t *testing.T) {
	template := edgeFile(t, "deploy/edge/default.conf.template")
	if !strings.Contains(template, "return 301 https://${SERVER_NAME}$edge_https_suffix$request_uri;") || strings.Contains(template, "https://$host") {
		t.Error("HTTP redirect must use the configured server name")
	}
	for _, directive := range []string{
		"absolute_redirect off;", "auth_basic_user_file /etc/nginx/edge-auth/htpasswd;",
		"ssl_protocols TLSv1.2 TLSv1.3;", "server_tokens off;", "max-age=${EDGE_HSTS_MAX_AGE}",
		"X-Content-Type-Options nosniff always;", "Referrer-Policy no-referrer always;", "X-Frame-Options DENY always;", "frame-ancestors 'none'",
		"resolver 127.0.0.11 valid=1s ipv6=off;", "set $gkg_upstream http://gkg:8080;",
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

func TestEdgeHealthcheckUsesHostname(t *testing.T) {
	compose := edgeFile(t, "deploy/docker-compose.yml")
	if !strings.Contains(compose, `"https://localhost:8443/healthz"`) {
		t.Fatal("edge healthcheck must avoid CRS numeric Host-header matches")
	}
}

func TestEdgeBodyLimitStatusPolicy(t *testing.T) {
	setup := edgeFile(t, "deploy/edge/setup.conf.template")
	rule := `SecRule REQBODY_ERROR_MSG "@streq Request body excluding files is bigger than the maximum expected." "id:10004,phase:2,t:none,log,deny,status:413,msg:'Request body exceeds non-file limit.'"`
	if !strings.HasPrefix(setup, "#") || !strings.Contains(setup, rule) {
		t.Fatal("only the pinned non-file size error must become 413")
	}
	previous := strings.Index(setup, rule)
	for _, include := range []string{
		"Include /etc/modsecurity.d/modsecurity.conf",
		"Include /etc/modsecurity.d/modsecurity-override.conf",
		"Include /etc/modsecurity.d/owasp-crs/crs-setup.conf",
		"Include /etc/modsecurity.d/owasp-crs/rules/*.conf",
	} {
		next := strings.Index(setup, include)
		if next <= previous {
			t.Fatal("body-limit status policy must precede the core parse-error handler and retain CRS")
		}
		previous = next
	}
	if strings.Contains(setup, "SecRuleRemove") || regexp.MustCompile(`(?i)(SecRuleEngine|ctl:ruleEngine)`).MatchString(setup) || strings.Count(setup, "SecRule ") != 1 {
		t.Fatal("status policy must not disable or replace other body validation")
	}
}

func TestEdgeCheckCleanupSurvivesEvidenceFailure(t *testing.T) {
	script := edgeFile(t, "scripts/edge-check.sh")
	cleanup := regexp.MustCompile(`(?ms)^cleanup\(\) \{.*?^\}`).FindString(script)
	if cleanup == "" {
		t.Fatal("cleanup function missing")
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail
started=$(date +%s)
tmp="$TEST_WORK"
active=1
make_args=(fixture)
compose=(true)
mkdir() { return 77; }
cp() { return 77; }
make() { printf '%s\n' "$*" "profiles=${COMPOSE_PROFILES:-}" > "$TEST_CALLS"; }
`+cleanup+"\ncleanup\n")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TEST_WORK=" + work, "TEST_CALLS=" + filepath.Join(dir, "calls"), "EDGE_CHECK_EVIDENCE_DIR=" + filepath.Join(dir, "evidence")}
	cmd.CombinedOutput()
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "destroy CONFIRM=yes fixture") {
		t.Error("evidence failures must not prevent project cleanup")
	}
	if !strings.Contains(string(calls), "profiles=cert\n") {
		t.Error("cleanup must include cert-only volumes left by removed renewal one-shots")
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Error("evidence failures must not prevent temp cleanup")
	}
}

func TestEdgeCheckTrustRefreshIsAtomic(t *testing.T) {
	script := edgeFile(t, "scripts/edge-check.sh")
	wrapper := regexp.MustCompile(`(?ms)^\{\n  printf '#!/bin/bash.*?^\} >"\$tmp/renew-held.sh"`).FindString(script)
	if wrapper == "" {
		t.Fatal("held-renewal wrapper missing")
	}
	dir := t.TempDir()
	for name, content := range map[string]string{
		"ca.crt": "old trust\n",
		"make":   "#!/bin/sh\nexit 0\n",
		"certificate-fixture": `#!/bin/sh
# A concurrent reader must still see the old trust file while extraction runs.
[ "$(cat "$TEST_DIR/ca.crt")" = 'old trust' ] || exit 71
printf 'new trust\n'
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/bash", "-c", `set -e
tmp="$TEST_DIR"
make_args=(fixture)
compose=("$TEST_DIR/certificate-fixture")
`+wrapper+"\n/bin/bash \"$tmp/renew-held.sh\"\n")
	cmd.Env = append(os.Environ(), "TEST_DIR="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("trust extraction truncated the active CA file: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil || string(data) != "new trust\n" {
		t.Error("new trust anchor was not published")
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

func TestEdgeExclusionsScoped(t *testing.T) {
	before := edgeFile(t, "deploy/edge/modsec/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf")
	if !scopedExclusions(before) {
		t.Fatal("exclusions must chain exact method/path and remove only approved rule/target combinations")
	}
	for _, name := range []string{"deploy/edge/modsec/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf", "deploy/edge/modsec/RESPONSE-999-EXCLUSION-RULES-AFTER-CRS.conf", "deploy/edge/default.conf.template"} {
		if regexp.MustCompile(`(?i)(SecRuleEngine|ctl:ruleEngine)`).MatchString(edgeFile(t, name)) {
			t.Error("edge policy must never override the rule engine")
		}
	}
}

func TestEdgeExclusionScopeGuard(t *testing.T) {
	valid := `SecRule REQUEST_METHOD "@streq POST" "id:10001,phase:1,pass,nolog,chain"
    SecRule REQUEST_FILENAME "@streq /v1/search" "ctl:ruleRemoveTargetById=942100;ARGS:json.query"`
	if !scopedExclusions(valid) {
		t.Fatal("guard rejected scoped rule")
	}
	for _, invalid := range []string{
		strings.ReplaceAll(valid, "REQUEST_METHOD", "REQUEST_HEADERS:Host"),
		strings.ReplaceAll(valid, "REQUEST_FILENAME", "REQUEST_URI"),
		strings.ReplaceAll(valid, "ARGS:json.query", "ARGS"),
		strings.ReplaceAll(valid, "ARGS:json.query", "ARGS:json.method"),
		strings.ReplaceAll(valid, "ARGS:json.query", "ARGS:json.params.name"),
		strings.ReplaceAll(valid, "@streq /v1/search", "@beginsWith /v1/"),
		valid + "\nSecRuleUpdateTargetById 942100 !ARGS:json.text",
	} {
		if scopedExclusions(invalid) {
			t.Error("guard accepted an unscoped or protected-field exclusion")
		}
	}
}

func scopedExclusions(text string) bool {
	text = strings.ReplaceAll(text, "\\\n", "")
	text = regexp.MustCompile(`(?m)^\s*#[^\n]*`).ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	action := `ctl:ruleRemoveTargetBy(?:Id=[0-9]{6}|Tag=(?:attack-(?:sqli|rce|xss|lfi|rfi|injection-php|injection-generic|fixation|protocol)|OWASP_CRS/ATTACK-JAVA));(?:ARGS:json\.[a-zA-Z0-9_.]+|REQUEST_BODY|FILES:file)`
	block := regexp.MustCompile(`(?m)^SecRule REQUEST_METHOD "@streq POST" "id:[0-9]+,phase:1,pass,nolog,chain"\s+SecRule REQUEST_FILENAME "@streq (/[^" ]+)" "(` + action + `(?:,` + action + `)*)"`)
	allowed := map[string]map[string]bool{
		"/v1/ingest": {"json.text": true}, "/v1/search": {"json.query": true},
		"/v1/agent/ask": {"json.question": true}, "/v1/agent/ask/stream": {"json.question": true},
		"/mcp": {"json.params.arguments.text": true, "json.params.arguments.query": true},
	}
	for _, match := range block.FindAllStringSubmatch(text, -1) {
		for _, removal := range strings.Split(match[2], ",") {
			selector, target, _ := strings.Cut(removal, ";")
			switch target {
			case "REQUEST_BODY":
				if (match[1] != "/v1/ingest" && match[1] != "/mcp") || selector != "ctl:ruleRemoveTargetByTag=OWASP_CRS/ATTACK-JAVA" {
					return false
				}
			case "FILES:file":
				if match[1] != "/v1/ingest" || selector != "ctl:ruleRemoveTargetById=932180" {
					return false
				}
			default:
				if selector == "ctl:ruleRemoveTargetByTag=OWASP_CRS/ATTACK-JAVA" || !allowed[match[1]][strings.TrimPrefix(target, "ARGS:")] {
					return false
				}
			}
		}
	}
	return strings.TrimSpace(block.ReplaceAllString(text, "")) == ""
}

func TestEdgeTagCoverage(t *testing.T) {
	before := edgeFile(t, "deploy/edge/modsec/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf")
	for _, field := range []string{"json.text", "json.query", "json.question", "json.params.arguments.text", "json.params.arguments.query"} {
		for _, tag := range []string{"attack-sqli", "attack-rce", "attack-xss", "attack-lfi", "attack-rfi", "attack-injection-php", "attack-injection-generic", "attack-fixation", "attack-protocol"} {
			if !strings.Contains(before, "ctl:ruleRemoveTargetByTag="+tag+";ARGS:"+field) {
				t.Errorf("missing %s on %s", tag, field)
			}
		}
	}
	if strings.Contains(before, "ByTag=attack-injection-java;") {
		t.Error("Java filename-only tag must not exclude named ARGS fields")
	}
	if strings.Contains(before, "arguments.question") {
		t.Error("nonexistent MCP question argument must not be excluded")
	}
}

func TestEdgeFilenameScopeGuard(t *testing.T) {
	valid := `SecRule REQUEST_METHOD "@streq POST" "id:10010,phase:1,pass,nolog,chain"
    SecRule REQUEST_FILENAME "@streq /v1/ingest" "ctl:ruleRemoveTargetById=932180;FILES:file"`
	if !scopedExclusions(valid) {
		t.Error("guard rejected the approved filename exclusion")
	}
	for _, invalid := range []string{
		strings.ReplaceAll(valid, "/v1/ingest", "/mcp"),
		strings.ReplaceAll(valid, "/v1/ingest", "/v1/ingest/"),
		strings.ReplaceAll(valid, "POST", "GET"),
		strings.ReplaceAll(valid, "932180", "932181"),
		strings.ReplaceAll(valid, "ById=932180", "ByTag=attack-rce"),
		strings.ReplaceAll(valid, "FILES:file", "FILES"),
		strings.ReplaceAll(valid, "FILES:file", "FILES_NAMES:file"),
		strings.ReplaceAll(valid, "FILES:file", "FILES:other"),
		strings.ReplaceAll(valid, "FILES:file", "REQUEST_BODY"),
	} {
		if scopedExclusions(invalid) {
			t.Errorf("guard accepted broadened filename exclusion: %s", invalid)
		}
	}
}

func TestEdgeFilenameExclusion(t *testing.T) {
	before := edgeFile(t, "deploy/edge/modsec/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf")
	if !strings.Contains(before, "ctl:ruleRemoveTargetById=932180;FILES:file") {
		t.Fatal("missing observed dockerfile.md multipart filename exclusion")
	}
}

func TestEdgeBodyScopeGuard(t *testing.T) {
	for _, path := range []string{"/v1/ingest", "/mcp"} {
		valid := `SecRule REQUEST_METHOD "@streq POST" "id:10011,phase:1,pass,nolog,chain"
    SecRule REQUEST_FILENAME "@streq ` + path + `" "ctl:ruleRemoveTargetByTag=OWASP_CRS/ATTACK-JAVA;REQUEST_BODY"`
		if !scopedExclusions(valid) {
			t.Errorf("guard rejected approved Java body exclusion on %s", path)
		}
		for _, route := range []string{"/v1/search", "/v1/agent/ask", "/v1/agent/ask/stream", "/ui/", "/mcp/", "/v1/ingest/", "/other"} {
			if scopedExclusions(strings.ReplaceAll(valid, path, route)) {
				t.Errorf("guard accepted REQUEST_BODY removal on %s", route)
			}
		}
		for _, selector := range []string{"Tag=attack-java", "Tag=attack-rce", "Tag=attack-protocol", "Tag=attack-sqli", "Tag=language-java", "Tag=OWASP_CRS/ATTACK-JAVA-extra", "Id=944100", "Id=921130"} {
			if scopedExclusions(strings.ReplaceAll(valid, "Tag=OWASP_CRS/ATTACK-JAVA", selector)) {
				t.Errorf("guard accepted REQUEST_BODY removal for %s", selector)
			}
		}
		for _, invalid := range []string{
			strings.ReplaceAll(valid, "POST", "GET"),
			strings.ReplaceAll(valid, "@streq "+path, "@beginsWith "+path),
			strings.ReplaceAll(valid, "REQUEST_BODY", "ARGS:json.text"),
			valid + "\nSecRuleUpdateTargetById 944100 !REQUEST_BODY",
		} {
			if scopedExclusions(invalid) {
				t.Error("guard accepted broadened Java body exclusion")
			}
		}
	}
}

func TestEdgeBodyExclusions(t *testing.T) {
	before := edgeFile(t, "deploy/edge/modsec/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf")
	for _, path := range []string{"/v1/ingest", "/mcp"} {
		if !strings.Contains(before, `SecRule REQUEST_FILENAME "@streq `+path+`" "ctl:ruleRemoveTargetByTag=OWASP_CRS/ATTACK-JAVA;REQUEST_BODY"`) {
			t.Errorf("missing observed Java corpus REQUEST_BODY exclusion on %s", path)
		}
	}
}

func TestEdgeTagScopeGuard(t *testing.T) {
	valid := `SecRule REQUEST_METHOD "@streq POST" "id:10001,phase:1,pass,nolog,chain"
    SecRule REQUEST_FILENAME "@streq /v1/search" "ctl:ruleRemoveTargetByTag=attack-sqli;ARGS:json.query"`
	if !scopedExclusions(valid) {
		t.Fatal("guard rejected scoped attack tag")
	}
	for _, invalid := range []string{
		strings.ReplaceAll(valid, "ARGS:json.query", "ARGS"),
		strings.ReplaceAll(valid, "json.query", "json.method"),
		strings.ReplaceAll(valid, "json.query", "json.params.name"),
		strings.ReplaceAll(valid, "json.query", "json.source"),
		strings.ReplaceAll(valid, "/v1/search", "/v1/ingest"),
		strings.ReplaceAll(valid, "@streq /v1/search", "@beginsWith /v1/"),
		strings.ReplaceAll(valid, "REQUEST_METHOD", "REQUEST_URI"),
		strings.ReplaceAll(valid, "attack-sqli", "paranoia-level/1"),
		strings.ReplaceAll(valid, "attack-sqli", "attack-injection-java"),
		strings.ReplaceAll(valid, "attack-sqli", "OWASP_CRS/ATTACK-JAVA"),
	} {
		if scopedExclusions(invalid) {
			t.Error("guard accepted broadened exclusion")
		}
	}
}

func TestEdgePolyglotCorpus(t *testing.T) {
	for _, name := range []string{"design", "sql", "go", "python", "javascript", "java", "yaml", "dockerfile", "bash", "cypher", "regex", "html", "italian"} {
		if len(edgeFile(t, "deploy/edge/testdata/"+name+".md")) < 40 {
			t.Errorf("empty synthetic corpus %s", name)
		}
	}
}

func TestEdgeAskTimeouts(t *testing.T) {
	script := filepath.Join(repoRoot(t), "deploy/edge/15-validate-env.sh")
	for _, tc := range []struct{ name, agent, ask, want, rejected string }{
		{name: "defaults", want: "90"},
		{name: "derived", agent: "100", want: "130"},
		{name: "long ask", agent: "300", want: "330"},
		{name: "explicit margin", agent: "100", ask: "115", want: "115"},
		{name: "explicit higher", agent: "100", ask: "200", want: "200"},
		{name: "undercuts agent", agent: "100", ask: "90", rejected: "EDGE_ASK_TIMEOUT_S"},
		{name: "below margin", agent: "100", ask: "114", rejected: "EDGE_ASK_TIMEOUT_S"},
		{name: "default margin", ask: "74", rejected: "EDGE_ASK_TIMEOUT_S"},
		{name: "zero agent uses default", agent: "0", want: "90"},
		{name: "decimal leading zero", agent: "000100", ask: "000115", want: "115"},
		{name: "bad agent", agent: "bad", rejected: "GKG_AGENT_TIMEOUT_S"},
		{name: "agent injection", agent: "60;bad", rejected: "GKG_AGENT_TIMEOUT_S"},
		{name: "ask injection", ask: "90;bad", rejected: "EDGE_ASK_TIMEOUT_S"},
		{name: "negative ask", ask: "-1", rejected: "EDGE_ASK_TIMEOUT_S"},
		{name: "zero ask", ask: "0", rejected: "EDGE_ASK_TIMEOUT_S"},
		{name: "large agent", agent: "999999999999", rejected: "GKG_AGENT_TIMEOUT_S"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Sourcing models the image entrypoint's *.envsh handling. The child
			// shell proves the derived value is exported for the envsubst process.
			cmd := exec.Command("sh", "-c", `. "$1"; sh -c 'printf "%s" "$EDGE_ASK_TIMEOUT_S"'`, "sh", script)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GKG_API_KEY=synthetic-test-key", "SERVER_NAME=localhost", "GKG_AGENT_TIMEOUT_S=" + tc.agent, "EDGE_ASK_TIMEOUT_S=" + tc.ask}
			out, err := cmd.CombinedOutput()
			if tc.rejected != "" {
				if err == nil || !strings.Contains(string(out), tc.rejected) {
					t.Fatalf("wanted rejection of %s; err=%v output=%s", tc.rejected, err, out)
				}
				if tc.rejected == "EDGE_ASK_TIMEOUT_S" && tc.ask == "114" && !strings.Contains(string(out), "+ 15") {
					t.Fatal("must explain the required agent margin")
				}
			} else if err != nil || string(out) != tc.want {
				t.Fatalf("timeout=%q err=%v, want %s", out, err, tc.want)
			}
		})
	}
}

func TestEdgeDerivedTimeoutReachesEnvsubst(t *testing.T) {
	envsubst, err := exec.LookPath("envsubst")
	if err != nil {
		t.Skip("envsubst is required for the entrypoint integration check")
	}
	compose := edgeFile(t, "deploy/docker-compose.yml")
	for _, want := range []string{
		`GKG_AGENT_TIMEOUT_S: ${GKG_AGENT_TIMEOUT_S:-60}`,
		`EDGE_ASK_TIMEOUT_S: "${EDGE_ASK_TIMEOUT_S:-}"`,
		`./edge/15-validate-env.sh:/docker-entrypoint.d/15-validate-env.envsh:ro`,
	} {
		if !strings.Contains(compose, want) {
			t.Fatalf("missing edge environment wiring: %s", want)
		}
	}
	dir := t.TempDir()
	validator := filepath.Join(dir, "15-validate-env.envsh")
	source := filepath.Join(repoRoot(t), "deploy/edge/15-validate-env.sh")
	info, err := os.Stat(source)
	if err != nil || info.Mode()&0111 == 0 {
		t.Fatal("sourced entrypoint files must be executable")
	}
	if err := os.Symlink(source, validator); err != nil {
		t.Fatal(err)
	}
	renderer := filepath.Join(dir, "20-envsubst-on-templates.sh")
	if err := os.WriteFile(renderer, []byte("#!/bin/sh\nexec \"$ENVSUBST\" '${EDGE_ASK_TIMEOUT_S}' < \"$TEMPLATE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// The published entrypoint sources *.envsh and executes *.sh in the
	// same loop shell. An executed validator would lose its exports.
	cmd := exec.Command("sh", "-c", `set -e
printf '%s\n' "$1" "$2" | while read -r f; do
 case "$f" in
  *.envsh) [ -x "$f" ] && . "$f" ;;
  *.sh) [ -x "$f" ] && "$f" ;;
 esac
done`, "sh", validator, renderer)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GKG_API_KEY=synthetic-test-key", "SERVER_NAME=localhost", "GKG_AGENT_TIMEOUT_S=100", "ENVSUBST=" + envsubst, "TEMPLATE=" + filepath.Join(repoRoot(t), "deploy/edge/default.conf.template")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if strings.Count(string(out), "proxy_read_timeout 130s;") != 2 || strings.Contains(string(out), "${EDGE_ASK_TIMEOUT_S}") {
		t.Fatalf("derived timeout missing from ask/stream locations:\n%s", out)
	}
}
