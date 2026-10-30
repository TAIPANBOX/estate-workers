// Package gates runs a repository's own checks on the model's changes, in a
// bubblewrap sandbox with no network, no credentials and only the job's
// working tree writable. The checks are code the model could have edited,
// so they run with nothing worth stealing in reach.
package gates

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type Sandbox struct {
	Bwrap      string
	GoRoot     string
	GoModCache string
	GitDir     string
	Timeout    time.Duration
}

// Args is the bwrap command line for one gate command.
func (s Sandbox) Args(repoDir string, argv []string) []string {
	a := []string{
		"--unshare-all", "--die-with-parent", "--new-session",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--ro-bind-try", "/etc/alternatives", "/etc/alternatives",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		"--ro-bind-try", "/etc/passwd", "/etc/passwd",
		"--ro-bind-try", "/etc/group", "/etc/group",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", s.GoRoot, s.GoRoot,
		"--ro-bind", s.GoModCache, s.GoModCache,
		"--ro-bind", s.GitDir, s.GitDir,
		"--bind", repoDir, repoDir,
		"--chdir", repoDir,
		"--clearenv",
		"--setenv", "PATH", s.GoRoot + "/bin:/usr/bin:/bin",
		"--setenv", "HOME", "/tmp",
		"--setenv", "LANG", "C.UTF-8",
		"--setenv", "GOPATH", "/tmp/gopath",
		"--setenv", "GOMODCACHE", s.GoModCache,
		"--setenv", "GOCACHE", "/tmp/gocache",
		"--setenv", "GOFLAGS", "-mod=mod",
		"--setenv", "GOPROXY", "off",
		"--setenv", "GOTOOLCHAIN", "local",
		"--setenv", "GIT_CONFIG_NOSYSTEM", "1",
		"--setenv", "GIT_CONFIG_GLOBAL", "/dev/null",
		"--",
	}
	return append(a, argv...)
}

// Run runs every gate in order and stops at the first that fails. Output
// goes to log; the error names the gate and its exit status.
func (s Sandbox) Run(ctx context.Context, repoDir string, cmds [][]string, log io.Writer) error {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	for _, argv := range cmds {
		if len(argv) == 0 {
			return fmt.Errorf("gates: empty command")
		}
		fmt.Fprintf(log, "$ %s\n", strings.Join(argv, " "))
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(cctx, s.Bwrap, s.Args(repoDir, argv)...)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.Env = []string{}
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		cancel()
		if err != nil {
			fmt.Fprintf(log, "gate failed: %v\n", err)
			return fmt.Errorf("gate %q: %w", strings.Join(argv, " "), err)
		}
		fmt.Fprintf(log, "gate passed\n")
	}
	return nil
}
