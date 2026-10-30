package push

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/github"
	"github.com/TAIPANBOX/estate-workers/internal/gitx"
	"github.com/TAIPANBOX/estate-workers/internal/task"
	"github.com/TAIPANBOX/estate-workers/internal/template"
)

var ctx = context.Background()

const jobID = "20261030T120000Z-abcdef"

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

type fakeTokens struct{ calls int }

func (f *fakeTokens) RepoToken(ctx context.Context, owner, repo string) (string, error) {
	f.calls++
	return "test-token", nil
}

type fakePRs struct{ got []github.NewPR }

func (f *fakePRs) CreatePR(ctx context.Context, token, owner, repo string, pr github.NewPR) (string, error) {
	f.got = append(f.got, pr)
	return "https://github.com/" + owner + "/" + repo + "/pull/1", nil
}

type env struct {
	svc      *Service
	prs      *fakePRs
	scans    *int
	remote   string // bare repo playing GitHub
	runs     string
	g        gitx.Git // orchestrator-side git on the job
	base     string
	identity []string
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *env {
	t.Helper()
	bin := gitBin(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	identity := []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x"}

	// The remote, with a main branch carrying the dossier.
	seed := gitx.Git{Bin: bin, Dir: filepath.Join(root, "seed"), Home: home, Env: identity, AllowFile: true}
	os.MkdirAll(seed.Dir, 0o755)
	mustRun(t, seed, "init", "-q", "-b", "main")
	write(t, filepath.Join(seed.Dir, "services/agent-stack-go.md"), "verified_at: aaa\n")
	write(t, filepath.Join(seed.Dir, "README.md"), "index\n")
	mustRun(t, seed, "add", "-A")
	mustRun(t, seed, "commit", "-q", "-m", "seed")
	remote := filepath.Join(root, "remote", "architecture.git")
	mustRun(t, seed, "clone", "-q", "--bare", seed.Dir, remote)

	// The orchestrator's job: a separate git dir under runs, a work tree under jobs.
	runs := filepath.Join(root, "runs")
	gitDir := filepath.Join(runs, jobID, "git")
	work := filepath.Join(root, "jobs", jobID, "repo")
	os.MkdirAll(filepath.Join(runs, jobID), 0o755)
	cl := gitx.Git{Bin: bin, Dir: root, Home: home, AllowFile: true}
	mustRun(t, cl, "clone", "-q", "--separate-git-dir", gitDir, "file://"+remote, work)
	g := gitx.Git{Bin: bin, GitDir: gitDir, WorkTree: work, Dir: work, Home: home, Env: identity, AllowFile: true}
	base := mustOut(t, g, "rev-parse", "HEAD")

	cfg := config.Config{
		Owner: "TAIPANBOX", Repos: []string{"architecture", "agent-stack-go"}, PrivateRepos: []string{"architecture"},
		DayCapUSD: 6, MonthCapUSD: 80, UnverifiedCapUSD: 0.1, MaxConsecutiveFailures: 3,
		RunCapUSD: map[string]float64{"claude-haiku-5-5": 0.5, "claude-sonnet-5-5": 2},
	}
	tpls, err := template.Load()
	if err != nil {
		t.Fatal(err)
	}
	tk := task.Task{ID: jobID, Template: "dossier-bump", Repo: "architecture",
		Params: map[string]string{"service": "agent-stack-go"}, RequestedBy: "supervisor", CreatedAt: time.Now().UTC()}
	writeJSON(t, filepath.Join(runs, jobID, "task.json"), tk)

	scans := 0
	prs := &fakePRs{}
	svc := &Service{
		Cfg: cfg, Templates: tpls, Tokens: &fakeTokens{}, PRs: prs, GitBin: bin,
		RunsDir: runs, CacheDir: filepath.Join(root, "gh", "cache"), ExportDir: filepath.Join(root, "gh", "export"), Home: home,
		RemoteURL: func(owner, repo string) string { return "file://" + filepath.Join(root, "remote", repo+".git") },
		AuthConfig: func(token string) [][2]string {
			return [][2]string{{"http.https://example.invalid/.extraHeader", "X: " + token}}
		},
		Secretscan: func(ctx context.Context, gitDir, rng string) (bool, error) { scans++; return false, nil },
	}
	os.MkdirAll(svc.CacheDir, 0o755)
	os.MkdirAll(svc.ExportDir, 0o755)
	return &env{svc: svc, prs: prs, scans: &scans, remote: remote, runs: runs, g: g, base: base, identity: identity}
}

func mustRun(t *testing.T, g gitx.Git, args ...string) {
	t.Helper()
	if _, err := g.Run(ctx, args...); err != nil {
		t.Fatal(err)
	}
}

func mustOut(t *testing.T, g gitx.Git, args ...string) string {
	t.Helper()
	s, err := g.Out(ctx, args...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(b))
}

const branch = "worker/dossier-bump/" + jobID

// commitAndBundle commits the work tree's state, bundles the branch and
// writes the manifest, as the orchestrator does after a run.
func (e *env) commitAndBundle(t *testing.T, files map[string]string, mutate func(*Manifest)) Manifest {
	t.Helper()
	for p, body := range files {
		write(t, filepath.Join(e.g.WorkTree, p), body)
	}
	mustRun(t, e.g, "add", "-A")
	mustRun(t, e.g, "commit", "-q", "-m", "docs: bump")
	head := mustOut(t, e.g, "rev-parse", "HEAD")
	mustRun(t, e.g, "update-ref", "refs/heads/"+branch, head)
	mustRun(t, e.g, "bundle", "create", "-q", filepath.Join(e.runs, jobID, "branch.bundle"), "refs/heads/"+branch, "^"+e.base)
	m := Manifest{ID: jobID, Repo: "architecture", Template: "dossier-bump", Branch: branch,
		BaseBranch: "main", BaseSHA: e.base, HeadSHA: head, Body: "body"}
	if mutate != nil {
		mutate(&m)
	}
	writeJSON(t, filepath.Join(e.runs, jobID, "manifest.json"), m)
	return m
}

func (e *env) remoteHas(t *testing.T, ref string) bool {
	t.Helper()
	g := gitx.Git{Bin: e.g.Bin, GitDir: e.remote, Dir: e.runs, Home: e.g.Home}
	_, err := g.Run(ctx, "rev-parse", "--verify", "-q", ref)
	return err == nil
}

func TestPushOpensAPullRequestForAnAllowedChange(t *testing.T) {
	e := setup(t)
	m := e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "verified_at: bbb\n"}, nil)
	resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID})
	if !resp.OK {
		t.Fatalf("push refused: %+v", resp)
	}
	if !e.remoteHas(t, "refs/heads/"+branch) {
		t.Fatal("branch not on the remote")
	}
	g := gitx.Git{Bin: e.g.Bin, GitDir: e.remote, Dir: e.runs, Home: e.g.Home}
	if got := mustOut(t, g, "rev-parse", "refs/heads/"+branch); got != m.HeadSHA {
		t.Errorf("remote head %s, want %s", got, m.HeadSHA)
	}
	if len(e.prs.got) != 1 || e.prs.got[0].Base != "main" || e.prs.got[0].Head != branch || !strings.Contains(e.prs.got[0].Title, "agent-stack-go") {
		t.Errorf("pull request %+v", e.prs.got)
	}
	if *e.scans != 1 {
		t.Errorf("secret scan ran %d times", *e.scans)
	}
}

