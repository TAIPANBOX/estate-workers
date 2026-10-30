// Command claudew runs estate workers on one host: it queues tasks, drains
// them one at a time through Claude Code, and, as a separate user, serves
// the push service that alone holds the GitHub App key.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TAIPANBOX/estate-workers/internal/agent"
	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/drain"
	"github.com/TAIPANBOX/estate-workers/internal/gates"
	"github.com/TAIPANBOX/estate-workers/internal/github"
	"github.com/TAIPANBOX/estate-workers/internal/gitx"
	"github.com/TAIPANBOX/estate-workers/internal/ledger"
	"github.com/TAIPANBOX/estate-workers/internal/push"
	"github.com/TAIPANBOX/estate-workers/internal/stop"
	"github.com/TAIPANBOX/estate-workers/internal/task"
	"github.com/TAIPANBOX/estate-workers/internal/template"
)

const usage = `usage: claudew [-config path] <command>

  enqueue <template> [repo=R] [model=M] [reason=TEXT] [by=NAME] [param=value ...]
  drain        run at most one task from the queue
  status       queue, spend, stop state and recent runs
  stop [why]   write the manual stop file (as root)
  resume       remove both stop files (as root)
  push-serve   serve the push service on the systemd socket (as claudew-gh)
  managed-settings  print /etc/claude-code/managed-settings.json
`

