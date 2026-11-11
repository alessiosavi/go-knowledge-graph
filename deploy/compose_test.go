package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func defaultCompose(t *testing.T) composeModel {
	t.Helper()
	return renderCompose(t, writeFixture(t, "MEMGRAPH_PASSWORD=test-only", "GKG_API_KEY=synthetic-test-key"))
}

func wantServices(t *testing.T, model composeModel, want ...string) {
	t.Helper()
	var got []string
	for name, service := range model.Services {
		if len(service.Profiles) > 0 {
			continue
		}
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
	wantServices(t, model, "memgraph", "gkg", "sidecar", "docling", "edge-init", "edge")
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

func TestComposeSidecarCustomModelRevision(t *testing.T) {
	revision := strings.Repeat("a", 40)
	model := renderCompose(t, writeFixture(t,
		"MEMGRAPH_PASSWORD=test-only", "GKG_API_KEY=synthetic-test-key",
		"GLINEREL_MODEL=org/custom", "GLINEREL_MODEL_REVISION="+revision))
	sidecar := model.Services["sidecar"]
	if sidecar.Build.Args["GLINEREL_MODEL"] != "org/custom" || sidecar.Build.Args["GLINEREL_MODEL_REVISION"] != revision {
		t.Fatal("sidecar build must receive the custom repository and its immutable revision")
	}
	wantEnv(t, sidecar, "GLINEREL_MODEL", "org/custom")
}

func TestComposeDoclingEntrypoint(t *testing.T) {
	for _, key := range []string{"", "synthetic-test-key"} {
		model := renderCompose(t, writeFixture(t,
			"MEMGRAPH_PASSWORD=test-only", "GKG_API_KEY=synthetic-test-key", "DOCLING_API_KEY="+key))
		docling := model.Services["docling"]
		if len(docling.Entrypoint) != 4 || !slices.Equal(docling.Entrypoint[:2], []string{"/bin/sh", "-c"}) || docling.Entrypoint[3] != "--" {
			t.Fatal("docling must wrap the image entrypoint with a shell and an explicit $0")
		}
		if !slices.Equal(docling.Command, []string{"docling-serve", "run"}) {
			t.Fatal("docling must explicitly restore the image command")
		}
		wantEnv(t, docling, "DOCLING_SERVE_API_KEY", key)
		wantEnv(t, model.Services["gkg"], "GKG_CONVERTER_API_KEY", key)
	}
}

func TestDoclingEntrypointAPIKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy/docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Services map[string]struct {
			Entrypoint []string `yaml:"entrypoint"`
			Command    []string `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	docling := config.Services["docling"]
	if len(docling.Entrypoint) != 4 || docling.Entrypoint[0] != "/bin/sh" || docling.Entrypoint[1] != "-c" {
		t.Fatal("docling must have a shell wrapper with an explicit $0")
	}
	// Compose turns $$ into a literal $ before handing the script to /bin/sh.
	docling.Entrypoint[2] = strings.ReplaceAll(docling.Entrypoint[2], "$$", "$")
	bin := t.TempDir()
	stub := `#!/bin/sh
printf 'present=%s\nkey=%s\n' "${DOCLING_SERVE_API_KEY+x}" "${DOCLING_SERVE_API_KEY-}"
printf 'arg=%s\n' "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "container-entrypoint"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, key, present string
		set                bool
	}{
		{name: "empty", set: true},
		{name: "unset"},
		{name: "non-empty", key: `synthetic $key # "quoted"`, present: "x", set: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(slices.Clone(docling.Entrypoint[1:]), docling.Command...)
			cmd := exec.Command(docling.Entrypoint[0], args...)
			cmd.Env = []string{"PATH=" + bin}
			if tc.set {
				cmd.Env = append(cmd.Env, "DOCLING_SERVE_API_KEY="+tc.key)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("docling wrapper: %v\n%s", err, out)
			}
			want := "present=" + tc.present + "\nkey=" + tc.key + "\narg=docling-serve\narg=run\n"
			if string(out) != want {
				t.Fatalf("wrapper output = %q, want %q", out, want)
			}
		})
	}
}

func TestComposeEnvFileFollowsGKGEnvFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy/docker-compose.yml"))
	if err != nil {
		t.Fatal("read compose source")
	}
	if !strings.Contains(string(data), "${GKG_ENV_FILE:-../.env}") {
		t.Error("gkg env_file does not follow GKG_ENV_FILE")
	}
	model := renderCompose(t, writeFixture(t, "MEMGRAPH_PASSWORD=test-only", "GKG_API_KEY=synthetic-test-key", "GKG_PROBE_ONLY_IN_FILE=from-fixture"))
	wantEnv(t, model.Services["gkg"], "GKG_PROBE_ONLY_IN_FILE", "from-fixture")
}