func TestPushRefusesAForbiddenPathAndPushesNothing(t *testing.T) {
	e := setup(t)
	e.commitAndBundle(t, map[string]string{
		"services/agent-stack-go.md":  "verified_at: bbb\n",
		".github/workflows/steal.yml": "on: push\n",
	}, nil)
	resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID})
	if resp.OK || resp.Code != CodeRefused || !strings.Contains(resp.Error, ".github") {
		t.Fatalf("response %+v", resp)
	}
	if e.remoteHas(t, "refs/heads/"+branch) || len(e.prs.got) != 0 || *e.scans != 0 {
		t.Error("something left the host")
	}
}

func TestPushRefusesWhenTheScannerFindsASecret(t *testing.T) {
	e := setup(t)
	e.svc.Secretscan = func(ctx context.Context, gitDir, rng string) (bool, error) { return true, nil }
	e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "x\n"}, nil)
	resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID})
	if resp.OK || resp.Code != CodeSecret {
		t.Fatalf("response %+v", resp)
	}
	if e.remoteHas(t, "refs/heads/"+branch) {
		t.Error("pushed despite a finding")
	}
}

func TestPushRefusesWithoutAScannerOrWhenItFails(t *testing.T) {
	e := setup(t)
	e.svc.Secretscan = nil
	e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "x\n"}, nil)
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK {
		t.Fatal("pushed with no scanner")
	}
	e.svc.Secretscan = func(ctx context.Context, gitDir, rng string) (bool, error) { return false, os.ErrDeadlineExceeded }
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK {
		t.Fatal("pushed when the scan failed")
	}
}

