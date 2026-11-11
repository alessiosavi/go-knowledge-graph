package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func certificatePython(t *testing.T) string {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		python := filepath.Join(dir, "python3")
		if exec.Command(python, "-c", "import cryptography").Run() == nil {
			return python
		}
	}
	t.Skip("Python cryptography unavailable (provided by the certbot image in production)")
	return ""
}

func TestEdgeCertificateBundleAtomicInstall(t *testing.T) {
	for _, scenario := range []string{"install", "interrupted", "unchanged", "mismatched"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(certificatePython(t), "-c", certificateInstallHarness)
			cmd.Env = append(os.Environ(), "TEST_DIR="+t.TempDir(), "TEST_CASE="+scenario, "TEST_INSTALLER="+filepath.Join(repoRoot(t), "deploy/edge/install-cert.sh"))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("bundle installer: %v\n%s", err, output)
			}
		})
	}
}

const certificateInstallHarness = `
import datetime
import os
import sys
from pathlib import Path
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID

root = Path(os.environ['TEST_DIR'])
live, dest = root / 'live', root / 'certs'
(live / 'fixture.invalid').mkdir(parents=True)
dest.mkdir()
def pair(serial):
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, 'fixture.invalid')])
    now = datetime.datetime.now(datetime.timezone.utc)
    cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name)
            .public_key(key.public_key()).serial_number(serial)
            .not_valid_before(now).not_valid_after(now + datetime.timedelta(days=365))
            .sign(key, hashes.SHA256()))
    return (cert.public_bytes(serialization.Encoding.PEM),
            key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
def public(key):
    return key.public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
def matching(data):
    cert = x509.load_pem_x509_certificate(data)
    key = serialization.load_pem_private_key(data, password=None)
    assert public(cert.public_key()) == public(key.public_key()), 'certificate/key mismatch'

old_cert, old_key = pair(1)
new_cert, new_key = pair(2)
current = dest / 'current.pem'
previous = old_cert + old_key
current.write_bytes(previous)
current.chmod(0o640)
scenario = os.environ['TEST_CASE']
cert, key = (old_cert, old_key) if scenario == 'unchanged' else (new_cert, new_key)
if scenario == 'mismatched':
    key = old_key
(live / 'fixture.invalid' / 'fullchain.pem').write_bytes(cert)
(live / 'fixture.invalid' / 'privkey.pem').write_bytes(key)
# Legacy files must not determine whether the combined generation is current.
(dest / 'current.crt').write_bytes(old_cert)
(dest / 'current.key').write_bytes(old_key)
source = Path(os.environ['TEST_INSTALLER']).read_text().split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
source = source.replace('/etc/letsencrypt/live', str(live)).replace('/etc/nginx/edge-certs', str(dest))
real_replace = os.replace
publications = []
owners = []
def replace(source, target):
    assert Path(target) == current, 'TLS generation must be published by one current.pem replacement'
    matching(Path(source).read_bytes())
    assert Path(source).stat().st_mode & 0o777 == 0o640, 'private bundle permissions'
    publications.append(Path(target))
    if scenario == 'interrupted':
        raise OSError('injected failure after write, before replace')
    real_replace(source, target)
def chown(fd, uid, gid):
    owners.append((uid, gid))
    assert (uid, gid) == (0, 101), 'bundle owner/group'
os.replace, os.fchown = replace, chown
sys.argv = ['installer', 'fixture.invalid']
status = 0
try:
    exec(compile(source, 'installer', 'exec'), {})
except SystemExit as error:
    status = error.code
if scenario == 'install':
    assert status == 0 and publications == [current] and owners == [(0, 101)], 'single bundle publication'
    assert current.read_bytes() == new_cert + new_key, 'new generation was not installed'
elif scenario == 'interrupted':
    assert status == 1 and publications == [current], 'failure injection did not reach publication'
    assert current.read_bytes() == previous, 'failed install changed the previous generation'
elif scenario == 'unchanged':
    assert status == 0 and publications == [], 'idempotent install rewrote the generation'
    assert current.read_bytes() == previous
else:
    assert status == 1 and publications == [], 'mismatched live pair was accepted'
    assert current.read_bytes() == previous
matching(current.read_bytes())
assert not list(dest.glob('.install-*')), 'pending bundle leaked'
`

