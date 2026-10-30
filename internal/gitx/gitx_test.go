package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var ctx = context.Background()

func gitBin(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/git", "/opt/homebrew/bin/git", "/usr/local/bin/git"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no git")
	return ""
}

func repo(t *testing.T) Git {
	t.Helper()
	dir := t.TempDir()
	g := Git{Bin: gitBin(t), Dir: dir, Home: t.TempDir(), Env: []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x"}}
	if _, err := g.Run(ctx, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestHostileRepoConfigCannotRunCommands(t *testing.T) {
	g := repo(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	hooks := filepath.Join(g.Dir, ".hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"pre-commit", "commit-msg", "post-commit"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(g.Dir, ".git", "hooks", h), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fsmon := filepath.Join(g.Dir, "fsmon.sh")
	if err := os.WriteFile(fsmon, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"core.hooksPath", hooks}, {"core.fsmonitor", fsmon}} {
		if _, err := g.Run(ctx, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(g.Dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "commit", "-q", "-m", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook or fsmonitor from the repository ran")
	}
}

func TestOnlyHTTPSUnlessFileIsAllowed(t *testing.T) {
	src := repo(t)
	if err := os.WriteFile(filepath.Join(src.Dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Run(ctx, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Run(ctx, "commit", "-q", "-m", "x"); err != nil {
		t.Fatal(err)
	}
	dst := Git{Bin: src.Bin, Dir: t.TempDir(), Home: t.TempDir()}
	if _, err := dst.Run(ctx, "clone", "-q", "file://"+src.Dir, "c"); err == nil {
		t.Fatal("a file:// clone succeeded without AllowFile")
	}
	dst.AllowFile = true
	if _, err := dst.Run(ctx, "clone", "-q", "file://"+src.Dir, "c"); err != nil {
		t.Fatalf("a file:// clone with AllowFile failed: %v", err)
	}
	if _, err := dst.Run(ctx, "clone", "-q", "ext::sh -c touch% /tmp/x", "d"); err == nil {
		t.Fatal("an ext:: transport was allowed")
	}
}

func TestEnvironIsClosedAndCarriesTheConfig(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GH_TOKEN", "ghp_x")
	g := Git{Bin: "/usr/bin/git", Home: "/h", Config: [][2]string{{"http.extraHeader", "X: y"}}, Env: []string{"A=b"}}
	env := g.Environ()
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GH_TOKEN=") {
			t.Errorf("parent variable leaked: %s", kv)
		}
	}
	for _, must := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "HOME=/h", "A=b"} {
		if !slices.Contains(env, must) {
			t.Errorf("missing %s", must)
		}
	}
	joined := strings.Join(env, "\n")
	for _, must := range []string{"core.hooksPath", "/dev/null", "protocol.allow", "http.extraHeader"} {
		if !strings.Contains(joined, must) {
			t.Errorf("config misses %s", must)
		}
	}
}

func TestCommandPutsGitDirAndWorkTreeFirst(t *testing.T) {
	g := Git{Bin: "/usr/bin/git", GitDir: "/r/git", WorkTree: "/j/repo"}
	cmd := g.Command(ctx, "status")
	if !slices.Equal(cmd.Args[1:], []string{"--git-dir=/r/git", "--work-tree=/j/repo", "status"}) {
		t.Errorf("args %v", cmd.Args)
	}
}

func TestRunReportsStderrNotStdout(t *testing.T) {
	g := repo(t)
	_, err := g.Run(ctx, "rev-parse", "does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "rev-parse") {
		t.Fatalf("err %v", err)
	}
	if _, err := (Git{}).Run(ctx, "status"); err == nil {
		t.Error("Run without a binary succeeded")
	}
}

func TestIsAncestor(t *testing.T) {
	g := repo(t)
	var shas []string
	for i := range 2 {
		if err := os.WriteFile(filepath.Join(g.Dir, "a.txt"), []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Run(ctx, "add", "-A"); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Run(ctx, "commit", "-q", "-m", "x"); err != nil {
			t.Fatal(err)
		}
		s, err := g.Out(ctx, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		shas = append(shas, s)
	}
	if ok, err := g.IsAncestor(ctx, shas[0], shas[1]); !ok || err != nil {
		t.Errorf("first is not ancestor of second: %v %v", ok, err)
	}
	if ok, err := g.IsAncestor(ctx, shas[1], shas[0]); ok || err != nil {
		t.Errorf("second is ancestor of first: %v %v", ok, err)
	}
	if _, err := g.IsAncestor(ctx, strings.Repeat("0", 40), shas[0]); err == nil {
		t.Error("an unknown object gave no error")
	}
}

func TestIsSHA(t *testing.T) {
	if !IsSHA(strings.Repeat("a1", 20)) || IsSHA("abc") || IsSHA(strings.Repeat("A", 40)) || IsSHA(strings.Repeat("g", 40)) {
		t.Error("IsSHA wrong")
	}
}

func TestOutputIsBounded(t *testing.T) {
	l := &limited{max: 4}
	if n, _ := l.Write([]byte("abcdef")); n != 6 || l.buf.String() != "abcd" {
		t.Errorf("limited wrote %q", l.buf.String())
	}
}
