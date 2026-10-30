// Package drain is the orchestrator: it takes the oldest task from the
// queue, checks the money, runs the model in the task's own directory,
// checks what changed, commits it, runs the gates offline, hands the branch
// to the push service and records every cent in the ledger.
package drain

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/TAIPANBOX/estate-workers/internal/agent"
	"github.com/TAIPANBOX/estate-workers/internal/budget"
	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/diffcheck"
	"github.com/TAIPANBOX/estate-workers/internal/gitx"
	"github.com/TAIPANBOX/estate-workers/internal/ledger"
	"github.com/TAIPANBOX/estate-workers/internal/models"
	"github.com/TAIPANBOX/estate-workers/internal/push"
	"github.com/TAIPANBOX/estate-workers/internal/stop"
	"github.com/TAIPANBOX/estate-workers/internal/task"
	"github.com/TAIPANBOX/estate-workers/internal/template"
)

//go:embed rules.md
var Rules string

var fileRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-z0-9]{6}\.json$`)

// Outcome of a drain call that ran no task.
const OutcomeDeferred = "deferred"

type Deps struct {
	Cfg       config.Config
	Templates map[string]template.Template
	Now       func() time.Time
	GitBin    string
	ClaudeBin string
	GoRoot    string
	// GoModCache is shared by every job; the orchestrator fills it before
	// the model starts, and the model and the gates only read it.
	GoModCache string
	StopPoll   time.Duration

	Clone        func(ctx context.Context, r task.Resolved, gitDir, workTree string) error
	CloneContext func(ctx context.Context, repo, dir string) error
	// Prefetch downloads the job's Go modules from the unmodified main
	// branch, before the model can change anything. It may be nil.
	Prefetch func(ctx context.Context, workTree string) error
	Agent    func(ctx context.Context, run agent.Run) (agent.Result, error)
	Gates    func(ctx context.Context, run GateRun, log io.Writer) error
	Push     func(ctx context.Context, id string) (push.Response, error)
	ReadKey  func() (string, error)
}

type Report struct {
	ID      string  `json:"id,omitempty"`
	Outcome string  `json:"outcome,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	PRURL   string  `json:"pr_url,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	Idle    bool    `json:"idle,omitempty"`
}

// Layout names every path under the state directory.
type Layout struct{ State string }

// GateRun is what the gates get: a tree to run in, the paths they may
// read besides it, and the template's environment.
type GateRun struct {
	Tree     string
	ReadOnly []string
	Env      []string
	Cmds     [][]string
}

func (l Layout) Incoming() string { return filepath.Join(l.State, "queue", "incoming") }
func (l Layout) Running() string  { return filepath.Join(l.State, "queue", "running") }
func (l Layout) Done() string     { return filepath.Join(l.State, "queue", "done") }
func (l Layout) Failed() string   { return filepath.Join(l.State, "queue", "failed") }
func (l Layout) Ledger() string   { return filepath.Join(l.State, "ledger.jsonl") }

// AutoStop sits in a directory of its own: the model's write deny then
// names a directory, and Claude Code's shell sandbox does not create the
// file while a command runs, as it does for a denied file that is missing.
func (l Layout) AutoStop() string     { return filepath.Join(l.State, "stop", "STOP") }
func (l Layout) Run(id string) string { return filepath.Join(l.State, "runs", id) }
func (l Layout) Job(id string) string { return filepath.Join(l.State, "jobs", id) }

func (l Layout) ensure() error {
	for _, d := range []string{l.Incoming(), l.Running(), l.Done(), l.Failed(),
		filepath.Join(l.State, "runs"), filepath.Join(l.State, "jobs"), filepath.Dir(l.AutoStop())} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	return nil
}

func (d Deps) layout() Layout { return Layout{State: d.Cfg.Paths.State} }

