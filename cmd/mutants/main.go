// Command mutants breaks the product code on purpose, one named fault at a
// time, in a copy of the module, and requires the tests of the touched
// package to go red. A mutant that survives means a silent wrong answer no
// test would catch. Run from the module root: go run ./cmd/mutants
package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type mutant struct {
	name, file, old, new string
}

var mutants = []mutant{
	{"a run offers only the minimal-mode tools", "internal/agent/agent.go",
		`"--tools", strings.Join(BuiltinTools(in.AllowedTools), ","),`, ``},
	{"the deny covers the whole state directory again", "internal/agent/agent.go",
		`"Edit(//var/lib/claudew/queue/**)", `, `"Edit(//var/lib/claudew/**)", `},
	{"the git home is writable by the model", "internal/agent/agent.go",
		`"Edit(//var/lib/claudew/jobs/*/githome/**)", `, ``},
	{"budget ignores the run's own reservation against the month cap", "internal/budget/budget.go",
		"case s.Month+runCapUSD > cfg.MonthCapUSD:", "case s.Month > cfg.MonthCapUSD:"},
	{"budget forgets unverified billing", "internal/budget/budget.go",
		"case !cfg.BillingVerified && s.Total+runCapUSD > cfg.EffectiveUnverifiedCap():", "case false:"},
	{"budget runs on an untrusted ledger", "internal/budget/budget.go",
		"case s.Malformed > 0:", "case s.Malformed > 1000:"},
	{"budget ignores a failure streak", "internal/budget/budget.go",
		"case s.ConsecutiveFailures >= cfg.MaxConsecutiveFailures:", "case false:"},
	{"an unknown cost is billed as zero", "internal/agent/agent.go",
		"if !r.CostKnown || math.IsNaN(r.CostUSD)", "if math.IsNaN(r.CostUSD)"},
	{"several results take the last cost, not the highest", "internal/agent/agent.go",
		"if !res.CostKnown || c > res.CostUSD {", "if true {"},
	{"claude runs without --bare", "internal/agent/agent.go",
		`"--bare", "-p",`, `"-p",`},
	{"subprocesses keep the key in their environment", "internal/agent/agent.go",
		`"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1",`, ``},
	{"the sandbox may fall back to running unsandboxed", "internal/agent/agent.go",
		"s.Sandbox.FailIfUnavailable = true", "s.Sandbox.FailIfUnavailable = false"},
	{"forbidden paths are not checked", "internal/diffcheck/diffcheck.go",
		"if slices.ContainsFunc(Forbidden, func(p string) bool { return glob.Match(p, c.Path) }) {", "if false {"},
	{"modes other than a regular file pass", "internal/diffcheck/diffcheck.go",
		"case c.NewMode != RegularFile:", "case false:"},
	{"the added-line limit is ignored", "internal/diffcheck/diffcheck.go",
		"if added > r.MaxAddedLines {", "if false {"},
	{"a climbing path is accepted", "internal/diffcheck/diffcheck.go",
		`if slices.Contains(strings.Split(p, "/"), "..") {`, `if false {`},
	{"glob ** stops matching deeper paths", "internal/glob/glob.go",
		"for i := 0; i <= len(n); i++ {", "for i := 0; i < 1; i++ {"},
	{"push accepts a base that is not on main", "internal/push/push.go",
		`if ok, err := g.IsAncestor(ctx, m.BaseSHA, "refs/remotes/origin/"+base); err != nil || !ok {`, `if false {`},
	{"push ignores a secret finding", "internal/push/push.go",
		"if found {", "if found && false {"},
	{"push skips its own diff check", "internal/push/push.go",
		`Allowed: r.AllowedPaths, MaxFiles: r.Tpl.MaxFiles, MaxAddedLines: r.Tpl.MaxAddedLines}); len(v) > 0 {`,
		`Allowed: r.AllowedPaths, MaxFiles: r.Tpl.MaxFiles, MaxAddedLines: r.Tpl.MaxAddedLines}); len(v) > 100 {`},
	{"push trusts the manifest head", "internal/push/push.go",
		"if head != m.HeadSHA {", "if false {"},
	{"ledger accepts a cost on a closing entry", "internal/ledger/ledger.go",
		"case e.Outcome != OutcomeSpent && e.CostUSD != 0:", "case false:"},
	{"ledger counts every day of the month as today", "internal/ledger/ledger.go",
		"if at.Day() == now.Day() {", "if true {"},
	{"ledger accepts future-dated entries", "internal/ledger/ledger.go",
		"|| e.At.After(now.Add(futureSlack))", ""},
	{"a task may target a repo the config does not list", "internal/task/task.go",
		"if !cfg.RepoAllowed(t.Repo) {", "if false {"},
	{"a context repo is not checked", "internal/task/task.go",
		"if !cfg.RepoAllowed(repo) {", "if false {"},
	{"refused shell words are allowed", "internal/template/template.go",
		"if slices.Contains(refusedFirstWords, fields[0]) {", "if false {"},
	{"a git subcommand that writes or fetches is allowed", "internal/template/template.go",
		"if i >= len(fields) || !slices.Contains(gitReadOnly, fields[i]) {", "if i >= len(fields) {"},
	{"git --output is allowed", "internal/template/template.go",
		`strings.HasPrefix(f, "--output") || f == "--ext-diff" ||`, `f == "--ext-diff" ||`},
	{"Fable is allowed", "internal/models/models.go",
		`if strings.Contains(lower, "fable") || strings.Contains(lower, "mythos") {`,
		`if strings.Contains(lower, "fable-x") || strings.Contains(lower, "mythos-x") {`},
	{"Opus runs without a reason", "internal/models/models.go",
		"if model == Opus && len(strings.TrimSpace(reason)) < MinOpusReason {", "if false {"},
	{"a hard-denied repo may be configured", "internal/config/config.go",
		"case isHardDenied(r):", "case false:"},
	{"the unverified cap may exceed ten cents", "internal/config/config.go",
		"return min(c.UnverifiedCapUSD, UnverifiedCeilingUSD)", "return c.UnverifiedCapUSD"},
	{"an unreadable stop file means running", "internal/stop/stop.go",
		`return true, fmt.Sprintf("%s cannot be checked (%v), treated as stopped", p, err)`,
		`return false, fmt.Sprintf("%s cannot be checked (%v), treated as stopped", p, err)`},
	{"repository hooks run", "internal/gitx/gitx.go",
		`{"core.hooksPath", "/dev/null"},`, ``},
	{"gates share the network", "internal/gates/gates.go",
		`"--unshare-all", "--die-with-parent", "--new-session",`, `"--unshare-user", "--die-with-parent", "--new-session",`},
	{"a refused diff does not stop the workers", "internal/drain/drain.go",
		`_ = stop.Write(l.AutoStop(), r.ID+": "+reason, d.now())`, `_ = reason`},
	{"a stop file does not cancel a running model", "internal/drain/drain.go",
		"stopped.Store(true)\n\t\t\t\t\tcancel()", "stopped.Store(true)"},
	{"an interrupted run is not charged", "internal/drain/drain.go",
		"if f, err := os.Open(transcript); err == nil && !sums.Spent[id] {", "if f, err := os.Open(transcript); err == nil && !sums.Spent[id] && false {"},
	{"the App token asks for every repository", "internal/github/github.go",
		`"repositories": []string{repo},`, ``},
}

