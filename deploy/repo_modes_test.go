package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type modeFixture struct {
	data string
	mode os.FileMode
}

// Build an index, then materialize it with the same private umask as an old
// deploy script. All Git writes are confined to this temporary fixture repo.
func privateCheckout(t *testing.T, files map[string]modeFixture) string {
	t.Helper()
	dir := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet")
	for path, file := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(file.data), file.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	git("-c", "core.filemode=true", "add", "--all")
	for path := range files {
		if err := os.Remove(filepath.Join(dir, path)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/bash", "-c", "umask 077; exec git checkout-index --all --force")
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private checkout: %v\n%s", err, out)
	}
	return dir
}

func wantMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != want || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("%q mode = %s, want %04o", path, st.Mode(), want)
	}
}

func TestRepoModes(t *testing.T) {
	files := map[string]modeFixture{
		".gitignore":  {"ignored.env\nprivate/\n", 0644},
		"read me.txt": {"public", 0644},
		"dir with spaces\nand newline/file\tname\n": {"template", 0644},
		"exec file.sh":       {"#!/bin/sh\n", 0755},
		"missing.txt":        {"deleted", 0644},
		"replaced":           {"regular in index", 0644},
		"redirect/child.txt": {"regular in index", 0644},
	}
	dir := privateCheckout(t, files)
	wantMode(t, filepath.Join(dir, "read me.txt"), 0600)
	wantMode(t, filepath.Join(dir, "exec file.sh"), 0700)
	outside := t.TempDir()
	// t.TempDir subdirectories follow the process umask; pin the mode under test.
	if err := os.Chmod(outside, 0700); err != nil {
		t.Fatal(err)
	}
	privateFile := filepath.Join(outside, "child.txt")
	if err := os.WriteFile(privateFile, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	// Track a symlink and a gitlink without ever checking out their targets.
	if err := os.Symlink(privateFile, filepath.Join(dir, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "tracked-link"},
		{"update-index", "--add", "--cacheinfo", "160000," + strings.Repeat("a", 40) + ",submodule"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("index special paths: %v\n%s", err, out)
		}
	}
	for _, path := range []string{"missing.txt", "replaced", "redirect"} {
		if err := os.RemoveAll(filepath.Join(dir, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(privateFile, filepath.Join(dir, "replaced")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "redirect")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"untracked", "ignored.env", "private/secret", "deploy/aws/box.netrc", "submodule/untracked"} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "chmod-calls")
	chmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/bash\nprintf 'chmod\\n' >>\"$CHMOD_LOG\"\nexec \"$REAL_CHMOD\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "chmod"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		cmd := exec.Command("/bin/bash", filepath.Join(repoRoot(t), "scripts/repo-modes.sh"))
		// Also prove invocation from a subdirectory repairs the entire work tree.
		cmd.Dir = filepath.Join(dir, "private")
		cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "CHMOD_LOG=" + log, "REAL_CHMOD=" + chmod}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("repo-modes: %v\n%s", err, out)
		}
	}
	run()
	for path, file := range files {
		if path == "missing.txt" || path == "replaced" || path == "redirect/child.txt" {
			continue
		}
		wantMode(t, filepath.Join(dir, path), file.mode)
		for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
			wantMode(t, filepath.Join(dir, parent), 0755)
			if parent == "." {
				break
			}
		}
	}
	for _, path := range []string{"untracked", "ignored.env", "private/secret", "deploy/aws/box.netrc", "submodule/untracked"} {
		wantMode(t, filepath.Join(dir, path), 0600)
	}
	for _, path := range []string{"private", "deploy", "deploy/aws", "submodule"} {
		wantMode(t, filepath.Join(dir, path), 0700)
	}
	wantMode(t, privateFile, 0600)
	wantMode(t, outside, 0700)
	before, err := os.ReadFile(log)
	if err != nil || len(before) == 0 {
		t.Fatal("mode repair must call chmod on private checkout paths")
	}
	run()
	after, err := os.ReadFile(log)
	if err != nil || string(after) != string(before) {
		t.Fatal("already-correct modes must not call chmod")
	}
}

func TestRepoModesOutsideWorktree(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", filepath.Join(repoRoot(t), "scripts/repo-modes.sh"))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repo-modes outside Git: %v\n%s", err, out)
	}
	wantMode(t, dir, 0700)
}