// Credential names under $CREDENTIALS_DIRECTORY.
const (
	credAnthropic = "anthropic-workers"
	credGitHubApp = "github-app-workers"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("claudew", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/claudew/config.json", "config file")
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "claudew:", err)
		return 1
	}
	tpls, err := template.Load()
	if err != nil {
		fmt.Fprintln(stderr, "claudew:", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch rest[0] {
	case "enqueue":
		t, err := parseEnqueue(rest[1:], time.Now(), rand.Reader)
		if err == nil {
			err = drain.Enqueue(drain.Deps{Cfg: cfg, Templates: tpls}, t)
		}
		if err != nil {
			fmt.Fprintln(stderr, "claudew enqueue:", err)
			return 1
		}
		fmt.Fprintln(stdout, t.ID)
	case "drain":
		rep, err := drain.Once(ctx, production(cfg, tpls))
		out, _ := json.Marshal(rep)
		fmt.Fprintln(stdout, string(out))
		if err != nil {
			fmt.Fprintln(stderr, "claudew drain:", err)
			return 1
		}
	case "status":
		if err := status(cfg, stdout, time.Now()); err != nil {
			fmt.Fprintln(stderr, "claudew status:", err)
			return 1
		}
	case "stop":
		why := strings.Join(rest[1:], " ")
		if why == "" {
			why = "stopped by hand"
		}
		if err := stop.Write(cfg.Paths.ManualStop, why, time.Now()); err != nil {
			fmt.Fprintln(stderr, "claudew stop:", err)
			return 1
		}
		fmt.Fprintln(stdout, "stopped; a running model gets SIGINT within a few seconds")
	case "resume":
		if err := stop.Clear(cfg.Paths.ManualStop, drain.Layout{State: cfg.Paths.State}.AutoStop()); err != nil {
			fmt.Fprintln(stderr, "claudew resume:", err)
			return 1
		}
		fmt.Fprintln(stdout, "resumed")
	case "managed-settings":
		data, err := agent.ManagedSettings()
		if err != nil {
			fmt.Fprintln(stderr, "claudew managed-settings:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
	case "push-serve":
		if err := pushServe(ctx, cfg, tpls); err != nil {
			fmt.Fprintln(stderr, "claudew push-serve:", err)
			return 1
		}
	default:
		fs.Usage()
		return 2
	}
	return 0
}

// parseEnqueue turns `<template> key=value ...` into a task. The reserved
// keys repo, model, reason and by fill the task; every other key is a
// template parameter.
func parseEnqueue(args []string, now time.Time, rnd io.Reader) (task.Task, error) {
	if len(args) == 0 {
		return task.Task{}, errors.New("no template named")
	}
	id, err := task.NewID(now, rnd)
	if err != nil {
		return task.Task{}, err
	}
	t := task.Task{ID: id, Template: args[0], RequestedBy: "supervisor", CreatedAt: now.UTC()}
	for _, kv := range args[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return task.Task{}, fmt.Errorf("argument %q is not key=value", kv)
		}
		switch k {
		case "repo":
			t.Repo = v
		case "model":
			t.Model = v
		case "reason":
			t.Reason = v
		case "by":
			t.RequestedBy = v
		default:
			if t.Params == nil {
				t.Params = map[string]string{}
			}
			if _, dup := t.Params[k]; dup {
				return task.Task{}, fmt.Errorf("parameter %q given twice", k)
			}
			t.Params[k] = v
		}
	}
	return t, nil
}

// readCredential reads one systemd credential and refuses anything that
// does not look like a single token.
func readCredential(name string) (string, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return "", errors.New("CREDENTIALS_DIRECTORY is not set; run under systemd with LoadCredentialEncrypted")
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	return checkToken(string(data))
}

func checkToken(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 20 || len(s) > 400 || strings.ContainsAny(s, " \t\r\n") {
		return "", errors.New("credential does not look like a single token")
	}
	return s, nil
}

func gitBin(cfg config.Config) string {
	if cfg.Paths.Git != "" {
		return cfg.Paths.Git
	}
	return "/usr/bin/git"
}

func production(cfg config.Config, tpls map[string]template.Template) drain.Deps {
	git := gitBin(cfg)
	modCache := filepath.Join(cfg.Paths.State, "cache", "gomod")
	githome := filepath.Join(cfg.Paths.State, "cache", "githome")
	_ = os.MkdirAll(modCache, 0o750)
	_ = os.MkdirAll(githome, 0o750)
	clone := func(ctx context.Context, repo, gitDir, work string) error {
		g := gitx.Git{Bin: git, Dir: filepath.Dir(work), Home: githome}
		args := []string{"clone", "-q", "--no-tags", "--single-branch"}
		if gitDir != "" {
			args = append(args, "--separate-git-dir", gitDir)
		}
		_ = os.Remove(work)
		_, err := g.Run(ctx, append(args, "https://github.com/"+cfg.Owner+"/"+repo+".git", work)...)
		return err
	}
	return drain.Deps{
		Cfg: cfg, Templates: tpls, Now: time.Now, GitBin: git,
		ClaudeBin: cfg.Paths.Claude, GoRoot: cfg.Paths.GoRoot, GoModCache: modCache, StopPoll: 2 * time.Second,
		Clone: func(ctx context.Context, r task.Resolved, gitDir, work string) error {
			if !cfg.IsPrivate(r.Repo) {
				return clone(ctx, r.Repo, gitDir, work)
			}
			resp, err := push.Call(ctx, cfg.Paths.PushSocket, push.Request{Op: push.OpExport, JobID: r.ID})
			if err != nil {
				return err
			}
			if !resp.OK {
				return fmt.Errorf("export %s: %s", resp.Code, resp.Error)
			}
			g := gitx.Git{Bin: git, Dir: filepath.Dir(work), Home: githome, AllowFile: true}
			_ = os.Remove(work)
			_, err = g.Run(ctx, "clone", "-q", "--separate-git-dir", gitDir, resp.Bundle, work)
			return err
		},
		CloneContext: func(ctx context.Context, repo, dir string) error {
			if cfg.IsPrivate(repo) {
				return fmt.Errorf("context repo %q is private; only public repos can be context", repo)
			}
			return clone(ctx, repo, "", dir)
		},
		Prefetch: func(ctx context.Context, work string) error {
			if _, err := os.Stat(filepath.Join(work, "go.mod")); err != nil {
				return nil
			}
			cmd := exec.CommandContext(ctx, filepath.Join(cfg.Paths.GoRoot, "bin", "go"), "mod", "download")
			cmd.Dir = work
			cmd.Env = []string{"PATH=" + cfg.Paths.GoRoot + "/bin:/usr/bin:/bin", "HOME=" + githome,
				"GOMODCACHE=" + modCache, "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOPROXY=https://proxy.golang.org",
				"GOCACHE=" + filepath.Join(githome, "gocache")}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("go mod download: %w: %.500s", err, out)
			}
			return nil
		},
		Agent: func(ctx context.Context, r agent.Run) (agent.Result, error) { return r.Do(ctx) },
		Gates: func(ctx context.Context, repoDir, gitDir string, cmds [][]string, log io.Writer) error {
			s := gates.Sandbox{Bwrap: cfg.Paths.Bwrap, GoRoot: cfg.Paths.GoRoot, GoModCache: modCache, GitDir: gitDir}
			return s.Run(ctx, repoDir, cmds, log)
		},
		Push: func(ctx context.Context, id string) (push.Response, error) {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			defer cancel()
			return push.Call(cctx, cfg.Paths.PushSocket, push.Request{Op: push.OpPush, JobID: id})
		},
		ReadKey: func() (string, error) { return readCredential(credAnthropic) },
	}
}

func status(cfg config.Config, w io.Writer, now time.Time) error {
	l := drain.Layout{State: cfg.Paths.State}
	if stopped, why := (stop.Files{Manual: cfg.Paths.ManualStop, Auto: l.AutoStop()}).Stopped(); stopped {
		fmt.Fprintln(w, "STOPPED:", why)
	} else {
		fmt.Fprintln(w, "running")
	}
	for name, dir := range map[string]string{"incoming": l.Incoming(), "running": l.Running(), "done": l.Done(), "failed": l.Failed()} {
		entries, _ := os.ReadDir(dir)
		fmt.Fprintf(w, "queue %-8s %d\n", name, len(entries))
	}
	s, err := ledger.SumFile(l.Ledger(), now)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "spend today $%.4f (cap %.2f), this month $%.4f (cap %.2f), total $%.4f\n",
		s.Day, cfg.DayCapUSD, s.Month, cfg.MonthCapUSD, s.Total)
	fmt.Fprintf(w, "billing verified: %v; consecutive failures: %d; untrusted ledger lines: %d\n",
		cfg.BillingVerified, s.ConsecutiveFailures, s.Malformed)
	data, err := os.ReadFile(l.Ledger())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	for _, line := range lines {
		if line != "" {
			fmt.Fprintln(w, line)
		}
	}
	return nil
}