var failRe = regexp.MustCompile(`(?m)^\s*--- FAIL: (\S+)`)

func copyModule(dst string) error {
	return filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (p == ".git" || p == "bin") {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, p)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func main() {
	start := time.Now()
	killed, failed := 0, 0
	for i, m := range mutants {
		tmp, err := os.MkdirTemp("", "mutant")
		if err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		if err := copyModule(tmp); err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		path := filepath.Join(tmp, m.file)
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		if n := bytes.Count(src, []byte(m.old)); n != 1 {
			fmt.Printf("M%02d NOT APPLIED  %s: the text to replace occurs %d times\n", i+1, m.name, n)
			failed++
			os.RemoveAll(tmp)
			continue
		}
		if err := os.WriteFile(path, bytes.Replace(src, []byte(m.old), []byte(m.new), 1), 0o644); err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		pkg := "./" + filepath.Dir(m.file) + "/"
		cmd := exec.Command("go", "test", "-count=1", "-timeout", "120s", pkg)
		cmd.Dir = tmp
		out, err := cmd.CombinedOutput()
		os.RemoveAll(tmp)
		if err == nil {
			fmt.Printf("M%02d SURVIVED     %s (%s)\n", i+1, m.name, m.file)
			failed++
			continue
		}
		var tests []string
		for _, sub := range failRe.FindAllStringSubmatch(string(out), -1) {
			tests = append(tests, sub[1])
		}
		if len(tests) == 0 {
			tests = []string{"build failed: " + firstLine(string(out))}
		}
		killed++
		fmt.Printf("M%02d killed       %s\n            by %s\n", i+1, m.name, strings.Join(tests, ", "))
	}
	fmt.Printf("\n%d of %d mutants killed in %s\n", killed, len(mutants), time.Since(start).Round(time.Second))
	if failed > 0 {
		os.Exit(1)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