func TestPushRefusesABaseThatIsNotOnTheRemote(t *testing.T) {
	e := setup(t)
	// A local commit carrying a forbidden file becomes the claimed base, so
	// base..head would look clean while the branch smuggles the workflow.
	write(t, filepath.Join(e.g.WorkTree, ".github/workflows/steal.yml"), "on: push\n")
	mustRun(t, e.g, "add", "-A")
	mustRun(t, e.g, "commit", "-q", "-m", "smuggle")
	forged := mustOut(t, e.g, "rev-parse", "HEAD")
	e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "x\n"}, func(m *Manifest) { m.BaseSHA = forged })
	// Re-bundle against the real base so the bundle itself is complete.
	mustRun(t, e.g, "bundle", "create", "-q", filepath.Join(e.runs, jobID, "branch.bundle"), "refs/heads/"+branch, "^"+e.base)
	resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID})
	if resp.OK || !strings.Contains(resp.Error, "not on main") {
		t.Fatalf("response %+v", resp)
	}
	if e.remoteHas(t, "refs/heads/"+branch) {
		t.Error("pushed on a forged base")
	}
}

func TestPushRefusesAManifestHeadThatIsNotTheBundledBranch(t *testing.T) {
	e := setup(t)
	// Two allowed commits; the bundle's branch points at the second, the
	// manifest names the first. Every other check would pass on the first.
	write(t, filepath.Join(e.g.WorkTree, "services/agent-stack-go.md"), "first\n")
	mustRun(t, e.g, "add", "-A")
	mustRun(t, e.g, "commit", "-q", "-m", "first")
	first := mustOut(t, e.g, "rev-parse", "HEAD")
	e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "second\n"}, func(m *Manifest) { m.HeadSHA = first })
	resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID})
	if resp.OK || !strings.Contains(resp.Error, "bundle head") {
		t.Fatalf("response %+v", resp)
	}
	if e.remoteHas(t, "refs/heads/"+branch) {
		t.Error("pushed a head the bundle did not carry")
	}
}

func TestPushRefusesManifestsThatDisagree(t *testing.T) {
	cases := map[string]func(*Manifest){
		"branch":      func(m *Manifest) { m.Branch = "worker/dossier-bump/other" },
		"head":        func(m *Manifest) { m.HeadSHA = strings.Repeat("a", 40) },
		"short sha":   func(m *Manifest) { m.BaseSHA = "abc" },
		"repo":        func(m *Manifest) { m.Repo = "agent-stack-go" },
		"template":    func(m *Manifest) { m.Template = "credit-probe" },
		"base branch": func(m *Manifest) { m.BaseBranch = "release" },
	}
	for name, mutate := range cases {
		e := setup(t)
		e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "x\n"}, mutate)
		if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK {
			t.Errorf("%s: pushed", name)
		}
		if e.remoteHas(t, "refs/heads/"+branch) {
			t.Errorf("%s: branch on the remote", name)
		}
	}
}

