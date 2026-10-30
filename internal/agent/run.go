package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Run is one execution of Claude Code. The prompt comes on stdin, the
// stream goes to the transcript file and to the parser at once.
type Run struct {
	Bin            string
	Args           []string
	Env            []string
	Dir            string
	PromptFile     string
	TranscriptFile string
	StderrFile     string
	// Grace is how long a cancelled run gets after SIGINT before it and
	// everything it started are killed.
	Grace time.Duration
}

// Do runs Claude Code and returns what its stream reported. When ctx ends,
// the process group gets SIGINT, then SIGKILL after Grace.
func (r Run) Do(ctx context.Context) (Result, error) {
	if r.Bin == "" {
		return Result{}, errors.New("agent: no claude binary configured")
	}
	prompt, err := os.Open(r.PromptFile)
	if err != nil {
		return Result{}, err
	}
	defer prompt.Close()
	transcript, err := os.OpenFile(r.TranscriptFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return Result{}, err
	}
	defer transcript.Close()
	stderr, err := os.OpenFile(r.StderrFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return Result{}, err
	}
	defer stderr.Close()

	pr, pw := io.Pipe()
	parsed := make(chan struct {
		res Result
		err error
	}, 1)
	go func() {
		res, err := Parse(pr)
		_, _ = io.Copy(io.Discard, pr)
		parsed <- struct {
			res Result
			err error
		}{res, err}
	}()

	grace := r.Grace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	cmd := exec.CommandContext(ctx, r.Bin, r.Args...)
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	cmd.Stdin = prompt
	cmd.Stdout = io.MultiWriter(transcript, pw)
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	}
	cmd.WaitDelay = grace

	runErr := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	pw.Close()
	p := <-parsed
	if runErr != nil {
		return p.res, runErr
	}
	return p.res, p.err
}
