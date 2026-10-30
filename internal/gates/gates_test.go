package gates

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func sandbox() Sandbox {
	return Sandbox{Bwrap: "/usr/bin/bwrap", GoRoot: "/usr/local/go", GoModCache: "/var/lib/claudew/cache/gomod", GitDir: "/var/lib/claudew/runs/x/git"}
}

func TestArgsCutTheNetworkAndHideCredentials(t *testing.T) {
	a := sandbox().Args("/var/lib/claudew/jobs/x/repo", []string{"make", "gates"})
	for _, must := range []string{"--unshare-all", "--die-with-parent", "--clearenv"} {
		if !slices.Contains(a, must) {
			t.Errorf("missing %s", must)
		}
	}
	if slices.Contains(a, "--share-net") {
		t.Error("network shared")
	}
	joined := strings.Join(a, " ")
	for _, never := range []string{"/run", "/home", "claudew-gh", "credstore", "/etc/claudew", "/var/lib/claudew/ledger"} {
		if strings.Contains(joined, " "+never) {
			t.Errorf("binds %s", never)
		}
	}
	if i := slices.Index(a, "--"); i < 0 || !slices.Equal(a[i+1:], []string{"make", "gates"}) {
		t.Errorf("command not after --: %v", a)
	}
	// Only the job's working tree is writable.
	writable := 0
	for i, x := range a {
		if x == "--bind" {
			writable++
			if a[i+1] != "/var/lib/claudew/jobs/x/repo" {
				t.Errorf("writable bind %s", a[i+1])
			}
		}
	}
	if writable != 1 {
		t.Errorf("%d writable binds, want 1", writable)
	}
	if v := a[slices.Index(a, "GOPROXY")+1]; v != "off" {
		t.Errorf("GOPROXY %s", v)
	}
}

// fakeBwrap stands in for bubblewrap: it drops everything up to "--" and
// runs the rest, so Run's sequencing can be tested where bwrap is absent.
func fakeBwrap(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bwrap")
	script := "#!/bin/sh\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunStopsAtTheFirstFailingGate(t *testing.T) {
	s := sandbox()
	s.Bwrap = fakeBwrap(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "third-ran")
	var log bytes.Buffer
	err := s.Run(context.Background(), dir, [][]string{
		{"/bin/sh", "-c", "echo first"},
		{"/bin/sh", "-c", "echo second; exit 3"},
		{"/bin/sh", "-c", "touch " + marker},
	}, &log)
	if err == nil || !strings.Contains(err.Error(), "second") {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("a gate after the failure ran")
	}
	for _, want := range []string{"first", "second", "gate failed"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log misses %q:\n%s", want, log.String())
		}
	}
}

func TestRunPassesAllGatesAndRefusesEmptyOnes(t *testing.T) {
	s := sandbox()
	s.Bwrap = fakeBwrap(t)
	var log bytes.Buffer
	if err := s.Run(context.Background(), t.TempDir(), [][]string{{"/bin/sh", "-c", "true"}, {"/bin/sh", "-c", "true"}}, &log); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Count(log.String(), "gate passed") != 2 {
		t.Errorf("log %s", log.String())
	}
	if err := s.Run(context.Background(), t.TempDir(), [][]string{{}}, &log); err == nil {
		t.Error("an empty gate ran")
	}
}

func TestRunHonoursTheTimeout(t *testing.T) {
	s := sandbox()
	s.Bwrap = fakeBwrap(t)
	s.Timeout = 200 * time.Millisecond
	start := time.Now()
	var log bytes.Buffer
	if err := s.Run(context.Background(), t.TempDir(), [][]string{{"/bin/sleep", "5"}}, &log); err == nil {
		t.Fatal("a gate past its timeout passed")
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}
}