func (d Deps) stops() stop.Files {
	return stop.Files{Manual: d.Cfg.Paths.ManualStop, Auto: d.layout().AutoStop()}
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Enqueue validates a task and puts it in the queue.
func Enqueue(d Deps, t task.Task) error {
	if _, err := t.Validate(d.Cfg, d.Templates); err != nil {
		return err
	}
	l := d.layout()
	if err := l.ensure(); err != nil {
		return err
	}
	name := t.ID + ".json"
	for _, dir := range []string{l.Incoming(), l.Running(), l.Done(), l.Failed()} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("task %s already exists", t.ID)
		}
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(l.Incoming(), "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(l.Incoming(), name))
}

func oldest(dir string) (string, bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", false, nil
	}
	return names[0], true, nil
}

func bounded(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (d Deps) record(e ledger.Entry) error {
	e.At = d.now()
	return ledger.Append(d.layout().Ledger(), e)
}

// Once does at most one task. It returns Idle when there is nothing to do or
// the workers are stopped.
func Once(ctx context.Context, d Deps) (Report, error) {
	l := d.layout()
	if err := l.ensure(); err != nil {
		return Report{}, err
	}
	if err := recoverRunning(d); err != nil {
		return Report{}, err
	}
	if stopped, why := d.stops().Stopped(); stopped {
		return Report{Idle: true, Reason: why}, nil
	}
	name, ok, err := oldest(l.Incoming())
	if err != nil || !ok {
		return Report{Idle: true, Reason: "queue empty"}, err
	}
	running := filepath.Join(l.Running(), name)
	if err := os.Rename(filepath.Join(l.Incoming(), name), running); err != nil {
		return Report{}, err
	}
	id := strings.TrimSuffix(name, ".json")
	if !fileRe.MatchString(name) {
		return d.finish(id, running, ledger.OutcomeRefused, "queue file name is not a task id", "", Report{ID: id})
	}

	r, err := load(d, running, id)
	if err != nil {
		return d.finish(id, running, ledger.OutcomeRefused, "invalid task: "+err.Error(), "", Report{ID: id})
	}
	sums, err := ledger.SumFile(l.Ledger(), d.now())
	if err != nil {
		_ = os.Rename(running, filepath.Join(l.Incoming(), name))
		reason := "ledger unreadable: " + err.Error()
		_ = stop.Write(l.AutoStop(), reason, d.now())
		return Report{ID: id, Outcome: OutcomeDeferred, Reason: reason}, nil
	}
	stopped, why := d.stops().Stopped()
	dec := budget.Decide(d.Cfg, sums, r.RunCapUSD, stopped, why)
	if !dec.Allow {
		if err := os.Rename(running, filepath.Join(l.Incoming(), name)); err != nil {
			return Report{}, err
		}
		if dec.Stop {
			_ = stop.Write(l.AutoStop(), dec.Reason, d.now())
		}
		return Report{ID: id, Outcome: OutcomeDeferred, Reason: dec.Reason}, nil
	}

	outcome, reason, prURL, cost := d.execute(ctx, r)
	return d.finish(id, running, outcome, reason, prURL, Report{ID: id, CostUSD: cost})
}

func load(d Deps, path, id string) (task.Resolved, error) {
	f, err := os.Open(path)
	if err != nil {
		return task.Resolved{}, err
	}
	defer f.Close()
	t, err := task.Parse(f)
	if err != nil {
		return task.Resolved{}, err
	}
	if t.ID != id {
		return task.Resolved{}, fmt.Errorf("file %s holds task %q", id, t.ID)
	}
	return t.Validate(d.Cfg, d.Templates)
}

func (d Deps) finish(id, running, outcome, reason, prURL string, rep Report) (Report, error) {
	l := d.layout()
	rep.Outcome, rep.Reason, rep.PRURL = outcome, bounded(reason, 500), prURL
	err := d.record(ledger.Entry{ID: id, Outcome: outcome, PRURL: prURL, Note: bounded(reason, 300)})
	dest := l.Failed()
	if outcome == ledger.OutcomeOK || outcome == ledger.OutcomeNoChange {
		dest = l.Done()
	}
	if mvErr := os.Rename(running, filepath.Join(dest, filepath.Base(running))); mvErr != nil {
		err = errors.Join(err, mvErr)
	}
	if err != nil {
		_ = stop.Write(l.AutoStop(), "could not close task "+id+": "+err.Error(), d.now())
	}
	return rep, err
}

// recoverRunning closes tasks a crash left in running. A run whose model
// started but whose spend never reached the ledger is charged from its
// transcript, or its whole cap when the transcript has no result.
func recoverRunning(d Deps) error {
	l := d.layout()
	entries, err := os.ReadDir(l.Running())
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	sums, err := ledger.SumFile(l.Ledger(), d.now())
	if err != nil {
		return err
	}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		running := filepath.Join(l.Running(), e.Name())
		transcript := filepath.Join(l.Run(id), "transcript.jsonl")
		if f, err := os.Open(transcript); err == nil && !sums.Spent[id] {
			res, _ := agent.Parse(f)
			f.Close()
			capUSD := models.HardMaxRunUSD[models.Sonnet]
			if r, err := load(d, running, id); err == nil {
				capUSD = r.RunCapUSD
			}
			if err := d.record(ledger.Entry{ID: id, Outcome: ledger.OutcomeSpent, CostUSD: res.BilledCost(capUSD),
				Note: "recovered after an interrupted run"}); err != nil {
				return err
			}
		}
		if _, err := d.finish(id, running, ledger.OutcomeError, "interrupted: found in running at start", "", Report{}); err != nil {
			return err
		}
	}
	return nil
}

