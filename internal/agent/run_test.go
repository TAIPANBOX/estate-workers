package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for claude when CLAUDEW_FAKE is set.
func TestMain(m *testing.M) {
	switch os.Getenv("CLAUDEW_FAKE") {
	case "":
		os.Exit(m.Run())
	case "ok":
		in, _ := io.ReadAll(os.Stdin)
		os.WriteFile(os.Getenv("CLAUDEW_ECHO"), []byte(strings.Join(os.Args[1:], "\n")+"\n--env--\n"+strings.Join(os.Environ(), "\n")+"\n--stdin--\n"+string(in)), 0o600)
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"total_cost_usd":0.0123,"result":"done"}`)
		os.Exit(0)
	case "interruptible":
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT)
		fmt.Println(`{"type":"system","subtype":"init"}`)
		<-sig
		fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true,"total_cost_usd":0.05}`)
		os.Exit(130)
	case "stubborn":
		signal.Ignore(syscall.SIGINT)
		fmt.Println(`{"type":"system","subtype":"init"}`)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(2)
}

func run(t *testing.T, mode string) (Run, string) {
	t.Helper()
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(prompt, []byte("the prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	echo := filepath.Join(dir, "echo.txt")
	return Run{
		Bin: exe, Args: []string{"--bare", "-p"}, Dir: dir,
		Env:        []string{"CLAUDEW_FAKE=" + mode, "CLAUDEW_ECHO=" + echo, "ANTHROPIC_API_KEY=k"},
		PromptFile: prompt, TranscriptFile: filepath.Join(dir, "t.jsonl"), StderrFile: filepath.Join(dir, "e.log"),
		Grace: 500 * time.Millisecond,
	}, echo
}

func TestDoFeedsThePromptAndReadsTheResult(t *testing.T) {
	t.Setenv("GH_TOKEN", "should-not-pass")
	r, echo := run(t, "ok")
	res, err := r.Do(context.Background())
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !res.CostKnown || res.CostUSD != 0.0123 || res.Text != "done" {
		t.Errorf("result %+v", res)
	}
	seen, _ := os.ReadFile(echo)
	if !strings.Contains(string(seen), "--stdin--\nthe prompt") || !strings.HasPrefix(string(seen), "--bare\n-p\n") {
		t.Errorf("child saw %q", seen)
	}
	if strings.Contains(string(seen), "should-not-pass") {
		t.Error("parent environment reached the child")
	}
	transcript, _ := os.ReadFile(r.TranscriptFile)
	if !strings.Contains(string(transcript), `"type":"result"`) {
		t.Error("transcript misses the result")
	}
}

func TestCancelSendsSIGINTAndKeepsTheCost(t *testing.T) {
	r, _ := run(t, "interruptible")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	res, err := r.Do(ctx)
	if err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if !res.CostKnown || res.CostUSD != 0.05 {
		t.Errorf("cost after SIGINT %+v", res)
	}
}

func TestAStubbornRunIsKilledAfterTheGrace(t *testing.T) {
	r, _ := run(t, "stubborn")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	if _, err := r.Do(ctx); err == nil {
		t.Fatal("a killed run reported success")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("kill took %v", d)
	}
}

func TestDoRefusesMissingInputs(t *testing.T) {
	r, _ := run(t, "ok")
	r.Bin = ""
	if _, err := r.Do(context.Background()); err == nil {
		t.Error("ran without a binary")
	}
	r, _ = run(t, "ok")
	r.PromptFile = filepath.Join(t.TempDir(), "missing")
	if _, err := r.Do(context.Background()); err == nil {
		t.Error("ran without a prompt")
	}
}