func TestComposePortsLoopbackOnly(t *testing.T) {
	for name, service := range defaultCompose(t).Services {
		if len(service.Profiles) > 0 {
			continue
		}
		if len(service.Ports) == 0 && name != "edge-init" {
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
		lines := []string{"MEMGRAPH_PASSWORD=test-only", "GKG_API_KEY=synthetic-test-key"}
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
		if len(service.Profiles) > 0 {
			continue
		}
		if service.Restart != "unless-stopped" && !(name == "edge-init" && service.Restart == "no") {
			t.Errorf("%s restart policy missing", name)
		}
		if service.Logging.Driver != "json-file" || service.Logging.Options["max-size"] != "10m" || service.Logging.Options["max-file"] != "3" {
			t.Errorf("%s log rotation differs", name)
		}
	}
}

func TestComposeMemoryBudget(t *testing.T) {
	model := defaultCompose(t)
	if model.Services["edge"].MemLimit != 704<<20 || model.Services["memgraph"].MemLimit != 1344<<20 || !slices.Contains(model.Services["memgraph"].Command, "--memory-limit=832") {
		t.Error("provisional edge budget must keep Memgraph's 512 MiB headroom and the 14 GiB total")
	}
	floors := map[string]int64{"memgraph": 1 << 30, "gkg": 512 << 20, "sidecar": 2 << 30, "docling": 2 << 30, "edge": 128 << 20}
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
	for _, name := range []string{"memgraph", "docling", "edge-init", "edge"} {
		if !strings.Contains(model.Services[name].Image, "@sha256:") {
			t.Errorf("%s image is not digest-pinned", name)
		}
	}
}

func TestComposeLightOverride(t *testing.T) {
	for _, mode := range []string{"mock", "openai"} {
		lines := []string{"MEMGRAPH_PASSWORD=test-only", "GKG_API_KEY=synthetic-test-key"}
		if mode == "openai" {
			lines = append(lines, "GKG_LIGHT_EXTRACTOR_MODE=openai")
		}
		model := renderCompose(t, writeFixture(t, lines...), "deploy/docker-compose.yml", "deploy/compose.light.yml")
		wantServices(t, model, "memgraph", "gkg", "edge-init", "edge")
		wantDependencies(t, model.Services["gkg"], "memgraph")
		wantEnv(t, model.Services["gkg"], "GKG_EXTRACTOR_MODE", mode)
		wantEnv(t, model.Services["gkg"], "GKG_CONVERTER_MODE", "off")
	}
}

func TestComposeEdgeWiring(t *testing.T) {
	model := defaultCompose(t)
	edge, init := model.Services["edge"], model.Services["edge-init"]
	if edge.DependsOn["gkg"].Condition != "service_healthy" || edge.DependsOn["edge-init"].Condition != "service_completed_successfully" {
		t.Error("edge needs healthy gkg and completed edge-init")
	}
	for key, value := range map[string]string{
		"MODSEC_RESP_BODY_ACCESS": "Off", "ALLOWED_METHODS": "GET HEAD POST OPTIONS DELETE",
		"MODSEC_REQ_BODY_LIMIT": "22020096", "MODSEC_REQ_BODY_NOFILES_LIMIT": "4259840",
		"MODSEC_REQ_BODY_LIMIT_ACTION": "Reject", "NGINX_WORKER_PROCESSES": "2",
		"MODSEC_AUDIT_LOG_PARTS": "ADEFHZ", "PARANOIA": "1", "BLOCKING_PARANOIA": "1", "ANOMALY_INBOUND": "5",
		"GKG_API_KEY":         "synthetic-test-key",
		"GKG_AGENT_TIMEOUT_S": "60", "EDGE_ASK_TIMEOUT_S": "",
	} {
		wantEnv(t, edge, key, value)
	}
	if init.User != "0:0" || init.MemLimit != 64<<20 {
		t.Error("edge-init must be a bounded root one-shot")
	}
	mounts := map[string]bool{}
	for _, v := range edge.Volumes {
		mounts[v.Target] = v.ReadOnly
	}
	for _, target := range []string{
		"/etc/nginx/templates/conf.d/default.conf.template", "/etc/nginx/edge-certs", "/etc/nginx/edge-auth", "/var/www/acme",
		"/etc/nginx/templates/modsecurity.d/setup.conf.template",
		"/docker-entrypoint.d/15-validate-env.envsh",
		"/etc/modsecurity.d/owasp-crs/rules/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf",
		"/etc/modsecurity.d/owasp-crs/rules/RESPONSE-999-EXCLUSION-RULES-AFTER-CRS.conf",
	} {
		if !mounts[target] {
			t.Errorf("read-only mount missing: %s", target)
		}
	}
	found := false
	for _, v := range init.Volumes {
		if v.Type == "bind" && strings.HasSuffix(v.Source, "/htpasswd") {
			found = v.ReadOnly && !v.Bind.CreateHostPath
		}
	}
	if !found {
		t.Error("htpasswd bind must be read-only and refuse auto-creation")
	}
	for _, name := range []string{"edge-certs", "edge-auth", "acme-webroot", "letsencrypt"} {
		if _, ok := model.Volumes[name]; !ok {
			t.Errorf("volume missing: %s", name)
		}
	}
}

func TestComposeEdgeCheckUsesProductionLimits(t *testing.T) {
	fixture := writeFixture(t, "MEMGRAPH_PASSWORD=test-only", "GKG_API_KEY=synthetic-test-key")
	model := renderCompose(t, fixture, "deploy/docker-compose.yml", "deploy/compose.light.yml", "deploy/compose.edgecheck.yml")
	edge := model.Services["edge"]
	if edge.MemLimit != 704<<20 {
		t.Fatal("edge acceptance must use the production memory limit")
	}
	for key, value := range map[string]string{
		"MODSEC_REQ_BODY_LIMIT": "22020096", "MODSEC_REQ_BODY_NOFILES_LIMIT": "4259840",
		"MODSEC_REQ_BODY_LIMIT_ACTION": "Reject", "NGINX_WORKER_PROCESSES": "2",
	} {
		wantEnv(t, edge, key, value)
	}
}

func TestComposeEdgeBind(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "0.0.0.0"} {
		model := renderCompose(t, writeFixture(t, "MEMGRAPH_PASSWORD=test-only", "GKG_API_KEY=synthetic-test-key", "EDGE_BIND="+bind, "EDGE_HTTP_PORT=28080", "EDGE_HTTPS_PORT=28443"))
		ports := model.Services["edge"].Ports
		if len(ports) != 2 {
			t.Fatal("edge must publish exactly HTTP and HTTPS")
		}
		for _, port := range ports {
			if port.HostIP != bind || (port.Target != 8080 && port.Target != 8443) || (port.Target == 8080 && port.Published != "28080") || (port.Target == 8443 && port.Published != "28443") {
				t.Error("edge port mapping differs")
			}
		}
	}
}

