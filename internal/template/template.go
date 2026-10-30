// Package template holds the closed set of jobs a worker can do. A template
// fixes the repositories, the files that may change, the model, the tools,
// the budget and the gates; a task only fills in its parameters. Templates
// are compiled into the binary, so changing one is a reviewed pull request.
package template

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/TAIPANBOX/estate-workers/internal/models"
)

//go:embed templates/*.json
var embedded embed.FS

const (
	KindRepo  = "repo"
	KindProbe = "probe"
)

type Template struct {
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	Tier          string            `json:"tier"`
	Repos         []string          `json:"repos"`
	ContextRepos  []string          `json:"context_repos"`
	Params        map[string]string `json:"params"`
	Model         string            `json:"model"`
	AllowedModels []string          `json:"allowed_models"`
	MaxTurns      int               `json:"max_turns"`
	MaxBudgetUSD  float64           `json:"max_budget_usd"`
	AllowedTools  []string          `json:"allowed_tools"`
	AllowedPaths  []string          `json:"allowed_paths"`
	MaxFiles      int               `json:"max_files"`
	MaxAddedLines int               `json:"max_added_lines"`
	Gates         [][]string        `json:"gates"`
	Prompt        string            `json:"prompt"`
	PRTitle       string            `json:"pr_title"`
	PadTokens     int               `json:"pad_tokens"`
}

var (
	nameRe        = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)
	placeholderRe = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)
	// Shell words a template may never allow, whatever follows them.
	refusedFirstWords = []string{
		"sudo", "su", "curl", "wget", "ssh", "scp", "sftp", "nc", "ncat", "socat",
		"cargo", "rustc", "rustup", "docker", "podman", "systemctl", "rm", "chmod",
		"chown", "dd", "mount", "kill", "pkill", "env", "eval", "exec", "sh", "bash",
		"zsh", "python", "python3", "perl", "node", "npx", "npm", "pip", "gh",
	}
	// Plain tools that read and need no scope.
	plainTools = []string{"Read", "Glob", "Grep"}
	// Scoped edit tools: only the working directory of the run.
	scopedEditTools = []string{"Edit(./**)", "Write(./**)"}
)

// Load returns the templates compiled into the binary, keyed by name.
func Load() (map[string]Template, error) {
	return load(embedded, "templates")
}

func load(fsys fs.FS, dir string) (map[string]Template, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	out := map[string]Template{}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		t, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if strings.TrimSuffix(e.Name(), ".json") != t.Name {
			return nil, fmt.Errorf("%s: file name does not match template name %q", e.Name(), t.Name)
		}
		if _, dup := out[t.Name]; dup {
			return nil, fmt.Errorf("template %q defined twice", t.Name)
		}
		out[t.Name] = t
	}
	if len(out) == 0 {
		return nil, errors.New("no templates found")
	}
	return out, nil
}

// Parse decodes one template strictly and validates it.
func Parse(data []byte) (Template, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var t Template
	if err := dec.Decode(&t); err != nil {
		return Template{}, err
	}
	if dec.More() {
		return Template{}, errors.New("trailing data after the template")
	}
	return t, t.Validate()
}

// CheckTool refuses tools that would let a run reach past its working
// directory, the network or another agent.
func CheckTool(tool string) error {
	if slices.Contains(plainTools, tool) || slices.Contains(scopedEditTools, tool) {
		return nil
	}
	inner, ok := strings.CutPrefix(tool, "Bash(")
	if !ok || !strings.HasSuffix(inner, ")") {
		return fmt.Errorf("tool %q is not allowed in a template", tool)
	}
	inner = strings.TrimSpace(strings.TrimSuffix(inner, ")"))
	if inner == "" || strings.HasPrefix(inner, "*") {
		return fmt.Errorf("tool %q allows any command", tool)
	}
	if strings.ContainsAny(inner, ";&|`$<>\n") {
		return fmt.Errorf("tool %q carries shell syntax", tool)
	}
	fields := strings.Fields(inner)
	if slices.Contains(refusedFirstWords, fields[0]) {
		return fmt.Errorf("tool %q starts with a refused command %q", tool, fields[0])
	}
	if fields[0] == "git" {
		return checkGit(tool, fields)
	}
	return nil
}

// gitReadOnly are the git subcommands a template may allow. Anything that
// writes, fetches, pushes or reads config is the orchestrator's job.
var gitReadOnly = []string{"log", "show", "rev-parse", "diff", "status", "ls-files", "cat-file", "blame", "grep", "describe", "shortlog"}

// checkGit requires `git [-C <literal dir>]... <read-only subcommand> ...`
// with no config injection anywhere: `git -c core.pager=...` or
// `--exec-path` would turn an allowed git call into any command.
func checkGit(tool string, fields []string) error {
	for _, f := range fields[1:] {
		if f == "-c" || strings.HasPrefix(f, "-c") && !strings.HasPrefix(f, "-C") ||
			strings.HasPrefix(f, "--config-env") || strings.HasPrefix(f, "--exec-path") ||
			strings.HasPrefix(f, "--upload-pack") || strings.HasPrefix(f, "--receive-pack") ||
			strings.HasPrefix(f, "--git-dir") || strings.HasPrefix(f, "--work-tree") ||
			strings.HasPrefix(f, "--output") || f == "--ext-diff" || strings.HasPrefix(f, "--textconv") {
			return fmt.Errorf("tool %q injects git config or paths (%s)", tool, f)
		}
	}
	i := 1
	for i < len(fields) && fields[i] == "-C" {
		if i+1 >= len(fields) || strings.Contains(fields[i+1], "*") {
			return fmt.Errorf("tool %q needs a literal directory after -C", tool)
		}
		i += 2
	}
	if i >= len(fields) || !slices.Contains(gitReadOnly, fields[i]) {
		return fmt.Errorf("tool %q is not a read-only git subcommand %v", tool, gitReadOnly)
	}
	// A wildcard lets the model add flags at run time (--output, --no-index,
	// --ext-diff). Only the history readers need free arguments, and the
	// deny rules in package agent refuse those flags on them.
	if slices.Contains(fields[i+1:], "*") && !slices.Contains(gitWildcardOK, fields[i]) {
		return fmt.Errorf("tool %q: only %v may end in a wildcard; list the exact command", tool, gitWildcardOK)
	}
	return nil
}