// execute runs one validated task and returns its outcome. Every path that
// spent money has already written the spend to the ledger.
func (d Deps) execute(ctx context.Context, r task.Resolved) (outcome, reason, prURL string, cost float64) {
	l := d.layout()
	runDir, jobDir := l.Run(r.ID), l.Job(r.ID)
	work := filepath.Join(jobDir, "repo")
	for _, dir := range []string{runDir, work, filepath.Join(jobDir, "context"), filepath.Join(jobDir, "home"),
		filepath.Join(jobDir, "tmp"), filepath.Join(jobDir, "githome")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return ledger.OutcomeError, err.Error(), "", 0
		}
	}
	if err := writeJSON(filepath.Join(runDir, "task.json"), r.Task); err != nil {
		return ledger.OutcomeError, err.Error(), "", 0
	}

	gitDir := filepath.Join(runDir, "git")
	g := gitx.Git{Bin: d.GitBin, GitDir: gitDir, WorkTree: work, Dir: work, Home: filepath.Join(jobDir, "githome"), Env: d.identity()}
	var base, baseBranch string
	if r.Tpl.Kind == template.KindRepo {
		if err := d.Clone(ctx, r, gitDir, work); err != nil {
			return ledger.OutcomeError, "clone: " + err.Error(), "", 0
		}
		for _, repo := range r.ContextRepos {
			if err := d.CloneContext(ctx, repo, filepath.Join(jobDir, "context", repo)); err != nil {
				return ledger.OutcomeError, "context clone: " + err.Error(), "", 0
			}
		}
		if err := writeFiles(r, jobDir); err != nil {
			return ledger.OutcomeError, "template files: " + err.Error(), "", 0
		}
		var err error
		if base, err = g.Out(ctx, "rev-parse", "HEAD"); err == nil {
			baseBranch, err = g.Out(ctx, "symbolic-ref", "--short", "HEAD")
		}
		if err != nil {
			return ledger.OutcomeError, err.Error(), "", 0
		}
		if d.Prefetch != nil {
			if err := d.Prefetch(ctx, work); err != nil {
				return ledger.OutcomeError, "prefetch: " + err.Error(), "", 0
			}
		}
	}

	run, err := d.prepare(r, runDir, jobDir, work)
	if err != nil {
		return ledger.OutcomeError, err.Error(), "", 0
	}
	res, runErr, stoppedDuring := d.runAgent(ctx, run)
	cost = res.BilledCost(r.RunCapUSD)
	if err := d.record(ledger.Entry{ID: r.ID, Template: r.Template, Repo: r.Repo, Model: r.Model,
		Outcome: ledger.OutcomeSpent, CostUSD: cost, InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
		CacheRead: res.CacheRead, CacheWrite: res.CacheWrite}); err != nil {
		_ = stop.Write(l.AutoStop(), "spend of "+r.ID+" could not be recorded: "+err.Error(), d.now())
		return ledger.OutcomeError, "ledger: " + err.Error(), "", cost
	}
	_ = writeJSON(filepath.Join(runDir, "result.json"), res)

	switch {
	case stoppedDuring:
		return ledger.OutcomeStopped, "stopped during the run", "", cost
	case runErr != nil:
		return ledger.OutcomeFailed, "agent: " + runErr.Error(), "", cost
	case res.IsError:
		return ledger.OutcomeFailed, "agent reported an error: " + res.Subtype, "", cost
	case r.Tpl.Kind == template.KindProbe:
		return ledger.OutcomeOK, fmt.Sprintf("probe answered, cost %.4f", cost), "", cost
	}
	outcome, reason, prURL = d.deliver(ctx, r, g, base, baseBranch, runDir, work, res, cost)
	return outcome, reason, prURL, cost
}