func TestPushRefusesAManifestWithUnknownFields(t *testing.T) {
	e := setup(t)
	e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "x\n"}, nil)
	p := filepath.Join(e.runs, jobID, "manifest.json")
	b, _ := os.ReadFile(p)
	write(t, p, strings.Replace(string(b), `"id"`, `"force":true,"id"`, 1))
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK {
		t.Error("pushed with an unknown manifest field")
	}
}

func TestBadRequestsAreRefusedBeforeAnyWork(t *testing.T) {
	e := setup(t)
	for _, req := range []Request{
		{Op: OpPush, JobID: "../../etc"},
		{Op: OpPush, JobID: ""},
		{Op: "merge", JobID: jobID},
	} {
		if resp := e.svc.Handle(ctx, req); resp.OK {
			t.Errorf("%+v accepted", req)
		}
	}
	if e.svc.Tokens.(*fakeTokens).calls != 0 {
		t.Error("a token was minted for a bad request")
	}
}

func TestATaskForADeniedRepoIsRefused(t *testing.T) {
	e := setup(t)
	tk := task.Task{ID: jobID, Template: "dossier-bump", Repo: "hermes-openwrt",
		Params: map[string]string{"service": "agent-stack-go"}, RequestedBy: "supervisor"}
	writeJSON(t, filepath.Join(e.runs, jobID, "task.json"), tk)
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK || resp.Code != CodeRefused {
		t.Errorf("response %+v", resp)
	}
}

func TestExportBundlesAPrivateRepoAndRefusesAPublicOne(t *testing.T) {
	e := setup(t)
	resp := e.svc.Handle(ctx, Request{Op: OpExport, JobID: jobID})
	if !resp.OK {
		t.Fatalf("export: %+v", resp)
	}
	clone := filepath.Join(t.TempDir(), "c")
	g := gitx.Git{Bin: e.g.Bin, Dir: filepath.Dir(clone), Home: e.g.Home, AllowFile: true}
	mustRun(t, g, "clone", "-q", resp.Bundle, clone)
	if _, err := os.Stat(filepath.Join(clone, "services/agent-stack-go.md")); err != nil {
		t.Errorf("clone from the export misses the dossier: %v", err)
	}
	e.svc.Cfg.PrivateRepos = nil
	if resp := e.svc.Handle(ctx, Request{Op: OpExport, JobID: jobID}); resp.OK {
		t.Error("a public repo was exported")
	}
}

func TestServeAnswersOverTheSocket(t *testing.T) {
	e := setup(t)
	dir, err := os.MkdirTemp("/tmp", "cw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "push.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.svc.Serve(cctx, ln) }()

	e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "x\n"}, nil)
	tctx, tcancel := context.WithTimeout(ctx, 30*time.Second)
	defer tcancel()
	resp, err := Call(tctx, sock, Request{Op: OpPush, JobID: jobID})
	if err != nil || !resp.OK || !strings.HasPrefix(resp.PRURL, "https://") {
		t.Fatalf("Call: %+v %v", resp, err)
	}
	for _, raw := range []string{"{not json}\n", strings.Repeat("x", MaxRequest+10) + "\n", `{"op":"push","job_id":"` + jobID + `","x":1}` + "\n"} {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte(raw))
		var r Response
		json.NewDecoder(conn).Decode(&r)
		conn.Close()
		if r.OK || r.Code != CodeBadRequest {
			t.Errorf("raw %.30q answered %+v", raw, r)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
	if _, err := Call(ctx, filepath.Join(dir, "missing.sock"), Request{}); err == nil {
		t.Error("Call to a missing socket succeeded")
	}
}

