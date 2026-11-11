package deploy_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const awsMarker = "fixture-secret-marker"

// Re-execute this test binary as a strict subprocess double. No stub can fall
// through to a real service; unknown operations leave a failure flag as well as
// exiting nonzero (even when a script intentionally catches an error).
func init() {
	if os.Getenv("GKG_AWS_STUB") == "1" {
		awsStub()
		os.Exit(0)
	}
}
func awsStub() {
	split := 0
	for i, a := range os.Args {
		if a == "--" {
			split = i + 1
			break
		}
	}
	args := os.Args[split:]
	name := filepath.Base(args[0])
	a := args[1:]
	dir := os.Getenv("STUB_DIR")
	mode := os.Getenv("STUB_MODE")
	raw, _ := json.Marshal(append([]string{name}, a...))
	f, _ := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	fmt.Fprintln(f, string(raw))
	f.Close()
	has := func(s string) bool {
		for _, v := range a {
			if v == s {
				return true
			}
		}
		return false
	}
	val := func(s string) string {
		for i, v := range a {
			if v == s && i+1 < len(a) {
				return a[i+1]
			}
			if strings.HasPrefix(v, s+"=") {
				return strings.TrimPrefix(v, s+"=")
			}
		}
		return ""
	}
	project := os.Getenv("STUB_NAME")
	if project == "" {
		project = "gkg"
	}
	emit := func(s string) { fmt.Print(strings.ReplaceAll(s, "gkg", project)) }
	die := func(s string) { fmt.Fprintln(os.Stderr, s); os.Exit(1) }
	unexpected := func() { os.WriteFile(filepath.Join(dir, "unexpected"), raw, 0600); die("unexpected stub operation") }
	switch name {
	case "aws":
		if len(a) < 2 {
			unexpected()
		}
		op := a[0] + " " + a[1]
		switch op {
		case "sts get-caller-identity":
			if mode == "sts-fail" {
				die("SSO expired")
			}
			if mode == "wrong-account" {
				emit("wrong")
			} else {
				emit("123456789012")
			}
		case "ec2 describe-security-groups":
			if has("--filters") {
				if mode == "new-sg" {
					emit(`{"SecurityGroups":[]}`)
					break
				}
				emit(`{"SecurityGroups":[{"GroupId":"sg-example"}]}`)
			} else {
				ports := `[ {"IpProtocol":"tcp","FromPort":80,"ToPort":80,"IpRanges":[{"CidrIp":"0.0.0.0/0"}],"Ipv6Ranges":[],"PrefixListIds":[],"UserIdGroupPairs":[]}, {"IpProtocol":"tcp","FromPort":443,"ToPort":443,"IpRanges":[{"CidrIp":"0.0.0.0/0"}],"Ipv6Ranges":[],"PrefixListIds":[],"UserIdGroupPairs":[]}`
				if mode == "partial-sg" {
					if _, e := os.Stat(filepath.Join(dir, "port443")); e != nil {
						ports = strings.Split(ports, ", {\"IpProtocol\":\"tcp\",\"FromPort\":443")[0]
					}
				}
				if mode == "extra-ingress" {
					ports += `, {"IpProtocol":"tcp","FromPort":22,"ToPort":22,"IpRanges":[{"CidrIp":"0.0.0.0/0"}]}`
				}
				ports += `]`
				emit(`{"SecurityGroups":[{"GroupId":"sg-example","Tags":[{"Key":"project","Value":"gkg"}],"IpPermissions":` + ports + `}]}`)
			}
		case "ec2 create-security-group":
			emit(`{"GroupId":"sg-example"}`)
		case "ec2 describe-tags":
			if mode == "foreign" {
				emit(`{"Tags":[]}`)
			} else {
				emit(`{"Tags":[{"Key":"project","Value":"gkg"}]}`)
			}
		case "iam get-role":
			if val("--role-name") == "AWSDataLifecycleManagerDefaultRole" {
				if mode == "dlm-role-missing" {
					if _, err := os.Stat(filepath.Join(dir, "dlm-role-created")); err != nil {
						die("NoSuchEntity")
					}
				}
				if mode == "dlm-role-denied" {
					die("AccessDenied")
				}
				path := ""
				if mode == "dlm-role-console" {
					path = "service-role/"
				}
				emit(`{"Role":{"Arn":"arn:aws:iam::123456789012:role/` + path + `AWSDataLifecycleManagerDefaultRole"}}`)
				break
			}
			if mode == "foreign" {
				emit(`{"Role":{"Tags":[]}}`)
				break
			}
			emit(`{"Role":{"Tags":[{"Key":"project","Value":"gkg"}]}}`)
		case "iam get-instance-profile":
			if mode == "foreign" {
				emit(`{"InstanceProfile":{"Tags":[]}}`)
				break
			}
			if mode == "no-profile" {
				if _, err := os.Stat(filepath.Join(dir, "profile-created")); err != nil {
					die("NoSuchEntity")
				}
			}
			emit(`{"InstanceProfile":{"Roles":[{"RoleName":"gkg-ssm-role"}],"Tags":[{"Key":"project","Value":"gkg"}]}}`)
		case "iam put-role-policy":
			p, err := os.ReadFile(strings.TrimPrefix(val("--policy-document"), "file://"))
			if err != nil {
				unexpected()
			}
			os.WriteFile(filepath.Join(dir, "policy"), p, 0600)
		case "ec2 describe-addresses":
			if _, err := os.Stat(filepath.Join(dir, "released")); err == nil {
				emit(`{"Addresses":[]}`)
				break
			}
			addr := `{"AllocationId":"eipalloc-example","PublicIp":"192.0.2.10","Tags":[{"Key":"project","Value":"gkg"}]}`
			if mode == "no-eip" {
				if _, err := os.Stat(filepath.Join(dir, "allocated")); err != nil {
					emit(`{"Addresses":[]}`)
					break
				}
			}
			if mode == "eip-other" {
				addr = `{"AllocationId":"eipalloc-example","PublicIp":"192.0.2.10","AssociationId":"eipassoc-example","InstanceId":"i-foreign","NetworkInterfaceId":"eni-example"}`
			}
			if mode == "two-eips" {
				addr += "," + addr
			}
			emit(`{"Addresses":[` + addr + `]}`)
		case "ec2 allocate-address":
			os.WriteFile(filepath.Join(dir, "allocated"), nil, 0600)
			emit(`{"AllocationId":"eipalloc-example","PublicIp":"192.0.2.10"}`)
		case "ec2 describe-instances":
			if _, err := os.Stat(filepath.Join(dir, "terminated")); err == nil {
				emit(`{"Reservations":[]}`)
				break
			}
			inst := `{"InstanceId":"i-example","State":{"Name":"running"},"SourceDestCheck":true,"MetadataOptions":{"HttpTokens":"required","HttpPutResponseHopLimit":1},"SecurityGroups":[{"GroupId":"sg-example"}],"SubnetId":"subnet-example","VpcId":"vpc-0example","IamInstanceProfile":{"Arn":"arn:aws:iam::123456789012:instance-profile/gkg-ssm-profile"},"Tags":[{"Key":"project","Value":"gkg"}]}`
			if mode == "metadata-off" {
				inst = strings.Replace(inst, `"HttpTokens":"required"`, `"HttpTokens":"optional"`, 1)
			}
			if mode == "extra-sg" {
				inst = strings.Replace(inst, `"SecurityGroups":[{"GroupId":"sg-example"}]`, `"SecurityGroups":[{"GroupId":"sg-example"},{"GroupId":"sg-foreign"}]`, 1)
			}
			if mode == "wrong-profile" {
				inst = strings.Replace(inst, "instance-profile/gkg-ssm-profile", "instance-profile/foreign", 1)
			}
			if mode == "source-dest-off" {
				inst = strings.Replace(inst, `"SourceDestCheck":true`, `"SourceDestCheck":false`, 1)
			}
			if mode == "new" || mode == "launch-fail" {
				if _, err := os.Stat(filepath.Join(dir, "launched")); err != nil {
					emit(`{"Reservations":[]}`)
					break
				}
			}
			if mode == "two-instances" {
				inst += "," + inst
			}
			emit(`{"Reservations":[{"Instances":[` + inst + `]}]}`)
		case "ec2 run-instances":
			b, _ := os.ReadFile(os.Getenv("BOX_IDS_FILE"))
			if !strings.Contains(string(b), "CLIENT_TOKEN="+val("--client-token")) || val("--client-token") == "" {
				unexpected()
			}
			if mode == "launch-fail" {
				die("injected launch failure")
			}
			os.WriteFile(filepath.Join(dir, "launched"), nil, 0600)
			emit(`{"Instances":[{"InstanceId":"i-example"}]}`)
		case "ec2 wait":
			if !has("instance-running") && !has("instance-terminated") {
				unexpected()
			}
		case "ec2 terminate-instances":
			os.WriteFile(filepath.Join(dir, "terminated"), nil, 0600)
		case "ec2 release-address":
			os.WriteFile(filepath.Join(dir, "released"), nil, 0600)
		case "ssm get-parameter":
			if (mode == "env-fail" && val("--name") == "/gkg/env") || (mode == "auth-fail" && val("--name") == "/gkg/htpasswd") {
				die("injected secret fetch failure")
			}
			switch strings.ReplaceAll(val("--name"), "/"+project+"/", "/gkg/") {
			case "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64":
				emit(`{"Parameter":{"Value":"ami-example"}}`)
			case "/gkg/env":
				emit("MEMGRAPH_PASSWORD=" + awsMarker + "\nGKG_API_KEY=" + awsMarker + "\n")
			case "/gkg/htpasswd":
				emit("deploy:" + awsMarker + "\n")
			default:
				unexpected()
			}
		case "ssm put-parameter":
			p := strings.TrimPrefix(val("--cli-input-json"), "file://")
			b, e := os.ReadFile(p)
			if e != nil {
				unexpected()
			}
			info, _ := os.Stat(p)
			if info.Mode().Perm() != 0600 {
				unexpected()
			}
			var request map[string]any
			if json.Unmarshal(b, &request) != nil {
				unexpected()
			}
			name, _ := request["Name"].(string)
			os.WriteFile(filepath.Join(dir, "uploaded-"+filepath.Base(name)), b, 0600)
			if mode == "upload-fail" {
				die("injected upload failure")
			}
		case "ssm send-command":
			b, e := os.ReadFile(strings.TrimPrefix(val("--parameters"), "file://"))
			if e != nil {
				unexpected()
			}
			os.WriteFile(filepath.Join(dir, "payload"), b, 0600)
			emit(`{"Command":{"CommandId":"cmd-example"}}`)
		case "ssm describe-instance-information":
			if !strings.Contains(val("--filters"), "i-example") {
				unexpected()
			}
			n, _ := os.ReadFile(filepath.Join(dir, "ping-attempts"))
			attempts, _ := strconv.Atoi(string(n))
			attempts++
			os.WriteFile(filepath.Join(dir, "ping-attempts"), []byte(strconv.Itoa(attempts)), 0600)
			status, timestamp := "Online", time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			if mode == "ping-timeout" || attempts == 2 {
				timestamp = "2000-01-01T00:00:00+00:00"
			}
			if attempts == 1 {
				status = "ConnectionLost"
			}
			emit(`{"InstanceInformationList":[{"InstanceId":"i-example","PingStatus":"` + status + `","LastPingDateTime":"` + timestamp + `"}]}`)
			if status == "Online" && attempts >= 3 && mode != "ping-timeout" {
				os.WriteFile(filepath.Join(dir, "reconnected"), nil, 0600)
			}
		case "ssm get-command-invocation":
			if val("--command-id") != "cmd-example" || val("--instance-id") != "i-example" {
				unexpected()
			}
			if mode == "remote-fail" {
				emit(`{"Status":"Failed","ResponseCode":42,"StandardOutputContent":"","StandardErrorContent":"remote failure"}`)
			} else if mode == "invocation-delay" {
				if _, e := os.Stat(filepath.Join(dir, "polled")); e != nil {
					os.WriteFile(filepath.Join(dir, "polled"), nil, 0600)
					die("InvocationDoesNotExist")
				}
				emit(`{"Status":"Success","ResponseCode":0,"StandardOutputContent":"abc123\n","StandardErrorContent":""}`)
			} else {
				emit(`{"Status":"Success","ResponseCode":0,"StandardOutputContent":"abc123\n","StandardErrorContent":""}`)
			}
		case "dlm get-lifecycle-policies":
			if strings.HasPrefix(mode, "dlm-") || mode == "no-dlm" {
				if _, err := os.Stat(filepath.Join(dir, "dlm-created")); err != nil {
					emit(`{"Policies":[]}`)
					break
				}
			}
			emit(`{"Policies":[{"PolicyId":"policy-example","Tags":{"project":"gkg"}}]}`)
		case "dlm get-lifecycle-policy":
			if mode == "foreign" {
				emit(`{"Policy":{"Tags":{}}}`)
				break
			}
			emit(`{"Policy":{"Tags":{"project":"gkg"}}}`)
		case "dlm create-default-role":
			if val("--resource-type") != "snapshot" {
				unexpected()
			}
			os.WriteFile(filepath.Join(dir, "dlm-role-created"), nil, 0600)
		case "dlm create-lifecycle-policy":
			if mode == "dlm-role-missing" || mode == "dlm-role-unusable" || mode == "dlm-other-error" {
				n, _ := os.ReadFile(filepath.Join(dir, "dlm-attempts"))
				attempts, _ := strconv.Atoi(string(n))
				attempts++
				os.WriteFile(filepath.Join(dir, "dlm-attempts"), []byte(strconv.Itoa(attempts)), 0600)
				if mode == "dlm-other-error" {
					die("AccessDeniedException")
				}
				if mode == "dlm-role-unusable" || attempts < 3 {
					die("InvalidRequestException: The execution role is not assumable by DLM")
				}
			}
			b, e := os.ReadFile(strings.TrimPrefix(val("--policy-details"), "file://"))
			if e != nil {
				unexpected()
			}
			os.WriteFile(filepath.Join(dir, "dlm-details"), b, 0600)
			os.WriteFile(filepath.Join(dir, "dlm-created"), nil, 0600)
			emit(`{"PolicyId":"policy-example"}`)
		case "route53 get-hosted-zone":
			emit(`{"HostedZone":{"Name":"example.com."},"DelegationSet":{"NameServers":["ns-b.example.","NS-A.EXAMPLE"]}}`)
		case "route53 list-resource-record-sets":
			rr := ""
			if mode == "dns-conflict" {
				rr = `{"Name":"gkg.example.com.","Type":"A","TTL":60,"ResourceRecords":[{"Value":"192.0.2.99"}]}`
			}
			if mode == "dns-aaaa" {
				rr = `{"Name":"gkg.example.com.","Type":"AAAA","TTL":60,"ResourceRecords":[{"Value":"2001:db8::1"}]}`
			}
			emit(`{"ResourceRecordSets":[` + rr + `]}`)
		case "route53 change-resource-record-sets":
			b, e := os.ReadFile(strings.TrimPrefix(val("--change-batch"), "file://"))
			if e != nil {
				unexpected()
			}
			os.WriteFile(filepath.Join(dir, "dns-batch"), b, 0600)
		case "ec2 describe-snapshots":
			emit(`{"Snapshots":[]}`)
		case "ssm list-tags-for-resource":
			if mode == "foreign" {
				emit(`{"TagList":[]}`)
				break
			}
			emit(`{"TagList":[{"Key":"project","Value":"gkg"}]}`)
		case "ec2 authorize-security-group-ingress":
			if val("--port") == "443" {
				os.WriteFile(filepath.Join(dir, "port443"), nil, 0600)
			}
		case "iam create-instance-profile":
			os.WriteFile(filepath.Join(dir, "profile-created"), nil, 0600)
		case "ssm add-tags-to-resource", "iam attach-role-policy", "iam add-role-to-instance-profile", "iam create-role", "ec2 associate-address", "ec2 delete-security-group", "dlm delete-lifecycle-policy", "iam remove-role-from-instance-profile", "iam delete-instance-profile", "iam detach-role-policy", "iam delete-role-policy", "iam delete-role", "ssm delete-parameter", "ec2 delete-snapshot":
		default:
			unexpected()
		}
	case "dig":
		typ := ""
		for _, v := range a {
			if v == "NS" || v == "A" || v == "AAAA" {
				typ = v
			}
		}
		if mode == "dns-error" || (mode == "aaaa-error" && typ == "AAAA") {
			emit(";; ->>HEADER<<- opcode: QUERY, status: SERVFAIL, id: 1\n")
			break
		}
		emit(";; ->>HEADER<<- opcode: QUERY, status: NOERROR, id: 1\n")
		switch typ {
		case "NS":
			if mode == "wrong-ns" {
				emit("example.com. 60 IN NS ns-other.example.\n")
			} else {
				emit("example.com. 60 IN NS ns-a.example.\nexample.com. 60 IN NS ns-b.example.\n")
			}
		case "A":
			if mode == "dns-absent" {
				break
			}
			if mode == "dns-conflict" || (mode == "dns-split" && has("@dns.google")) {
				emit("gkg.example.com. 60 IN A 192.0.2.99\n")
				break
			}
			emit("gkg.example.com. 60 IN A 192.0.2.10\n")
		case "AAAA":
			if mode == "dns-aaaa" {
				emit("gkg.example.com. 60 IN AAAA 2001:db8::1\n")
			}
		default:
			unexpected()
		}
	case "openssl":
		if has("rand") {
			if has("-hex") {
				emit(strings.Repeat("a", 64))
			} else {
				emit(awsMarker)
			}
		} else {
			unexpected()
		}
	case "make":
		if has("passwd") && os.Getenv("STUB_REAL_PASSWD") == "1" {
			c := exec.Command("/usr/bin/make", a...)
			c.Env = []string{}
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "GKG_AWS_STUB=") {
					c.Env = append(c.Env, e)
				}
			}
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if e := c.Run(); e != nil {
				if ee, ok := e.(*exec.ExitError); ok {
					os.Exit(ee.ExitCode())
				}
				die("could not start make")
			}
			break
		}
		switch {
		case has("passwd"):
			emit(awsMarker + "\n")
			os.WriteFile(val("EDGE_HTPASSWD"), nil, 0600)
		case has("build"):
			if mode == "build-fail" {
				if _, e := os.Stat(filepath.Join(dir, "build-failed")); e != nil {
					os.WriteFile(filepath.Join(dir, "build-failed"), nil, 0600)
					die("build failed")
				}
			}
		case has("up"):
			if mode == "up-fail" || mode == "recovery-fail" {
				_, err := os.Stat(filepath.Join(dir, "up-failed"))
				if err != nil || mode == "recovery-fail" {
					os.WriteFile(filepath.Join(dir, "up-failed"), nil, 0600)
					die("startup failed")
				}
			}
		case has("up-cached"), has("renew"), has("cert"):
		default:
			unexpected()
		}
	case "git":
		switch {
		case has("fetch"):
			if mode == "fetch-fail" {
				die("fetch failed")
			}
		case has("rev-parse"):
			if mode == "unknown-ref" && !has("HEAD") {
				die("unknown ref")
			}
			emit("abc123\n")
		case has("checkout"):
			if mode == "checkout-fail" && !has("prev123") {
				die("checkout failed")
			}
		case has("clone"):
		default:
			unexpected()
		}
	case "curl":
		if os.Getenv("STUB_REQUIRE_RECONNECT") == "1" && strings.HasPrefix(a[len(a)-1], "https://gkg.example.com/") {
			if _, err := os.Stat(filepath.Join(dir, "reconnected")); err != nil {
				unexpected()
			}
		}
		if mode == "edge-fail" || mode == "health-fail" {
			emit("503")
			break
		}
		if has("--output") {
			path := val("--output")
			if path != "/dev/null" {
				url := a[len(a)-1]
				body := filepath.Base(url)
				if mode == "acme-body-bad" {
					body = "wrong-body"
				}
				os.WriteFile(path, []byte(body), 0600)
			}
		}
		emit("200")
	case "docker":
		if has("version") {
			if has("compose") {
				emit("5.7.0\n")
			} else if mode == "engine-wrong" {
				emit("25.0.18\n")
			} else {
				emit("29.9.3\n")
			}
		} else if has("htpasswd") {
			b, e := os.ReadFile("/dev/stdin")
			if e != nil {
				unexpected()
			}
			os.WriteFile(filepath.Join(dir, "bcrypt-stdin"), b, 0600)
			emit("deploy:$2y$synthetic-hash\n")
		} else if has("printenv") && has("GKG_API_KEY") {
			emit(awsMarker + "\n")
		} else if has("certbot") {
		} else {
			unexpected()
		}
	case "sleep":
	case "fallocate":
		os.WriteFile(a[len(a)-1], nil, 0600)
	case "systemctl":
		if mode == "unit-fail" && has("daemon-reload") {
			if _, e := os.Stat(filepath.Join(dir, "unit-failed")); e != nil {
				os.WriteFile(filepath.Join(dir, "unit-failed"), nil, 0600)
				die("injected unit install failure")
			}
		}
	case "rpm":
		if len(a) != 2 || a[0] != "-q" {
			unexpected()
		}
		if mode == "rpm-"+a[1] {
			break
		}
		os.Exit(1)
	case "tar":
		target := val("-C")
		if target == "" {
			unexpected()
		}
		os.MkdirAll(filepath.Join(target, "docker"), 0700)
		for _, binary := range []string{"docker", "dockerd", "containerd", "containerd-shim-runc-v2", "ctr", "docker-init", "docker-proxy", "runc"} {
			os.WriteFile(filepath.Join(target, "docker", binary), []byte("fixture-"+binary), 0755)
		}
	case "install":
		if len(a) != 4 || a[0] != "-m" {
			unexpected()
		}
		b, err := os.ReadFile(a[2])
		if err != nil {
			unexpected()
		}
		perm, _ := strconv.ParseUint(a[1], 8, 32)
		if os.WriteFile(a[3], b, os.FileMode(perm)) != nil {
			unexpected()
		}
	case "dnf", "swapon", "mkswap", "reboot", "groupadd", "modprobe", "sysctl", "systemd-run":
	case "sha256sum":
		b, e := os.ReadFile("/dev/stdin")
		if e != nil {
			unexpected()
		}
		input := string(b)
		kind := ""
		for k, digest := range map[string]string{"engine": "3fb5d1f13014c18cc54945bc177c58a9056a482dada031e17d12cf2e6922c973", "compose": "7758048ad38e580f22559603312874fec8bb638d021a9e64182385fc17cc12a2", "buildx": "7992ec09413b80ff57ba4e081f56fc833d7bac1d5e71143c6c809e7e6065f6d6"} {
			if strings.Contains(input, digest) {
				kind = k
			}
		}
		if kind == "" {
			unexpected()
		}
		if mode == "checksum-fail" || mode == "checksum-"+kind {
			die("checksum mismatch")
		}
		emit("checksum: OK\n")
	default:
		unexpected()
	}
}