func (d Deps) identity() []string {
	name, email := d.Cfg.CommitName, d.Cfg.CommitEmail
	if name == "" {
		name = "estate-workers"
	}
	if email == "" {
		email = "estate-workers@users.noreply.github.com"
	}
	return []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
}

// prepare writes the prompt, rules and settings and builds the command.
func (d Deps) prepare(r task.Resolved, runDir, jobDir, work string) (agent.Run, error) {
	prompt := r.Prompt
	if r.Tpl.PadTokens > 0 {
		prompt = pad(r.Tpl.PadTokens) + prompt
	}
	home, tmp := filepath.Join(jobDir, "home"), filepath.Join(jobDir, "tmp")
	settings, err := agent.Settings(r.Tools, []string{home, tmp})
	if err != nil {
		return agent.Run{}, err
	}
	files := map[string][]byte{
		"prompt.txt":    []byte(prompt),
		"rules.md":      []byte(Rules),
		"settings.json": settings,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(runDir, name), data, 0o640); err != nil {
			return agent.Run{}, err
		}
	}
	repoRules := ""
	if r.Tpl.Kind == template.KindRepo {
		if fi, err := os.Lstat(filepath.Join(work, "CLAUDE.md")); err == nil && fi.Mode().IsRegular() {
			repoRules = filepath.Join(work, "CLAUDE.md")
		}
	}
	key, err := d.ReadKey()
	if err != nil {
		return agent.Run{}, fmt.Errorf("key: %w", err)
	}
	args := agent.Args(agent.Inputs{
		Model: r.Model, MaxTurns: r.Tpl.MaxTurns, MaxBudgetUSD: r.RunCapUSD, AllowedTools: r.Tools,
		RulesFile: filepath.Join(runDir, "rules.md"), RepoRules: repoRules, SettingsFile: filepath.Join(runDir, "settings.json"),
	})
	env := agent.Env(key, home, tmp, d.GoRoot)
	env = append(env, "GOCACHE="+filepath.Join(tmp, "gocache"))
	env = append(env, templateEnv(r, jobDir)...)
	if d.GoModCache != "" {
		env = append(env, "GOMODCACHE="+d.GoModCache)
	}
	return agent.Run{
		Bin: d.ClaudeBin, Args: args,
		Env:        env,
		Dir:        work,
		PromptFile: filepath.Join(runDir, "prompt.txt"), TranscriptFile: filepath.Join(runDir, "transcript.jsonl"),
		StderrFile: filepath.Join(runDir, "stderr.log"), Grace: 10 * time.Second,
	}, nil
}

// pad is filler for the credit probe, roughly tokens long, so the probe's
// cost shows in cents on the Console.
func pad(tokens int) string {
	var b strings.Builder
	for i := 0; i < tokens/12+1; i++ {
		fmt.Fprintf(&b, "filler line %05d for the credit check, no instruction here.\n", i)
	}
	return b.String()
}

