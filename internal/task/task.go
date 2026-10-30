// Package task is one request for a worker: a template name and the values
// of its parameters. Everything that matters for money or reach comes from
// the template and the config; a task can only narrow, never widen.
package task

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/models"
	"github.com/TAIPANBOX/estate-workers/internal/template"
)

// MaxSize bounds a task file.
const MaxSize = 64 << 10

// MaxParamLen bounds one parameter value.
const MaxParamLen = 100

var (
	idRe        = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-z0-9]{6}$`)
	requesterRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	idAlphabet  = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
)

type Task struct {
	ID          string            `json:"id"`
	Template    string            `json:"template"`
	Repo        string            `json:"repo,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	Model       string            `json:"model,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	RequestedBy string            `json:"requested_by"`
	CreatedAt   time.Time         `json:"created_at"`
}

// Resolved is a task checked against its template and the config, with
// every placeholder filled.
type Resolved struct {
	Task
	Tpl          template.Template
	Model        string
	RunCapUSD    float64
	Branch       string
	AllowedPaths []string
	ContextRepos []string
	Tools        []string
	Files        map[string]string
	Env          map[string]string
	Prompt       string
	Title        string
}

// Parse decodes a task strictly and refuses anything over MaxSize.
func Parse(r io.Reader) (Task, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return Task{}, err
	}
	if len(data) > MaxSize {
		return Task{}, fmt.Errorf("task is larger than %d bytes", MaxSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var t Task
	if err := dec.Decode(&t); err != nil {
		return Task{}, fmt.Errorf("task: %w", err)
	}
	if dec.More() {
		return Task{}, errors.New("task: trailing data after the object")
	}
	return t, nil
}

// Validate checks the task against its template and the config and fills
// in everything the run needs.
func (t Task) Validate(cfg config.Config, tpls map[string]template.Template) (Resolved, error) {
	if !idRe.MatchString(t.ID) {
		return Resolved{}, fmt.Errorf("task id %q is not of the form 20060102T150405Z-xxxxxx", t.ID)
	}
	if !requesterRe.MatchString(t.RequestedBy) {
		return Resolved{}, fmt.Errorf("requested_by %q is not a short lower-case name", t.RequestedBy)
	}
	tpl, ok := tpls[t.Template]
	if !ok {
		return Resolved{}, fmt.Errorf("no template %q", t.Template)
	}

	switch tpl.Kind {
	case template.KindProbe:
		if t.Repo != "" || len(t.Params) != 0 {
			return Resolved{}, errors.New("a probe takes no repo and no params")
		}
	case template.KindRepo:
		if !cfg.RepoAllowed(t.Repo) {
			return Resolved{}, fmt.Errorf("repo %q is not allowed by the config", t.Repo)
		}
		if !slices.Contains(tpl.Repos, "*") && !slices.Contains(tpl.Repos, t.Repo) {
			return Resolved{}, fmt.Errorf("template %q does not work on repo %q", tpl.Name, t.Repo)
		}
		if err := checkParams(tpl.Params, t.Params); err != nil {
			return Resolved{}, err
		}
	default:
		return Resolved{}, fmt.Errorf("template %q has unknown kind %q", tpl.Name, tpl.Kind)
	}

	model := t.Model
	if model == "" {
		model = tpl.Model
	}
	if err := models.Check(model, t.Reason); err != nil {
		return Resolved{}, err
	}
	if !slices.Contains(tpl.AllowedModels, model) {
		return Resolved{}, fmt.Errorf("template %q does not allow model %q", tpl.Name, model)
	}
	cfgCap, ok := cfg.RunCapUSD[model]
	if !ok {
		return Resolved{}, fmt.Errorf("config has no run cap for %q", model)
	}

	r := Resolved{
		Task:      t,
		Tpl:       tpl,
		Model:     model,
		RunCapUSD: min(tpl.MaxBudgetUSD, cfgCap, models.HardMaxRunUSD[model]),
		Prompt:    template.Expand(tpl.Prompt, t.Params),
	}
	for _, tool := range tpl.AllowedTools {
		expanded := template.Expand(tool, t.Params)
		if err := template.CheckTool(expanded); err != nil {
			return Resolved{}, fmt.Errorf("expanded tool: %w", err)
		}
		r.Tools = append(r.Tools, expanded)
	}
	if tpl.Kind == template.KindRepo {
		r.Branch = "worker/" + tpl.Name + "/" + t.ID
		r.Title = template.Expand(tpl.PRTitle, t.Params)
		for _, p := range tpl.AllowedPaths {
			r.AllowedPaths = append(r.AllowedPaths, template.Expand(p, t.Params))
		}
		if len(tpl.Files) > 0 {
			r.Files = map[string]string{}
			for name, content := range tpl.Files {
				r.Files[name] = template.Expand(content, t.Params)
			}
		}
		r.Env = tpl.Env
		for _, c := range tpl.ContextRepos {
			repo := template.Expand(c, t.Params)
			if !cfg.RepoAllowed(repo) {
				return Resolved{}, fmt.Errorf("context repo %q is not allowed by the config", repo)
			}
			r.ContextRepos = append(r.ContextRepos, repo)
		}
	}
	return r, nil
}

func checkParams(declared, given map[string]string) error {
	var missing, extra []string
	for k := range declared {
		if _, ok := given[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range given {
		if _, ok := declared[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing)+len(extra) > 0 {
		return fmt.Errorf("params: missing %v, unexpected %v", missing, extra)
	}
	for k, v := range given {
		if len(v) > MaxParamLen {
			return fmt.Errorf("param %q is longer than %d", k, MaxParamLen)
		}
		if !regexp.MustCompile(declared[k]).MatchString(v) {
			return fmt.Errorf("param %q value %q does not match %s", k, v, declared[k])
		}
	}
	return nil
}

// NewID makes a sortable task id from the time and six random characters.
func NewID(now time.Time, rnd io.Reader) (string, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", fmt.Errorf("task id: %w", err)
	}
	suffix := strings.ToLower(idAlphabet.EncodeToString(b))[:6]
	return now.UTC().Format("20060102T150405Z") + "-" + suffix, nil
}
