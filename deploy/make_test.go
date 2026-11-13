package deploy_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type stubOpts struct {
	PortOut, ExecOut, IDsOut                string
	CertName, InstallOut, DiskFP, ServedOut string
	RenewFail, ReloadFail, VerifyFail       bool
	EnvFile                                 string
	RepoDir, ModeProbe                      string
	NoEdge                                  bool
	Input                                   string
	EdgeEnvLog                              *[]string
	StdinLog                                *string
	EnvLog                                  *[]string
	Env                                     []string
}

func runMake(t *testing.T, opts stubOpts, args ...string) (string, []string, int) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "docker")
	log := filepath.Join(dir, "calls")
	envLog := filepath.Join(dir, "env-calls")
	edgeEnvLog := filepath.Join(dir, "edge-env-calls")
	stdinLog := filepath.Join(dir, "stdin")
	script := `#!/bin/bash
if [ -n "${STUB_MODE_PROBE:-}" ]; then
  mode=$(stat -c %a "$STUB_MODE_PROBE" 2>/dev/null) || mode=$(stat -f %Lp "$STUB_MODE_PROBE")
  [ "$mode" = 644 ] || { echo 'compose ran before mode repair' >&2; exit 74; }
fi
printf '%s\n' "${*//$'\n'/ }" >> "$STUB_CALLS"
printf '%s\n' "${GKG_ENV_FILE:-}" >> "$STUB_ENV_LOG"
printf '%s|%s\n' "${EDGE_HTPASSWD:-}" "${EDGE_WAF_ENV:-}" >> "$STUB_EDGE_ENV_LOG"
case " $* " in
 *" htpasswd "*) cat > "$STUB_STDIN"; printf '%s:$2y$synthetic-hash\n' "${@: -1}" ;;
  *" port "*) printf '%s' "$STUB_PORT" ;;
  *" printenv SERVER_NAME "*) printf '%s' "$STUB_CERT_NAME" ;;
  *" certbot certonly "*|*" certbot renew "*) [ "$STUB_RENEW_FAIL" != true ] || exit 71 ;;
  *" /edge-install-cert.sh certbot "*) printf '%s\n' "$STUB_INSTALL" ;;
  *" nginx -s reload "*) [ "$STUB_RELOAD_FAIL" != true ] || exit 72 ;;
  *" exec -T edge openssl x509 "*) printf '%s\n' "$STUB_DISK_FP" ;;
  *" exec -T edge sh "*) [ "$STUB_VERIFY_FAIL" != true ] || exit 73; printf '%s\n' "$STUB_SERVED" ;;
  *" exec "*) printf '%s' "$STUB_EXEC" ;;
  *" ps -q "*) printf '%s' "$STUB_IDS" ;;
esac
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal("write docker stub")
	}
	if opts.EnvFile == "" {
		opts.EnvFile = writeFixture(t, "MEMGRAPH_PASSWORD=s3cr3t-fixture-value")
	}
	edgeFile := filepath.Join(dir, "htpasswd")
	if !opts.NoEdge {
		if err := os.WriteFile(edgeFile, []byte("fixture:synthetic\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if opts.RepoDir == "" {
		opts.RepoDir = repoRoot(t)
	}
	makeArgs := []string{"-C", opts.RepoDir, "DOCKER=" + stub, "ENV_FILE=" + opts.EnvFile}
	if !opts.NoEdge {
		makeArgs = append(makeArgs, "EDGE_HTPASSWD="+edgeFile)
	}
	cmd := exec.Command("make", append(makeArgs, args...)...)
	cmd.Stdin = strings.NewReader(opts.Input)
	// Stub first in PATH AND a dead socket: accidental bare docker calls
	// cannot reach the daemon. Do not inherit Docker contexts or Make flags.
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + curlConfigHome(t), "DOCKER_HOST=unix:///nonexistent/docker.sock",
		"STUB_CALLS=" + log, "STUB_ENV_LOG=" + envLog, "STUB_EDGE_ENV_LOG=" + edgeEnvLog, "STUB_STDIN=" + stdinLog,
		"STUB_PORT=" + opts.PortOut, "STUB_EXEC=" + opts.ExecOut, "STUB_IDS=" + opts.IDsOut,
		"STUB_MODE_PROBE=" + opts.ModeProbe,
		"SMOKE_TIMEOUT_S=5", "SMOKE_POLL_S=0.1", "SMOKE_REQ_TIMEOUT_S=1",
	}
	cmd.Env = append(cmd.Env, "STUB_CERT_NAME="+opts.CertName, "STUB_INSTALL="+opts.InstallOut, "STUB_DISK_FP="+opts.DiskFP, "STUB_SERVED="+opts.ServedOut,
		"STUB_RENEW_FAIL="+fmt.Sprint(opts.RenewFail), "STUB_RELOAD_FAIL="+fmt.Sprint(opts.ReloadFail), "STUB_VERIFY_FAIL="+fmt.Sprint(opts.VerifyFail))
	cmd.Env = append(cmd.Env, opts.Env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal("start make")
		}
	}
	readLines := func(path string) []string {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal("read stub evidence")
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	if opts.EnvLog != nil {
		*opts.EnvLog = readLines(envLog)
	}
	if opts.EdgeEnvLog != nil {
		*opts.EdgeEnvLog = readLines(edgeEnvLog)
	}
	if opts.StdinLog != nil {
		data, _ := os.ReadFile(stdinLog)
		*opts.StdinLog = string(data)
	}
	return string(out), readLines(log), code
}

func oneCall(t *testing.T, calls []string) string {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("docker calls = %d, want 1", len(calls))
	}
	return calls[0]
}

func requireMakeOK(t *testing.T, code int) {
	t.Helper()
	if code != 0 {
		t.Fatalf("make exit = %d, want 0", code)
	}
}

func TestMakeDefaultIsHelp(t *testing.T) {
	out, calls, code := runMake(t, stubOpts{})
	requireMakeOK(t, code)
	for _, target := range []string{"up", "down", "destroy", "smoke"} {
		if !strings.Contains(out, target) {
			t.Errorf("help omits %s", target)
		}
	}
	if len(calls) != 0 {
		t.Error("help called docker")
	}
}

func TestMakeUp(t *testing.T) {
	fixture := writeFixture(t, "MEMGRAPH_PASSWORD=s3cr3t-fixture-value")
	out, calls, code := runMake(t, stubOpts{EnvFile: fixture}, "up")
	requireMakeOK(t, code)
	call := oneCall(t, calls)
	for _, part := range []string{"compose", "--env-file " + fixture, "-f deploy/docker-compose.yml", "up -d --build --wait --remove-orphans"} {
		if !strings.Contains(call, part) {
			t.Error("up omits required compose argument")
		}
	}
	if strings.Contains(call, "compose.light.yml") || strings.Contains(call, " -p ") {
		t.Error("default up unexpectedly selects light mode or project")
	}
	if strings.Contains(out, "s3cr3t-fixture-value") {
		t.Error("make output exposes fixture secret")
	}
}

func TestMakeRepairsModesBeforeCompose(t *testing.T) {
	files := map[string]modeFixture{"labels/template": {"public", 0644}}
	for _, path := range []string{"Makefile", "scripts/repo-modes.sh", "scripts/edge-renew.sh"} {
		data, err := os.ReadFile(filepath.Join(repoRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = modeFixture{string(data), 0644}
	}
	for _, target := range []string{"up", "up-cached", "cert", "renew", "restart"} {
		t.Run(target, func(t *testing.T) {
			dir := privateCheckout(t, files)
			probe := filepath.Join(dir, "labels/template")
			wantMode(t, probe, 0600)
			out, calls, code := runMake(t, stubOpts{
				RepoDir: dir, ModeProbe: probe, CertName: "fixture.example.com",
				InstallOut: "unchanged", DiskFP: "fixture-fingerprint", ServedOut: "fixture-fingerprint\n30",
			}, target)
			if code != 0 || len(calls) == 0 {
				t.Fatalf("%s exit=%d, compose calls=%d: %s", target, code, len(calls), out)
			}
			wantMode(t, filepath.Join(dir, "labels"), 0755)
		})
	}
}

func TestMakePassesEnvFileToContainers(t *testing.T) {
	fixture := writeFixture(t, "MEMGRAPH_PASSWORD=test-only")
	relative, err := filepath.Rel(repoRoot(t), fixture)
	if err != nil {
		t.Fatal("relative fixture path")
	}
	for _, file := range []string{fixture, relative} {
		var envs []string
		_, calls, code := runMake(t, stubOpts{EnvFile: file, EnvLog: &envs}, "up")
		requireMakeOK(t, code)
		oneCall(t, calls)
		if len(envs) != 1 || envs[0] != fixture {
			t.Error("GKG_ENV_FILE must be the absolute fixture path")
		}
	}
}

func TestMakeUpLight(t *testing.T) {
	for _, light := range []string{"1", "0"} {
		_, calls, code := runMake(t, stubOpts{}, "up", "LIGHT="+light)
		requireMakeOK(t, code)
		if strings.Contains(oneCall(t, calls), "-f deploy/compose.light.yml") != (light == "1") {
			t.Errorf("LIGHT=%s selected wrong files", light)
		}
	}
}

func probeProject(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("gkg-probe-%d-%d", os.Getpid(), time.Now().UnixNano())
}

func TestMakeProject(t *testing.T) {
	project := probeProject(t)
	for _, target := range []string{"up", "destroy"} {
		_, calls, code := runMake(t, stubOpts{}, "PROJECT="+project, target, "CONFIRM=yes")
		requireMakeOK(t, code)
		if !strings.Contains(oneCall(t, calls), "-p "+project+" ") {
			t.Error("compose did not select the requested project")
		}
	}
}

func TestMakeIgnoresEnvironmentProject(t *testing.T) {
	_, calls, code := runMake(t, stubOpts{Env: []string{"PROJECT=" + probeProject(t)}}, "up")
	requireMakeOK(t, code)
	if strings.Contains(oneCall(t, calls), " -p ") {
		t.Error("PROJECT from environment must not select a compose project")
	}
}

func TestMakeRejectsWhitespaceEnvFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a b", "fx.env")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("MEMGRAPH_PASSWORD=test-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"help", "up", "down", "destroy", "prune", "smoke"} {
		t.Run(target, func(t *testing.T) {
			out, calls, code := runMake(t, stubOpts{EnvFile: file}, target, "CONFIRM=yes")
			if code == 0 || !strings.Contains(out, "ENV_FILE") || len(calls) != 0 {
				t.Fatal("ENV_FILE with whitespace must refuse every target before docker")
			}
		})
	}
}

func TestMakeUpRefusesWithoutEnvFile(t *testing.T) {
	out, calls, code := runMake(t, stubOpts{EnvFile: filepath.Join(t.TempDir(), "missing")}, "up")
	if code == 0 || !strings.Contains(out, "cp .env.example .env") || len(calls) != 0 {
		t.Fatal("missing env file must refuse with copy instructions before docker")
	}
}

func TestMakeDownKeepsVolumes(t *testing.T) {
	for _, light := range []string{"0", "1"} {
		_, calls, code := runMake(t, stubOpts{}, "down", "LIGHT="+light)
		requireMakeOK(t, code)
		call := oneCall(t, calls)
		if !strings.Contains(call, "down --remove-orphans") || strings.Contains(call, " -v") || strings.Contains(call, "--volumes") {
			t.Error("down must remove orphans and preserve volumes")
		}
	}
}

func TestMakeDestroyRequiresConfirm(t *testing.T) {
	out, calls, code := runMake(t, stubOpts{}, "destroy")
	if code == 0 || !strings.Contains(out, "CONFIRM=yes") || len(calls) != 0 {
		t.Error("destroy must refuse before docker without CONFIRM=yes")
	}
	_, calls, code = runMake(t, stubOpts{}, "PROJECT="+probeProject(t), "destroy", "CONFIRM=yes")
	requireMakeOK(t, code)
	if !strings.Contains(oneCall(t, calls), "down -v --remove-orphans") {
		t.Error("confirmed destroy must remove volumes and orphans")
	}
}

func TestMakeDestroyRejectsEnvironmentConfirm(t *testing.T) {
	out, calls, code := runMake(t, stubOpts{Env: []string{"CONFIRM=yes"}}, "destroy")
	if code == 0 || !strings.Contains(out, "CONFIRM=yes") || len(calls) != 0 {
		t.Error("destroy must refuse environment CONFIRM=yes before docker")
	}
}

func TestMakeHelpers(t *testing.T) {
	for _, tc := range []struct {
		args []string
		end  string
	}{
		{[]string{"logs", "S=gkg"}, "logs -f --tail=200 gkg"},
		{[]string{"restart", "S=sidecar"}, "restart sidecar"},
		{[]string{"build"}, " build"},
		{[]string{"pull"}, "pull --ignore-buildable"},
		{[]string{"ps"}, " ps"},
	} {
		_, calls, code := runMake(t, stubOpts{}, tc.args...)
		requireMakeOK(t, code)
		if !strings.HasSuffix(oneCall(t, calls), tc.end) {
			t.Errorf("%s arguments differ", tc.args[0])
		}
	}
}

func TestMakeStatusScopedToProject(t *testing.T) {
	for _, ids := range []string{"", "abc123\ndef456\n"} {
		_, calls, code := runMake(t, stubOpts{IDsOut: ids}, "status")
		requireMakeOK(t, code)
		wantCalls := 2
		if ids != "" {
			wantCalls = 3
		}
		if len(calls) != wantCalls || !strings.HasSuffix(calls[0], " ps") || !strings.HasSuffix(calls[1], " ps -q") {
			t.Fatal("status must query only this compose project")
		}
		if ids != "" && calls[2] != "stats --no-stream abc123 def456" {
			t.Error("stats must receive only the project container IDs")
		}
	}
}

func TestMakePruneKeepsVolumes(t *testing.T) {
	_, calls, code := runMake(t, stubOpts{}, "prune")
	requireMakeOK(t, code)
	if len(calls) != 2 || calls[0] != "image prune -f" || calls[1] != "builder prune -f --filter until=168h" {
		t.Error("prune must only remove dangling images and old builder cache")
	}
}

func TestMakeEdgePaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	out, calls, code := runMake(t, stubOpts{NoEdge: true}, "up", "EDGE_HTPASSWD="+missing)
	if code == 0 || !strings.Contains(out, "make passwd LOGIN=") || len(calls) != 0 {
		t.Error("missing htpasswd must refuse before Docker")
	}
	for _, custom := range []bool{false, true} {
		var envs []string
		opts := stubOpts{NoEdge: true, EdgeEnvLog: &envs, Env: []string{"EDGE_HTPASSWD=ignored", "EDGE_WAF_ENV=ignored"}}
		args := []string{"down"}
		auth, waf := filepath.Join(repoRoot(t), "deploy/edge/htpasswd"), filepath.Join(repoRoot(t), "deploy/edge/waf.env")
		if custom {
			auth, waf = "fixture-auth", "fixture-waf"
			args = append(args, "EDGE_HTPASSWD="+auth, "EDGE_WAF_ENV="+waf)
			auth = filepath.Join(repoRoot(t), auth)
			waf = filepath.Join(repoRoot(t), waf)
		}
		_, calls, code := runMake(t, opts, args...)
		requireMakeOK(t, code)
		oneCall(t, calls)
		if len(envs) != 1 || envs[0] != auth+"|"+waf {
			t.Error("edge paths must be absolute, command-line-only, and rooted at repository")
		}
	}
}

func TestMakeComposeExtra(t *testing.T) {
	for _, commandLine := range []bool{false, true} {
		args := []string{"up"}
		if commandLine {
			args = append(args, "COMPOSE_EXTRA=fixture.yml")
		}
		_, calls, code := runMake(t, stubOpts{Env: []string{"COMPOSE_EXTRA=fixture.yml"}}, args...)
		requireMakeOK(t, code)
		if strings.Contains(oneCall(t, calls), "-f fixture.yml") != commandLine {
			t.Error("COMPOSE_EXTRA must come only from command line")
		}
	}
}

func TestMakePasswd(t *testing.T) {
	out, calls, code := runMake(t, stubOpts{}, "passwd")
	if code == 0 || !strings.Contains(out, "LOGIN") || len(calls) != 0 {
		t.Error("passwd needs LOGIN before Docker")
	}
	file := filepath.Join(t.TempDir(), "htpasswd")
	if err := os.WriteFile(file, []byte("bob:kept\nalice:old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	password := "test $:'\" password"
	var stdin string
	out, calls, code = runMake(t, stubOpts{Input: password + "\n", StdinLog: &stdin}, "passwd", "LOGIN=alice", "EDGE_HTPASSWD="+file)
	requireMakeOK(t, code)
	call := oneCall(t, calls)
	if !strings.Contains(call, "run --rm -i httpd:2.4.") || !strings.Contains(call, "@sha256:") || !strings.HasSuffix(call, " htpasswd -niB alice") || strings.Contains(call, password) || strings.Contains(out, password) || stdin != password+"\n" {
		t.Error("password must reach pinned bcrypt helper only on stdin")
	}
	data, _ := os.ReadFile(file)
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 || string(data) != "bob:kept\nalice:$2y$synthetic-hash\n" {
		t.Error("passwd must atomically replace only selected login with private permissions")
	}
	for _, login := range []string{"-bad", "a:b", "a b"} {
		_, calls, code := runMake(t, stubOpts{Input: password + "\n"}, "passwd", "LOGIN="+login)
		if code == 0 || len(calls) != 0 {
			t.Error("invalid login must refuse before Docker")
		}
	}
}

func TestMakePasswdGenerate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "htpasswd")
	var stdin string
	out, calls, code := runMake(t, stubOpts{StdinLog: &stdin}, "passwd", "LOGIN=alice", "GENERATE=1", "EDGE_HTPASSWD="+file)
	requireMakeOK(t, code)
	oneCall(t, calls)
	password := strings.TrimSuffix(stdin, "\n")
	if len(password) != 24 || strings.Count(out, password) != 1 {
		t.Error("generate must print a 24-character password once")
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), password) {
		t.Error("generated password must not be saved in plaintext")
	}
}

func TestMakeWAFMode(t *testing.T) {
	for _, mode := range []string{"", "off", "On extra"} {
		out, calls, code := runMake(t, stubOpts{}, "waf-mode", "MODE="+mode)
		if code == 0 || len(calls) != 0 || !strings.Contains(out, "MODE") {
			t.Error("invalid MODE must refuse before Docker")
		}
	}
	for _, mode := range []string{"On", "DetectionOnly"} {
		file := filepath.Join(t.TempDir(), "waf.env")
		os.WriteFile(file, []byte("old-state\n"), 0644)
		_, calls, code := runMake(t, stubOpts{}, "waf-mode", "MODE="+mode, "EDGE_WAF_ENV="+file, "PROJECT=fixture-project", "LIGHT=1", "COMPOSE_EXTRA=deploy/compose.edgecheck.yml")
		requireMakeOK(t, code)
		call := oneCall(t, calls)
		if !strings.HasSuffix(call, " up -d --force-recreate --no-deps edge") || !strings.Contains(call, "-p fixture-project") || !strings.Contains(call, "-f deploy/compose.edgecheck.yml") {
			t.Error("waf-mode must recreate only this project's edge")
		}
		data, _ := os.ReadFile(file)
		info, err := os.Stat(file)
		if err != nil || string(data) != "MODSEC_RULE_ENGINE="+mode+"\n" || info.Mode().Perm() != 0600 {
			t.Error("waf-mode must replace the whole private state file")
		}
	}
	_, calls, code := runMake(t, stubOpts{Env: []string{"MODE=On"}}, "waf-mode")
	if code == 0 || len(calls) != 0 {
		t.Error("MODE must come only from the command line")
	}
}

func TestMakeCert(t *testing.T) {
	for _, name := range []string{"", "localhost", "bad/name", "-bad.example"} {
		out, calls, code := runMake(t, stubOpts{CertName: name}, "cert")
		if code == 0 || len(calls) != 1 || !strings.Contains(out, "SERVER_NAME") {
			t.Errorf("invalid edge name must refuse issuance")
		}
	}
	for _, staging := range []bool{false, true} {
		args := []string{"cert", "PROJECT=fixture-project", "LIGHT=1", "COMPOSE_EXTRA=deploy/compose.edgecheck.yml"}
		if staging {
			args = append(args, "STAGING=1")
		}
		out, calls, code := runMake(t, stubOpts{CertName: "edge.example", InstallOut: "unchanged", DiskFP: "FP", ServedOut: "FP\n365"}, args...)
		if code != 0 {
			t.Fatalf("cert failed: %s", out)
		}
		joined := strings.Join(calls, "\n")
		want := "--profile cert run --rm certbot certonly --webroot -w /var/www/acme -d edge.example --non-interactive --agree-tos --register-unsafely-without-email"
		if !strings.Contains(joined, want) || strings.Contains(joined, "--test-cert") != staging || !strings.Contains(joined, "--entrypoint /edge-install-cert.sh certbot edge.example") {
			t.Error("cert must issue for the running edge then install/verify")
		}
		for _, call := range calls {
			if !strings.Contains(call, "-p fixture-project") || !strings.Contains(call, "-f deploy/compose.edgecheck.yml") {
				t.Error("certificate operations lost compose selection")
			}
		}
	}
}

func TestMakeRenewConvergence(t *testing.T) {
	for _, test := range []struct {
		name, install, disk, served                     string
		reload, fail, renewFail, reloadFail, verifyFail bool
	}{
		{name: "no-live", install: "unchanged", disk: "FP", served: "FP\n365"},
		{name: "installed", install: "installed", disk: "FP", served: "FP\n365", reload: true},
		{name: "repair-failed-reload", install: "unchanged", disk: "NEW", served: "OLD\n365", reload: true, fail: true},
		{name: "short-expiry", install: "installed", disk: "FP", served: "FP\n7", reload: true, fail: true},
		{name: "install-failure", install: "unexpected", fail: true},
		{name: "certbot-failure", renewFail: true, fail: true},
		{name: "reload-fallback", install: "installed", disk: "FP", served: "FP\n365", reload: true, reloadFail: true},
		{name: "verification-failure", install: "installed", disk: "FP", reload: true, verifyFail: true, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, calls, code := runMake(t, stubOpts{CertName: "localhost", InstallOut: test.install, DiskFP: test.disk, ServedOut: test.served, RenewFail: test.renewFail, ReloadFail: test.reloadFail, VerifyFail: test.verifyFail, Env: []string{"EDGE_RENEW_ATTEMPTS=1"}}, "renew")
			if (code != 0) != test.fail {
				t.Fatalf("wrong renewal result: %s", out)
			}
			joined := strings.Join(calls, "\n")
			if !strings.Contains(joined, "--profile cert run --rm certbot renew") {
				t.Error("renew did not call certbot")
			}
			if strings.Contains(joined, "nginx -s reload") != test.reload {
				t.Error("wrong reload decision")
			}
			if test.reloadFail && (!strings.Contains(joined, "restart edge") || !strings.Contains(out, "fallback")) {
				t.Error("reload fallback must be recorded")
			}
			if test.name == "short-expiry" && !strings.Contains(out, "7 days") {
				t.Error("expiry failure must name remaining days")
			}
			if test.renewFail && strings.Contains(joined, "/edge-install-cert.sh") {
				t.Error("failed certbot must stop renewal")
			}
		})
	}
}

// makeVariable prints one Makefile variable as make itself expands it.
func makeVariable(t *testing.T, name string, dir ...string) string {
	t.Helper()
	root := repoRoot(t)
	if len(dir) == 1 {
		root = dir[0]
	}
	// A second makefile on stdin, not --eval: make 3.81 (macOS) lacks --eval.
	cmd := exec.Command("make", "-s", "-C", root, "-f", "Makefile", "-f", "-", "print-var")
	cmd.Env = minimalEnv()
	cmd.Stdin = strings.NewReader("print-var: ; @printf '%s' '$(" + name + ")'\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make print %s: %v\n%s", name, err, out)
	}
	return string(out)
}

func TestMakeEdgeConfigDigest(t *testing.T) {
	root := repoRoot(t)
	digest := makeVariable(t, "EDGE_CONFIG_DIGEST")
	if !regexp.MustCompile(`^[0-9]+-[0-9]+$`).MatchString(digest) {
		t.Fatalf("EDGE_CONFIG_DIGEST=%q, want cksum CRC-size", digest)
	}
	if env := makeVariable(t, "COMPOSE_ENV"); !strings.Contains(env, "EDGE_CONFIG_DIGEST="+digest) {
		t.Fatal("compose must receive the edge config digest")
	}
	// Changing any listed file changes the digest: copy the Makefile and the
	// listed files, edit one copy, and compare.
	dir := t.TempDir()
	files := append([]string{"Makefile"}, strings.Fields(makeVariable(t, "EDGE_CONFIG_FILES"))...)
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := makeVariable(t, "EDGE_CONFIG_DIGEST", dir); got != digest {
		t.Fatalf("copied tree digest %q, want %q", got, digest)
	}
	for _, file := range files[1:] {
		path := filepath.Join(dir, file)
		data, _ := os.ReadFile(path)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		changed := makeVariable(t, "EDGE_CONFIG_DIGEST", dir)
		if changed == digest {
			t.Errorf("editing %s left the digest unchanged", file)
		}
		digest = changed
	}
	// Moving bytes across a file boundary keeps the concatenation identical;
	// the digest must still change (e.g. a rule moved from the before-CRS to
	// the after-CRS exclusions).
	for i := 1; i+1 < len(files); i++ {
		a, b := filepath.Join(dir, files[i]), filepath.Join(dir, files[i+1])
		da, _ := os.ReadFile(a)
		db, _ := os.ReadFile(b)
		if len(da) == 0 {
			continue
		}
		if err := os.WriteFile(a, da[:len(da)-1], 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b, append([]byte{da[len(da)-1]}, db...), 0o644); err != nil {
			t.Fatal(err)
		}
		moved := makeVariable(t, "EDGE_CONFIG_DIGEST", dir)
		if moved == digest {
			t.Errorf("moving a byte from %s to %s left the digest unchanged", files[i], files[i+1])
		}
		digest = moved
	}
}
