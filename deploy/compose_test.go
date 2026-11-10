package deploy_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func defaultCompose(t *testing.T) composeModel {
	t.Helper()
	return renderCompose(t, writeFixture(t, "MEMGRAPH_PASSWORD=test-only"))
}

func wantServices(t *testing.T, model composeModel, want ...string) {
	t.Helper()
	var got []string
	for name := range model.Services {
		got = append(got, name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("services = %v, want %v", got, want)
	}
}

func wantDependencies(t *testing.T, gkg composeService, want ...string) {
	t.Helper()
	var got []string
	for name, dep := range gkg.DependsOn {
		got = append(got, name)
		if dep.Condition != "service_healthy" {
			t.Errorf("dependency %s must be healthy", name)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dependencies = %v, want %v", got, want)
	}
}

func TestComposeDefaultStackRunsModelServices(t *testing.T) {
	model := defaultCompose(t)
	wantServices(t, model, "memgraph", "gkg", "sidecar", "docling")
	for _, name := range []string{"sidecar", "docling"} {
		if len(model.Services[name].Profiles) != 0 {
			t.Errorf("%s still requires a profile", name)
		}
	}
	gkg := model.Services["gkg"]
	wantDependencies(t, gkg, "memgraph", "sidecar", "docling")
	wantEnv(t, gkg, "GKG_EXTRACTOR_MODE", "sidecar")
	wantEnv(t, gkg, "GKG_CONVERTER_MODE", "docling")
	wantEnv(t, gkg, "GKG_EXTRACTOR_SIDECAR_URL", "http://sidecar:8090")
	wantEnv(t, gkg, "GKG_CONVERTER_URL", "http://docling:5001")
}

func TestComposeEnvFileFollowsGKGEnvFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy/docker-compose.yml"))
	if err != nil {
		t.Fatal("read compose source")
	}
	if !strings.Contains(string(data), "${GKG_ENV_FILE:-../.env}") {
		t.Error("gkg env_file does not follow GKG_ENV_FILE")
	}
	model := renderCompose(t, writeFixture(t, "MEMGRAPH_PASSWORD=test-only", "GKG_PROBE_ONLY_IN_FILE=from-fixture"))
	wantEnv(t, model.Services["gkg"], "GKG_PROBE_ONLY_IN_FILE", "from-fixture")
}

func TestComposePortsLoopbackOnly(t *testing.T) {
	for name, service := range defaultCompose(t).Services {
		if len(service.Ports) == 0 {
			t.Errorf("%s has no published port", name)
		}
		for _, port := range service.Ports {
			if port.HostIP != "127.0.0.1" {
				t.Errorf("%s published port is not loopback-only", name)
			}
		}
	}
}

func TestComposePortOverrides(t *testing.T) {
	for _, overrides := range []bool{false, true} {
		lines := []string{"MEMGRAPH_PASSWORD=test-only"}
		want := map[string]string{"memgraph": "7687", "gkg": "8080", "sidecar": "8090", "docling": "5001"}
		if overrides {
			lines = append(lines, "MEMGRAPH_PORT=17687", "GKG_PORT=18080", "SIDECAR_PORT=18090", "DOCLING_PORT=15001")
			want = map[string]string{"memgraph": "17687", "gkg": "18080", "sidecar": "18090", "docling": "15001"}
		}
		model := renderCompose(t, writeFixture(t, lines...))
		for name, published := range want {
			ports := model.Services[name].Ports
			targets := map[string]int{"memgraph": 7687, "gkg": 8080, "sidecar": 8090, "docling": 5001}
			if len(ports) != 1 || ports[0].Published != published || ports[0].Target != targets[name] {
				t.Errorf("%s port mapping differs (overrides=%v)", name, overrides)
			}
		}
	}
}

func TestComposeRestartAndLogRotation(t *testing.T) {
	for name, service := range defaultCompose(t).Services {
		if service.Restart != "unless-stopped" {
			t.Errorf("%s restart policy missing", name)
		}
		if service.Logging.Driver != "json-file" || service.Logging.Options["max-size"] != "10m" || service.Logging.Options["max-file"] != "3" {
			t.Errorf("%s log rotation differs", name)
		}
	}
}

func TestComposeMemoryBudget(t *testing.T) {
	model := defaultCompose(t)
	floors := map[string]int64{"memgraph": 1 << 30, "gkg": 512 << 20, "sidecar": 2 << 30, "docling": 2 << 30}
	var total int64
	for name, floor := range floors {
		limit := model.Services[name].MemLimit
		if limit < floor {
			t.Errorf("%s memory limit below floor", name)
		}
		total += limit
	}
	if total > 14<<30 {
		t.Error("memory budget exceeds 14 GiB")
	}
	var internal int64
	for _, arg := range model.Services["memgraph"].Command {
		if value, ok := strings.CutPrefix(arg, "--memory-limit="); ok {
			internal, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	if internal <= 0 || internal >= model.Services["memgraph"].MemLimit>>20 {
		t.Error("memgraph internal memory limit must be positive and below its container limit")
	}
}

func TestComposeImagesPinned(t *testing.T) {
	model := defaultCompose(t)
	for _, name := range []string{"memgraph", "docling"} {
		if !strings.Contains(model.Services[name].Image, "@sha256:") {
			t.Errorf("%s image is not digest-pinned", name)
		}
	}
}

func TestComposeLightOverride(t *testing.T) {
	for _, mode := range []string{"mock", "openai"} {
		lines := []string{"MEMGRAPH_PASSWORD=test-only"}
		if mode == "openai" {
			lines = append(lines, "GKG_LIGHT_EXTRACTOR_MODE=openai")
		}
		model := renderCompose(t, writeFixture(t, lines...), "deploy/docker-compose.yml", "deploy/compose.light.yml")
		wantServices(t, model, "memgraph", "gkg")
		wantDependencies(t, model.Services["gkg"], "memgraph")
		wantEnv(t, model.Services["gkg"], "GKG_EXTRACTOR_MODE", mode)
		wantEnv(t, model.Services["gkg"], "GKG_CONVERTER_MODE", "off")
	}
}
