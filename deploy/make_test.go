package deploy_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stubOpts struct {
	PortOut, ExecOut, IDsOut string
	EnvFile                  string
	NoEdge                   bool
	Input                    string
	EdgeEnvLog               *[]string
	StdinLog                 *string
	EnvLog                   *[]string
	Env                      []string
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
printf '%s\n' "$*" >> "$STUB_CALLS"
printf '%s\n' "${GKG_ENV_FILE:-}" >> "$STUB_ENV_LOG"
printf '%s|%s\n' "${EDGE_HTPASSWD:-}" "${EDGE_WAF_ENV:-}" >> "$STUB_EDGE_ENV_LOG"
case " $* " in
 *" htpasswd "*) cat > "$STUB_STDIN"; printf '%s:$2y$synthetic-hash\n' "${@: -1}" ;;
  *" port "*) printf '%s' "$STUB_PORT" ;;
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
	makeArgs := []string{"-C", repoRoot(t), "DOCKER=" + stub, "ENV_FILE=" + opts.EnvFile}
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
		"SMOKE_TIMEOUT_S=5", "SMOKE_POLL_S=0.1", "SMOKE_REQ_TIMEOUT_S=1",
	}
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
