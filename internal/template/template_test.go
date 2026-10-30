package template

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedTemplatesLoad(t *testing.T) {
	tpls, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range []string{"dossier-bump", "credit-probe"} {
		if _, ok := tpls[name]; !ok {
			t.Errorf("template %q missing", name)
		}
	}
}

func TestCreditProbeStaysUnderTheUnverifiedCeiling(t *testing.T) {
	tpls, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := tpls["credit-probe"]
	if p.Kind != KindProbe || p.MaxBudgetUSD >= 0.10 {
		t.Errorf("credit-probe kind %q budget %v, want a probe under $0.10", p.Kind, p.MaxBudgetUSD)
	}
}

func valid() Template {
	return Template{
		Name: "demo", Kind: KindRepo, Tier: "T1",
		Repos: []string{"architecture"}, ContextRepos: []string{"{{service}}"},
		Params: map[string]string{"service": "^[a-z]+$"},
		Model:  "claude-haiku-5-5", AllowedModels: []string{"claude-haiku-5-5"},
		MaxTurns: 10, MaxBudgetUSD: 0.5,
		AllowedTools: []string{"Read", "Edit(./**)", "Bash(make gates)"},
		AllowedPaths: []string{"services/{{service}}.md"},
		MaxFiles:     1, MaxAddedLines: 10,
		Gates:  [][]string{{"make", "gates"}},
		Prompt: "do {{service}}", PRTitle: "docs({{service}}): x",
	}
}