type failingTokens struct{}

func (failingTokens) RepoToken(context.Context, string, string) (string, error) {
	return "", os.ErrPermission
}

func TestTokenAndRemoteFailuresAreReportedNotPushed(t *testing.T) {
	e := setup(t)
	e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "x\n"}, nil)
	e.svc.Tokens = failingTokens{}
	for _, op := range []string{OpPush, OpExport} {
		if resp := e.svc.Handle(ctx, Request{Op: op, JobID: jobID}); resp.OK || resp.Code != CodeGitHub {
			t.Errorf("%s with no token: %+v", op, resp)
		}
	}
	e.svc.Tokens = &fakeTokens{}
	e.svc.RemoteURL = func(owner, repo string) string { return "file:///nonexistent/" + repo + ".git" }
	for _, op := range []string{OpPush, OpExport} {
		if resp := e.svc.Handle(ctx, Request{Op: op, JobID: jobID}); resp.OK || resp.Code != CodeGit {
			t.Errorf("%s with no remote: %+v", op, resp)
		}
	}
}

func TestResolveRefusesMismatchedOrProbeTasks(t *testing.T) {
	e := setup(t)
	writeJSON(t, filepath.Join(e.runs, jobID, "task.json"), task.Task{ID: "20261030T120000Z-zzzzzz", Template: "dossier-bump",
		Repo: "architecture", Params: map[string]string{"service": "agent-stack-go"}, RequestedBy: "supervisor"})
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK || !strings.Contains(resp.Error, "names id") {
		t.Errorf("mismatched id: %+v", resp)
	}
	writeJSON(t, filepath.Join(e.runs, jobID, "task.json"), task.Task{ID: jobID, Template: "credit-probe", RequestedBy: "supervisor"})
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK || !strings.Contains(resp.Error, "touches no repository") {
		t.Errorf("probe: %+v", resp)
	}
	os.Remove(filepath.Join(e.runs, jobID, "task.json"))
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK {
		t.Error("no task file accepted")
	}
	write(t, filepath.Join(e.runs, jobID, "task.json"), "{")
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK {
		t.Error("a broken task file accepted")
	}
}

func TestPushRefusesAMissingOrOversizeManifestAndAPRFailure(t *testing.T) {
	e := setup(t)
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK {
		t.Error("pushed with no manifest")
	}
	write(t, filepath.Join(e.runs, jobID, "manifest.json"), strings.Repeat(" ", MaxManifest+1))
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK || !strings.Contains(resp.Error, "too large") {
		t.Errorf("oversize manifest: %+v", resp)
	}
	e.commitAndBundle(t, map[string]string{"services/agent-stack-go.md": "x\n"}, func(m *Manifest) { m.Body = strings.Repeat("b", MaxBody+10) })
	e.svc.PRs = failingPRs{}
	if resp := e.svc.Handle(ctx, Request{Op: OpPush, JobID: jobID}); resp.OK || resp.Code != CodeGitHub {
		t.Errorf("PR failure: %+v", resp)
	}
}

type failingPRs struct{}

func (failingPRs) CreatePR(_ context.Context, _, _, _ string, pr github.NewPR) (string, error) {
	if len(pr.Body) > MaxBody {
		return "", os.ErrInvalid
	}
	return "", os.ErrPermission
}

func TestDefaultsPointAtGitHubOverHTTPS(t *testing.T) {
	s := &Service{}
	if got := s.remote("TAIPANBOX", "qryx"); got != "https://github.com/TAIPANBOX/qryx.git" {
		t.Errorf("remote %q", got)
	}
	if got := s.auth("tok"); len(got) != 1 || !strings.HasPrefix(got[0][0], "http.https://github.com/") {
		t.Errorf("auth %v", got)
	}
}
