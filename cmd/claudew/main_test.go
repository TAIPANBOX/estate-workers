package main

import (
	"bytes"
	"fmt"
	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/drain"
	"github.com/TAIPANBOX/estate-workers/internal/ledger"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func TestParseEnqueueFillsTheTaskAndTheParams(t *testing.T) {
	tk, err := parseEnqueue([]string{"dossier-bump", "repo=architecture", "service=qryx", "model=claude-haiku-5-5", "by=ops"}, now, bytes.NewReader([]byte("abcdefgh")))
	if err != nil {
		t.Fatal(err)
	}
	if tk.Template != "dossier-bump" || tk.Repo != "architecture" || tk.Params["service"] != "qryx" || tk.Model != "claude-haiku-5-5" || tk.RequestedBy != "ops" {
		t.Errorf("task %+v", tk)
	}
	if !strings.HasPrefix(tk.ID, "20261030T120000Z-") {
		t.Errorf("id %q", tk.ID)
	}
}

func TestParseEnqueueRefusesJunk(t *testing.T) {
	for _, args := range [][]string{nil, {"t", "novalue"}, {"t", "=x"}, {"t", "a=1", "a=2"}} {
		if _, err := parseEnqueue(args, now, bytes.NewReader([]byte("abcdefgh"))); err == nil {
			t.Errorf("parseEnqueue(%v) accepted", args)
		}
	}
}

func TestCheckTokenRefusesWhatIsNotOneToken(t *testing.T) {
	if _, err := checkToken("  test-token-abcdefghijklmnopqrstuvwxyz\n"); err != nil {
		t.Errorf("good token refused: %v", err)
	}
	for _, bad := range []string{"", "short", "two tokens here, both long enough", strings.Repeat("a", 500)} {
		if _, err := checkToken(bad); err == nil {
			t.Errorf("checkToken(%q) accepted", bad)
		}
	}
}

func TestReadCredentialNeedsSystemd(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := readCredential("x"); err == nil {
		t.Error("read a credential without CREDENTIALS_DIRECTORY")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "anthropic-workers"), []byte("test-token-abcdefghijklmnopqrstuvwxyz\n"), 0o600)
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if k, err := readCredential("anthropic-workers"); err != nil || !strings.HasPrefix(k, "test-token-") {
		t.Errorf("key %q err %v", k, err)
	}
}

func testConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := `{"owner":"TAIPANBOX","repos":["architecture","qryx"],"private_repos":["architecture"],
"day_cap_usd":6,"month_cap_usd":80,"unverified_cap_usd":0.1,
"run_cap_usd":{"claude-haiku-5-5":0.5,"claude-sonnet-5-5":2},"max_consecutive_failures":3,
"paths":{"state":"` + filepath.Join(dir, "state") + `","manual_stop":"` + filepath.Join(dir, "STOP") + `"}}`
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte(cfg), 0o600)
	return p
}

func TestEnqueueStatusStopAndResumeFromTheCommandLine(t *testing.T) {
	p := testConfig(t)
	var out, errb bytes.Buffer
	if code := run([]string{"-config", p, "enqueue", "dossier-bump", "repo=architecture", "service=qryx"}, &out, &errb); code != 0 {
		t.Fatalf("enqueue exit %d: %s", code, errb.String())
	}
	out.Reset()
	if code := run([]string{"-config", p, "stop", "maintenance"}, &out, &errb); code != 0 {
		t.Fatalf("stop exit %d: %s", code, errb.String())
	}
	out.Reset()
	run([]string{"-config", p, "status"}, &out, &errb)
	if !strings.Contains(out.String(), "STOPPED") || !strings.Contains(out.String(), "queue incoming 1") {
		t.Errorf("status:\n%s", out.String())
	}
	out.Reset()
	run([]string{"-config", p, "drain"}, &out, &errb)
	if !strings.Contains(out.String(), `"idle":true`) {
		t.Errorf("drain while stopped: %s", out.String())
	}
	run([]string{"-config", p, "resume"}, &out, &errb)
	out.Reset()
	run([]string{"-config", p, "status"}, &out, &errb)
	if !strings.HasPrefix(out.String(), "running") {
		t.Errorf("status after resume:\n%s", out.String())
	}
}

func TestBadInvocationsExitNonZero(t *testing.T) {
	p := testConfig(t)
	var out, errb bytes.Buffer
	for _, args := range [][]string{
		{}, {"-config", p}, {"-config", p, "merge"}, {"-config", filepath.Join(t.TempDir(), "none.json"), "status"},
		{"-config", p, "enqueue", "dossier-bump", "repo=hermes-openwrt", "service=qryx"},
		{"-config", p, "enqueue", "shell"}, {"-bogus"},
	} {
		if code := run(args, &out, &errb); code == 0 {
			t.Errorf("run(%v) exited 0", args)
		}
	}
}

func TestSystemdListenerNeedsItsSocket(t *testing.T) {
	t.Setenv("LISTEN_PID", "1")
	if _, err := systemdListener(); err == nil {
		t.Error("listener without systemd")
	}
}

func TestManagedSettingsPrintsJSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"-config", testConfig(t), "managed-settings"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"disableBypassPermissionsMode": "disable"`) {
		t.Errorf("exit %d out %.200s err %s", code, out.String(), errb.String())
	}
}

// Three failures stop the workers; resume must let them run again, which
// needs the failure count reset as well as the stop files cleared.
func TestResumeAfterRepeatedFailuresLetsTheWorkersRunAgain(t *testing.T) {
	p := testConfig(t)
	var out, errb bytes.Buffer
	if code := run([]string{"-config", p, "enqueue", "dossier-bump", "repo=architecture", "service=qryx"}, &out, &errb); code != 0 {
		t.Fatalf("enqueue exit %d: %s", code, errb.String())
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	led := drain.Layout{State: cfg.Paths.State}.Ledger()
	for i := 0; i < 3; i++ {
		if err := ledger.Append(led, ledger.Entry{ID: fmt.Sprintf("e%d", i), Outcome: ledger.OutcomeError, At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	run([]string{"-config", p, "status"}, &out, &errb)
	if !strings.Contains(out.String(), "consecutive failures: 3") {
		t.Fatalf("status before resume:\n%s", out.String())
	}
	if code := run([]string{"-config", p, "resume"}, &out, &errb); code != 0 {
		t.Fatalf("resume exit %d: %s", code, errb.String())
	}
	out.Reset()
	run([]string{"-config", p, "status"}, &out, &errb)
	if !strings.Contains(out.String(), "consecutive failures: 0") || !strings.Contains(out.String(), "untrusted ledger lines: 0") {
		t.Errorf("status after resume:\n%s", out.String())
	}
}
