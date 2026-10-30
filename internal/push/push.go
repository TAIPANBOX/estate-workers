// Package push is the only part of the workers that holds a GitHub
// credential. It runs as its own user, listens on a unix socket, and does
// two things on request: export a private repository for the orchestrator
// to clone, and push one job's branch and open its pull request.
//
// It trusts nothing the orchestrator's user could have written after the
// model ran. It re-reads the task, re-derives the rules from its own
// compiled templates, imports the branch from a bundle into its own
// repository, checks the base is on the remote default branch, re-checks
// the diff and scans it for secrets before pushing.
package push

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/diffcheck"
	"github.com/TAIPANBOX/estate-workers/internal/github"
	"github.com/TAIPANBOX/estate-workers/internal/gitx"
	"github.com/TAIPANBOX/estate-workers/internal/task"
	"github.com/TAIPANBOX/estate-workers/internal/template"
)

const (
	OpPush   = "push"
	OpExport = "export"

	CodeBadRequest = "bad-request"
	CodeRefused    = "refused"
	CodeSecret     = "secret"
	CodeGitHub     = "github"
	CodeGit        = "git"

	// MaxRequest bounds one request line on the socket.
	MaxRequest = 4 << 10
	// MaxManifest bounds the manifest file.
	MaxManifest = 256 << 10
	// MaxBody bounds the pull request body.
	MaxBody = 60000
)

var (
	idRe     = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-z0-9]{6}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
)

type Request struct {
	Op    string `json:"op"`
	JobID string `json:"job_id"`
}

