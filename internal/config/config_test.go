package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `{
  "owner": "TAIPANBOX",
  "repos": ["architecture", "agent-stack-go", "tokenfuse"],
  "private_repos": ["architecture"],
  "day_cap_usd": 6,
  "month_cap_usd": 80,
  "billing_verified": false,
  "unverified_cap_usd": 0.10,
  "run_cap_usd": {"claude-haiku-5-5": 0.5, "claude-sonnet-5-5": 2},
  "max_consecutive_failures": 3,
  "commit_name": "taipanbox-workers[bot]",
  "commit_email": "1+taipanbox-workers[bot]@users.noreply.github.com",
  "app_id": "123456",
  "paths": {"state": "/var/lib/claudew", "manual_stop": "/etc/claudew/STOP", "git": "/usr/bin/git"}
}`

func mustParse(t *testing.T, s string) Config {
	t.Helper()
	c, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func TestAGoodConfigParses(t *testing.T) {
	c := mustParse(t, good)
	if !c.RepoAllowed("tokenfuse") || c.RepoAllowed("hermes-openwrt") || c.RepoAllowed("unlisted") {
		t.Error("RepoAllowed does not follow the list")
	}
	if !c.IsPrivate("architecture") || c.IsPrivate("tokenfuse") {
		t.Error("IsPrivate does not follow private_repos")
	}
}

func TestLoadReadsAFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}

func TestHardDeniedRepoCannotBeListed(t *testing.T) {
	for _, r := range []string{"hermes-openwrt", "HERMES-OPENWRT", "estate-workers", "it-rat.github.io"} {
		s := strings.Replace(good, `"tokenfuse"`, `"`+r+`"`, 1)
		if _, err := Parse(strings.NewReader(s)); err == nil || !strings.Contains(err.Error(), "hard-denied") {
			t.Errorf("listing %q: err = %v, want hard-denied", r, err)
		}
	}
}

func TestUnknownFieldIsAnError(t *testing.T) {
	s := strings.Replace(good, `"day_cap_usd"`, `"day_cap_dollars": 1, "day_cap_usd"`, 1)
	if _, err := Parse(strings.NewReader(s)); err == nil {
		t.Error("an unknown field was accepted")
	}
}

func TestTrailingDataIsAnError(t *testing.T) {
	if _, err := Parse(strings.NewReader(good + `{}`)); err == nil {
		t.Error("trailing data was accepted")
	}
}

func TestOversizeConfigIsAnError(t *testing.T) {
	if _, err := Parse(strings.NewReader(strings.Repeat(" ", 1<<20+5))); err == nil {
		t.Error("an oversize config was accepted")
	}
}

func TestUnverifiedCapCannotExceedTheCeiling(t *testing.T) {
	s := strings.Replace(good, `"unverified_cap_usd": 0.10`, `"unverified_cap_usd": 0.11`, 1)
	if _, err := Parse(strings.NewReader(s)); err == nil {
		t.Error("an unverified cap above the ceiling was accepted")
	}
	c := mustParse(t, good)
	c.UnverifiedCapUSD = 5
	if c.EffectiveUnverifiedCap() != UnverifiedCeilingUSD {
		t.Errorf("EffectiveUnverifiedCap = %v, want the ceiling", c.EffectiveUnverifiedCap())
	}
}

func TestCapsAndRunCapsAreChecked(t *testing.T) {
	cases := map[string]string{
		"zero day cap":      strings.Replace(good, `"day_cap_usd": 6`, `"day_cap_usd": 0`, 1),
		"month below day":   strings.Replace(good, `"month_cap_usd": 80`, `"month_cap_usd": 5`, 1),
		"sonnet above hard": strings.Replace(good, `"claude-sonnet-5-5": 2`, `"claude-sonnet-5-5": 2.5`, 1),
		"missing sonnet":    strings.Replace(good, `, "claude-sonnet-5-5": 2`, ``, 1),
		"fable run cap":     strings.Replace(good, `"claude-sonnet-5-5": 2`, `"claude-sonnet-5-5": 2, "claude-fable-5-1": 1`, 1),
		"zero failures":     strings.Replace(good, `"max_consecutive_failures": 3`, `"max_consecutive_failures": 0`, 1),
		"bad owner":         strings.Replace(good, `"TAIPANBOX"`, `"TAIPAN BOX"`, 1),
		"bad repo":          strings.Replace(good, `"tokenfuse"`, `"../etc"`, 1),
		"duplicate repo":    strings.Replace(good, `"tokenfuse"`, `"agent-stack-go"`, 1),
		"private unlisted":  strings.Replace(good, `"private_repos": ["architecture"]`, `"private_repos": ["secret"]`, 1),
		"relative path":     strings.Replace(good, `"/usr/bin/git"`, `"usr/bin/git"`, 1),
		"unclean path":      strings.Replace(good, `"/usr/bin/git"`, `"/usr/bin/../bin/git"`, 1),
		"no repos":          strings.Replace(good, `["architecture", "agent-stack-go", "tokenfuse"]`, `[]`, 1),
		"bad app id":        strings.Replace(good, `"123456"`, `"12x"`, 1),
	}
	for name, s := range cases {
		if _, err := Parse(strings.NewReader(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
