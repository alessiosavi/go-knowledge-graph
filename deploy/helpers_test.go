package deploy_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate deploy tests")
	}
	return filepath.Dir(filepath.Dir(file))
}

func writeFixture(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.env")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal("write env fixture")
	}
	return path
}

func minimalEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type composeModel struct {
	Services map[string]composeService `json:"services"`
	Volumes  map[string]any            `json:"volumes"`
}

type composeService struct {
	Build struct {
		Args map[string]string `json:"args"`
	} `json:"build"`
	User    string `json:"user"`
	Volumes []struct {
		Type     string `json:"type"`
		Source   string `json:"source"`
		Target   string `json:"target"`
		ReadOnly bool   `json:"read_only"`
		Bind     struct {
			CreateHostPath bool `json:"create_host_path"`
		} `json:"bind"`
	} `json:"volumes"`
	Image       string             `json:"image"`
	Profiles    []string           `json:"profiles"`
	Restart     string             `json:"restart"`
	Environment map[string]*string `json:"environment"`
	DependsOn   map[string]struct {
		Condition string `json:"condition"`
	} `json:"depends_on"`
	Command  []string `json:"command"`
	MemLimit int64    `json:"mem_limit,string"`
	Logging  struct {
		Driver  string            `json:"driver"`
		Options map[string]string `json:"options"`
	} `json:"logging"`
	Ports []struct {
		HostIP    string `json:"host_ip"`
		Published string `json:"published"`
		Target    int    `json:"target"`
	} `json:"ports"`
}

func renderCompose(t *testing.T, envFile string, files ...string) composeModel {
	t.Helper()
	env := append(minimalEnv(), "GKG_ENV_FILE="+envFile, "EDGE_WAF_ENV="+filepath.Join(t.TempDir(), "absent.env"))
	version := exec.Command("docker", "compose", "version")
	version.Env = env
	if err := version.Run(); err != nil {
		t.Skip("docker compose unavailable")
	}
	if len(files) == 0 {
		files = []string{"deploy/docker-compose.yml"}
	}
	args := []string{"compose", "--profile", "cert", "--env-file", envFile}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(repoRoot(t), file))
		if err != nil {
			t.Fatalf("compose input %s is missing", file)
		}
		// The pre-change config hardcodes ../.env. Sanitize only that legacy
		// reference in a temporary copy so RED never reads developer secrets.
		if strings.Contains(string(data), "      - ../.env\n") {
			data = []byte(strings.ReplaceAll(string(data), "      - ../.env\n", "      - "+envFile+"\n"))
			file = filepath.Join(t.TempDir(), "baseline.yml")
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal("write isolated baseline config")
			}
		}
		args = append(args, "-f", file)
	}
	args = append(args, "config", "--format", "json")
	cmd := exec.Command("docker", args...)
	cmd.Dir, cmd.Env = repoRoot(t), env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("compose config failed (rendered environment withheld)")
	}
	var model composeModel
	if err := json.Unmarshal(out, &model); err != nil {
		t.Fatal("decode compose model")
	}
	return model
}

func wantEnv(t *testing.T, service composeService, key, want string) {
	t.Helper()
	if value := service.Environment[key]; value == nil || *value != want {
		t.Errorf("%s differs from expected fixture/default", key)
	}
}