type awsHarness struct {
	t              *testing.T
	dir, root, bin string
	env            []string
}

func newAWS(t *testing.T) *awsHarness {
	t.Helper()
	h := &awsHarness{t: t, dir: t.TempDir()}
	h.root = filepath.Join(h.dir, "repo")
	h.bin = filepath.Join(h.dir, "bin")
	os.MkdirAll(filepath.Join(h.root, "deploy/aws/box"), 0700)
	os.MkdirAll(h.bin, 0700)
	for _, n := range []string{"bash", "awk", "cat", "chmod", "cmp", "dirname", "date", "grep", "head", "jq", "mkdir", "mktemp", "mv", "rm", "sed", "sort", "tr", "wc"} {
		path, err := exec.LookPath(n)
		if err != nil {
			t.Fatalf("required local utility %s missing", n)
		}
		if err := os.Symlink(path, filepath.Join(h.bin, n)); err != nil {
			t.Fatal(err)
		}
	}
	// Copy only task scripts, never any local configuration or secret file.
	sourceRoot := repoRoot(t)
	if baseline := os.Getenv("GKG_AWS_TEST_SOURCE"); baseline != "" {
		sourceRoot = baseline
	}
	files, _ := filepath.Glob(filepath.Join(sourceRoot, "deploy/aws/*.sh"))
	boxes, _ := filepath.Glob(filepath.Join(sourceRoot, "deploy/aws/box/*"))
	nested, _ := filepath.Glob(filepath.Join(sourceRoot, "deploy/aws/box/systemd/*"))
	boxes = append(boxes, nested...)
	files = append(files, boxes...)
	files = append(files, filepath.Join(sourceRoot, "Makefile"), filepath.Join(sourceRoot, "scripts/edge-passwd.sh"))
	os.MkdirAll(filepath.Join(h.root, "scripts"), 0700)
	for _, p := range files {
		info, _ := os.Stat(p)
		if info != nil && info.IsDir() {
			continue
		}
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		rel, _ := filepath.Rel(sourceRoot, p)
		os.MkdirAll(filepath.Dir(filepath.Join(h.root, rel)), 0700)
		os.WriteFile(filepath.Join(h.root, rel), b, 0700)
	}
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	stub := "#!/bin/bash\nexport GKG_AWS_STUB=1\nexec \"$AWS_TEST_BINARY\" -- \"$0\" \"$@\"\n"
	for _, n := range []string{"aws", "docker", "dig", "curl", "make", "openssl", "git", "sleep", "dnf", "systemctl", "install", "swapon", "mkswap", "fallocate", "sha256sum", "reboot", "go", "rpm", "tar", "groupadd", "modprobe", "sysctl", "systemd-run"} {
		os.WriteFile(filepath.Join(h.bin, n), []byte(stub), 0700)
	}
	config := "AWS_PROFILE=example\nAWS_REGION=eu-west-1\nEXPECTED_ACCOUNT=123456789012\nVPC_ID=vpc-0example\nSUBNET_ID=subnet-example\nNAME=gkg\nHOSTNAME=gkg.example.com\nHOSTED_ZONE_ID=ZEXAMPLE\nINSTANCE_TYPE=t3a.xlarge\nDISK_GB=100\nREPO_URL=https://github.com/example/gkg.git\n"
	h.write("box.config", config)
	h.write("netrc", "machine gkg.example.com login deploy password "+awsMarker+"\n")
	h.env = []string{"PATH=" + h.bin, "HOME=" + h.dir, "AWS_CONFIG_FILE=" + filepath.Join(h.dir, "aws-config"), "AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(h.dir, "aws-credentials"), "DOCKER_HOST=unix:///nonexistent/docker.sock", "AWS_TEST_BINARY=" + binary, "STUB_DIR=" + h.dir, "BOX_ENV_FILE=" + filepath.Join(h.dir, "box.config"), "BOX_IDS_FILE=" + filepath.Join(h.dir, "ids"), "BOX_SECRETS_FILE=" + filepath.Join(h.dir, "secrets.env"), "BOX_HTPASSWD=" + filepath.Join(h.dir, "htpasswd"), "BOX_NETRC=" + filepath.Join(h.dir, "netrc"), "SOURCE_ENV=" + filepath.Join(h.dir, "source.env"), "GKG_ROOT=" + h.root, "GKG_ENV_FILE=" + filepath.Join(h.dir, "runtime.env"), "GKG_SYSTEMD_DIR=" + filepath.Join(h.dir, "units"), "GKG_DOCKER_DIR=" + filepath.Join(h.dir, "docker-config"), "GKG_PLUGIN_DIR=" + filepath.Join(h.dir, "plugins"), "GKG_SWAP_FILE=" + filepath.Join(h.dir, "swap"), "GKG_DOCKER_BIN_DIR=" + filepath.Join(h.dir, "docker-bin"), "GKG_MODULES_DIR=" + filepath.Join(h.dir, "modules"), "GKG_SYSCTL_DIR=" + filepath.Join(h.dir, "sysctl"), "GKG_SSM_PREFIX=/gkg", "TMPDIR=" + h.dir, "BOX_CHECK_TIMEOUT=5", "REPO_URL=https://github.com/example/gkg.git"}
	return h
}
func (h *awsHarness) write(name, s string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(s), 0600); err != nil {
		h.t.Fatal(err)
	}
}
func (h *awsHarness) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.dir, name))
	return string(b)
}
func (h *awsHarness) mode(s string) { h.env = append(h.env, "STUB_MODE="+s) }
func (h *awsHarness) run(script string, args ...string) (string, int) {
	h.t.Helper()
	if _, err := os.Stat(filepath.Join(h.root, "deploy/aws", script)); err != nil {
		h.t.Fatalf("%s missing", script)
	}
	c := exec.Command("/bin/bash", append([]string{filepath.Join(h.root, "deploy/aws", script)}, args...)...)
	c.Dir = h.root
	c.Env = h.env
	b, e := c.CombinedOutput()
	code := 0
	if e != nil {
		if ee, ok := e.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			h.t.Fatal(e)
		}
	}
	if h.read("unexpected") != "" {
		h.t.Fatalf("unexpected subprocess: %s", h.read("unexpected"))
	}
	if strings.Contains(string(b), awsMarker) || strings.Contains(h.read("calls"), awsMarker) || strings.Contains(h.read("payload"), awsMarker) {
		h.t.Fatal("secret exposed in output, argv or SSM payload")
	}
	return string(b), code
}
func (h *awsHarness) ok(script string, args ...string) string {
	h.t.Helper()
	out, code := h.run(script, args...)
	if code != 0 {
		h.t.Fatalf("%s exit=%d: %s", script, code, out)
	}
	return out
}
func (h *awsHarness) calls() [][]string {
	var a [][]string
	for _, l := range strings.Split(strings.TrimSpace(h.read("calls")), "\n") {
		var v []string
		if json.Unmarshal([]byte(l), &v) == nil {
			a = append(a, v)
		}
	}
	return a
}
func (h *awsHarness) count(op string) int {
	n := 0
	for _, a := range h.calls() {
		if strings.Contains(strings.Join(a, " "), op) {
			n++
		}
	}
	return n
}
func (h *awsHarness) private(name string) {
	h.t.Helper()
	st, e := os.Stat(filepath.Join(h.dir, name))
	if e != nil || st.Mode().Perm() != 0600 {
		h.t.Fatalf("%s must exist with mode 0600", name)
	}
}
func (h *awsHarness) ids() {
	h.write("ids", "SG_ID=sg-example\nEIP_ALLOC=eipalloc-example\nEIP_IP=192.0.2.10\nROLE=gkg-ssm-role\nPROFILE=gkg-ssm-profile\nINSTANCE_ID=i-example\nDLM_POLICY_ID=policy-example\n")
}