func TestValidTemplatePasses(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestTemplateRefusesBroadOrDangerousTools(t *testing.T) {
	for _, tool := range []string{
		"Bash", "Bash(*)", "Bash( * )", "Bash()", "Edit", "Write", "Edit(//etc/**)", "WebFetch", "WebSearch",
		"Agent", "mcp__x__y", "Bash(curl https://x)", "Bash(sudo make)", "Bash(cargo build)",
		"Bash(git push origin)", "Bash(git commit -m x)", "Bash(git -C . config core.x y)",
		"Bash(make gates; curl x)", "Bash(make $(id))", "Bash(gh pr merge)", "Bash(rm -rf .)",
		"Bash(bash -c x)", "Bash(env X=1 make)",
		"Bash(git -C *)", "Bash(git *)", "Bash(git -C x)", "Bash(git -c core.pager=sh log)",
		"Bash(git -C x -c a=b log *)", "Bash(git -ccore.pager=x log)", "Bash(git --exec-path=/x log)",
		"Bash(git --config-env=a=B log)", "Bash(git -C x fetch *)", "Bash(git -C x push *)",
		"Bash(git -C * log *)", "Bash(git diff --output=/tmp/x *)", "Bash(git diff --ext-diff *)",
		"Bash(git --git-dir=/x log)", "Bash(git log --upload-pack=x)",
		"Bash(git diff *)", "Bash(git status *)", "Bash(git -C x rev-parse *)", "Bash(git -C x grep *)",
		"Bash(git diff --output=/tmp/x)", "Bash(git diff --no-ext-diff --ext-diff)", "Bash(git log --exec-path=/x)",
	} {
		tpl := valid()
		tpl.AllowedTools = append(tpl.AllowedTools, tool)
		if err := tpl.Validate(); err == nil {
			t.Errorf("tool %q was accepted", tool)
		}
	}
}

func TestTemplateAcceptsPinnedReadOnlyGit(t *testing.T) {
	for _, tool := range []string{
		"Bash(git -C ../context/{{service}} log *)", "Bash(git -C ../context/x show *)",
		"Bash(git -C ../context/x rev-parse HEAD)", "Bash(git diff)", "Bash(git status --porcelain)",
		"Bash(git -C a -C b log)",
	} {
		tpl := valid()
		tpl.AllowedTools = append(tpl.AllowedTools, tool)
		if err := tpl.Validate(); err != nil {
			t.Errorf("tool %q refused: %v", tool, err)
		}
	}
}

func TestTemplateRefusesBroadOrEscapingPaths(t *testing.T) {
	for _, p := range []string{"", "*", "**", "**/*", "/etc/passwd", "./x", "../x", "a/../../b", ".git/config", ".github/workflows/ci.yml"} {
		tpl := valid()
		tpl.AllowedPaths = []string{p}
		if err := tpl.Validate(); err == nil {
			t.Errorf("path %q was accepted", p)
		}
	}
}

func TestTemplateRefusesFableAndBudgetsAboveTheHardCap(t *testing.T) {
	tpl := valid()
	tpl.AllowedModels = append(tpl.AllowedModels, "claude-fable-5-1")
	if err := tpl.Validate(); err == nil {
		t.Error("a template listing Fable was accepted")
	}
	tpl = valid()
	tpl.MaxBudgetUSD = 0.51
	if err := tpl.Validate(); err == nil {
		t.Error("a Haiku budget above $0.50 was accepted")
	}
	tpl = valid()
	tpl.Model = "claude-sonnet-5-5"
	if err := tpl.Validate(); err == nil {
		t.Error("a default model outside allowed_models was accepted")
	}
}

func TestTemplateShapeIsChecked(t *testing.T) {
	cases := map[string]func(*Template){
		"bad name":           func(t *Template) { t.Name = "Bad Name" },
		"bad tier":           func(t *Template) { t.Tier = "T9" },
		"unanchored param":   func(t *Template) { t.Params["service"] = "[a-z]+" },
		"bad param regex":    func(t *Template) { t.Params["service"] = "^([a-z]$" },
		"bad param name":     func(t *Template) { t.Params["Service"] = "^x$" },
		"no gates":           func(t *Template) { t.Gates = nil },
		"empty gate":         func(t *Template) { t.Gates = [][]string{{}} },
		"no repos":           func(t *Template) { t.Repos = nil },
		"no paths":           func(t *Template) { t.AllowedPaths = nil },
		"zero turns":         func(t *Template) { t.MaxTurns = 0 },
		"too many files":     func(t *Template) { t.MaxFiles = 500 },
		"zero lines":         func(t *Template) { t.MaxAddedLines = 0 },
		"empty prompt":       func(t *Template) { t.Prompt = " " },
		"empty title":        func(t *Template) { t.PRTitle = "" },
		"unknown kind":       func(t *Template) { t.Kind = "shell" },
		"unknown param used": func(t *Template) { t.Prompt = "do {{other}}" },
		"pad on repo":        func(t *Template) { t.PadTokens = 5 },
	}
	for name, mutate := range cases {
		tpl := valid()
		tpl.Params = map[string]string{"service": "^[a-z]+$"}
		mutate(&tpl)
		if err := tpl.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProbeMayTouchNothing(t *testing.T) {
	probe := Template{
		Name: "probe-x", Kind: KindProbe, Tier: "T0", Model: "claude-haiku-5-5",
		AllowedModels: []string{"claude-haiku-5-5"}, MaxTurns: 1, MaxBudgetUSD: 0.05, Prompt: "ok",
	}
	if err := probe.Validate(); err != nil {
		t.Fatalf("plain probe: %v", err)
	}
	for name, mutate := range map[string]func(*Template){
		"repo":  func(t *Template) { t.Repos = []string{"x"} },
		"tool":  func(t *Template) { t.AllowedTools = []string{"Read"} },
		"param": func(t *Template) { t.Params = map[string]string{"a": "^a$"} },
		"pad":   func(t *Template) { t.PadTokens = 60000 },
	} {
		p := probe
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("probe with %s accepted", name)
		}
	}
}

func TestExpandFillsOnlyKnownPlaceholders(t *testing.T) {
	got := Expand("services/{{service}}.md {{other}}", map[string]string{"service": "qryx"})
	if got != "services/qryx.md {{other}}" {
		t.Errorf("Expand = %q", got)
	}
}

func TestLoadRefusesMismatchedNamesDuplicatesAndBadFiles(t *testing.T) {
	good := `{"name":"probe-a","kind":"probe","tier":"T0","model":"claude-haiku-5-5","allowed_models":["claude-haiku-5-5"],"max_turns":1,"max_budget_usd":0.05,"prompt":"ok"}`
	if _, err := load(fstest.MapFS{"t/probe-a.json": {Data: []byte(good)}}, "t"); err != nil {
		t.Fatalf("good fs: %v", err)
	}
	cases := map[string]fstest.MapFS{
		"name mismatch": {"t/other.json": {Data: []byte(good)}},
		"bad json":      {"t/probe-a.json": {Data: []byte(`{`)}},
		"unknown field": {"t/probe-a.json": {Data: []byte(strings.Replace(good, `"prompt"`, `"extra":1,"prompt"`, 1))}},
		"trailing":      {"t/probe-a.json": {Data: []byte(good + `{}`)}},
		"empty":         {"t/readme.txt": {Data: []byte("x")}},
	}
	for name, fsys := range cases {
		if _, err := load(fsys, "t"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := load(fstest.MapFS{}, "missing"); err == nil {
		t.Error("missing dir accepted")
	}
}

// A template may hand a tool one configuration file and point one
// NAME_CONFIG variable at it, and nothing wider: no variable a shell, git,
// Go, the dynamic loader or Claude Code reads, no value outside the
// template's own files, no file name that leaves the files directory.
func TestTemplateFilesAndEnvStayNarrow(t *testing.T) {
	withFiles := func() Template {
		tpl := valid()
		tpl.Files = map[string]string{"tool.json": `{"root": "{{context}}/{{service}}"}`}
		tpl.Env = map[string]string{"TOOL_CONFIG": "{{files}}/tool.json"}
		return tpl
	}
	if err := withFiles().Validate(); err != nil {
		t.Fatalf("a narrow file and its variable are refused: %v", err)
	}
	cases := map[string]func(*Template){
		"name without _CONFIG":       func(t *Template) { t.Env = map[string]string{"TOOL": "{{files}}/tool.json"} },
		"git reads it":               func(t *Template) { t.Env = map[string]string{"GIT_CONFIG": "{{files}}/tool.json"} },
		"git global config":          func(t *Template) { t.Env = map[string]string{"GIT_CONFIG_GLOBAL": "{{files}}/tool.json"} },
		"claude code reads it":       func(t *Template) { t.Env = map[string]string{"CLAUDE_CONFIG": "{{files}}/tool.json"} },
		"the loader reads it":        func(t *Template) { t.Env = map[string]string{"LD_CONFIG": "{{files}}/tool.json"} },
		"go reads it":                func(t *Template) { t.Env = map[string]string{"GOENV_CONFIG": "{{files}}/tool.json"} },
		"lower case":                 func(t *Template) { t.Env = map[string]string{"tool_CONFIG": "{{files}}/tool.json"} },
		"value is a host path":       func(t *Template) { t.Env = map[string]string{"TOOL_CONFIG": "/etc/passwd"} },
		"value is a context path":    func(t *Template) { t.Env = map[string]string{"TOOL_CONFIG": "{{context}}/x"} },
		"value names no file":        func(t *Template) { t.Env = map[string]string{"TOOL_CONFIG": "{{files}}/other.json"} },
		"value climbs out":           func(t *Template) { t.Env = map[string]string{"TOOL_CONFIG": "{{files}}/../tool.json"} },
		"file name climbs out":       func(t *Template) { t.Files = map[string]string{"../tool.json": "x"} },
		"hidden file name":           func(t *Template) { t.Files = map[string]string{".tool.json": "x"} },
		"file in a subdirectory":     func(t *Template) { t.Files = map[string]string{"a/tool.json": "x"} },
		"file is too large":          func(t *Template) { t.Files["tool.json"] = strings.Repeat("x", 16<<10+1) },
		"file uses an unknown param": func(t *Template) { t.Files["tool.json"] = "{{other}}" },
		"param shadows context":      func(t *Template) { t.Params["context"] = "^x$" },
		"param shadows files":        func(t *Template) { t.Params["files"] = "^x$" },
		"probe with files": func(t *Template) {
			*t = Template{Name: "probe", Kind: KindProbe, Tier: "T0", Model: "claude-haiku-5-5",
				AllowedModels: []string{"claude-haiku-5-5"}, MaxTurns: 1, MaxBudgetUSD: 0.05, Prompt: "hi",
				Files: map[string]string{"tool.json": "x"}}
		},
	}
	for name, mutate := range cases {
		tpl := withFiles()
		mutate(&tpl)
		if err := tpl.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
