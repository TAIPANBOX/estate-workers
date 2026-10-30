package diffcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// changesOf builds a real repository, applies edit, and returns the parsed
// changes exactly as the orchestrator will see them.
func changesOf(t *testing.T, edit func(dir string)) []Change {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "services/qryx.md", "one\ntwo\n")
	write(t, dir, "README.md", "readme\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(string(git(t, dir, "rev-parse", "HEAD")))
	edit(dir)
	git(t, dir, "add", "-A")
	raw := git(t, dir, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev", base)
	num := git(t, dir, "diff", "--cached", "--numstat", "-z", "--no-renames", base)
	ch, err := ParseRaw(raw)
	if err != nil {
		t.Fatalf("ParseRaw: %v", err)
	}
	if err := MergeNumstat(ch, num); err != nil {
		t.Fatalf("MergeNumstat: %v", err)
	}
	return ch
}

var rules = Rules{Allowed: []string{"services/qryx.md", "README.md"}, MaxFiles: 2, MaxAddedLines: 50}

func TestAnAllowedEditPasses(t *testing.T) {
	ch := changesOf(t, func(d string) {
		write(t, d, "services/qryx.md", "one\ntwo\nthree\n")
		write(t, d, "README.md", "readme v2\n")
	})
	if v := Check(ch, rules); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
	if len(ch) != 2 || ch[0].Added+ch[1].Added != 2 {
		t.Errorf("changes %+v", ch)
	}
}

func TestEmptyDiffIsAViolation(t *testing.T) {
	if v := Check(nil, rules); len(v) == 0 {
		t.Error("an empty diff passed")
	}
}

func TestPathOutsideTheAllowListIsRefused(t *testing.T) {
	ch := changesOf(t, func(d string) { write(t, d, "services/other.md", "x\n") })
	if v := Check(ch, rules); len(v) == 0 {
		t.Error("services/other.md passed")
	}
}

func TestForbiddenPathsAreRefusedEvenIfAllowed(t *testing.T) {
	wide := Rules{Allowed: []string{"**/*.md", "**/*.yml", "**/*.json", "**/*.pem", "**/*"}, MaxFiles: 10, MaxAddedLines: 50}
	for _, p := range []string{".github/workflows/ci.yml", ".claude/settings.json", "sub/.mcp.json", "keys/x.pem", ".secretscan-allow", "a/.env.local"} {
		ch := changesOf(t, func(d string) { write(t, d, p, "x\n") })
		if v := Check(ch, wide); len(v) == 0 {
			t.Errorf("%s passed", p)
		}
	}
}

func TestSymlinksAreRefused(t *testing.T) {
	ch := changesOf(t, func(d string) {
		if err := os.Remove(filepath.Join(d, "README.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/passwd", filepath.Join(d, "README.md")); err != nil {
			t.Fatal(err)
		}
	})
	if v := Check(ch, rules); len(v) == 0 {
		t.Error("a symlink passed")
	}
}

func TestDeletionIsRefused(t *testing.T) {
	ch := changesOf(t, func(d string) {
		if err := os.Remove(filepath.Join(d, "README.md")); err != nil {
			t.Fatal(err)
		}
	})
	if v := Check(ch, rules); len(v) == 0 {
		t.Error("a deletion passed")
	}
}

func TestBinaryAndModeChangesAreRefused(t *testing.T) {
	ch := changesOf(t, func(d string) { write(t, d, "README.md", "a\x00b\n") })
	if v := Check(ch, rules); len(v) == 0 {
		t.Error("a binary file passed")
	}
	ch = changesOf(t, func(d string) {
		if err := os.Chmod(filepath.Join(d, "README.md"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	if v := Check(ch, rules); len(v) == 0 {
		t.Error("an executable bit passed")
	}
}

func TestSizeLimitsAreEnforced(t *testing.T) {
	ch := changesOf(t, func(d string) { write(t, d, "README.md", strings.Repeat("line\n", 60)) })
	if v := Check(ch, rules); len(v) == 0 {
		t.Error("60 added lines passed a 50 line limit")
	}
	tight := rules
	tight.MaxFiles = 1
	ch = changesOf(t, func(d string) {
		write(t, d, "services/qryx.md", "x\n")
		write(t, d, "README.md", "y\n")
	})
	if v := Check(ch, tight); len(v) == 0 {
		t.Error("two files passed a one file limit")
	}
}

func TestParseRawRefusesHostileInput(t *testing.T) {
	for _, raw := range []string{
		"garbage\x00",
		":100644 100644 aaa bbb M\x00",                  // header with no path
		":100644 100644 aaa M\x00x\x00",                 // too few fields
		":100644 100644 aaa bbb MM\x00x\x00",            // status too long
		"100644 100644 aaa bbb M\x00x\x00",              // no colon
		":100644 100644 aaa bbb M\x00/etc/passwd\x00",   // absolute path
		":100644 100644 aaa bbb M\x00a/../../b\x00",     // climbing path
		":100644 100644 aaa bbb M\x00a\nb\x00",          // control character
		":100644 100644 aaa bbb R100\x00old\x00new\x00", // rename slipped through
	} {
		if _, err := ParseRaw([]byte(raw)); err == nil {
			t.Errorf("ParseRaw(%q) accepted", raw)
		}
	}
	if ch, err := ParseRaw(nil); err != nil || len(ch) != 0 {
		t.Errorf("empty raw: %v %v", ch, err)
	}
}

func TestMergeNumstatRefusesUnknownPathsAndJunk(t *testing.T) {
	ch := []Change{{Path: "README.md", Status: 'M', OldMode: "100644", NewMode: "100644"}}
	for _, num := range []string{"1\t0\tother.md\x00", "x\t0\tREADME.md\x00", "1\t0\x00", "1\ty\tREADME.md\x00"} {
		c := append([]Change(nil), ch...)
		if err := MergeNumstat(c, []byte(num)); err == nil {
			t.Errorf("MergeNumstat(%q) accepted", num)
		}
	}
	c := append([]Change(nil), ch...)
	if err := MergeNumstat(c, []byte("-\t-\tREADME.md\x00")); err != nil || !c[0].Binary {
		t.Errorf("binary numstat: err=%v change=%+v", err, c[0])
	}
}