func TestAWSAccountGuard(t *testing.T) {
	for _, mode := range []string{"wrong-account", "sts-fail"} {
		h := newAWS(t)
		h.mode(mode)
		for _, s := range []string{"provision.sh", "dns.sh", "upload-secrets.sh", "remote.sh", "teardown.sh"} {
			h.write("calls", "")
			h.ids()
			out, code := h.run(s, "deploy.sh", "abc123")
			if code == 0 || !strings.Contains(out, "account") || len(h.calls()) != 1 || h.calls()[0][1] != "sts" {
				t.Fatalf("%s must refuse at account assertion before writes", s)
			}
		}
	}
}
func TestAWSDataConfig(t *testing.T) {
	h := newAWS(t)
	h.write("box.config", h.read("box.config")+"UNKNOWN=$(touch forbidden)\n")
	_, code := h.run("provision.sh")
	if code == 0 || len(h.calls()) != 0 {
		t.Fatal("unknown data keys must refuse before subprocesses")
	}
	h.write("box.config", "AWS_REGION=x\nAWS_REGION=y\n")
	_, code = h.run("provision.sh")
	if code == 0 {
		t.Fatal("duplicate keys accepted")
	}
}
func TestAWSProvisionConverges(t *testing.T) {
	for _, mode := range []string{"new", "no-eip", "no-profile", "no-dlm", "partial-sg", ""} {
		t.Run(mode, func(t *testing.T) {
			h := newAWS(t)
			h.mode(mode)
			h.ok("provision.sh")
			if strings.HasPrefix(mode, "dlm-") || mode == "no-dlm" {
				var d struct {
					PolicyType    string
					ResourceTypes []string
					TargetTags    []struct{ Key, Value string }
					Schedules     []struct {
						CreateRule struct {
							Interval     int
							IntervalUnit string
						}
						RetainRule struct{ Count int }
					}
				}
				if json.Unmarshal([]byte(h.read("dlm-details")), &d) != nil || d.PolicyType != "EBS_SNAPSHOT_MANAGEMENT" || !reflect.DeepEqual(d.ResourceTypes, []string{"VOLUME"}) || len(d.TargetTags) != 1 || d.TargetTags[0].Key != "gkg-backup" || d.TargetTags[0].Value != "daily" || len(d.Schedules) != 1 || d.Schedules[0].CreateRule.Interval != 24 || d.Schedules[0].CreateRule.IntervalUnit != "HOURS" || d.Schedules[0].RetainRule.Count != 7 {
					t.Fatal("DLM must snapshot tagged volumes daily and retain seven")
				}
			}
			h.ok("provision.sh")
			if h.count("run-instances") > 1 || h.count("allocate-address") > 1 {
				t.Fatal("rerun duplicated resources")
			}
			if mode == "no-profile" && h.count("create-instance-profile") != 1 {
				t.Fatal("profile creation did not converge")
			}
			if mode == "new" && h.count("run-instances") != 1 {
				t.Fatal("missing launch")
			}
			h.private("ids")
			os.Remove(filepath.Join(h.dir, "ids"))
			h.ok("provision.sh")
			if h.count("run-instances") > 1 {
				t.Fatal("lost ids duplicated instance")
			}
		})
	}
}
func TestAWSProvisionSafety(t *testing.T) {
	for _, mode := range []string{"metadata-off", "extra-sg", "wrong-profile", "extra-ingress", "two-eips", "two-instances", "source-dest-off", "eip-other"} {
		h := newAWS(t)
		h.mode(mode)
		_, code := h.run("provision.sh")
		if code == 0 || h.count("run-instances") != 0 || h.count("revoke-security") != 0 {
			t.Fatalf("unsafe %s accepted or mutated instance", mode)
		}
	}
}
func TestAWSIAMAndLaunch(t *testing.T) {
	h := newAWS(t)
	h.mode("new")
	h.ok("provision.sh")
	var p struct {
		Statement []struct {
			Effect                string
			Action                any
			Resource, NotResource []string
		}
	}
	if e := json.Unmarshal([]byte(h.read("policy")), &p); e != nil {
		t.Fatal("invalid IAM policy")
	}
	wantARNs := []string{"arn:aws:ssm:eu-west-1:123456789012:parameter/gkg/env", "arn:aws:ssm:eu-west-1:123456789012:parameter/gkg/htpasswd"}
	if len(p.Statement) != 2 || !reflect.DeepEqual(p.Statement[0].Resource, wantARNs) || p.Statement[0].Effect != "Allow" || p.Statement[1].Effect != "Deny" || !reflect.DeepEqual(p.Statement[0].Resource, p.Statement[1].NotResource) || len(p.Statement[0].Resource) != 2 || !strings.Contains(fmt.Sprint(p.Statement[1].Action), "ssm:GetParameter*") {
		t.Fatal("two-parameter Allow and explicit NotResource Deny required")
	}
	for _, a := range h.calls() {
		s := strings.Join(a, " ")
		if strings.Contains(s, "run-instances") {
			for _, v := range []string{"--client-token", "HttpTokens=required", "HttpPutResponseHopLimit=1", "gp3", "Encrypted", "gkg-backup", "daily"} {
				if !strings.Contains(s, v) {
					t.Fatalf("launch omits %s", v)
				}
			}
			if strings.Contains(s, "--key-name") {
				t.Fatal("SSH key supplied")
			}
		}
	}
}
func TestAWSLaunchTokenSurvivesFailure(t *testing.T) {
	h := newAWS(t)
	h.mode("launch-fail")
	_, code := h.run("provision.sh")
	if code == 0 || !strings.Contains(h.read("ids"), "CLIENT_TOKEN=") {
		t.Fatal("token not persisted before failed launch")
	}
	token := h.read("ids")
	h.mode("new")
	h.ok("provision.sh")
	if !strings.Contains(h.read("ids"), strings.Split(strings.Split(token, "CLIENT_TOKEN=")[1], "\n")[0]) {
		t.Fatal("client token changed")
	}
}
func TestAWSDNSGuards(t *testing.T) {
	for _, mode := range []string{"dns-conflict", "dns-aaaa", "wrong-ns", "dns-error", "aaaa-error"} {
		h := newAWS(t)
		h.ids()
		h.mode(mode)
		_, code := h.run("dns.sh")
		if code == 0 || h.count("change-resource-record-sets") != 0 {
			t.Fatalf("unsafe DNS %s wrote", mode)
		}
	}
	h := newAWS(t)
	h.ids()
	h.ok("dns.sh")
	var b struct {
		Changes []struct {
			Action            string
			ResourceRecordSet struct{ Name, Type string }
		}
	}
	json.Unmarshal([]byte(h.read("dns-batch")), &b)
	if len(b.Changes) != 1 || b.Changes[0].ResourceRecordSet.Name != "gkg.example.com." || b.Changes[0].ResourceRecordSet.Type != "A" {
		t.Fatal("DNS batch must contain only the chosen A record")
	}
	h = newAWS(t)
	h.ids()
	h.mode("dns-conflict")
	h.env = append(h.env, "REPLACE_A=1")
	h.ok("dns.sh")
}
func TestAWSACMEProbe(t *testing.T) {
	bad := newAWS(t)
	bad.ids()
	bad.mode("acme-body-bad")
	_, code := bad.run("dns.sh", "--check")
	if code == 0 || bad.count("ssm send-command") != 2 || !strings.Contains(bad.read("payload"), "--remove") {
		t.Fatal("failed ACME probe did not remove the nonce")
	}
	h := newAWS(t)
	h.ids()
	h.ok("dns.sh", "--check")
	if h.count("ssm send-command") != 2 || h.count("curl") != 1 {
		t.Fatal("external probe requires write, exact fetch and cleanup")
	}
	h.ok("box/acme-probe.sh", "nonce123")
	h.ok("box/acme-probe.sh", "nonce123", "--remove")
	if h.count("certbot") != 2 {
		t.Fatal("ACME writes must use certbot webroot")
	}
}
func TestAWSSecretEnvPreservation(t *testing.T) {
	h := newAWS(t)
	source := "GKG_AGENT_API_KEY='" + awsMarker + " $ a # = \"'\nGKG_EMBEDDER_BASE_URL=https://example.com/$literal\nEMBED_API_KEY='" + awsMarker + "'\nGKG_AGENT_ENABLED=false\nIGNORED_KEY=unused\n"
	h.write("source.env", source)
	h.ok("make-box-env.sh")
	h.private("secrets.env")
	b := h.read("secrets.env")
	for _, l := range strings.Split(source, "\n")[:3] {
		if !strings.Contains(b, l+"\n") {
			t.Fatal("provider line changed")
		}
	}
	if strings.Contains(b, "IGNORED_KEY") || !strings.Contains(b, "GKG_AGENT_ENABLED=true") {
		t.Fatal("allowlist or enabled override wrong")
	}
	h.write("secrets.env", strings.Replace(b, "EDGE_HSTS_MAX_AGE=0", "EDGE_HSTS_MAX_AGE=31536000", 1))
	h.ok("make-box-env.sh")
	if !strings.Contains(h.read("secrets.env"), "EDGE_HSTS_MAX_AGE=31536000") || h.count("openssl rand") != 2 {
		t.Fatal("rotation changed stable secrets or HSTS")
	}
}
func TestAWSComposeEffectiveValues(t *testing.T) {
	h := newAWS(t)
	h.write("source.env", "GKG_AGENT_API_KEY='a $ b # = \" c'\nGKG_JUDGE_API_KEY=unquoted$$literal\nGKG_EMBEDDER_API_KEY=\"double ' quote\"\nMEMGRAPH_PASSWORD=test-only\nGKG_API_KEY=synthetic-test-key\n")
	h.ok("make-box-env.sh")
	// Discover availability before isolating HOME: Desktop installs Compose there.
	available := exec.Command("docker", "compose", "version")
	available.Env = append(os.Environ(), "COMPOSE_DISABLE_ENV_FILE=1")
	if available.Run() != nil {
		t.Skip("docker compose unavailable")
	}
	// Compose config never uses a daemon.
	configDir := os.Getenv("DOCKER_CONFIG")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		configDir = filepath.Join(home, ".docker")
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.dir, "DOCKER_CONFIG=" + configDir, "DOCKER_HOST=unix:///nonexistent/docker.sock", "COMPOSE_DISABLE_ENV_FILE=1", "EDGE_WAF_ENV=" + filepath.Join(h.dir, "absent")}
	version := exec.Command("docker", "compose", "version")
	version.Env = env
	if version.Run() != nil {
		t.Fatal("installed Compose became unavailable under isolated HOME")
	}
	render := func(file string) composeModel {
		c := exec.Command("docker", "compose", "--env-file", file, "-f", "deploy/docker-compose.yml", "config", "--format", "json")
		c.Dir = repoRoot(t)
		c.Env = append(env, "GKG_ENV_FILE="+file)
		b, e := c.Output()
		if e != nil {
			t.Fatal("compose config failed (values withheld)")
		}
		var m composeModel
		if json.Unmarshal(b, &m) != nil {
			t.Fatal("invalid compose JSON")
		}
		return m
	}
	a, b := render(filepath.Join(h.dir, "source.env")), render(filepath.Join(h.dir, "secrets.env"))
	for _, k := range []string{"GKG_AGENT_API_KEY", "GKG_JUDGE_API_KEY", "GKG_EMBEDDER_API_KEY"} {
		if !reflect.DeepEqual(a.Services["gkg"].Environment[k], b.Services["gkg"].Environment[k]) {
			t.Fatalf("effective %s differs", k)
		}
	}
}
func TestAWSDeployLogin(t *testing.T) {
	for _, real := range []string{"0", "1"} {
		h := newAWS(t)
		h.env = append(h.env, "STUB_REAL_PASSWD="+real)
		out := h.ok("deploy-login.sh")
		if out != "" {
			t.Fatal("login printed output")
		}
		h.private("netrc")
		if !strings.Contains(h.read("netrc"), "machine gkg.example.com login deploy password "+awsMarker) {
			t.Fatal("password not captured into netrc")
		}
		if real == "1" {
			h.private("htpasswd")
			if h.read("bcrypt-stdin") != awsMarker+"\n" {
				t.Fatal("bcrypt did not receive generated password on stdin")
			}
		}
	}
}
func TestAWSSecretUpload(t *testing.T) {
	for _, mode := range []string{"", "upload-fail"} {
		h := newAWS(t)
		h.mode(mode)
		h.write("secrets.env", "# comment\n\nMEMGRAPH_PASSWORD="+awsMarker+"\nGKG_API_KEY="+awsMarker+"\n")
		h.write("htpasswd", "deploy:"+awsMarker+"\n")
		_, code := h.run("upload-secrets.sh")
		if (code == 0) != (mode == "") {
			t.Fatal("upload failure not propagated")
		}
		if h.count("put-parameter") == 0 {
			t.Fatal("no upload")
		}
		var request struct {
			Name, Type, Value, Tier string
			Overwrite               bool
		}
		if json.Unmarshal([]byte(h.read("uploaded-env")), &request) != nil || request.Value != "MEMGRAPH_PASSWORD="+awsMarker+"\nGKG_API_KEY="+awsMarker+"\n" || request.Name != "/gkg/env" || request.Type != "SecureString" || request.Tier != "Standard" || !request.Overwrite {
			t.Fatal("uploaded environment changed or not a Standard SecureString")
		}

		for _, a := range h.calls() {
			for _, v := range a {
				if strings.HasPrefix(v, "file://") {
					if _, e := os.Stat(strings.TrimPrefix(v, "file://")); !os.IsNotExist(e) {
						t.Fatal("secret temp retained")
					}
				}
			}
		}
	}
	for _, s := range []string{"GKG_API_KEY=x\n", "GKG_API_KEY=x\nMEMGRAPH_PASSWORD=x\nPAD=" + strings.Repeat("x", 4096)} {
		h := newAWS(t)
		h.write("secrets.env", s)
		h.write("htpasswd", "hash\n")
		_, code := h.run("upload-secrets.sh")
		if code == 0 || h.count("put-parameter") != 0 {
			t.Fatal("invalid secret bundle uploaded")
		}
	}
}
func TestAWSFetchSecrets(t *testing.T) {
	for _, mode := range []string{"env-fail", "auth-fail"} {
		h := newAWS(t)
		h.mode(mode)
		h.write("runtime.env", "old-env\n")
		os.MkdirAll(filepath.Join(h.root, "deploy/edge"), 0700)
		os.WriteFile(filepath.Join(h.root, "deploy/edge/htpasswd"), []byte("old-auth\n"), 0600)
		_, code := h.run("box/fetch-secrets.sh")
		b, _ := os.ReadFile(filepath.Join(h.root, "deploy/edge/htpasswd"))
		if code == 0 || h.read("runtime.env") != "old-env\n" || string(b) != "old-auth\n" {
			t.Fatal("failed secret fetch replaced live files")
		}
	}
	h := newAWS(t)
	h.ok("box/fetch-secrets.sh")
	h.private("runtime.env")
	b, e := os.ReadFile(filepath.Join(h.root, "deploy/edge/htpasswd"))
	if e != nil || !strings.Contains(string(b), awsMarker) {
		t.Fatal("htpasswd not installed")
	}
	if h.count("get-parameter") != 2 {
		t.Fatal("must read exactly two parameters")
	}
}
func TestAWSRemoteProtocol(t *testing.T) {
	for _, script := range []string{"deploy.sh", "bootstrap.sh", "acme-probe.sh"} {
		h := newAWS(t)
		h.ids()
		h.mode("invocation-delay")
		h.ok("remote.sh", script, "abc123")
		var p map[string][]string
		if json.Unmarshal([]byte(h.read("payload")), &p) != nil || len(p["commands"]) != 1 {
			t.Fatal("remote requires one command")
		}
		cmd := p["commands"][0]
		if !strings.Contains(cmd, "flock /run/gkg-ops.lock bash /opt/gkg/deploy/aws/box/"+script) {
			t.Fatal("remote missing host lock")
		}
		want := "900"
		if script == "bootstrap.sh" {
			want = "5400"
		}
		if script == "deploy.sh" {
			want = "3600"
		}
		if !reflect.DeepEqual(p["executionTimeout"], []string{want}) || h.count("--timeout-seconds 600") != 1 {
			t.Fatal("remote timeout wrong")
		}
	}
	h := newAWS(t)
	h.ids()
	h.mode("remote-fail")
	_, code := h.run("remote.sh", "deploy.sh", "abc123")
	if code != 42 {
		t.Fatal("remote response code not propagated")
	}
	h = newAWS(t)
	h.ids()
	_, code = h.run("remote.sh", "deploy.sh", "bad;echo")
	if code == 0 || h.count("send-command") != 0 {
		t.Fatal("unsafe remote argument accepted")
	}
}
func TestAWSDeployRollback(t *testing.T) {
	for _, mode := range []string{"fetch-fail", "unknown-ref", "checkout-fail", "build-fail"} {
		h := newAWS(t)
		h.mode(mode)
		os.WriteFile(filepath.Join(h.root, ".deployed-sha"), []byte("prev123\n"), 0600)
		_, code := h.run("box/deploy.sh", "next123")
		if code == 0 || h.count("make up") != 0 {
			t.Fatalf("%s started stack or succeeded", mode)
		}
		b, _ := os.ReadFile(filepath.Join(h.root, ".deployed-sha"))
		if string(b) != "prev123\n" {
			t.Fatal("failed deploy changed boot SHA")
		}
		if h.count("checkout --detach prev123") == 0 {
			t.Fatal("failed deploy did not restore previous checkout")
		}
		if mode == "build-fail" && h.count("make build") != 2 {
			t.Fatal("build failure did not restore revision and image tags")
		}
	}
}
func TestAWSDeployHealthGate(t *testing.T) {
	h := newAWS(t)
	os.WriteFile(filepath.Join(h.root, ".deployed-sha"), []byte("prev123\n"), 0600)
	h.ok("box/deploy.sh", "next123")
	if h.count("get-parameter") != 2 || h.count("-H @") == 0 {
		t.Fatal("deploy did not refresh secrets and use a header file")
	}
	h = newAWS(t)
	h.ids()
	h.mode("edge-fail")
	// Bash SECONDS can tick immediately; allow a full edge-check iteration.
	h.env = append(h.env, "BOX_CHECK_TIMEOUT=2")
	out, code := h.run("remote.sh", "deploy.sh", "abc123")
	if code == 0 || strings.Contains(out, "abc123") {
		t.Fatal("failed through-edge check printed deployed SHA")
	}
	for _, s := range []string{"/healthz", "/v1/agent/status", "/v1/documents", "--netrc-file"} {
		if h.count(s) == 0 {
			t.Fatalf("failed through-edge check missing %s", s)
		}
	}
	h = newAWS(t)
	h.ids()
	h.ok("remote.sh", "deploy.sh", "abc123")
	for _, s := range []string{"/healthz", "/v1/agent/status", "/v1/documents", "--netrc-file"} {
		if h.count(s) == 0 {
			t.Fatalf("through-edge check missing %s", s)
		}
	}
}
func TestAWSBootstrapAndUnits(t *testing.T) {
	partial := newAWS(t)
	partial.mode("unit-fail")
	_, failureCode := partial.run("box/bootstrap.sh", "abc123")
	if failureCode == 0 {
		t.Fatal("unit install failure ignored")
	}
	if _, e := os.Stat(filepath.Join(partial.root, ".deployed-sha")); !os.IsNotExist(e) {
		t.Fatal("incomplete bootstrap finalized its deployed marker")
	}
	partial.ok("box/bootstrap.sh", "abc123")
	bad := newAWS(t)
	bad.mode("checksum-fail")
	_, code := bad.run("box/bootstrap.sh", "abc123")
	if code == 0 || bad.count("install -m") != 0 || bad.count("make up") != 0 {
		t.Fatal("checksum failure installed plugins or started stack")
	}
	h := newAWS(t)
	h.ok("box/bootstrap.sh", "abc123")
	if h.count("sha256sum") != 3 || h.count("make up") != 1 {
		t.Fatal("bootstrap missing verified plugins or startup")
	}
	units := map[string][]string{"gkg-stack.service": {"Requires=docker.service", "After=docker.service network-online.target", "Wants=network-online.target", "up-cached", "TimeoutStartSec=900", "Restart=on-failure", "StartLimitBurst="}, "gkg-renew.service": {"flock /run/gkg-ops.lock", "make -C /opt/gkg renew"}, "gkg-renew.timer": {"RandomizedDelaySec=3600", "OnCalendar=", "Persistent=true"}}
	for file, parts := range units {
		b, e := os.ReadFile(filepath.Join(h.root, "deploy/aws/box", file))
		if e != nil {
			t.Fatalf("missing %s", file)
		}
		for _, p := range parts {
			if !strings.Contains(string(b), p) {
				t.Fatalf("%s missing %s", file, p)
			}
		}
	}
}
func TestAWSTeardownSafety(t *testing.T) {
	foreign := newAWS(t)
	foreign.ids()
	foreign.write("ids", strings.Replace(foreign.read("ids"), "INSTANCE_ID=i-example\n", "", 1))
	foreign.mode("foreign")
	foreign.env = append(foreign.env, "YES=1", "PURGE=1")
	foreign.ok("teardown.sh")
	for _, op := range []string{"release-address", "delete-security-group", "delete-lifecycle-policy", "delete-role", "delete-instance-profile", "ssm delete-parameter"} {
		if foreign.count(op) != 0 {
			t.Fatalf("foreign resource reached %s", op)
		}
	}
	purge := newAWS(t)
	purge.ids()
	purge.env = append(purge.env, "YES=1", "PURGE=1", "PURGE_SNAPSHOTS=1")
	purge.ok("teardown.sh")
	for _, op := range []string{"delete-lifecycle-policy", "delete-role", "delete-instance-profile", "ssm delete-parameter", "describe-snapshots"} {
		if purge.count(op) == 0 {
			t.Fatalf("purge missing %s", op)
		}
	}
	h := newAWS(t)
	h.ids()
	_, code := h.run("teardown.sh")
	if code == 0 || h.count("terminate-instances") != 0 {
		t.Fatal("teardown needs YES")
	}
	h = newAWS(t)
	h.ids()
	h.mode("foreign")
	h.env = append(h.env, "YES=1")
	h.ok("teardown.sh")
	if h.count("terminate-instances") != 0 || h.count("release-address") != 0 {
		t.Fatal("foreign resource deleted")
	}
	h = newAWS(t)
	h.ids()
	h.env = append(h.env, "YES=1")
	h.ok("teardown.sh")
	h.ok("teardown.sh")
	s := h.read("calls")
	term, wait, release := strings.Index(s, "terminate-instances"), strings.Index(s, "instance-terminated"), strings.Index(s, "release-address")
	if term < 0 || wait < term || release < wait {
		t.Fatal("termination wait must precede EIP release")
	}
	if h.count("delete-snapshot") != 0 {
		t.Fatal("snapshots deleted by default")
	}
}
func TestAWSMakeCachedAndGuards(t *testing.T) {
	for _, args := range [][]string{{"box-provision"}, {"box-dns"}, {"box-env"}, {"box-login"}, {"box-secrets"}, {"box-bootstrap", "REF=abc123"}, {"deploy", "REF=abc123"}, {"box-update"}, {"box-docker"}, {"box-dns-check"}, {"box-teardown", "YES=1", "PURGE=1"}} {
		h := newAWS(t)
		h.ids()
		h.write("source.env", "GKG_AGENT_API_KEY='"+awsMarker+"'\n")
		h.write("secrets.env", "GKG_API_KEY="+awsMarker+"\nMEMGRAPH_PASSWORD="+awsMarker+"\n")
		h.write("htpasswd", "deploy:hash\n")
		c := exec.Command("/usr/bin/make", append([]string{"--no-print-directory", "-s", "ENV_FILE=" + filepath.Join(h.dir, "source.env")}, args...)...)
		c.Dir = h.root
		c.Env = h.env
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("Make target %s failed: %s", args[0], b)
		}
		if h.read("unexpected") != "" {
			t.Fatalf("unexpected subprocess in %s", args[0])
		}
		if strings.Contains(string(b), awsMarker) || strings.Contains(h.read("calls"), awsMarker) {
			t.Fatal("make exposed a secret")
		}
	}
	for _, target := range []string{"box-bootstrap", "deploy", "box-teardown"} {
		h := newAWS(t)
		h.ids()
		c := exec.Command("/usr/bin/make", "-s", target)
		c.Dir = h.root
		c.Env = append(h.env, "REF=abc123", "YES=1", "PURGE=1")
		if c.Run() == nil {
			t.Fatalf("%s accepted inherited consent/ref", target)
		}
		if h.count("send-command") != 0 || h.count("terminate-instances") != 0 {
			t.Fatal("origin guard allowed a write")
		}
	}
	_, calls, code := runMake(t, stubOpts{}, "up-cached")
	requireMakeOK(t, code)
	if !strings.Contains(oneCall(t, calls), "up -d --wait --no-build --pull never --remove-orphans") {
		t.Fatal("boot must use cached images")
	}
	data, _ := os.ReadFile(filepath.Join(repoRoot(t), "deploy/docker-compose.yml"))
	var model struct {
		Services map[string]struct {
			NetworkMode string `yaml:"network_mode"`
			PID         string `yaml:"pid"`
		}
	}
	if yaml.Unmarshal(data, &model) != nil {
		t.Fatal("invalid compose YAML")
	}
	for _, s := range model.Services {
		if s.NetworkMode == "host" || s.PID == "host" {
			t.Fatal("host namespaces bypass IMDS hop limit")
		}
	}
	for _, file := range []string{".gitignore", ".dockerignore"} {
		b, _ := os.ReadFile(filepath.Join(repoRoot(t), file))
		for _, p := range []string{"deploy/aws/box.htpasswd", "deploy/aws/box.netrc"} {
			if !strings.Contains(string(b), p) {
				t.Fatalf("%s omits %s", file, p)
			}
		}
	}
}
func TestAWSEdgeGoOverride(t *testing.T) {
	h := newAWS(t)
	stub := filepath.Join(h.bin, "custom-go")
	os.WriteFile(stub, []byte("#!/bin/bash\nprintf invoked > \"$STUB_DIR/go-called\"\nexit 77\n"), 0700)
	c := exec.Command("/bin/bash", "scripts/edge-check.sh")
	c.Dir = repoRoot(t)
	c.Env = append(h.env, "EDGECHECK_GO="+stub)
	c.CombinedOutput()
	if h.read("go-called") == "" {
		t.Fatal("edge-check did not invoke EDGECHECK_GO")
	}
}

