// Package features binds every Gherkin scenario to the Go tests that prove
// it, both ways: a scenario without a test fails, and a binding that names
// a test that does not exist fails.
package features

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type scenario struct {
	name  string
	tests []string
	line  int
}

func scenarios(t *testing.T, path string) []scenario {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []scenario
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if name, ok := strings.CutPrefix(line, "Scenario:"); ok {
			out = append(out, scenario{name: strings.TrimSpace(name), line: n})
			continue
		}
		if list, ok := strings.CutPrefix(line, "# test:"); ok && len(out) > 0 {
			for _, name := range strings.Split(list, ",") {
				if name = strings.TrimSpace(name); name != "" {
					out[len(out)-1].tests = append(out[len(out)-1].tests, name)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// testFuncs lists every Test function in the module.
func testFuncs(t *testing.T, root string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "bin") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				found[fn.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestEveryScenarioIsBoundToTestsThatExist(t *testing.T) {
	files, err := filepath.Glob("*.feature")
	if err != nil || len(files) == 0 {
		t.Fatalf("no feature files: %v", err)
	}
	tests := testFuncs(t, "..")
	total := 0
	for _, f := range files {
		for _, s := range scenarios(t, f) {
			total++
			if len(s.tests) == 0 {
				t.Errorf("%s:%d scenario %q has no # test: binding", f, s.line, s.name)
			}
			for _, name := range s.tests {
				if !tests[name] {
					t.Errorf("%s:%d scenario %q binds %s, which does not exist", f, s.line, s.name, name)
				}
			}
		}
	}
	if total == 0 {
		t.Error("measured nothing: no scenarios found")
	}
}

func TestTheBindingGateHasTeeth(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.feature")
	os.WriteFile(bad, []byte("Feature: x\n  Scenario: unbound\n  Scenario: dangling\n    # test: TestNothingByThisName\n"), 0o644)
	got := scenarios(t, bad)
	tests := testFuncs(t, "..")
	if len(got) != 2 || len(got[0].tests) != 0 || tests[got[1].tests[0]] {
		t.Errorf("the gate would not catch an unbound or dangling scenario: %+v", got)
	}
}
