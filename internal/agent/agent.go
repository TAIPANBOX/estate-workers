// Package agent builds the one command line a worker may run Claude Code
// with, and reads what the run reports back.
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/TAIPANBOX/estate-workers/internal/template"
)

// Inputs come from a validated task and template, never from the model.
type Inputs struct {
	Model        string
	MaxTurns     int
	MaxBudgetUSD float64
	AllowedTools []string
	RulesFile    string
	RepoRules    string
	SettingsFile string
}

// MaxLine bounds one line of the stream; Claude Code writes one JSON event
// per line, and a tool result can be large, but not this large.
const MaxLine = 8 << 20

// MaxText bounds the final reply kept for the pull request body.
const MaxText = 8000

// GoProxyHosts are the only hosts the Bash sandbox may reach.
var GoProxyHosts = []string{"proxy.golang.org", "sum.golang.org"}

// Deny rules every run carries on top of the root-owned managed settings.
var Deny = []string{
	"WebFetch", "WebSearch", "Agent",
	"Bash(git push *)", "Bash(git commit *)", "Bash(git config *)", "Bash(git remote *)",
	"Bash(git -c *)", "Bash(git -C * -c *)", "Bash(git -C * push *)", "Bash(git -C * config *)",
	"Bash(git -C * commit *)", "Bash(git --exec-path *)", "Bash(git --config-env *)",
	"Bash(git * --output*)", "Bash(git * --no-index*)", "Bash(git * --ext-diff*)", "Bash(git * --textconv*)",
	"Bash(git * --upload-pack*)", "Bash(git * --exec*)",
	"Bash(curl *)", "Bash(wget *)", "Bash(ssh *)", "Bash(scp *)", "Bash(sudo *)",
	"Bash(cargo *)", "Bash(rustc *)", "Bash(docker *)", "Bash(gh *)",
	"Read(//run/credentials/**)", "Read(//run/claudew-push/**)", "Read(//var/lib/claudew-gh/**)",
	"Read(//etc/credstore.encrypted/**)", "Read(//etc/credstore/**)", "Read(~/.ssh/**)",
	"Edit(./.git)", "Write(./.git)", "Edit(./.git/**)", "Write(./.git/**)", "Edit(./.github/**)", "Write(./.github/**)",
	"Edit(./.claude/**)", "Write(./.claude/**)", "Edit(./.mcp.json)", "Write(./.mcp.json)",
	// The worker's own state, never the job tree the model works in: Claude
	// Code applies these rules to the paths shell commands write as well.
	"Edit(//var/lib/claudew/*.jsonl)", "Write(//var/lib/claudew/*.jsonl)",
	"Edit(//var/lib/claudew/STOP)", "Write(//var/lib/claudew/STOP)",
	"Edit(//var/lib/claudew/queue/**)", "Write(//var/lib/claudew/queue/**)",
	"Edit(//var/lib/claudew/runs/**)", "Write(//var/lib/claudew/runs/**)",
	"Edit(//var/lib/claudew/cache/**)", "Write(//var/lib/claudew/cache/**)",
	"Edit(//var/lib/claudew/jobs/*/githome/**)", "Write(//var/lib/claudew/jobs/*/githome/**)",
	"Edit(//var/lib/claudew/jobs/*/context/**)", "Write(//var/lib/claudew/jobs/*/context/**)",
	"Edit(//var/lib/claudew/jobs/*/files/**)", "Write(//var/lib/claudew/jobs/*/files/**)",
	"Edit(//etc/**)", "Write(//etc/**)",
}

// DenyRead hides these paths from sandboxed shell commands.
var DenyRead = []string{
	"/run/credentials", "/run/claudew-push", "/var/lib/claudew-gh",
	"/etc/credstore", "/etc/credstore.encrypted", "/etc/claudew",
}

// Args builds the command line. It starts with --bare and -p, so no
// subscription login, repository hook, MCP server or plugin is ever read,
// and it never carries a credential.
func Args(in Inputs) []string {
	a := []string{
		"--bare", "-p",
		"--model", in.Model,
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
		"--max-turns", strconv.Itoa(in.MaxTurns),
		"--max-budget-usd", strconv.FormatFloat(in.MaxBudgetUSD, 'f', 2, 64),
	}
	if len(in.AllowedTools) == 0 {
		a = append(a, "--disallowedTools", "*")
	} else {
		a = append(a,
			"--tools", strings.Join(BuiltinTools(in.AllowedTools), ","),
			"--allowedTools", strings.Join(in.AllowedTools, ","),
			"--disallowedTools", "WebFetch,WebSearch,Agent")
	}
	a = append(a, "--append-system-prompt-file", in.RulesFile)
	if in.RepoRules != "" {
		a = append(a, "--append-system-prompt-file", in.RepoRules)
	}
	return append(a,
		"--settings", in.SettingsFile,
		"--no-session-persistence",
		"--output-format", "stream-json", "--verbose")
}

// Env is the whole environment of the run, built from nothing: the parent's
// variables never reach the model. The key appears exactly once.
func Env(key, home, tmp, goRoot string) []string {
	return []string{
		"ANTHROPIC_API_KEY=" + key,
		"PATH=" + goRoot + "/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"LANG=C.UTF-8",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1",
		"DISABLE_UPDATES=1",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
		"DISABLE_TELEMETRY=1",
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
		"GOPROXY=https://proxy.golang.org",
	}
}

