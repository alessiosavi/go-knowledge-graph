package deploy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type smokeRequest struct {
	Method, Path, Auth string
	Body               []byte
	At                 time.Time
}

type smokeAPI struct {
	mu                      sync.Mutex
	requests                []smokeRequest
	status                  string
	searchMiss, deleteFails bool
	notReady                bool
	searchDelay             time.Duration
	polls                   int
	deleted                 bool
}

func (api *smokeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	api.mu.Lock()
	api.requests = append(api.requests, smokeRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), body, time.Now()})
	api.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /readyz":
		if api.notReady {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		io.WriteString(w, `{"status":"ready"}`)
	case "POST /v1/ingest":
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"document_id":"doc-smoke-1","noop":false}`)
	case "GET /v1/documents/doc-smoke-1/status":
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.deleted {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"not_found"}`)
			return
		}
		api.polls++
		status := api.status
		if status == "" {
			status = "received"
			if api.polls > 1 {
				status = "done"
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"id": "doc-smoke-1", "status": status})
	case "POST /v1/search":
		if api.searchDelay > 0 {
			select {
			case <-time.After(api.searchDelay):
			case <-r.Context().Done():
				return
			}
		}
		if api.searchMiss {
			io.WriteString(w, `{"hits":[]}`)
		} else {
			io.WriteString(w, `{"hits":[{"kind":"chunk","document_id":"doc-smoke-1"}]}`)
		}
	case "DELETE /v1/documents/doc-smoke-1":
		if api.deleteFails {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		api.mu.Lock()
		api.deleted = true
		api.mu.Unlock()
		io.WriteString(w, `{"deleted":1}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (api *smokeAPI) snapshot() []smokeRequest {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]smokeRequest(nil), api.requests...)
}

type smokeRunOpts struct {
	noKey, missingKey bool
	timeout           string
	curlLog           string
}

func curlConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".curlrc"), []byte("fail\n"), 0o600); err != nil {
		t.Fatal("write curl config fixture")
	}
	return dir
}

func runSmoke(t *testing.T, url string, opts smokeRunOpts) (string, int, time.Duration) {
	t.Helper()
	dir := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + curlConfigHome(t),
		"SMOKE_URL=" + url, "SMOKE_TIMEOUT_S=5", "SMOKE_POLL_S=0.1", "SMOKE_REQ_TIMEOUT_S=1"}
	if opts.timeout != "" {
		env = append(env, "SMOKE_TIMEOUT_S="+opts.timeout)
	}
	if !opts.noKey {
		key := filepath.Join(dir, "key")
		if !opts.missingKey {
			if err := os.WriteFile(key, []byte("smoke-test-key\n"), 0o600); err != nil {
				t.Fatal("write test key")
			}
		}
		env = append(env, "SMOKE_KEY_FILE="+key)
	}
	if opts.curlLog != "" {
		realCurl, err := exec.LookPath("curl")
		if err != nil {
			t.Fatal("curl unavailable")
		}
		shim := `#!/bin/bash
for arg in "$@"; do
  printf 'arg=%s\n' "$arg" >> "$CURL_ARG_LOG"
  case "$arg" in
    @*)
      file="${arg#@}"
      mode="$(stat -c %a "$file" 2>/dev/null || stat -f %Lp "$file")"
      printf 'mode=%s\n' "$mode" >> "$CURL_ARG_LOG"
      ;;
  esac
done
exec "$CURL_REAL" "$@"
`
		if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(shim), 0o700); err != nil {
			t.Fatal("write curl shim")
		}
		env[0] = "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
		env = append(env, "CURL_ARG_LOG="+opts.curlLog, "CURL_REAL="+realCurl)
	}
	cmd := exec.Command("bash", "scripts/smoke.sh")
	cmd.Dir, cmd.Env = repoRoot(t), env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal("start smoke script")
		}
	}
	return string(out), code, elapsed
}

func countRequests(api *smokeAPI, method string) int {
	n := 0
	for _, req := range api.snapshot() {
		if req.Method == method {
			n++
		}
	}
	return n
}

func TestSmokeHappyPath(t *testing.T) {
	api := &smokeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	out, code, _ := runSmoke(t, srv.URL, smokeRunOpts{})
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), "SMOKE PASS") {
		t.Fatalf("smoke happy path exit=%d; expected SMOKE PASS", code)
	}
	if strings.Contains(out, "smoke-test-key") {
		t.Error("smoke output exposes bearer")
	}
	var token string
	deleted, checked404 := false, false
	for _, req := range api.snapshot() {
		if strings.HasPrefix(req.Path, "/v1/") && req.Auth != "Bearer smoke-test-key" {
			t.Error("API request lacks expected bearer")
		}
		if req.Path == "/v1/ingest" {
			var body struct{ Text, Source string }
			if err := json.Unmarshal(req.Body, &body); err != nil {
				t.Fatal("ingest is not JSON")
			}
			token = regexp.MustCompile(`\bgkgsmoke[0-9]+\b`).FindString(body.Text)
			if !regexp.MustCompile(`^gkgsmoke[0-9]+$`).MatchString(token) || body.Source != "gkg-smoke-"+token {
				t.Error("ingest must use a unique smoke marker and matching source")
			}
		}
		if req.Path == "/v1/search" {
			var body struct{ Query, Kind string }
			if err := json.Unmarshal(req.Body, &body); err != nil || token == "" || body.Query != token || body.Kind != "chunk" {
				t.Error("search must query the ingested marker as chunks")
			}
		}
		if req.Method == "DELETE" && req.Path == "/v1/documents/doc-smoke-1" {
			deleted = true
		}
		if deleted && req.Method == "GET" && req.Path == "/v1/documents/doc-smoke-1/status" {
			checked404 = true
		}
	}
	if countRequests(api, "DELETE") != 1 || !checked404 {
		t.Error("smoke must delete exactly once and verify status is 404 afterwards")
	}
}

func TestSmokeNoKey(t *testing.T) {
	for _, missing := range []bool{false, true} {
		api := &smokeAPI{}
		srv := httptest.NewServer(api)
		out, code, _ := runSmoke(t, srv.URL, smokeRunOpts{noKey: !missing, missingKey: missing})
		srv.Close()
		if code != 0 || !strings.Contains(out, "SMOKE PASS") {
			t.Fatalf("smoke without key exit=%d, want 0", code)
		}
		for _, req := range api.snapshot() {
			if req.Auth != "" {
				t.Error("no-key request unexpectedly carries auth")
			}
		}
	}
}

func TestSmokeKeyNeverInArgs(t *testing.T) {
	api := &smokeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	log := filepath.Join(t.TempDir(), "curl-args")
	_, code, _ := runSmoke(t, srv.URL, smokeRunOpts{curlLog: log})
	if code != 0 {
		t.Fatalf("smoke with curl shim exit=%d, want 0", code)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal("read curl argument evidence")
	}
	if strings.Contains(string(data), "smoke-test-key") {
		t.Error("curl arguments expose bearer")
	}
	modes := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "mode=") {
			modes++
			if line != "mode=600" {
				t.Error("curl header file must have mode 600")
			}
		}
	}
	if modes == 0 {
		t.Error("curl never received a header file")
	}
}

func TestSmokeFailedDocumentStillCleansUp(t *testing.T) {
	api := &smokeAPI{status: "failed"}
	srv := httptest.NewServer(api)
	defer srv.Close()
	out, code, _ := runSmoke(t, srv.URL, smokeRunOpts{})
	if code != 1 || !strings.Contains(out, "SMOKE FAIL") || !strings.Contains(out, "doc-smoke-1") || countRequests(api, "DELETE") != 1 {
		t.Fatal("failed document must fail naming its ID and delete once")
	}
}

func TestSmokePendingTimesOut(t *testing.T) {
	api := &smokeAPI{status: "received"}
	srv := httptest.NewServer(api)
	defer srv.Close()
	out, code, elapsed := runSmoke(t, srv.URL, smokeRunOpts{timeout: "1"})
	if code != 1 || !strings.Contains(out, "doc-smoke-1") || elapsed > 4*time.Second || countRequests(api, "DELETE") != 1 {
		t.Fatalf("pending document must time out and clean up; exit=%d", code)
	}
}

func TestSmokeSearchMissStillCleansUp(t *testing.T) {
	api := &smokeAPI{searchMiss: true}
	srv := httptest.NewServer(api)
	defer srv.Close()
	out, code, _ := runSmoke(t, srv.URL, smokeRunOpts{})
	if code != 1 || !strings.Contains(out, "SMOKE FAIL") || countRequests(api, "DELETE") != 1 {
		t.Fatal("search miss must fail and delete once")
	}
}

func TestSmokeStalledRequest(t *testing.T) {
	api := &smokeAPI{searchDelay: 3 * time.Second}
	srv := httptest.NewServer(api)
	defer srv.Close()
	out, code, _ := runSmoke(t, srv.URL, smokeRunOpts{})
	var searchStart time.Time
	for _, req := range api.snapshot() {
		if req.Path == "/v1/search" {
			searchStart = req.At
		}
	}
	if code != 1 || !strings.Contains(out, "SMOKE FAIL") || searchStart.IsZero() || time.Since(searchStart) > 4*time.Second || countRequests(api, "DELETE") != 1 {
		t.Fatal("stalled search must be bounded and delete once")
	}
}

func TestSmokeDeleteFails(t *testing.T) {
	api := &smokeAPI{deleteFails: true}
	srv := httptest.NewServer(api)
	defer srv.Close()
	out, code, _ := runSmoke(t, srv.URL, smokeRunOpts{})
	if code != 1 || !strings.Contains(out, "delete") {
		t.Fatal("failed delete must fail the smoke check")
	}
}

func TestSmokeNotReady(t *testing.T) {
	api := &smokeAPI{notReady: true}
	srv := httptest.NewServer(api)
	defer srv.Close()
	out, code, elapsed := runSmoke(t, srv.URL, smokeRunOpts{timeout: "1"})
	if code != 1 || !strings.Contains(out, "SMOKE FAIL") || elapsed > 4*time.Second {
		t.Fatal("readiness failure must be bounded")
	}
	for _, req := range api.snapshot() {
		if req.Path == "/v1/ingest" {
			t.Error("unready API must not ingest")
		}
	}
}

func TestMakeSmokeUsesRunningProject(t *testing.T) {
	api := &smokeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	project := probeProject(t)
	out, calls, code := runMake(t, stubOpts{PortOut: strings.TrimPrefix(srv.URL, "http://"), ExecOut: "smoke-test-key"}, "smoke", "PROJECT="+project)
	if code != 0 || !strings.Contains(out, "SMOKE PASS") {
		t.Fatalf("make smoke exit=%d, want 0", code)
	}
	if strings.Contains(out, "smoke-test-key") {
		t.Error("make smoke exposes bearer")
	}
	for _, call := range calls {
		if !strings.Contains(call, "-p "+project+" ") {
			t.Error("make smoke queried a different project")
		}
	}
	foundBearer := false
	for _, req := range api.snapshot() {
		if strings.HasPrefix(req.Path, "/v1/") {
			foundBearer = true
			if req.Auth != "Bearer smoke-test-key" {
				t.Error("make smoke did not use the running container's bearer")
			}
		}
	}
	if !foundBearer {
		t.Error("make smoke never reached the project API")
	}
	out, _, code = runMake(t, stubOpts{}, "smoke")
	if code == 0 || !strings.Contains(out, "not running") {
		t.Error("make smoke must refuse when project has no gkg port")
	}
}

func TestMakeSmokePropagatesFailure(t *testing.T) {
	api := &smokeAPI{deleteFails: true}
	srv := httptest.NewServer(api)
	defer srv.Close()
	out, _, code := runMake(t, stubOpts{PortOut: strings.TrimPrefix(srv.URL, "http://")}, "smoke")
	if code == 0 || !strings.Contains(out, "delete failed") || strings.Contains(out, "SMOKE PASS") {
		t.Fatal("make smoke must propagate a failed delete as a non-zero exit")
	}
	if countRequests(api, "DELETE") == 0 {
		t.Fatal("make smoke never exercised the failing delete")
	}
}

func TestAcceptanceCurlConfigIsolation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "acceptance.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, function, setup, call string
	}{
		{"anonymous", "req", "", "req -sS test-input"},
		{"authenticated", "req", `printf 'Authorization: Bearer fixture\n' > "$TMP/auth"`, "req -sS test-input"},
		{"health", "wait_http", "", "wait_http test-input 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise the real shell functions with a curl stub: no service or
			// compose stack is started, and the private .env is never read.
			fn := regexp.MustCompile(`(?ms)^` + tc.function + `\(\) \{.*?^\}`).FindString(string(data))
			if fn == "" {
				t.Fatal("acceptance function missing")
			}
			script := `set -eu
curl() { [ "$1" = -q ] || { echo 'curl must have -q as its first argument'; return 77; }; }
` + fn + "\n" + tc.setup + "\n" + tc.call
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMP=" + t.TempDir(), "HOME=" + curlConfigHome(t)}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("acceptance curl isolation: %v: %s", err, out)
			}
		})
	}
}

func TestAcceptanceComposeRebuilds(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "acceptance.sh"))
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^\s*"\$\{COMPOSE\[@\]\}" up [^\n]+`).FindString(string(data))
	if line == "" {
		t.Fatal("acceptance compose startup command missing")
	}
	cmd := exec.Command("bash", "-c", "COMPOSE=(printf '%s\\n')\n"+line)
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Join(strings.Fields(string(out)), " ") != "up -d --build --wait gkg" {
		t.Fatal("acceptance compose must use up -d --build --wait gkg")
	}
}