func pushServe(ctx context.Context, cfg config.Config, tpls map[string]template.Template) error {
	pem, err := os.ReadFile(filepath.Join(os.Getenv("CREDENTIALS_DIRECTORY"), credGitHubApp))
	if err != nil {
		return fmt.Errorf("App key: %w", err)
	}
	key, err := github.ParseKey(pem)
	if err != nil {
		return err
	}
	app := github.App{ID: cfg.AppID, Key: key, BaseURL: "https://api.github.com", Now: time.Now}
	allow := filepath.Join(cfg.Paths.PushState, "empty-allowlist")
	if err := os.WriteFile(allow, nil, 0o600); err != nil {
		return err
	}
	svc := &push.Service{
		Cfg: cfg, Templates: tpls, Tokens: app, PRs: app, GitBin: gitBin(cfg),
		RunsDir:   filepath.Join(cfg.Paths.State, "runs"),
		CacheDir:  filepath.Join(cfg.Paths.PushState, "cache"),
		ExportDir: filepath.Join(cfg.Paths.PushState, "export"),
		Home:      filepath.Join(cfg.Paths.PushState, "home"),
		Secretscan: func(ctx context.Context, gitDir, rng string) (bool, error) {
			return secretscan(ctx, cfg.Paths.Secretscan, allow, gitDir, rng)
		},
	}
	for _, d := range []string{svc.CacheDir, svc.ExportDir, svc.Home} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	ln, err := systemdListener()
	if err != nil {
		return err
	}
	return svc.Serve(ctx, ln)
}

// secretscan runs the estate's scanner over a range with an empty
// allowlist: an allowlist entry the model added must not hide a finding.
func secretscan(ctx context.Context, bin, allow, gitDir, rng string) (bool, error) {
	cmd := exec.CommandContext(ctx, bin, "-C", gitDir, "-allow", allow, "diff", rng)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Dir(allow), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return true, nil
	}
	return false, fmt.Errorf("secretscan: %w: %.300s", err, out)
}

// systemdListener takes the one socket systemd passed in (fd 3).
func systemdListener() (net.Listener, error) {
	if pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID")); pid != os.Getpid() {
		return nil, errors.New("no socket from systemd (LISTEN_PID does not match)")
	}
	if n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS")); n != 1 {
		return nil, fmt.Errorf("want exactly one socket from systemd, got LISTEN_FDS=%q", os.Getenv("LISTEN_FDS"))
	}
	return net.FileListener(os.NewFile(3, "push.sock"))
}
