package stop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNoFilesMeansRunning(t *testing.T) {
	d := t.TempDir()
	f := Files{Manual: filepath.Join(d, "manual"), Auto: filepath.Join(d, "auto")}
	if stopped, why := f.Stopped(); stopped {
		t.Fatalf("stopped with no files: %s", why)
	}
}

func TestEitherFileStops(t *testing.T) {
	for _, which := range []string{"manual", "auto"} {
		d := t.TempDir()
		f := Files{Manual: filepath.Join(d, "manual"), Auto: filepath.Join(d, "auto")}
		if err := Write(filepath.Join(d, which), "month cap reached", time.Unix(0, 0)); err != nil {
			t.Fatal(err)
		}
		stopped, why := f.Stopped()
		if !stopped || !strings.Contains(why, "month cap reached") {
			t.Errorf("%s file: stopped=%v why=%q", which, stopped, why)
		}
	}
}

func TestEmptyStopFileStillStops(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "manual")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if stopped, why := (Files{Manual: p}).Stopped(); !stopped || !strings.Contains(why, "no reason") {
		t.Errorf("empty file: stopped=%v why=%q", stopped, why)
	}
}

func TestUncheckableStopFileMeansStopped(t *testing.T) {
	d := t.TempDir()
	// A directory where the file should be cannot be read as a file.
	p := filepath.Join(d, "manual")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if stopped, why := (Files{Manual: p}).Stopped(); !stopped || !strings.Contains(why, "cannot be checked") {
		t.Errorf("uncheckable: stopped=%v why=%q", stopped, why)
	}
}

func TestClearRemovesBothAndIgnoresMissing(t *testing.T) {
	d := t.TempDir()
	f := Files{Manual: filepath.Join(d, "manual"), Auto: filepath.Join(d, "auto")}
	if err := Write(f.Auto, "x", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Clear(f.Manual, f.Auto, ""); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if stopped, _ := f.Stopped(); stopped {
		t.Error("still stopped after Clear")
	}
	if err := Write("", "x", time.Now()); err == nil {
		t.Error("Write without a path succeeded")
	}
	if err := Clear(filepath.Join(d, "nested")); err != nil {
		t.Errorf("Clear of a missing file: %v", err)
	}
}

func TestReasonIsOneBoundedLine(t *testing.T) {
	if got := firstLine(strings.Repeat("x", 300) + "\nsecond"); len(got) != 200 {
		t.Errorf("firstLine length %d, want 200", len(got))
	}
}

// A stop written into a directory that is not there yet is still written:
// every automatic stop ignores Write's error, so a lost one would let the
// workers run on.
func TestWriteCreatesAMissingStopDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stop", "STOP")
	if err := Write(p, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if s, why := (Files{Auto: p}).Stopped(); !s {
		t.Errorf("not stopped after Write: %s", why)
	}
}
