package agent

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
)

const fakeKey = "sk-ant-api03-FAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE" // secretscan:allow test-only fake

func inputs() Inputs {
	return Inputs{
		Model: "claude-sonnet-5-5", MaxTurns: 40, MaxBudgetUSD: 2,
		AllowedTools: []string{"Read", "Edit(./**)", "Bash(make gates)"},
		RulesFile:    "/var/lib/claudew/runs/x/rules.md",
		RepoRules:    "/var/lib/claudew/jobs/x/repo/CLAUDE.md",
		SettingsFile: "/var/lib/claudew/runs/x/settings.json",
	}
}

func valueAfter(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func TestArgsAreBareAndPrintAndNeverCarryTheKey(t *testing.T) {
	args := Args(inputs())
	if len(args) < 2 || args[0] != "--bare" || args[1] != "-p" {
		t.Fatalf("args must start with --bare -p, got %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "sk-ant") || strings.Contains(a, fakeKey) {
			t.Fatalf("argv carries a key: %q", a)
		}
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypassPermissions") {
			t.Fatalf("argv carries a bypass: %q", a)
		}
	}
	want := map[string]string{
		"--model":              "claude-sonnet-5-5",
		"--permission-mode":    "dontAsk",
		"--permission-prompts": "none",
		"--max-turns":          "40",
		"--max-budget-usd":     "2.00",
		"--allowedTools":       "Read,Edit(./**),Bash(make gates)",
		"--disallowedTools":    "WebFetch,WebSearch,Agent",
		"--settings":           "/var/lib/claudew/runs/x/settings.json",
		"--output-format":      "stream-json",
	}
	for flag, v := range want {
		if got, ok := valueAfter(args, flag); !ok || got != v {
			t.Errorf("%s = %q, want %q", flag, got, v)
		}
	}
	for _, flag := range []string{"--no-session-persistence", "--verbose"} {
		if !slices.Contains(args, flag) {
			t.Errorf("missing %s", flag)
		}
	}
	files := 0
	for i, a := range args {
		if a == "--append-system-prompt-file" && i+1 < len(args) {
			files++
		}
	}
	if files != 2 {
		t.Errorf("want two --append-system-prompt-file, got %d", files)
	}
}

func TestArgsWithoutToolsRemoveEveryTool(t *testing.T) {
	in := inputs()
	in.AllowedTools = nil
	in.RepoRules = ""
	args := Args(in)
	if got, _ := valueAfter(args, "--disallowedTools"); got != "*" {
		t.Errorf("--disallowedTools = %q, want *", got)
	}
	if slices.Contains(args, "--allowedTools") {
		t.Error("a run without tools still passes --allowedTools")
	}
	n := 0
	for _, a := range args {
		if a == "--append-system-prompt-file" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("without repo rules want one prompt file, got %d", n)
	}
}

func TestEnvIsBuiltFromNothingAndCarriesTheKeyOnlyOnce(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_should_not_leak")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth_should_not_leak")
	env := Env(fakeKey, "/jobs/x/home", "/jobs/x/tmp", "/usr/local/go")
	keyed := 0
	for _, kv := range env {
		if strings.Contains(kv, "should_not_leak") || strings.HasPrefix(kv, "GH_TOKEN=") || strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Fatalf("parent environment leaked: %q", kv)
		}
		if strings.Contains(kv, fakeKey) {
			keyed++
			if kv != "ANTHROPIC_API_KEY="+fakeKey {
				t.Errorf("key in an unexpected variable: %q", kv)
			}
		}
	}
	if keyed != 1 {
		t.Errorf("key appears %d times, want 1", keyed)
	}
	for _, must := range []string{
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1", "DISABLE_UPDATES=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1", "HOME=/jobs/x/home", "TMPDIR=/jobs/x/tmp", "GOTOOLCHAIN=local",
	} {
		if !slices.Contains(env, must) {
			t.Errorf("env missing %q", must)
		}
	}
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=/usr/local/go/bin:") }) {
		t.Errorf("PATH does not start with the Go root: %v", env)
	}
}