type settings struct {
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
			AllowWrite []string `json:"allowWrite,omitempty"`
			DenyRead   []string `json:"denyRead"`
		} `json:"filesystem"`
		Network struct {
			AllowedDomains []string `json:"allowedDomains"`
		} `json:"network"`
	} `json:"sandbox"`
}

// Settings is the per-run settings file: the template's tools as the only
// allow rules, the fixed deny list, and a sandbox that must start or the
// run does not. allowWrite adds the job's own scratch directories to the
// working directory as writable.
func Settings(tools, allowWrite []string) ([]byte, error) {
	for _, p := range allowWrite {
		if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			return nil, fmt.Errorf("allowWrite %q must be an absolute path", p)
		}
	}
	for _, t := range tools {
		if err := template.CheckTool(t); err != nil {
			return nil, err
		}
	}
	var s settings
	s.Permissions.Allow = append([]string{}, tools...)
	s.Permissions.Deny = Deny
	s.Permissions.DefaultMode = "dontAsk"
	s.Sandbox.Enabled = true
	s.Sandbox.FailIfUnavailable = true
	s.Sandbox.AllowUnsandboxedCommands = false
	s.Sandbox.AutoAllowBashIfSandboxed = false
	s.Sandbox.Filesystem.AllowWrite = allowWrite
	s.Sandbox.Filesystem.DenyRead = DenyRead
	s.Sandbox.Network.AllowedDomains = GoProxyHosts
	return json.MarshalIndent(s, "", "  ")
}

type Result struct {
	CostUSD      float64
	CostKnown    bool
	IsError      bool
	Subtype      string
	NumTurns     int
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	SessionID    string
	Text         string
	Lines        int
	Malformed    int
	ResultEvents int
}

type event struct {
	Type         string   `json:"type"`
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	NumTurns     int      `json:"num_turns"`
	Result       string   `json:"result"`
	SessionID    string   `json:"session_id"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Parse reads a stream-json transcript. Lines that are not JSON are counted
// and skipped; a line over MaxLine is an error. With several result events
// the highest cost wins, so the ledger never under-counts.
func Parse(r io.Reader) (Result, error) {
	var res Result
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		res.Lines++
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			res.Malformed++
			continue
		}
		if e.Type != "result" {
			continue
		}
		res.ResultEvents++
		if e.TotalCostUSD != nil {
			c := *e.TotalCostUSD
			if !res.CostKnown || c > res.CostUSD {
				res.CostUSD = c
			}
			res.CostKnown = true
		}
		res.IsError = res.IsError || e.IsError
		res.Subtype = e.Subtype
		res.NumTurns = max(res.NumTurns, e.NumTurns)
		res.InputTokens = max(res.InputTokens, e.Usage.InputTokens)
		res.OutputTokens = max(res.OutputTokens, e.Usage.OutputTokens)
		res.CacheRead = max(res.CacheRead, e.Usage.CacheReadInputTokens)
		res.CacheWrite = max(res.CacheWrite, e.Usage.CacheCreationInputTokens)
		res.SessionID = e.SessionID
		res.Text = bound(e.Result, MaxText)
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("stream: %w", err)
	}
	return res, nil
}

// bound cuts s to at most n bytes without splitting a rune.
func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// BilledCost is what the ledger records. An unknown, negative or non-finite
// cost is recorded as the run's whole cap: the ledger may over-count, never
// under-count.
func (r Result) BilledCost(capUSD float64) float64 {
	if !r.CostKnown || math.IsNaN(r.CostUSD) || math.IsInf(r.CostUSD, 0) || r.CostUSD < 0 {
		return capUSD
	}
	return r.CostUSD
}

// ManagedEnv is forced on every Claude Code process on the host.
var ManagedEnv = map[string]string{
	"DISABLE_UPDATES":                          "1",
	"DISABLE_AUTOUPDATER":                      "1",
	"DISABLE_TELEMETRY":                        "1",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":         "1",
	"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":     "1",
	"CLAUDE_CODE_SUBAGENT_MODEL":               "claude-haiku-5-5",
}

// ManagedSettings is the root-owned /etc/claude-code/managed-settings.json.
// It repeats the deny list and the sandbox lock so a per-run settings file
// cannot loosen them, turns off bypass mode and pins the provider.
func ManagedSettings() ([]byte, error) {
	m := map[string]any{
		"disableBypassPermissionsMode": "disable",
		"allowedProviders":             []string{"anthropic"},
		"permissions":                  map[string]any{"deny": Deny, "defaultMode": "dontAsk"},
		"sandbox": map[string]any{
			"enabled": true, "failIfUnavailable": true, "allowUnsandboxedCommands": false,
			"autoAllowBashIfSandboxed": false,
			"filesystem":               map[string]any{"denyRead": DenyRead},
			"network":                  map[string]any{"allowedDomains": GoProxyHosts},
		},
		"env": ManagedEnv,
	}
	return json.MarshalIndent(m, "", "  ")
}

// BuiltinTools names the built-in tools a run's rules need, in order:
// "Edit(./**)" needs Edit and "Bash(make gates)" needs Bash. Without an
// explicit --tools, --bare offers only Bash, Read and Edit.
func BuiltinTools(allowed []string) []string {
	var out []string
	for _, r := range allowed {
		name, _, _ := strings.Cut(r, "(")
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}
