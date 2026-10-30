package drain

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TAIPANBOX/estate-workers/internal/agent"
	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/diffcheck"
	"github.com/TAIPANBOX/estate-workers/internal/github"
	"github.com/TAIPANBOX/estate-workers/internal/gitx"
	"github.com/TAIPANBOX/estate-workers/internal/ledger"
	"github.com/TAIPANBOX/estate-workers/internal/push"
	"github.com/TAIPANBOX/estate-workers/internal/task"
	"github.com/TAIPANBOX/estate-workers/internal/template"
)

var bg = context.Background()

func gitBin(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/git", "/opt/homebrew/bin/git"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no git")
	return ""
}

type fakeTokens struct{}

func (fakeTokens) RepoToken(context.Context, string, string) (string, error) { return "tok", nil }

type fakePRs struct{ got []github.NewPR }

func (f *fakePRs) CreatePR(_ context.Context, _, owner, repo string, pr github.NewPR) (string, error) {
	f.got = append(f.got, pr)
	return "https://github.com/" + owner + "/" + repo + "/pull/7", nil
}

type harness struct {
	d        Deps
	root     string
	remote   string
	prs      *fakePRs
	pushes   int
	agentRun []agent.Run
	mu       sync.Mutex
	edit     func(dir string)
	result   agent.Result
	agentErr error
	block    bool
	gateErr  error
	pushResp *push.Response
	identity []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	bin := gitBin(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	identity := []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x"}
	seed := gitx.Git{Bin: bin, Dir: filepath.Join(root, "seed"), Home: home, Env: identity}
	os.MkdirAll(seed.Dir, 0o755)
	if _, err := seed.Run(bg, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(seed.Dir, "services"), 0o755)
	os.WriteFile(filepath.Join(seed.Dir, "services", "agent-stack-go.md"), []byte("verified_at: aaa\n"), 0o644)
	os.WriteFile(filepath.Join(seed.Dir, "README.md"), []byte("index\n"), 0o644)
	os.WriteFile(filepath.Join(seed.Dir, "CLAUDE.md"), []byte("repo rules\n"), 0o644)
	seed.Run(bg, "add", "-A")
	if _, err := seed.Run(bg, "commit", "-q", "-m", "seed"); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "remote", "architecture.git")
	seed.AllowFile = true
	if _, err := seed.Run(bg, "clone", "-q", "--bare", seed.Dir, remote); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Owner: "TAIPANBOX", Repos: []string{"architecture", "agent-stack-go"}, PrivateRepos: []string{"architecture"},
		DayCapUSD: 6, MonthCapUSD: 80, BillingVerified: true, UnverifiedCapUSD: 0.1, MaxConsecutiveFailures: 3,
		RunCapUSD:  map[string]float64{"claude-haiku-5-5": 0.5, "claude-sonnet-5-5": 2},
		CommitName: "taipanbox-workers[bot]", CommitEmail: "1+taipanbox-workers[bot]@users.noreply.github.com",
		Paths: config.Paths{State: filepath.Join(root, "state"), ManualStop: filepath.Join(root, "STOP")},
	}
	tpls, err := template.Load()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{root: root, remote: remote, prs: &fakePRs{}, identity: identity,
		result: agent.Result{CostKnown: true, CostUSD: 0.12, NumTurns: 3, Text: "Updated verified_at."}}
	svc := &push.Service{
		Cfg: cfg, Templates: tpls, Tokens: fakeTokens{}, PRs: h.prs, GitBin: bin,
		RunsDir: filepath.Join(cfg.Paths.State, "runs"), CacheDir: filepath.Join(root, "gh", "cache"),
		ExportDir: filepath.Join(root, "gh", "export"), Home: home,
		RemoteURL:  func(owner, repo string) string { return "file://" + filepath.Join(root, "remote", repo+".git") },
		AuthConfig: func(string) [][2]string { return nil },
		Secretscan: func(context.Context, string, string) (bool, error) { return false, nil },
	}
	os.MkdirAll(svc.CacheDir, 0o755)
	os.MkdirAll(svc.ExportDir, 0o755)
	h.d = Deps{
		Cfg: cfg, Templates: tpls, GitBin: bin, ClaudeBin: "/usr/bin/claude", GoRoot: "/usr/local/go",
		StopPoll: 20 * time.Millisecond,
		Clone: func(ctx context.Context, r task.Resolved, gitDir, workTree string) error {
			g := gitx.Git{Bin: bin, Dir: filepath.Dir(workTree), Home: home, AllowFile: true}
			os.Remove(workTree)
			_, err := g.Run(ctx, "clone", "-q", "--separate-git-dir", gitDir, "file://"+remote, workTree)
			return err
		},
		CloneContext: func(ctx context.Context, repo, dir string) error { return os.MkdirAll(dir, 0o755) },
		Agent: func(ctx context.Context, run agent.Run) (agent.Result, error) {
			h.mu.Lock()
			h.agentRun = append(h.agentRun, run)
			h.mu.Unlock()
			if h.block {
				<-ctx.Done()
				return agent.Result{CostKnown: true, CostUSD: 0.03}, ctx.Err()
			}
			if h.edit != nil {
				h.edit(run.Dir)
			}
			return h.result, h.agentErr
		},
		Gates: func(ctx context.Context, repoDir, gitDir string, cmds [][]string, log io.Writer) error {
			io.WriteString(log, "gates ran\n")
			return h.gateErr
		},
		Push: func(ctx context.Context, id string) (push.Response, error) {
			h.pushes++
			if h.pushResp != nil {
				return *h.pushResp, nil
			}
			return svc.Handle(ctx, push.Request{Op: push.OpPush, JobID: id}), nil
		},
		ReadKey: func() (string, error) { return "sk-test-key", nil },
	}
	return h
}

