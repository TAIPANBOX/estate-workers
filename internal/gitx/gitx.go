// Package gitx runs git with an environment it controls: no system or
// global config, no hooks, no fsmonitor, no credential helpers, https only
// unless a caller opts into local files. Command-scope config outranks a
// repository's own .git/config, so a hostile repository cannot turn a git
// call into a command of its choosing.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// MaxOutput bounds what Run keeps from stdout.
const MaxOutput = 64 << 20

type Git struct {
	Bin       string
	GitDir    string
	WorkTree  string
	Dir       string
	Home      string
	AllowFile bool
	Config    [][2]string
	Env       []string
}

func (g Git) config() [][2]string {
	c := [][2]string{
		{"core.hooksPath", "/dev/null"},
		{"core.fsmonitor", "false"},
		{"core.sshCommand", "/bin/false"},
		{"credential.helper", ""},
		{"protocol.allow", "never"},
		{"protocol.https.allow", "always"},
		{"transfer.fsckObjects", "true"},
		{"init.defaultBranch", "main"},
		{"gc.auto", "0"},
		{"gc.autoDetach", "false"},
		{"maintenance.auto", "false"},
		{"fetch.writeCommitGraph", "false"},
		{"commit.gpgSign", "false"},
		{"tag.gpgSign", "false"},
	}
	if g.AllowFile {
		c = append(c, [2]string{"protocol.file.allow", "always"})
	}
	return append(c, g.Config...)
}

// Environ is the whole environment of a git call.
func (g Git) Environ() []string {
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + g.Home,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
	}
	cfg := g.config()
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		env = append(env,
			"GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}
	return append(env, g.Env...)
}

// Command builds the git command; --git-dir and --work-tree come first when
// set, so git never discovers a repository on its own.
func (g Git) Command(ctx context.Context, args ...string) *exec.Cmd {
	var full []string
	if g.GitDir != "" {
		full = append(full, "--git-dir="+g.GitDir)
	}
	if g.WorkTree != "" {
		full = append(full, "--work-tree="+g.WorkTree)
	}
	cmd := exec.CommandContext(ctx, g.Bin, append(full, args...)...)
	cmd.Dir = g.Dir
	cmd.Env = g.Environ()
	return cmd
}

type limited struct {
	buf bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	room := l.max - l.buf.Len()
	if room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// Run returns stdout. On failure the error carries the first 2 KiB of
// stderr, never stdout, which may hold file contents.
func (g Git) Run(ctx context.Context, args ...string) ([]byte, error) {
	if g.Bin == "" {
		return nil, errors.New("git: no binary configured")
	}
	cmd := g.Command(ctx, args...)
	out := &limited{max: MaxOutput}
	errb := &limited{max: 2048}
	cmd.Stdout, cmd.Stderr = out, errb
	if err := cmd.Run(); err != nil {
		name := "?"
		if len(args) > 0 {
			name = args[0]
		}
		return nil, fmt.Errorf("git %s: %w: %s", name, err, strings.TrimSpace(errb.buf.String()))
	}
	return out.buf.Bytes(), nil
}

// Out is Run with surrounding space trimmed.
func (g Git) Out(ctx context.Context, args ...string) (string, error) {
	b, err := g.Run(ctx, args...)
	return strings.TrimSpace(string(b)), err
}

// IsAncestor reports whether a is an ancestor of b. Any error other than
// "not an ancestor" is returned.
func (g Git) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	cmd := g.Command(ctx, "merge-base", "--is-ancestor", a, b)
	errb := &limited{max: 2048}
	cmd.Stderr = errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	}
	return false, fmt.Errorf("git merge-base: %w: %s", err, strings.TrimSpace(errb.buf.String()))
}

// IsSHA reports whether s is a full 40-character lower-case hex object name.
func IsSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