var gitWildcardOK = []string{"log", "show"}

func checkPathPattern(p string) error {
	switch {
	case p == "" || p == "*" || p == "**" || p == "**/*":
		return fmt.Errorf("allowed path %q allows everything", p)
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, "./"):
		return fmt.Errorf("allowed path %q must be relative to the repository root", p)
	case slices.Contains(strings.Split(p, "/"), ".."):
		return fmt.Errorf("allowed path %q climbs out of the repository", p)
	case strings.HasPrefix(p, ".git/") || strings.HasPrefix(p, ".github/"):
		return fmt.Errorf("allowed path %q is under a protected directory", p)
	}
	return nil
}

// Validate enforces the shape every template must keep.
func (t Template) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if !nameRe.MatchString(t.Name) {
		add("name %q is not a template name", t.Name)
	}
	if !slices.Contains([]string{"T0", "T1", "T2", "T3"}, t.Tier) {
		add("tier %q is not T0..T3", t.Tier)
	}
	for k, re := range t.Params {
		if !regexp.MustCompile(`^[a-z_]{1,20}$`).MatchString(k) {
			add("param name %q is not lower_snake", k)
		}
		if !strings.HasPrefix(re, "^") || !strings.HasSuffix(re, "$") {
			add("param %q regex must be anchored with ^ and $", k)
		}
		if _, err := regexp.Compile(re); err != nil {
			add("param %q regex: %v", k, err)
		}
	}
	if len(t.AllowedModels) == 0 || !slices.Contains(t.AllowedModels, t.Model) {
		add("model %q must be one of allowed_models", t.Model)
	}
	for _, m := range t.AllowedModels {
		if err := models.Check(m, "templates may list opus; tasks give the reason"); err != nil {
			add("allowed model: %v", err)
		}
	}
	if t.MaxTurns < 1 || t.MaxTurns > 100 {
		add("max_turns must be between 1 and 100")
	}
	if hard, ok := models.HardMaxRunUSD[t.Model]; !ok || !(t.MaxBudgetUSD > 0) || t.MaxBudgetUSD > hard {
		add("max_budget_usd must be above zero and at most the hard cap of %s", t.Model)
	}
	for _, tool := range t.AllowedTools {
		if err := CheckTool(tool); err != nil {
			add("%v", err)
		}
	}
	if strings.TrimSpace(t.Prompt) == "" {
		add("prompt is empty")
	}

	switch t.Kind {
	case KindRepo:
		if len(t.Repos) == 0 {
			add("a repo template needs repos")
		}
		if len(t.AllowedPaths) == 0 {
			add("a repo template needs allowed_paths")
		}
		for _, p := range t.AllowedPaths {
			if err := checkPathPattern(p); err != nil {
				add("%v", err)
			}
		}
		if len(t.Gates) == 0 {
			add("a repo template needs at least one gate")
		}
		for _, g := range t.Gates {
			if len(g) == 0 || g[0] == "" {
				add("a gate has an empty command")
			}
		}
		if t.MaxFiles < 1 || t.MaxFiles > 200 {
			add("max_files must be between 1 and 200")
		}
		if t.MaxAddedLines < 1 || t.MaxAddedLines > 20000 {
			add("max_added_lines must be between 1 and 20000")
		}
		if strings.TrimSpace(t.PRTitle) == "" {
			add("pr_title is empty")
		}
		if t.PadTokens != 0 {
			add("pad_tokens is only for probes")
		}
	case KindProbe:
		if len(t.Repos)+len(t.ContextRepos)+len(t.AllowedPaths)+len(t.Gates)+len(t.AllowedTools) != 0 {
			add("a probe touches no repository and has no tools, paths or gates")
		}
		if len(t.Params) != 0 {
			add("a probe takes no params")
		}
		if t.PadTokens < 0 || t.PadTokens > 50000 {
			add("pad_tokens must be between 0 and 50000")
		}
	default:
		add("kind %q is not repo or probe", t.Kind)
	}

	used := []string{t.Prompt, t.PRTitle}
	used = append(used, t.AllowedPaths...)
	used = append(used, t.ContextRepos...)
	used = append(used, t.AllowedTools...)
	for _, s := range used {
		for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
			if _, ok := t.Params[m[1]]; !ok {
				add("placeholder {{%s}} has no param", m[1])
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Expand fills {{name}} placeholders. Values were validated against the
// param regexes before they reach here.
func Expand(s string, params map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		k := m[2 : len(m)-2]
		if v, ok := params[k]; ok {
			return v
		}
		return m
	})
}
