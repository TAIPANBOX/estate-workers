package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/TAIPANBOX/estate-workers/internal/agent"
	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/drain"
	"github.com/TAIPANBOX/estate-workers/internal/task"
	"github.com/TAIPANBOX/estate-workers/internal/template"
)

var bg = context.Background()

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func prodConfig(t *testing.T) (config.Config, map[string]template.Template, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Owner: "TAIPANBOX", Repos: []string{"architecture", "qryx"}, PrivateRepos: []string{"architecture"},
		DayCapUSD: 6, MonthCapUSD: 80, UnverifiedCapUSD: 0.1, MaxConsecutiveFailures: 3,
		RunCapUSD: map[string]float64{"claude-haiku-5-5": 0.5, "claude-sonnet-5-5": 2},
		Paths: config.Paths{State: filepath.Join(dir, "state"), ManualStop: filepath.Join(dir, "STOP"),
			GoRoot: filepath.Join(dir, "go"), Bwrap: script(t, dir, "bwrap", `while [ "$1" != "--" ]; do shift; done; shift; exec "$@"`),
			PushSocket: filepath.Join(dir, "none.sock"), PushState: filepath.Join(dir, "gh"), Git: "/usr/bin/git"},
	}
	tpls, err := template.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg, tpls, dir
}

func TestSecretscanMapsExitCodes(t *testing.T) {
	dir := t.TempDir()
	for code, want := range map[string]struct {
		found bool
		err   bool
	}{"0": {false, false}, "1": {true, false}, "2": {false, true}, "3": {false, true}} {
		bin := script(t, dir, "scan"+code, "exit "+code)
		found, err := secretscan(bg, bin, filepath.Join(dir, "allow"), dir, "a..b")
		if found != want.found || (err != nil) != want.err {
			t.Errorf("exit %s: found=%v err=%v", code, found, err)
		}
	}
}

func TestSecretscanPassesTheEmptyAllowlistAndTheRange(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	bin := script(t, dir, "scan", `echo "$@" > `+out)
	if _, err := secretscan(bg, bin, "/x/empty-allowlist", "/x/repo.git", "a..b"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "-C /x/repo.git -allow /x/empty-allowlist diff a..b" {
		t.Errorf("args %q", got)
	}
}

func TestProductionWiringRefusesWhatItMust(t *testing.T) {
	cfg, tpls, dir := prodConfig(t)
	d := production(cfg, tpls)
	if err := d.CloneContext(bg, "architecture", filepath.Join(dir, "ctx")); err == nil {
		t.Error("a private repo was cloned as context")
	}
	r := task.Resolved{Task: task.Task{ID: "20261030T120000Z-abcdef", Repo: "architecture"}}
	if err := d.Clone(bg, r, filepath.Join(dir, "g"), filepath.Join(dir, "w")); err == nil {
		t.Error("a private clone worked without the push service")
	}
	if _, err := d.Push(bg, "20261030T120000Z-abcdef"); err == nil {
		t.Error("push worked without the push service")
	}
	if _, err := d.Agent(bg, agent.Run{}); err == nil {
		t.Error("an agent ran without a binary")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := d.ReadKey(); err == nil {
		t.Error("a key was read without systemd credentials")
	}
}

func TestPrefetchRunsGoModDownloadOnlyWhereThereIsAGoMod(t *testing.T) {
	cfg, tpls, dir := prodConfig(t)
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o755)
	d := production(cfg, tpls)
	if err := d.Prefetch(bg, work); err != nil {
		t.Fatalf("no go.mod: %v", err)
	}
	os.WriteFile(filepath.Join(work, "go.mod"), []byte("module x\n"), 0o644)
	marker := filepath.Join(dir, "ran")
	script(t, cfg.Paths.GoRoot, "bin/go", `echo "$@ $GOPROXY $GOTOOLCHAIN" > `+marker)
	if err := d.Prefetch(bg, work); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	got, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(got)) != "mod download https://proxy.golang.org local" {
		t.Errorf("go saw %q", got)
	}
	script(t, cfg.Paths.GoRoot, "bin/go", "exit 1")
	if err := d.Prefetch(bg, work); err == nil {
		t.Error("a failing go mod download passed")
	}
}

func TestGatesGoThroughTheConfiguredBwrap(t *testing.T) {
	cfg, tpls, dir := prodConfig(t)
	d := production(cfg, tpls)
	var log bytes.Buffer
	if err := d.Gates(bg, drain.GateRun{Tree: dir, Cmds: [][]string{{"/bin/sh", "-c", "echo gate-ok"}}}, &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "gate-ok") {
		t.Errorf("log %q", log.String())
	}
}

func TestPushServeNeedsItsKeyAndItsSocket(t *testing.T) {
	cfg, tpls, dir := prodConfig(t)
	creds := filepath.Join(dir, "creds")
	os.MkdirAll(creds, 0o700)
	os.MkdirAll(cfg.Paths.PushState, 0o700)
	t.Setenv("CREDENTIALS_DIRECTORY", creds)
	if err := pushServe(bg, cfg, tpls); err == nil || !strings.Contains(err.Error(), "App key") {
		t.Errorf("no key: %v", err)
	}
	os.WriteFile(filepath.Join(creds, credGitHubApp), []byte("not a key"), 0o600)
	if err := pushServe(bg, cfg, tpls); err == nil {
		t.Error("a bad key was accepted")
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(creds, credGitHubApp), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600)
	t.Setenv("LISTEN_PID", "")
	if err := pushServe(bg, cfg, tpls); err == nil || !strings.Contains(err.Error(), "systemd") {
		t.Errorf("no socket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Paths.PushState, "empty-allowlist")); err != nil {
		t.Errorf("empty allowlist not written: %v", err)
	}
}

func TestSystemdListenerNeedsExactlyOneSocket(t *testing.T) {
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "2")
	if _, err := systemdListener(); err == nil {
		t.Error("two sockets accepted")
	}
}

func TestGitBinDefaults(t *testing.T) {
	if gitBin(config.Config{}) != "/usr/bin/git" || gitBin(config.Config{Paths: config.Paths{Git: "/opt/git"}}) != "/opt/git" {
		t.Error("gitBin")
	}
}