func (h *harness) enqueue(t *testing.T, tk task.Task) {
	t.Helper()
	if tk.ID == "" {
		tk.ID = "20261030T120000Z-abcdef"
	}
	if tk.RequestedBy == "" {
		tk.RequestedBy = "supervisor"
	}
	if err := Enqueue(h.d, tk); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

func dossier() task.Task {
	return task.Task{Template: "dossier-bump", Repo: "architecture", Params: map[string]string{"service": "agent-stack-go"}}
}

func (h *harness) sums(t *testing.T) ledger.Sums {
	t.Helper()
	s, err := ledger.SumFile(h.d.layout().Ledger(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestOnceTakesADossierTaskToAPullRequest(t *testing.T) {
	h := newHarness(t)
	h.edit = func(dir string) {
		os.WriteFile(filepath.Join(dir, "services", "agent-stack-go.md"), []byte("verified_at: bbb\n"), 0o644)
	}
	h.enqueue(t, dossier())
	rep, err := Once(bg, h.d)
	if err != nil || rep.Outcome != ledger.OutcomeOK || !strings.HasPrefix(rep.PRURL, "https://github.com/TAIPANBOX/architecture/pull/") {
		t.Fatalf("report %+v err %v", rep, err)
	}
	l := h.d.layout()
	if !exists(filepath.Join(l.Done(), rep.ID+".json")) {
		t.Error("task not in done")
	}
	if s := h.sums(t); s.Total != 0.12 {
		t.Errorf("ledger total %v, want 0.12", s.Total)
	}
	run := h.agentRun[0]
	if run.Args[0] != "--bare" || run.Args[1] != "-p" {
		t.Errorf("args %v", run.Args[:2])
	}
	if !slices.Contains(run.Args, filepath.Join(run.Dir, "CLAUDE.md")) {
		t.Error("the repository's CLAUDE.md was not passed as rules")
	}
	if !slices.Contains(run.Env, "ANTHROPIC_API_KEY=sk-test-key") {
		t.Error("key not in the run's environment")
	}
	g := gitx.Git{Bin: h.d.GitBin, GitDir: h.remote, Dir: h.root, Home: h.root}
	msg, err := g.Out(bg, "log", "-1", "--format=%B%n%an", "refs/heads/worker/dossier-bump/"+rep.ID)
	if err != nil {
		t.Fatalf("branch not on the remote: %v", err)
	}
	if !strings.Contains(msg, "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>") || !strings.Contains(msg, "taipanbox-workers[bot]") {
		t.Errorf("commit %q", msg)
	}
	if len(h.prs.got) != 1 || !strings.Contains(h.prs.got[0].Body, "### NOT proven") {
		t.Errorf("pull request %+v", h.prs.got)
	}
}

func TestOnceIsIdleWhenTheQueueIsEmptyOrStopped(t *testing.T) {
	h := newHarness(t)
	if rep, err := Once(bg, h.d); err != nil || !rep.Idle {
		t.Fatalf("empty queue: %+v %v", rep, err)
	}
	h.enqueue(t, dossier())
	os.WriteFile(h.d.Cfg.Paths.ManualStop, []byte("stop requested"), 0o600)
	rep, err := Once(bg, h.d)
	if err != nil || !rep.Idle || !strings.Contains(rep.Reason, "stop requested") {
		t.Fatalf("stopped: %+v %v", rep, err)
	}
	if len(h.agentRun) != 0 {
		t.Error("the model ran while stopped")
	}
	if !exists(filepath.Join(h.d.layout().Incoming(), "20261030T120000Z-abcdef.json")) {
		t.Error("the task left the queue while stopped")
	}
}

func TestUnverifiedBillingDefersARunAndKeepsTheTask(t *testing.T) {
	h := newHarness(t)
	h.d.Cfg.BillingVerified = false
	h.enqueue(t, dossier())
	rep, err := Once(bg, h.d)
	if err != nil || rep.Outcome != OutcomeDeferred || !strings.Contains(rep.Reason, "billing") {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if len(h.agentRun) != 0 || h.sums(t).Total != 0 {
		t.Error("spent before billing was verified")
	}
	if !exists(filepath.Join(h.d.layout().Incoming(), rep.ID+".json")) {
		t.Error("deferred task not back in the queue")
	}
}

func TestTheCreditProbeRunsUnderTheCeiling(t *testing.T) {
	h := newHarness(t)
	h.d.Cfg.BillingVerified = false
	h.result = agent.Result{CostKnown: true, CostUSD: 0.06, Text: "ok"}
	h.enqueue(t, task.Task{Template: "credit-probe"})
	rep, err := Once(bg, h.d)
	if err != nil || rep.Outcome != ledger.OutcomeOK {
		t.Fatalf("report %+v err %v", rep, err)
	}
	run := h.agentRun[0]
	if i := slices.Index(run.Args, "--disallowedTools"); i < 0 || run.Args[i+1] != "*" {
		t.Errorf("probe did not remove every tool: %v", run.Args)
	}
	prompt, _ := os.ReadFile(run.PromptFile)
	if len(prompt) < 20000 || !strings.HasSuffix(string(prompt), "Reply with exactly: ok") {
		t.Errorf("probe prompt %d bytes", len(prompt))
	}
	if h.pushes != 0 {
		t.Error("a probe pushed")
	}
	// A second probe would take the total past ten cents: deferred.
	h.enqueue(t, task.Task{ID: "20261030T120001Z-bbbbbb", Template: "credit-probe"})
	if rep, _ := Once(bg, h.d); rep.Outcome != OutcomeDeferred {
		t.Errorf("second probe %+v", rep)
	}
}

func TestAForbiddenChangeIsRefusedAndStopsTheWorkers(t *testing.T) {
	h := newHarness(t)
	h.edit = func(dir string) {
		os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755)
		os.WriteFile(filepath.Join(dir, ".github", "workflows", "x.yml"), []byte("on: push\n"), 0o644)
	}
	h.enqueue(t, dossier())
	rep, err := Once(bg, h.d)
	if err != nil || rep.Outcome != ledger.OutcomeRefused {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if h.pushes != 0 {
		t.Error("a refused diff reached the push service")
	}
	if !exists(h.d.layout().AutoStop()) {
		t.Error("no automatic stop after a refused diff")
	}
	if !exists(filepath.Join(h.d.layout().Failed(), rep.ID+".json")) {
		t.Error("task not in failed")
	}
}

func TestNoChangeAndGateFailureDoNotPush(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, dossier())
	if rep, _ := Once(bg, h.d); rep.Outcome != ledger.OutcomeNoChange {
		t.Errorf("no edit: %+v", rep)
	}
	h.edit = func(dir string) {
		os.WriteFile(filepath.Join(dir, "README.md"), []byte("index v2\n"), 0o644)
	}
	h.gateErr = errors.New("make gates: exit 2")
	h.enqueue(t, task.Task{ID: "20261030T120001Z-bbbbbb", Template: "dossier-bump", Repo: "architecture", Params: map[string]string{"service": "agent-stack-go"}})
	rep, _ := Once(bg, h.d)
	if rep.Outcome != ledger.OutcomeFailed || !strings.Contains(rep.Reason, "gates") {
		t.Errorf("gate failure: %+v", rep)
	}
	if h.pushes != 0 {
		t.Error("pushed")
	}
}

func TestAStopFileCancelsARunningModelAndTheSpendIsStillRecorded(t *testing.T) {
	h := newHarness(t)
	h.block = true
	h.enqueue(t, dossier())
	time.AfterFunc(150*time.Millisecond, func() { os.WriteFile(h.d.Cfg.Paths.ManualStop, []byte("stop requested"), 0o600) })
	start := time.Now()
	// The deadline only bounds the test: a stop that does not cancel the
	// model would otherwise hang until the test binary times out.
	ctx, cancel := context.WithTimeout(bg, 8*time.Second)
	defer cancel()
	rep, err := Once(ctx, h.d)
	if err != nil || rep.Outcome != ledger.OutcomeStopped {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("stop took %v; the stop file did not cancel the model", time.Since(start))
	}
	if s := h.sums(t); s.Total != 0.03 {
		t.Errorf("spend %v, want 0.03", s.Total)
	}
}

func TestAnUnknownCostIsChargedAtTheRunCap(t *testing.T) {
	h := newHarness(t)
	h.result = agent.Result{CostKnown: false}
	h.enqueue(t, dossier())
	Once(bg, h.d)
	if s := h.sums(t); s.Total != 2 {
		t.Errorf("charged %v, want the run cap 2", s.Total)
	}
}

func TestAnAgentErrorFailsTheTask(t *testing.T) {
	h := newHarness(t)
	h.agentErr = errors.New("exit status 1")
	h.enqueue(t, dossier())
	if rep, _ := Once(bg, h.d); rep.Outcome != ledger.OutcomeFailed {
		t.Errorf("report %+v", rep)
	}
	h.agentErr = nil
	h.result = agent.Result{CostKnown: true, CostUSD: 0.1, IsError: true, Subtype: "error_max_turns"}
	h.enqueue(t, task.Task{ID: "20261030T120001Z-bbbbbb", Template: "dossier-bump", Repo: "architecture", Params: map[string]string{"service": "agent-stack-go"}})
	if rep, _ := Once(bg, h.d); rep.Outcome != ledger.OutcomeFailed || !strings.Contains(rep.Reason, "error_max_turns") {
		t.Errorf("report %+v", rep)
	}
}

func TestAPushServiceSecretFindingStops(t *testing.T) {
	h := newHarness(t)
	h.edit = func(dir string) { os.WriteFile(filepath.Join(dir, "README.md"), []byte("v2\n"), 0o644) }
	h.pushResp = &push.Response{Code: push.CodeSecret, Error: "finding"}
	h.enqueue(t, dossier())
	if rep, _ := Once(bg, h.d); rep.Outcome != ledger.OutcomeRefused {
		t.Errorf("report %+v", rep)
	}
	if !exists(h.d.layout().AutoStop()) {
		t.Error("no stop after a secret finding")
	}
}

func TestAnInvalidQueueFileIsRefused(t *testing.T) {
	h := newHarness(t)
	l := h.d.layout()
	l.ensure()
	os.WriteFile(filepath.Join(l.Incoming(), "20261030T120000Z-abcdef.json"), []byte(`{"id":"20261030T120000Z-abcdef","template":"shell","requested_by":"x"}`), 0o640)
	rep, err := Once(bg, h.d)
	if err != nil || rep.Outcome != ledger.OutcomeRefused || len(h.agentRun) != 0 {
		t.Fatalf("report %+v err %v", rep, err)
	}
	os.WriteFile(filepath.Join(l.Incoming(), "junk.json"), []byte(`{}`), 0o640)
	if rep, _ := Once(bg, h.d); rep.Outcome != ledger.OutcomeRefused {
		t.Errorf("junk name: %+v", rep)
	}
}

func TestAnInterruptedRunIsChargedAndClosedAtTheNextStart(t *testing.T) {
	h := newHarness(t)
	l := h.d.layout()
	l.ensure()
	id := "20261030T120000Z-abcdef"
	tk := dossier()
	tk.ID, tk.RequestedBy = id, "supervisor"
	writeJSON(filepath.Join(l.Running(), id+".json"), tk)
	os.MkdirAll(l.Run(id), 0o750)
	os.WriteFile(filepath.Join(l.Run(id), "transcript.jsonl"), []byte(`{"type":"system"}`+"\n"), 0o640)
	rep, err := Once(bg, h.d)
	if err != nil || !rep.Idle {
		t.Fatalf("report %+v err %v", rep, err)
	}
	s := h.sums(t)
	if s.Total != 2 || s.ConsecutiveFailures != 1 {
		t.Errorf("recovered spend %v failures %d", s.Total, s.ConsecutiveFailures)
	}
	if !exists(filepath.Join(l.Failed(), id+".json")) {
		t.Error("interrupted task not in failed")
	}
}

func TestEnqueueRefusesInvalidAndDuplicateTasks(t *testing.T) {
	h := newHarness(t)
	bad := dossier()
	bad.ID, bad.RequestedBy, bad.Repo = "20261030T120000Z-abcdef", "supervisor", "hermes-openwrt"
	if err := Enqueue(h.d, bad); err == nil {
		t.Error("a task for a denied repo was queued")
	}
	h.enqueue(t, dossier())
	dup := dossier()
	dup.ID, dup.RequestedBy = "20261030T120000Z-abcdef", "supervisor"
	if err := Enqueue(h.d, dup); err == nil {
		t.Error("a duplicate id was queued")
	}
}

func TestBodyHasTheFixedShapeAndFencesTheModelReport(t *testing.T) {
	cfg := config.Config{Owner: "TAIPANBOX", Repos: []string{"architecture", "qryx"}, DayCapUSD: 1, MonthCapUSD: 1,
		RunCapUSD: map[string]float64{"claude-haiku-5-5": 0.5, "claude-sonnet-5-5": 2}, MaxConsecutiveFailures: 1}
	tpls, _ := template.Load()
	tk := task.Task{ID: "20261030T120000Z-abcdef", Template: "dossier-bump", Repo: "architecture", Params: map[string]string{"service": "qryx"}, RequestedBy: "s"}
	r, err := tk.Validate(cfg, tpls)
	if err != nil {
		t.Fatal(err)
	}
	b := Body(r, agent.Result{Text: "done ```\nescape``` @someone", NumTurns: 4}, []diffcheck.Change{{Path: "services/qryx.md", Added: 3, Deleted: 1}}, 0.5)
	for _, want := range []string{"| Model | `claude-sonnet-5-5`, 4 turns |", "| Cost | $0.5000", "`services/qryx.md` (+3 -1)", "`make gates`", "### NOT proven", "```text\n"} {
		if !strings.Contains(b, want) {
			t.Errorf("body misses %q", want)
		}
	}
	if strings.Count(b, "```") != 2 {
		t.Errorf("the model's report broke out of its fence:\n%s", b)
	}
	if strings.Contains(b, "—") {
		t.Error("long dash in the body")
	}
}

func TestInfrastructureFailuresCloseTheTaskAsAnError(t *testing.T) {
	cases := map[string]func(h *harness){
		"clone": func(h *harness) {
			h.d.Clone = func(context.Context, task.Resolved, string, string) error { return errors.New("no network") }
		},
		"context": func(h *harness) {
			h.d.CloneContext = func(context.Context, string, string) error { return errors.New("no network") }
		},
		"key": func(h *harness) { h.d.ReadKey = func() (string, error) { return "", errors.New("no credential") } },
		"prefetch": func(h *harness) {
			h.d.Prefetch = func(context.Context, string) error { return errors.New("proxy down") }
		},
		"push transport": func(h *harness) {
			h.edit = func(dir string) { os.WriteFile(filepath.Join(dir, "README.md"), []byte("v2\n"), 0o644) }
			h.d.Push = func(context.Context, string) (push.Response, error) {
				return push.Response{}, errors.New("socket gone")
			}
		},
	}
	for name, setup := range cases {
		h := newHarness(t)
		setup(h)
		h.enqueue(t, dossier())
		rep, err := Once(bg, h.d)
		if err != nil || rep.Outcome != ledger.OutcomeError {
			t.Errorf("%s: report %+v err %v", name, rep, err)
		}
	}
}

func TestAPushFailureThatIsNotARefusalFailsWithoutStopping(t *testing.T) {
	h := newHarness(t)
	h.edit = func(dir string) { os.WriteFile(filepath.Join(dir, "README.md"), []byte("v2\n"), 0o644) }
	h.pushResp = &push.Response{Code: push.CodeGit, Error: "remote hung up"}
	h.enqueue(t, dossier())
	if rep, _ := Once(bg, h.d); rep.Outcome != ledger.OutcomeFailed {
		t.Errorf("report %+v", rep)
	}
	if exists(h.d.layout().AutoStop()) {
		t.Error("a transient push failure stopped the workers")
	}
}

func TestAnUnreadableLedgerDefersAndStops(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, dossier())
	os.WriteFile(h.d.layout().Ledger(), []byte(strings.Repeat("x", ledger.MaxLine+10)), 0o640)
	rep, err := Once(bg, h.d)
	if err != nil || rep.Outcome != OutcomeDeferred || !strings.Contains(rep.Reason, "ledger") {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if !exists(h.d.layout().AutoStop()) || len(h.agentRun) != 0 {
		t.Error("ran on an unreadable ledger, or did not stop")
	}
}

func TestATaskFileHoldingAnotherIDIsRefused(t *testing.T) {
	h := newHarness(t)
	l := h.d.layout()
	l.ensure()
	tk := dossier()
	tk.ID, tk.RequestedBy = "20261030T120000Z-zzzzzz", "supervisor"
	writeJSON(filepath.Join(l.Incoming(), "20261030T120000Z-abcdef.json"), tk)
	if rep, _ := Once(bg, h.d); rep.Outcome != ledger.OutcomeRefused || !strings.Contains(rep.Reason, "holds task") {
		t.Errorf("report %+v", rep)
	}
}

func TestIdentityHasDefaults(t *testing.T) {
	id := Deps{}.identity()
	if !slices.Contains(id, "GIT_AUTHOR_NAME=estate-workers") || !slices.Contains(id, "GIT_COMMITTER_EMAIL=estate-workers@users.noreply.github.com") {
		t.Errorf("identity %v", id)
	}
	if (Deps{}).now().IsZero() {
		t.Error("now is zero")
	}
}