func TestComposeEdgeRequiresKey(t *testing.T) {
	fixture := writeFixture(t, "MEMGRAPH_PASSWORD=test-only")
	cmd := exec.Command("docker", "compose", "--env-file", fixture, "-f", "deploy/docker-compose.yml", "config", "-q")
	cmd.Dir, cmd.Env = repoRoot(t), append(minimalEnv(), "GKG_ENV_FILE="+fixture, "EDGE_WAF_ENV="+filepath.Join(t.TempDir(), "absent.env"))
	version := exec.Command("docker", "compose", "version")
	version.Env = cmd.Env
	if version.Run() != nil {
		t.Skip("docker compose unavailable")
	}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "GKG_API_KEY") {
		t.Error("missing GKG_API_KEY must fail by variable name")
	}
}

func TestComposeCertProfile(t *testing.T) {
	model := defaultCompose(t)
	cert, ok := model.Services["certbot"]
	// config includes profiled services; they are not started by default.
	if !ok || !slices.Equal(cert.Profiles, []string{"cert"}) || !strings.Contains(cert.Image, "@sha256:") || cert.MemLimit != 256<<20 {
		t.Fatal("certbot must be pinned, bounded, and profile-only")
	}
}

func TestComposeCertInstaller(t *testing.T) {
	cert := defaultCompose(t).Services["certbot"]
	mounts := map[string]bool{}
	for _, v := range cert.Volumes {
		mounts[v.Target] = v.ReadOnly
	}
	for _, target := range []string{"/etc/letsencrypt", "/var/www/acme", "/etc/nginx/edge-certs"} {
		ro, ok := mounts[target]
		if !ok || ro {
			t.Errorf("certbot needs writable %s", target)
		}
	}
	if !mounts["/edge-install-cert.sh"] {
		t.Error("certbot installer must be mounted read-only")
	}
}
