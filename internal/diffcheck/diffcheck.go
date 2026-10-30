// Package diffcheck decides whether the changes a model made may leave the
// host. It reads git's own machine output and never trusts the model's
// account of what it changed.
package diffcheck

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/TAIPANBOX/estate-workers/internal/glob"
)

// Forbidden paths no template can allow: CI, git internals, agent and tool
// configs, the secret scanner's allowlist, key material.
var Forbidden = []string{
	".github/**", ".githooks/**", ".git/**", "**/.git/**",
	".claude/**", "**/.claude/**", ".mcp.json", "**/.mcp.json",
	".secretscan-allow", "**/.secretscan-allow",
	"**/*.pem", "**/*.key", "**/*.p12", "**/id_rsa*", "**/id_ed25519*",
	"**/.env", "**/.env.*", "**/.netrc", "**/.npmrc", "**/.pypirc",
}

// RegularFile is the only mode a worker may write.
const RegularFile = "100644"

type Change struct {
	Path    string
	Status  byte
	OldMode string
	NewMode string
	Added   int
	Deleted int
	Binary  bool
}

type Rules struct {
	Allowed       []string
	MaxFiles      int
	MaxAddedLines int
}

func checkPath(p string) error {
	if p == "" || !utf8.ValidString(p) {
		return fmt.Errorf("path %q is empty or not UTF-8", p)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q is absolute", p)
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("path %q climbs out of the repository", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path %q carries a control character", p)
		}
	}
	return nil
}

// ParseRaw reads `git diff --raw -z --no-renames --no-abbrev`.
func ParseRaw(raw []byte) ([]Change, error) {
	tokens := bytes.Split(raw, []byte{0})
	var out []Change
	for i := 0; i < len(tokens); i++ {
		head := string(tokens[i])
		if head == "" && i == len(tokens)-1 {
			break
		}
		if !strings.HasPrefix(head, ":") {
			return nil, fmt.Errorf("raw diff: record %q does not start with ':'", head)
		}
		f := strings.Fields(head[1:])
		if len(f) != 5 {
			return nil, fmt.Errorf("raw diff: record %q has %d fields, want 5", head, len(f))
		}
		status := f[4]
		if len(status) != 1 || !strings.Contains("AMDT", status) {
			return nil, fmt.Errorf("raw diff: status %q is not one of A, M, D, T", status)
		}
		if i+1 >= len(tokens) || len(tokens[i+1]) == 0 {
			return nil, fmt.Errorf("raw diff: record %q has no path", head)
		}
		i++
		p := string(tokens[i])
		if err := checkPath(p); err != nil {
			return nil, fmt.Errorf("raw diff: %w", err)
		}
		out = append(out, Change{Path: p, Status: status[0], OldMode: f[0], NewMode: f[1]})
	}
	return out, nil
}

// MergeNumstat adds line counts from `git diff --numstat -z --no-renames`.
func MergeNumstat(changes []Change, numstat []byte) error {
	for _, rec := range bytes.Split(numstat, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		parts := strings.SplitN(string(rec), "\t", 3)
		if len(parts) != 3 {
			return fmt.Errorf("numstat: record %q has %d fields, want 3", rec, len(parts))
		}
		idx := slices.IndexFunc(changes, func(c Change) bool { return c.Path == parts[2] })
		if idx < 0 {
			return fmt.Errorf("numstat: path %q is not in the raw diff", parts[2])
		}
		if parts[0] == "-" && parts[1] == "-" {
			changes[idx].Binary = true
			continue
		}
		added, err1 := strconv.Atoi(parts[0])
		deleted, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || added < 0 || deleted < 0 {
			return fmt.Errorf("numstat: counts %q %q are not numbers", parts[0], parts[1])
		}
		changes[idx].Added, changes[idx].Deleted = added, deleted
	}
	return nil
}

// Check lists every reason the changes may not leave. An empty list means
// they may.
func Check(changes []Change, r Rules) []string {
	if len(changes) == 0 {
		return []string{"empty diff: nothing to push"}
	}
	var v []string
	if len(changes) > r.MaxFiles {
		v = append(v, fmt.Sprintf("%d files changed, the limit is %d", len(changes), r.MaxFiles))
	}
	added := 0
	for _, c := range changes {
		added += c.Added
		if slices.ContainsFunc(Forbidden, func(p string) bool { return glob.Match(p, c.Path) }) {
			v = append(v, fmt.Sprintf("%s is a forbidden path", c.Path))
			continue
		}
		if !slices.ContainsFunc(r.Allowed, func(p string) bool { return glob.Match(p, c.Path) }) {
			v = append(v, fmt.Sprintf("%s is outside the allowed paths", c.Path))
		}
		switch {
		case c.Status == 'D':
			v = append(v, fmt.Sprintf("%s is deleted; workers do not delete", c.Path))
		case c.Status == 'T':
			v = append(v, fmt.Sprintf("%s changes type", c.Path))
		case c.NewMode != RegularFile:
			v = append(v, fmt.Sprintf("%s has mode %s; only regular files (%s) may be written", c.Path, c.NewMode, RegularFile))
		}
		if c.Binary {
			v = append(v, fmt.Sprintf("%s is binary", c.Path))
		}
	}
	if added > r.MaxAddedLines {
		v = append(v, fmt.Sprintf("%d lines added, the limit is %d", added, r.MaxAddedLines))
	}
	return v
}