func TestSettingsLockTheSandboxAndListOnlyTheTemplateTools(t *testing.T) {
	data, err := Settings([]string{"Read", "Bash(make gates)"}, []string{"/jobs/x/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Permissions struct {
			Allow       []string `json:"allow"`
			Deny        []string `json:"deny"`
			DefaultMode string   `json:"defaultMode"`
		} `json:"permissions"`
		Sandbox struct {
			Enabled                  bool `json:"enabled"`
			FailIfUnavailable        bool `json:"failIfUnavailable"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
			AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
			Filesystem               struct {
				DenyRead []string `json:"denyRead"`
			} `json:"filesystem"`
			Network struct {
				AllowedDomains []string `json:"allowedDomains"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("settings are not JSON: %v", err)
	}
	if !slices.Equal(s.Permissions.Allow, []string{"Read", "Bash(make gates)"}) {
		t.Errorf("allow = %v", s.Permissions.Allow)
	}
	if s.Permissions.DefaultMode != "dontAsk" {
		t.Errorf("defaultMode = %q", s.Permissions.DefaultMode)
	}
	for _, d := range []string{"WebFetch", "WebSearch", "Agent", "Read(//run/credentials/**)", "Edit(./.git/**)", "Bash(git push *)"} {
		if !slices.Contains(s.Permissions.Deny, d) {
			t.Errorf("deny misses %q", d)
		}
	}
	if !s.Sandbox.Enabled || !s.Sandbox.FailIfUnavailable || s.Sandbox.AllowUnsandboxedCommands || s.Sandbox.AutoAllowBashIfSandboxed {
		t.Errorf("sandbox not locked: %+v", s.Sandbox)
	}
	if !slices.Contains(s.Sandbox.Filesystem.DenyRead, "/run/credentials") {
		t.Errorf("denyRead misses /run/credentials: %v", s.Sandbox.Filesystem.DenyRead)
	}
	if !slices.Equal(s.Sandbox.Network.AllowedDomains, []string{"proxy.golang.org", "sum.golang.org"}) {
		t.Errorf("allowedDomains = %v", s.Sandbox.Network.AllowedDomains)
	}
}

func TestSettingsRefuseAToolTheTemplateCheckWouldRefuse(t *testing.T) {
	if _, err := Settings([]string{"Bash"}, nil); err == nil {
		t.Error("bare Bash accepted")
	}
	for _, p := range []string{"relative/tmp", "/jobs/../etc"} {
		if _, err := Settings([]string{"Read"}, []string{p}); err == nil {
			t.Errorf("allowWrite %q accepted", p)
		}
	}
}

func TestSettingsListTheJobScratchDirsAsWritable(t *testing.T) {
	data, err := Settings([]string{"Read"}, []string{"/jobs/x/home", "/jobs/x/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Sandbox struct {
			Filesystem struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Sandbox.Filesystem.AllowWrite, []string{"/jobs/x/home", "/jobs/x/tmp"}) {
		t.Errorf("allowWrite %v", s.Sandbox.Filesystem.AllowWrite)
	}
}

const stream = `{"type":"system","subtype":"init","model":"claude-sonnet-5-5"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
not json at all
{"type":"result","subtype":"success","is_error":false,"num_turns":7,"result":"Updated the dossier.","session_id":"s-1","total_cost_usd":0.4321,"usage":{"input_tokens":1200,"output_tokens":3400,"cache_read_input_tokens":900000,"cache_creation_input_tokens":45000}}
`

func TestParseReadsTheResultEvent(t *testing.T) {
	r, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !r.CostKnown || r.CostUSD != 0.4321 || r.NumTurns != 7 || r.IsError || r.Subtype != "success" {
		t.Errorf("result %+v", r)
	}
	if r.InputTokens != 1200 || r.OutputTokens != 3400 || r.CacheRead != 900000 || r.CacheWrite != 45000 {
		t.Errorf("tokens %+v", r)
	}
	if r.Text != "Updated the dossier." || r.SessionID != "s-1" || r.Lines != 4 || r.Malformed != 1 || r.ResultEvents != 1 {
		t.Errorf("text/lines %+v", r)
	}
}

func TestParseTakesTheHighestCostWhenThereAreSeveralResults(t *testing.T) {
	two := `{"type":"result","total_cost_usd":0.9,"is_error":false}
{"type":"result","total_cost_usd":0.2,"is_error":false}
`
	r, err := Parse(strings.NewReader(two))
	if err != nil {
		t.Fatal(err)
	}
	if r.CostUSD != 0.9 || r.ResultEvents != 2 {
		t.Errorf("cost %v events %d, want 0.9 and 2", r.CostUSD, r.ResultEvents)
	}
}

func TestParseWithoutAResultLeavesTheCostUnknown(t *testing.T) {
	r, err := Parse(strings.NewReader(`{"type":"assistant"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.CostKnown {
		t.Error("cost known without a result event")
	}
}

func TestParseRefusesAnOverlongLine(t *testing.T) {
	if _, err := Parse(strings.NewReader(strings.Repeat("x", 9<<20))); err == nil {
		t.Error("an overlong line was accepted")
	}
}

func TestParseBoundsTheReplyText(t *testing.T) {
	long := `{"type":"result","total_cost_usd":0.1,"result":"` + strings.Repeat("a", 20000) + `"}` + "\n"
	r, err := Parse(strings.NewReader(long))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Text) > 8000 {
		t.Errorf("reply text %d bytes, want at most 8000", len(r.Text))
	}
}

func TestBilledCostFailsClosed(t *testing.T) {
	cases := []struct {
		r    Result
		want float64
	}{
		{Result{CostKnown: true, CostUSD: 0.3}, 0.3},
		{Result{CostKnown: false}, 2},
		{Result{CostKnown: true, CostUSD: -1}, 2},
		{Result{CostKnown: true, CostUSD: math.NaN()}, 2},
		{Result{CostKnown: true, CostUSD: math.Inf(1)}, 2},
		{Result{CostKnown: true, CostUSD: 0}, 0},
		{Result{CostKnown: true, CostUSD: 3.5}, 3.5},
	}
	for _, c := range cases {
		if got := c.r.BilledCost(2); got != c.want {
			t.Errorf("BilledCost(%+v) = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestManagedSettingsLockWhatAPerRunFileCouldLoosen(t *testing.T) {
	data, err := ManagedSettings()
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		DisableBypass    string   `json:"disableBypassPermissionsMode"`
		AllowedProviders []string `json:"allowedProviders"`
		Permissions      struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
		Sandbox struct {
			Enabled                  bool `json:"enabled"`
			FailIfUnavailable        bool `json:"failIfUnavailable"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
		} `json:"sandbox"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.DisableBypass != "disable" || !slices.Equal(m.AllowedProviders, []string{"anthropic"}) {
		t.Errorf("bypass %q providers %v", m.DisableBypass, m.AllowedProviders)
	}
	if !slices.Equal(m.Permissions.Deny, Deny) {
		t.Error("managed deny list differs from the per-run one")
	}
	if !m.Sandbox.Enabled || !m.Sandbox.FailIfUnavailable || m.Sandbox.AllowUnsandboxedCommands {
		t.Errorf("sandbox %+v", m.Sandbox)
	}
	if m.Env["CLAUDE_CODE_SUBPROCESS_ENV_SCRUB"] != "1" || m.Env["DISABLE_UPDATES"] != "1" {
		t.Errorf("env %v", m.Env)
	}
}
