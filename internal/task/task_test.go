package task

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/template"
)

func setup(t *testing.T) (config.Config, map[string]template.Template) {
	t.Helper()
	cfg := config.Config{
		Owner: "TAIPANBOX", Repos: []string{"architecture", "agent-stack-go", "estate-gates", "qryx"},
		PrivateRepos: []string{"architecture"}, DayCapUSD: 6, MonthCapUSD: 80, UnverifiedCapUSD: 0.1,
		RunCapUSD:              map[string]float64{"claude-haiku-5-5": 0.3, "claude-sonnet-5-5": 2, "claude-opus-5-5": 4},
		MaxConsecutiveFailures: 3,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	tpls, err := template.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg, tpls
}

func dossier() Task {
	return Task{
		ID: "20261030T112233Z-ab12cd", Template: "dossier-bump", Repo: "architecture",
		Params: map[string]string{"service": "agent-stack-go"}, RequestedBy: "supervisor",
		CreatedAt: time.Date(2026, 10, 30, 11, 22, 33, 0, time.UTC),
	}
}

func TestAValidDossierTaskResolves(t *testing.T) {
	cfg, tpls := setup(t)
	r, err := dossier().Validate(cfg, tpls)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r.Model != "claude-sonnet-5-5" || r.RunCapUSD != 2 {
		t.Errorf("model %q cap %v", r.Model, r.RunCapUSD)
	}
	if r.Branch != "worker/dossier-bump/20261030T112233Z-ab12cd" {
		t.Errorf("branch %q", r.Branch)
	}
	if len(r.AllowedPaths) != 2 || r.AllowedPaths[0] != "services/agent-stack-go.md" {
		t.Errorf("paths %v", r.AllowedPaths)
	}
	if !slices.Equal(r.ContextRepos, []string{"agent-stack-go", "estate-gates"}) {
		t.Errorf("context %v", r.ContextRepos)
	}
	if f := r.Files["archguard.json"]; !strings.Contains(f, "{{context}}/estate-gates/estate.json") || r.Env["ARCHGUARD_CONFIG"] != "{{files}}/archguard.json" {
		t.Errorf("files %v env %v", r.Files, r.Env)
	}
	if !strings.Contains(r.Prompt, "services/agent-stack-go.md") || strings.Contains(r.Prompt, "{{") {
		t.Errorf("prompt not expanded: %.80q", r.Prompt)
	}
	if !strings.Contains(r.Title, "agent-stack-go") {
		t.Errorf("title %q", r.Title)
	}
	want := "Bash(git -C ../context/agent-stack-go log *)"
	if !slices.Contains(r.Tools, want) {
		t.Errorf("tools %v miss %q", r.Tools, want)
	}
	for _, tool := range r.Tools {
		if strings.Contains(tool, "{{") {
			t.Errorf("tool not expanded: %q", tool)
		}
	}
}

func TestRunCapIsTheLowestOfTemplateConfigAndHardCap(t *testing.T) {
	cfg, tpls := setup(t)
	task := dossier()
	task.Model = "claude-haiku-5-5"
	r, err := task.Validate(cfg, tpls)
	if err != nil {
		t.Fatal(err)
	}
	if r.RunCapUSD != 0.3 {
		t.Errorf("haiku cap %v, want the config's 0.3", r.RunCapUSD)
	}
}

func TestCreditProbeResolvesWithoutARepo(t *testing.T) {
	cfg, tpls := setup(t)
	p := Task{ID: "20261030T112233Z-aaaaaa", Template: "credit-probe", RequestedBy: "supervisor"}
	r, err := p.Validate(cfg, tpls)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if r.RunCapUSD >= 0.10 || r.Branch != "" {
		t.Errorf("probe cap %v branch %q", r.RunCapUSD, r.Branch)
	}
}

func TestTasksThatReachTooFarAreRefused(t *testing.T) {
	cfg, tpls := setup(t)
	cases := map[string]func(*Task){
		"unknown template":      func(x *Task) { x.Template = "shell" },
		"repo not in template":  func(x *Task) { x.Repo = "qryx" },
		"repo not in config":    func(x *Task) { x.Repo = "tokenfuse" },
		"hard denied repo":      func(x *Task) { x.Repo = "hermes-openwrt" },
		"missing param":         func(x *Task) { x.Params = nil },
		"extra param":           func(x *Task) { x.Params["path"] = "x" },
		"param regex":           func(x *Task) { x.Params["service"] = "../etc" },
		"context not allowed":   func(x *Task) { x.Params["service"] = "tokenfuse" },
		"context hard denied":   func(x *Task) { x.Params["service"] = "hermes-openwrt" },
		"model not in template": func(x *Task) { x.Model = "claude-opus-5-5"; x.Reason = "long enough reason" },
		"fable":                 func(x *Task) { x.Model = "claude-fable-5-1" },
		"bad id":                func(x *Task) { x.ID = "../../etc" },
		"bad requester":         func(x *Task) { x.RequestedBy = "Rm -rf" },
		"empty requester":       func(x *Task) { x.RequestedBy = "" },
		"long param":            func(x *Task) { x.Params["service"] = strings.Repeat("a", 101) },
	}
	for name, mutate := range cases {
		x := dossier()
		x.Params = map[string]string{"service": "agent-stack-go"}
		mutate(&x)
		if _, err := x.Validate(cfg, tpls); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAWildcardTemplateStillNeedsTheConfigToListTheRepo(t *testing.T) {
	cfg, tpls := setup(t)
	tpl := tpls["dossier-bump"]
	tpl.Repos = []string{"*"}
	tpls["dossier-bump"] = tpl
	for _, repo := range []string{"tokenfuse", "hermes-openwrt", "estate-workers"} {
		x := dossier()
		x.Repo = repo
		if _, err := x.Validate(cfg, tpls); err == nil {
			t.Errorf("repo %q accepted under a wildcard template", repo)
		}
	}
	x := dossier()
	x.Repo = "qryx"
	if _, err := x.Validate(cfg, tpls); err != nil {
		t.Errorf("a listed repo under a wildcard template refused: %v", err)
	}
}

func TestProbeRefusesARepoOrParams(t *testing.T) {
	cfg, tpls := setup(t)
	p := Task{ID: "20261030T112233Z-aaaaaa", Template: "credit-probe", RequestedBy: "supervisor", Repo: "qryx"}
	if _, err := p.Validate(cfg, tpls); err == nil {
		t.Error("probe with a repo accepted")
	}
	p.Repo = ""
	p.Params = map[string]string{"x": "y"}
	if _, err := p.Validate(cfg, tpls); err == nil {
		t.Error("probe with params accepted")
	}
}

func TestOpusNeedsAReasonAndATemplateThatAllowsIt(t *testing.T) {
	cfg, tpls := setup(t)
	tpl := tpls["dossier-bump"]
	tpl.AllowedModels = append(tpl.AllowedModels, "claude-opus-5-5")
	tpls["dossier-bump"] = tpl
	x := dossier()
	x.Model = "claude-opus-5-5"
	if _, err := x.Validate(cfg, tpls); err == nil {
		t.Error("Opus without a reason accepted")
	}
	x.Reason = "service rewrote its whole contract"
	r, err := x.Validate(cfg, tpls)
	if err != nil {
		t.Fatalf("Opus with a reason: %v", err)
	}
	if r.RunCapUSD != 2 {
		t.Errorf("Opus cap %v, want the template's 2", r.RunCapUSD)
	}
}

func TestParseIsStrictAndBounded(t *testing.T) {
	good := `{"id":"20261030T112233Z-ab12cd","template":"dossier-bump","repo":"architecture","params":{"service":"qryx"},"requested_by":"supervisor","created_at":"2026-10-30T11:22:33Z"}`
	if _, err := Parse(strings.NewReader(good)); err != nil {
		t.Fatalf("good task: %v", err)
	}
	for name, s := range map[string]string{
		"unknown field": strings.Replace(good, `"repo"`, `"shell":"x","repo"`, 1),
		"trailing":      good + `{}`,
		"not json":      `{`,
		"oversize":      `{"id":"` + strings.Repeat("a", 70000) + `"}`,
	} {
		if _, err := Parse(strings.NewReader(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewIDHasTheShapeValidateAccepts(t *testing.T) {
	id, err := NewID(time.Date(2026, 10, 30, 11, 22, 33, 0, time.UTC), bytes.NewReader([]byte("0123456789")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "20261030T112233Z-") || len(id) != len("20261030T112233Z-")+6 {
		t.Fatalf("id %q", id)
	}
	cfg, tpls := setup(t)
	x := dossier()
	x.ID = id
	if _, err := x.Validate(cfg, tpls); err != nil {
		t.Errorf("NewID result refused: %v", err)
	}
	if _, err := NewID(time.Now(), bytes.NewReader(nil)); err == nil {
		t.Error("NewID with no randomness succeeded")
	}
}