type Response struct {
	OK     bool   `json:"ok"`
	PRURL  string `json:"pr_url,omitempty"`
	Bundle string `json:"bundle,omitempty"`
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Manifest is written by the orchestrator next to the job's bundle.
type Manifest struct {
	ID         string `json:"id"`
	Repo       string `json:"repo"`
	Template   string `json:"template"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	BaseSHA    string `json:"base_sha"`
	HeadSHA    string `json:"head_sha"`
	Body       string `json:"body"`
}

type Tokens interface {
	RepoToken(ctx context.Context, owner, repo string) (string, error)
}

type PRs interface {
	CreatePR(ctx context.Context, token, owner, repo string, pr github.NewPR) (string, error)
}

type Service struct {
	Cfg       config.Config
	Templates map[string]template.Template
	Tokens    Tokens
	PRs       PRs
	GitBin    string
	RunsDir   string
	CacheDir  string
	ExportDir string
	Home      string
	// RemoteURL and AuthConfig default to github.com over https.
	RemoteURL  func(owner, repo string) string
	AuthConfig func(token string) [][2]string
	// Secretscan reports whether the range has findings.
	Secretscan func(ctx context.Context, gitDir, rng string) (bool, error)

	mu sync.Mutex
}

func fail(code, format string, a ...any) Response {
	return Response{Code: code, Error: fmt.Sprintf(format, a...)}
}

func (s *Service) remote(owner, repo string) string {
	if s.RemoteURL != nil {
		return s.RemoteURL(owner, repo)
	}
	return "https://github.com/" + owner + "/" + repo + ".git"
}

func (s *Service) auth(token string) [][2]string {
	if s.AuthConfig != nil {
		return s.AuthConfig(token)
	}
	return github.AuthConfig("github.com", token)
}

// Handle serves one request. Requests are handled one at a time.
func (s *Service) Handle(ctx context.Context, req Request) Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !idRe.MatchString(req.JobID) {
		return fail(CodeBadRequest, "job id %q is not a task id", req.JobID)
	}
	r, err := s.resolve(req.JobID)
	if err != nil {
		return fail(CodeRefused, "%v", err)
	}
	switch req.Op {
	case OpPush:
		return s.push(ctx, r)
	case OpExport:
		return s.export(ctx, r)
	}
	return fail(CodeBadRequest, "unknown op %q", req.Op)
}

// resolve re-validates the job's task with this service's own config and
// compiled templates.
func (s *Service) resolve(id string) (task.Resolved, error) {
	f, err := os.Open(filepath.Join(s.RunsDir, id, "task.json"))
	if err != nil {
		return task.Resolved{}, fmt.Errorf("task: %w", err)
	}
	defer f.Close()
	t, err := task.Parse(f)
	if err != nil {
		return task.Resolved{}, err
	}
	if t.ID != id {
		return task.Resolved{}, fmt.Errorf("task file names id %q, request names %q", t.ID, id)
	}
	r, err := t.Validate(s.Cfg, s.Templates)
	if err != nil {
		return task.Resolved{}, err
	}
	if r.Tpl.Kind != template.KindRepo {
		return task.Resolved{}, fmt.Errorf("template %q touches no repository", r.Tpl.Name)
	}
	return r, nil
}

func (s *Service) readManifest(id string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(s.RunsDir, id, "manifest.json"))
	if err != nil {
		return Manifest{}, err
	}
	if len(data) > MaxManifest {
		return Manifest{}, errors.New("manifest is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// cache returns this service's own bare repository for repo, fetched from
// the remote's default branch.
func (s *Service) cache(ctx context.Context, repo, token string) (gitx.Git, string, error) {
	dir := filepath.Join(s.CacheDir, repo+".git")
	g := gitx.Git{Bin: s.GitBin, GitDir: dir, Dir: s.CacheDir, Home: s.Home, AllowFile: true}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if _, err := g.Run(ctx, "init", "-q", "--bare"); err != nil {
			return g, "", err
		}
	}
	url := s.remote(s.Cfg.Owner, repo)
	authed := g
	authed.Config = s.auth(token)
	out, err := authed.Out(ctx, "ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return g, "", err
	}
	base := ""
	for _, line := range strings.Split(out, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			base, _, _ = strings.Cut(ref, "\t")
		}
	}
	if !branchRe.MatchString(base) || strings.HasPrefix(base, "worker/") {
		return g, "", fmt.Errorf("remote default branch %q is not usable", base)
	}
	if _, err := authed.Run(ctx, "fetch", "-q", "--no-tags", url,
		"+refs/heads/"+base+":refs/remotes/origin/"+base); err != nil {
		return g, "", err
	}
	return g, base, nil
}

func (s *Service) export(ctx context.Context, r task.Resolved) Response {
	if !s.Cfg.IsPrivate(r.Repo) {
		return fail(CodeRefused, "repo %q is public; clone it anonymously", r.Repo)
	}
	token, err := s.Tokens.RepoToken(ctx, s.Cfg.Owner, r.Repo)
	if err != nil {
		return fail(CodeGitHub, "token: %v", err)
	}
	g, base, err := s.cache(ctx, r.Repo, token)
	if err != nil {
		return fail(CodeGit, "%v", err)
	}
	if _, err := g.Run(ctx, "update-ref", "refs/heads/"+base, "refs/remotes/origin/"+base); err != nil {
		return fail(CodeGit, "%v", err)
	}
	if _, err := g.Run(ctx, "symbolic-ref", "HEAD", "refs/heads/"+base); err != nil {
		return fail(CodeGit, "%v", err)
	}
	out := filepath.Join(s.ExportDir, r.ID+".bundle")
	if _, err := g.Run(ctx, "bundle", "create", "-q", out, "HEAD", "refs/heads/"+base); err != nil {
		return fail(CodeGit, "%v", err)
	}
	if err := os.Chmod(out, 0o640); err != nil {
		return fail(CodeGit, "%v", err)
	}
	return Response{OK: true, Bundle: out}
}

func (s *Service) push(ctx context.Context, r task.Resolved) Response {
	m, err := s.readManifest(r.ID)
	if err != nil {
		return fail(CodeRefused, "%v", err)
	}
	switch {
	case m.ID != r.ID || m.Repo != r.Repo || m.Template != r.Template:
		return fail(CodeRefused, "manifest does not match the task")
	case m.Branch != r.Branch:
		return fail(CodeRefused, "manifest branch %q is not %q", m.Branch, r.Branch)
	case !gitx.IsSHA(m.BaseSHA) || !gitx.IsSHA(m.HeadSHA):
		return fail(CodeRefused, "manifest shas are not full object names")
	}
	token, err := s.Tokens.RepoToken(ctx, s.Cfg.Owner, r.Repo)
	if err != nil {
		return fail(CodeGitHub, "token: %v", err)
	}
	g, base, err := s.cache(ctx, r.Repo, token)
	if err != nil {
		return fail(CodeGit, "%v", err)
	}
	if m.BaseBranch != base {
		return fail(CodeRefused, "manifest base branch %q is not the remote default %q", m.BaseBranch, base)
	}
	bundle := filepath.Join(s.RunsDir, r.ID, "branch.bundle")
	if _, err := g.Run(ctx, "fetch", "-q", "--no-tags", bundle, "+refs/heads/"+r.Branch+":refs/heads/"+r.Branch); err != nil {
		return fail(CodeRefused, "bundle: %v", err)
	}
	head, err := g.Out(ctx, "rev-parse", "refs/heads/"+r.Branch)
	if err != nil {
		return fail(CodeGit, "%v", err)
	}
	if head != m.HeadSHA {
		return fail(CodeRefused, "bundle head %s is not the manifest head %s", head, m.HeadSHA)
	}
	if ok, err := g.IsAncestor(ctx, m.BaseSHA, "refs/remotes/origin/"+base); err != nil || !ok {
		return fail(CodeRefused, "base %s is not on %s (%v)", m.BaseSHA, base, err)
	}
	if ok, err := g.IsAncestor(ctx, m.BaseSHA, m.HeadSHA); err != nil || !ok {
		return fail(CodeRefused, "head does not descend from base (%v)", err)
	}

	rng := m.BaseSHA + ".." + m.HeadSHA
	raw, err := g.Run(ctx, "diff", "--raw", "-z", "--no-renames", "--no-abbrev", m.BaseSHA, m.HeadSHA)
	if err != nil {
		return fail(CodeGit, "%v", err)
	}
	num, err := g.Run(ctx, "diff", "--numstat", "-z", "--no-renames", m.BaseSHA, m.HeadSHA)
	if err != nil {
		return fail(CodeGit, "%v", err)
	}
	changes, err := diffcheck.ParseRaw(raw)
	if err == nil {
		err = diffcheck.MergeNumstat(changes, num)
	}
	if err != nil {
		return fail(CodeRefused, "%v", err)
	}
	if v := diffcheck.Check(changes, diffcheck.Rules{Allowed: r.AllowedPaths, MaxFiles: r.Tpl.MaxFiles, MaxAddedLines: r.Tpl.MaxAddedLines}); len(v) > 0 {
		return fail(CodeRefused, "diff: %s", strings.Join(v, "; "))
	}
	if s.Secretscan == nil {
		return fail(CodeRefused, "no secret scanner configured")
	}
	found, err := s.Secretscan(ctx, g.GitDir, rng)
	if err != nil {
		return fail(CodeRefused, "secret scan did not complete: %v", err)
	}
	if found {
		return fail(CodeSecret, "secret scan found something in %s; nothing was pushed", rng)
	}

	authed := g
	authed.Config = s.auth(token)
	if _, err := authed.Run(ctx, "push", "-q", "--no-verify", s.remote(s.Cfg.Owner, r.Repo),
		m.HeadSHA+":refs/heads/"+r.Branch); err != nil {
		return fail(CodeGit, "push: %v", err)
	}
	body := m.Body
	if len(body) > MaxBody {
		body = body[:MaxBody]
	}
	url, err := s.PRs.CreatePR(ctx, token, s.Cfg.Owner, r.Repo, github.NewPR{Title: r.Title, Head: r.Branch, Base: base, Body: body})
	if err != nil {
		return fail(CodeGitHub, "pull request: %v", err)
	}
	return Response{OK: true, PRURL: url}
}

// Serve answers one JSON request line per connection until ctx ends.
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.serveConn(ctx, conn)
	}
}

func (s *Service) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Minute))
	line, err := bufio.NewReader(io.LimitReader(conn, MaxRequest+1)).ReadBytes('\n')
	var resp Response
	var req Request
	switch {
	case len(line) > MaxRequest:
		resp = fail(CodeBadRequest, "request is larger than %d bytes", MaxRequest)
	case err != nil && !errors.Is(err, io.EOF):
		resp = fail(CodeBadRequest, "read: %v", err)
	default:
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			resp = fail(CodeBadRequest, "request: %v", err)
		} else {
			resp = s.Handle(ctx, req)
		}
	}
	out, _ := json.Marshal(resp)
	_, _ = conn.Write(append(out, '\n'))
}

// Call sends one request to the service at socket and returns its answer.
func Call(ctx context.Context, socket string, req Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("push service answer: %w", err)
	}
	return resp, nil
}
