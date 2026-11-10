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
	EnvLog                   *[]string
	Env                      []string
}

func runMake(t *testing.T, opts stubOpts, args ...string) (string, []string, int) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "docker")
	log := filepath.Join(dir, "calls")
	envLog := filepath.Join(dir, "env-calls")
	script := `#!/bin/bash
printf '%s\n' "$*" >> "$STUB_CALLS"
printf '%s\n' "${GKG_ENV_FILE:-}" >> "$STUB_ENV_LOG"
case " $* " in
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
	makeArgs := []string{"-C", repoRoot(t), "DOCKER=" + stub, "ENV_FILE=" + opts.EnvFile}
	cmd := exec.Command("make", append(makeArgs, args...)...)
	// Stub first in PATH AND a dead socket: accidental bare docker calls
	// cannot reach the daemon. Do not inherit Docker contexts or Make flags.
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + curlConfigHome(t), "DOCKER_HOST=unix:///nonexistent/docker.sock",
		"STUB_CALLS=" + log, "STUB_ENV_LOG=" + envLog,
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