func TestAWSNames(t *testing.T) {
	for _, name := range []string{"gkg", "box-42"} {
		t.Run(name, func(t *testing.T) {
			h := newAWS(t)
			h.write("box.config", strings.Replace(h.read("box.config"), "NAME=gkg\n", "NAME="+name+"\n", 1))
			h.env = append(h.env, "STUB_NAME="+name, "GKG_SSM_PREFIX=/"+name)
			for _, mode := range []string{"new", "no-dlm", "new-sg"} {
				h.mode(mode)
				h.ok("provision.sh")
			}
			for _, want := range []string{"Name=tag:project,Values=" + name, "Name=group-name,Values=" + name + "-sg", "--role-name " + name + "-ssm-role", "--instance-profile-name " + name + "-ssm-profile", "--policy-name " + name + "-secrets", "Key=project,Value=" + name, "Key=Name,Value=" + name, "Key=" + name + "-backup,Value=daily", "--tags project=" + name + ",Name=" + name} {
				if h.count(want) == 0 {
					t.Fatalf("missing derived name: %s", want)
				}
			}
			if !strings.Contains(h.read("policy"), "parameter/"+name+"/env") || !strings.Contains(h.read("dlm-details"), `"Key": "`+name+`-backup"`) {
				t.Fatal("policy paths/tags do not derive from NAME")
			}
			h.write("secrets.env", "MEMGRAPH_PASSWORD="+awsMarker+"\nGKG_API_KEY="+awsMarker+"\n")
			h.write("htpasswd", "deploy:hash\n")
			h.ok("upload-secrets.sh")
			if !strings.Contains(h.read("uploaded-env"), `"Name": "/`+name+`/env"`) {
				t.Fatal("upload name not derived")
			}
			h.ok("remote.sh", "deploy.sh", "abc123")
			if !strings.Contains(h.read("payload"), "GKG_SSM_PREFIX='/"+name+"'") {
				t.Fatal("remote prefix not exported")
			}
			h.ok("box/fetch-secrets.sh")
			if h.count("--name /"+name+"/env") != 1 || h.count("--name /"+name+"/htpasswd") != 1 {
				t.Fatal("box ignored SSM prefix")
			}
			h.env = append(h.env, "YES=1", "PURGE=1")
			h.ok("teardown.sh")
			if h.count("iam delete-role-policy --role-name "+name+"-ssm-role --policy-name "+name+"-secrets") != 1 || h.count("ssm delete-parameter --name /"+name+"/env") != 1 {
				t.Fatal("purge ignored NAME")
			}
		})
	}
	for _, name := range []string{"g", "Gkg", "-gkg", "gkg_2", "gkg/other", strings.Repeat("a", 32)} {
		t.Run("invalid-"+name, func(t *testing.T) {
			h := newAWS(t)
			h.write("box.config", strings.Replace(h.read("box.config"), "NAME=gkg\n", "NAME="+name+"\n", 1))
			_, code := h.run("provision.sh")
			if code == 0 || len(h.calls()) != 0 {
				t.Fatal("invalid NAME reached AWS")
			}
		})
	}
}
func TestAWSSSMPrefixGuard(t *testing.T) {
	for _, prefix := range []string{"", "/g", "/Gkg", "/gkg/other", "gkg", "/gkg;echo"} {
		t.Run(prefix, func(t *testing.T) {
			h := newAWS(t)
			h.env = append(h.env, "GKG_SSM_PREFIX="+prefix)
			_, code := h.run("box/fetch-secrets.sh")
			if code == 0 || len(h.calls()) != 0 {
				t.Fatal("missing/invalid prefix reached AWS")
			}
		})
	}
}
func TestAWSManualDNS(t *testing.T) {
	for _, mode := range []string{"dns-absent", "", "dns-conflict", "dns-split", "dns-aaaa", "dns-error", "aaaa-error"} {
		t.Run(mode, func(t *testing.T) {
			h := newAWS(t)
			h.ids()
			h.mode(mode)
			h.write("box.config", strings.Replace(h.read("box.config"), "HOSTED_ZONE_ID=ZEXAMPLE", "HOSTED_ZONE_ID=", 1))
			out, code := h.run("dns.sh")
			if h.count("route53") != 0 || h.count("send-command") != 0 {
				t.Fatal("manual DNS reached an AWS write")
			}
			switch mode {
			case "dns-absent":
				if code != 3 {
					t.Fatalf("absent manual DNS exit=%d, want 3: %s", code, out)
				}
				for _, part := range []string{"gkg.example.com", "Type: A", "192.0.2.10", "TTL: 600", "add it at the domain's DNS provider, then run make box-dns-check"} {
					if strings.Count(out, part) != 1 {
						t.Fatalf("instruction block missing or duplicates %q", part)
					}
				}
			case "":
				if code != 0 || !strings.Contains(out, "already published") {
					t.Fatal("matching manual DNS not accepted")
				}
			default:
				if code == 0 || code == 3 {
					t.Fatal("unsafe manual DNS accepted or instructed")
				}
			}
			if mode == "dns-absent" || mode == "" {
				for _, resolver := range []string{"@one.one.one.one", "@dns.google"} {
					if h.count(resolver) != 2 {
						t.Fatal("both A and AAAA must be checked via both resolvers")
					}
				}
			}
		})
	}
	h := newAWS(t)
	h.ids()
	h.env = append(h.env, "YES=1")
	h.write("box.config", strings.Replace(h.read("box.config"), "HOSTED_ZONE_ID=ZEXAMPLE", "HOSTED_ZONE_ID=", 1))
	out := h.ok("teardown.sh")
	if h.count("route53") != 0 || !strings.Contains(out, "delete the manual") {
		t.Fatal("manual teardown must skip Route 53 and remind operator")
	}
	h = newAWS(t)
	h.ids()
	h.write("box.config", strings.Replace(h.read("box.config"), "HOSTED_ZONE_ID=ZEXAMPLE", "HOSTED_ZONE_ID=", 1))
	h.ok("dns.sh", "--check")
	if h.count("send-command") != 2 {
		t.Fatal("manual check lost ACME probe")
	}
	h = newAWS(t)
	h.write("box.config", strings.Replace(h.read("box.config"), "AWS_REGION=eu-west-1", "AWS_REGION=", 1))
	_, code := h.run("provision.sh")
	if code == 0 || len(h.calls()) != 0 {
		t.Fatal("empty non-zone configuration accepted")
	}
}
func TestAWSDLMDefaultRole(t *testing.T) {
	for _, mode := range []string{"no-dlm", "dlm-role-console", "dlm-role-missing", "dlm-role-unusable", "dlm-other-error", "dlm-role-denied"} {
		t.Run(mode, func(t *testing.T) {
			h := newAWS(t)
			h.mode(mode)
			_, code := h.run("provision.sh")
			success := mode == "no-dlm" || mode == "dlm-role-missing" || mode == "dlm-role-console"
			if (code == 0) != success {
				t.Fatalf("DLM result exit=%d", code)
			}
			if h.count("iam get-role --role-name AWSDataLifecycleManagerDefaultRole") != 1 {
				t.Fatal("default role not inspected")
			}
			wantCreate := 0
			if mode == "dlm-role-missing" {
				wantCreate = 1
			}
			if h.count("dlm create-default-role --resource-type snapshot") != wantCreate {
				t.Fatal("default role creation wrong")
			}
			attempts := map[string]int{"no-dlm": 1, "dlm-role-console": 1, "dlm-role-missing": 3, "dlm-role-unusable": 12, "dlm-other-error": 1, "dlm-role-denied": 0}[mode]
			if h.count("dlm create-lifecycle-policy") != attempts {
				t.Fatal("unbounded retry or unrelated error retried")
			}
			if attempts > 0 {
				path := ""
				if mode == "dlm-role-console" {
					path = "service-role/"
				}
				if h.count("--execution-role-arn arn:aws:iam::123456789012:role/"+path+"AWSDataLifecycleManagerDefaultRole") != attempts {
					t.Fatal("DLM ignored IAM role ARN/path")
				}
			}
			sleeps := 0
			if mode == "dlm-role-missing" {
				sleeps = 2
			}
			if mode == "dlm-role-unusable" {
				sleeps = 11
			}
			if h.count("sleep 5") != sleeps {
				t.Fatal("DLM retry delay wrong")
			}
		})
	}
}
func TestAWSInstallDocker(t *testing.T) {
	for _, pkg := range []string{"docker", "docker-ce", "containerd", "containerd.io", "runc"} {
		t.Run("rpm-"+pkg, func(t *testing.T) {
			h := newAWS(t)
			h.mode("rpm-" + pkg)
			_, code := h.run("box/install-docker.sh")
			if code == 0 || h.count("dnf") != 0 || h.count("curl") != 0 || h.count("systemctl") != 0 {
				t.Fatal("installed RPM did not prevent mixed runtimes")
			}
		})
	}
	for _, kind := range []string{"engine", "compose", "buildx"} {
		t.Run("checksum-"+kind, func(t *testing.T) {
			h := newAWS(t)
			h.mode("checksum-" + kind)
			_, code := h.run("box/install-docker.sh")
			if code == 0 || h.count("install -m") != 0 || h.count("systemctl") != 0 {
				t.Fatal("unverified Docker artifacts installed")
			}
		})
	}
	t.Run("idempotent", func(t *testing.T) {
		h := newAWS(t)
		h.ok("box/install-docker.sh")
		for _, p := range []string{"docker", "dockerd", "containerd", "containerd-shim-runc-v2", "ctr", "docker-init", "docker-proxy", "runc"} {
			if h.read("docker-bin/"+p) != "fixture-"+p {
				t.Fatalf("matched binary %s not installed", p)
			}
		}
		for _, want := range []string{"docker-29.9.3.tgz", "/v5.7.0/docker-compose-linux-x86_64", "/v0.38.2/buildx-v0.38.2.linux-amd64", "dnf install -y git make tar gzip kmod shadow-utils iptables-nft nftables", "groupadd -f -r docker", "modprobe overlay", "modprobe br_netfilter", "sysctl --system", "systemctl daemon-reload", "systemctl enable --now containerd docker"} {
			if h.count(want) != 1 {
				t.Fatalf("installer missing %s", want)
			}
		}
		if h.read("modules/docker.conf") != "overlay\nbr_netfilter\n" || !strings.Contains(h.read("sysctl/99-docker.conf"), "net.ipv4.ip_forward=1") {
			t.Fatal("kernel networking configuration missing")
		}
		var daemon map[string]any
		if json.Unmarshal([]byte(h.read("docker-config/daemon.json")), &daemon) != nil || daemon["log-driver"] != "json-file" {
			t.Fatal("log driver missing")
		}
		for _, unit := range []string{"docker.service", "docker.socket", "containerd.service"} {
			b := h.read("units/" + unit)
			if !strings.Contains(b, "https://raw.githubusercontent.com/") || !strings.Contains(b, "Apache-2.0") {
				t.Fatal("unit missing provenance/license")
			}
			if unit == "containerd.service" && !strings.Contains(b, "ExecStart=/usr/bin/containerd") {
				t.Fatal("containerd path wrong")
			}
		}
		calls := h.read("calls")
		if strings.LastIndex(calls, `["sha256sum"`) > strings.Index(calls, `["install"`) {
			t.Fatal("installed before verifying all downloads")
		}
		restarts, installs := h.count("systemctl restart docker"), h.count("install -m")
		h.ok("box/install-docker.sh")
		if h.count("systemctl restart docker") != restarts || h.count("install -m") != installs {
			t.Fatal("unchanged installer restarted or reinstalled Docker")
		}
		os.WriteFile(filepath.Join(h.dir, "docker-config/daemon.json"), []byte("{}\n"), 0600)
		h.ok("box/install-docker.sh")
		if h.count("systemctl restart docker") != restarts+1 {
			t.Fatal("changed daemon config did not restart Docker")
		}
		os.WriteFile(filepath.Join(h.dir, "docker-bin/dockerd"), []byte("old"), 0755)
		h.ok("box/install-docker.sh")
		if h.count("systemctl restart docker") != restarts+2 {
			t.Fatal("changed engine did not restart Docker")
		}
	})
	t.Run("version-assertion", func(t *testing.T) {
		h := newAWS(t)
		h.mode("engine-wrong")
		_, code := h.run("box/install-docker.sh")
		if code == 0 {
			t.Fatal("wrong server version accepted")
		}
	})
	t.Run("remote-operation", func(t *testing.T) {
		h := newAWS(t)
		h.ids()
		h.ok("remote.sh", "--docker")
		if !strings.Contains(h.read("payload"), "flock /run/gkg-ops.lock bash /opt/gkg/deploy/aws/box/install-docker.sh") {
			t.Fatal("remote Docker install lacks lock")
		}
	})
}
func TestAWSDeployStartupRollback(t *testing.T) {
	for _, mode := range []string{"up-fail", "health-fail", "recovery-fail"} {
		t.Run(mode, func(t *testing.T) {
			h := newAWS(t)
			h.mode(mode)
			os.WriteFile(filepath.Join(h.root, ".deployed-sha"), []byte("prev123\n"), 0600)
			out, code := h.run("box/deploy.sh", "next123")
			if code == 0 || h.count("checkout --detach prev123") != 1 || h.count("make build") != 2 || h.count("make up") != 2 {
				t.Fatal("startup/health failure did not restore checkout, build and stack")
			}
			marker, _ := os.ReadFile(filepath.Join(h.root, ".deployed-sha"))
			if string(marker) != "prev123\n" {
				t.Fatal("failed deploy changed deployed SHA")
			}
			want := "previous revision restored"
			if mode == "recovery-fail" {
				want = "recovery required"
			}
			if !strings.Contains(out, want) {
				t.Fatal("recovery result not reported")
			}
		})
	}
}
func TestAWSUpdateReconnect(t *testing.T) {
	for _, mode := range []string{"", "ping-timeout", "edge-fail"} {
		t.Run(mode, func(t *testing.T) {
			h := newAWS(t)
			h.ids()
			h.mode(mode)
			// Successful polling must not race Bash's wall-clock SECONDS ticks.
			// A two-second failure deadline leaves at least one full second for
			// the offline and stale-ping polls; sleep is stubbed throughout.
			updateTimeout := "30"
			if mode == "ping-timeout" {
				updateTimeout = "2"
			}
			h.env = append(h.env, "STUB_REQUIRE_RECONNECT=1", "BOX_UPDATE_TIMEOUT="+updateTimeout)
			if mode == "edge-fail" {
				h.env = append(h.env, "BOX_CHECK_TIMEOUT=2")
			}
			out, code := h.run("remote.sh", "--update")
			if (code == 0) != (mode == "") {
				t.Fatalf("update result exit=%d: %s", code, out)
			}
			var p map[string][]string
			json.Unmarshal([]byte(h.read("payload")), &p)
			cmd := strings.Join(p["commands"], " ")
			if !strings.Contains(cmd, "dnf upgrade --releasever=latest -y") || !strings.Contains(cmd, "systemd-run --on-active=15 /usr/bin/systemctl reboot") || strings.Contains(cmd, "&& reboot") {
				t.Fatal("update reboots inside SSM invocation")
			}
			if h.count("describe-instance-information") < 3 {
				t.Fatal("update accepted offline or stale ping")
			}
			if mode == "ping-timeout" {
				if !strings.Contains(out, "SSM did not reconnect with a fresh ping") {
					t.Fatalf("update failed before reconnect timeout: %s", out)
				}
				if h.count("curl") != 0 {
					t.Fatal("edge checked before reconnection")
				}
				return
			}
			for _, path := range []string{"/healthz", "/v1/agent/status", "/v1/documents", "--netrc-file"} {
				if h.count(path) == 0 {
					t.Fatalf("update missing edge check %s", path)
				}
			}
		})
	}
}