// runAgent runs the model and cancels it when a stop file appears.
func (d Deps) runAgent(ctx context.Context, run agent.Run) (agent.Result, error, bool) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stopped atomic.Bool
	poll := d.StopPoll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	go func() {
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-actx.Done():
				return
			case <-t.C:
				if s, _ := d.stops().Stopped(); s {
					stopped.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	res, err := d.Agent(actx, run)
	return res, err, stopped.Load()
}

// deliver checks, commits, gates and hands the branch to the push service.
func (d Deps) deliver(ctx context.Context, r task.Resolved, g gitx.Git, base, baseBranch, runDir, work string, res agent.Result, cost float64) (string, string, string) {
	l := d.layout()
	if err := dropSandboxPlaceholders(ctx, g, work); err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	if _, err := g.Run(ctx, "add", "-A"); err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	raw, err := g.Run(ctx, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev", base)
	if err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	num, err := g.Run(ctx, "diff", "--cached", "--numstat", "-z", "--no-renames", base)
	if err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	changes, err := diffcheck.ParseRaw(raw)
	if err == nil {
		err = diffcheck.MergeNumstat(changes, num)
	}
	if err != nil {
		_ = stop.Write(l.AutoStop(), r.ID+": unreadable diff: "+err.Error(), d.now())
		return ledger.OutcomeRefused, err.Error(), ""
	}
	if len(changes) == 0 {
		return ledger.OutcomeNoChange, "the model changed nothing", ""
	}
	if v := diffcheck.Check(changes, diffcheck.Rules{Allowed: r.AllowedPaths, MaxFiles: r.Tpl.MaxFiles, MaxAddedLines: r.Tpl.MaxAddedLines}); len(v) > 0 {
		reason := "diff refused: " + strings.Join(v, "; ")
		_ = stop.Write(l.AutoStop(), r.ID+": "+reason, d.now())
		return ledger.OutcomeRefused, reason, ""
	}
	msg := fmt.Sprintf("%s\n\nWorker task %s, template %s, model %s.\n\nCo-Authored-By: %s <noreply@anthropic.com>\n",
		r.Title, r.ID, r.Template, r.Model, models.DisplayName(r.Model))
	if _, err := g.Run(ctx, "commit", "-q", "--no-verify", "-m", msg); err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	head, err := g.Out(ctx, "rev-parse", "HEAD")
	if err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	if _, err := g.Run(ctx, "update-ref", "refs/heads/"+r.Branch, head); err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}

	logf, err := os.OpenFile(filepath.Join(runDir, "gates.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	jobDir := filepath.Dir(work)
	tree := filepath.Join(jobDir, "gates")
	if err := os.RemoveAll(tree); err != nil {
		logf.Close()
		return ledger.OutcomeError, err.Error(), ""
	}
	// The gates are the repository's own code and some reset and clean
	// their tree, so they get a throwaway clone of the committed branch with
	// a git directory of its own; the tree and history pushed stay as checked.
	cg := gitx.Git{Bin: d.GitBin, Dir: jobDir, Home: g.Home, AllowFile: true}
	if _, err := cg.Run(ctx, "clone", "-q", "--no-hardlinks", "--template=", "--branch", r.Branch, g.GitDir, tree); err != nil {
		logf.Close()
		return ledger.OutcomeError, "gates clone: " + err.Error(), ""
	}
	ro := []string{filepath.Join(jobDir, "context")}
	if len(r.Files) > 0 {
		ro = append(ro, filesDir(jobDir))
	}
	gateErr := d.Gates(ctx, GateRun{Tree: tree, ReadOnly: ro, Env: templateEnv(r, jobDir), Cmds: r.Tpl.Gates}, logf)
	logf.Close()
	if gateErr != nil {
		return ledger.OutcomeFailed, "gates: " + gateErr.Error(), ""
	}

	if _, err := g.Run(ctx, "bundle", "create", "-q", filepath.Join(runDir, "branch.bundle"), "refs/heads/"+r.Branch, "^"+base); err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	m := push.Manifest{ID: r.ID, Repo: r.Repo, Template: r.Template, Branch: r.Branch, BaseBranch: baseBranch,
		BaseSHA: base, HeadSHA: head, Body: Body(r, res, changes, cost)}
	if err := writeJSON(filepath.Join(runDir, "manifest.json"), m); err != nil {
		return ledger.OutcomeError, err.Error(), ""
	}
	resp, err := d.Push(ctx, r.ID)
	if err != nil {
		return ledger.OutcomeError, "push service: " + err.Error(), ""
	}
	if !resp.OK {
		if resp.Code == push.CodeSecret {
			_ = stop.Write(l.AutoStop(), r.ID+": secret scan finding, nothing pushed", d.now())
		}
		outcome := ledger.OutcomeFailed
		if slices.Contains([]string{push.CodeRefused, push.CodeSecret}, resp.Code) {
			outcome = ledger.OutcomeRefused
		}
		return outcome, "push service " + resp.Code + ": " + resp.Error, ""
	}
	return ledger.OutcomeOK, "pull request opened", resp.PRURL
}

// Body is the pull request description: a fixed shape a person can check
// without reading the diff, and the model's own report fenced as text.
func Body(r task.Resolved, res agent.Result, changes []diffcheck.Change, cost float64) string {
	var files []string
	for _, c := range changes {
		files = append(files, fmt.Sprintf("`%s` (+%d -%d)", c.Path, c.Added, c.Deleted))
	}
	var gates []string
	for _, g := range r.Tpl.Gates {
		gates = append(gates, "`"+strings.Join(g, " ")+"`")
	}
	report := strings.ReplaceAll(res.Text, "```", "'''")
	if len(report) > 4000 {
		report = report[:4000]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Worker task `%s`, template `%s` (%s), run on forge.\n\n", r.ID, r.Template, r.Tpl.Tier)
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Model | `%s`, %d turns |\n", r.Model, res.NumTurns)
	fmt.Fprintf(&b, "| Cost | $%.4f (client estimate; the Console holds the bill) |\n", cost)
	fmt.Fprintf(&b, "| Tokens | in %d, out %d, cache read %d, cache write %d |\n", res.InputTokens, res.OutputTokens, res.CacheRead, res.CacheWrite)
	fmt.Fprintf(&b, "| Changed | %s |\n", strings.Join(files, ", "))
	fmt.Fprintf(&b, "| Gates | %s passed on forge, offline sandbox |\n", strings.Join(gates, ", "))
	b.WriteString("| Secret scan | clean, by the push service before this push |\n\n")
	b.WriteString("### What the model reported\n\n```text\n" + report + "\n```\n\n")
	b.WriteString("### NOT proven\n\n")
	b.WriteString("- Written by a model from a fixed template; no person has read this diff yet.\n")
	b.WriteString("- The gates show the repository's own checks pass, not that every statement is true.\n")
	b.WriteString("- Merged only after review by the supervisor session.\n")
	return b.String()
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o640)
}

// dropSandboxPlaceholders removes the empty files Claude Code's shell sandbox
// leaves in the working tree as mount points for paths it guards (.env*,
// package.json, lock files, .npmrc and the like, measured on the host
// 2026-10-30). Only untracked regular files of size zero go: a new file with
// content is the model's and the diff check judges it like any other, and a
// tracked file the model emptied is a change the check must see.
func dropSandboxPlaceholders(ctx context.Context, g gitx.Git, work string) error {
	out, err := g.Run(ctx, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		p := filepath.Join(work, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() && fi.Size() == 0 {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func filesDir(jobDir string) string { return filepath.Join(jobDir, "files") }

// writeFiles writes the template's files for a job, with {{context}} filled
// in. The model's deny rules keep it from editing them.
func writeFiles(r task.Resolved, jobDir string) error {
	if len(r.Files) == 0 {
		return nil
	}
	dir := filesDir(jobDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	ctxDir := filepath.Join(jobDir, "context")
	for name, content := range r.Files {
		data := strings.ReplaceAll(content, template.ContextPlaceholder, ctxDir)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o640); err != nil {
			return err
		}
	}
	return nil
}

// templateEnv is the template's NAME=path pairs for a job, sorted.
func templateEnv(r task.Resolved, jobDir string) []string {
	var env []string
	for name, value := range r.Env {
		env = append(env, name+"="+strings.Replace(value, template.FilesPlaceholder, filesDir(jobDir), 1))
	}
	slices.Sort(env)
	return env
}