func TestEdgeInitIgnoresLegacyCertificatePair(t *testing.T) {
	for _, state := range []string{"missing", "empty", "existing"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			certs := filepath.Join(dir, "certs")
			if err := os.Mkdir(certs, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"current.crt", "current.key"} {
				if err := os.WriteFile(filepath.Join(certs, name), []byte("broken legacy pair"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			bundle := filepath.Join(certs, "current.pem")
			if state != "missing" {
				data := []byte{}
				if state == "existing" {
					data = []byte("preserved existing bundle")
				}
				if err := os.WriteFile(bundle, data, 0640); err != nil {
					t.Fatal(err)
				}
			}
			auth := filepath.Join(dir, "fixture-auth")
			if err := os.WriteFile(auth, []byte("fixture:synthetic\n"), 0600); err != nil {
				t.Fatal(err)
			}
			script := edgeFile(t, "deploy/edge/init.sh")
			for from, to := range map[string]string{"/etc/nginx/edge-certs": certs, "/etc/nginx/edge-auth": filepath.Join(dir, "auth"), "/var/www/acme": filepath.Join(dir, "webroot"), "/run/edge/htpasswd": auth} {
				script = strings.ReplaceAll(script, from, to)
			}
			cmd := exec.Command("sh", "-c", "chown() { [ \"$1\" = 0:101 ]; }\n"+script)
			cmd.Env = append(os.Environ(), "SERVER_NAME=localhost")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolated init: %v\n%s", err, output)
			}
			data, err := os.ReadFile(bundle)
			if err != nil || len(data) == 0 {
				t.Fatal("legacy files prevented combined placeholder creation")
			}
			if state == "existing" {
				if string(data) != "preserved existing bundle" {
					t.Error("init replaced an existing combined certificate")
				}
			} else {
				cmd := exec.Command("openssl", "x509", "-in", bundle, "-noout", "-pubkey")
				cert, certErr := cmd.Output()
				cmd = exec.Command("openssl", "pkey", "-in", bundle, "-pubout")
				key, keyErr := cmd.Output()
				if certErr != nil || keyErr != nil || string(cert) != string(key) {
					t.Error("placeholder must contain a matching certificate and key")
				}
			}
			info, err := os.Stat(bundle)
			if err != nil || info.Mode().Perm() != 0640 {
				t.Error("combined PEM must have private permissions")
			}
			for _, name := range []string{"current.crt", "current.key"} {
				if _, err := os.Stat(filepath.Join(certs, name)); !os.IsNotExist(err) {
					t.Error("init must remove obsolete certificate/key files")
				}
			}
		})
	}
}

func TestEdgeTLSUsesCombinedPEM(t *testing.T) {
	template := edgeFile(t, "deploy/edge/default.conf.template")
	for _, directive := range []string{"ssl_certificate /etc/nginx/edge-certs/current.pem;", "ssl_certificate_key /etc/nginx/edge-certs/current.pem;"} {
		if !strings.Contains(template, directive) {
			t.Errorf("missing combined PEM directive: %s", directive)
		}
	}
	for _, name := range []string{"scripts/edge-renew.sh", "scripts/edge-check.sh"} {
		script := edgeFile(t, name)
		for _, legacy := range []string{"current.crt", "current.key"} {
			if strings.Contains(script, "-in /etc/nginx/edge-certs/"+legacy) || strings.Contains(script, "edge:/etc/nginx/edge-certs/"+legacy) {
				t.Errorf("%s still reads a legacy certificate generation", name)
			}
		}
	}
}
