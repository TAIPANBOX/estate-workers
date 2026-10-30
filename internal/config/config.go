// Package config reads the worker configuration that lives in a root-owned
// file on the host. The workers can read it but never change it.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/TAIPANBOX/estate-workers/internal/models"
)

// UnverifiedCeilingUSD is the most the workers may spend in total before a
// person has confirmed in the Console that runs draw the promotional credits
// and not a purchased balance. No config can raise it.
const UnverifiedCeilingUSD = 0.10

// HardDenied repositories are never a target or a context of a worker,
// whatever the config lists. estate-workers is here so that a worker can
// never edit its own code, templates or rules.
var HardDenied = []string{
	"hermes-openwrt",
	"it-rat.github.io",
	"bank-in-a-box",
	"homelab",
	"claude-memory",
	"llm-wiki",
	"YouTube",
	"YouTube-Music",
	"estate-workers",
}

type Paths struct {
	State      string `json:"state"`
	ManualStop string `json:"manual_stop"`
	Claude     string `json:"claude"`
	Secretscan string `json:"secretscan"`
	Bwrap      string `json:"bwrap"`
	Git        string `json:"git"`
	GoRoot     string `json:"go_root"`
	PushSocket string `json:"push_socket"`
	PushState  string `json:"push_state"`
}

type Config struct {
	Owner                  string             `json:"owner"`
	Repos                  []string           `json:"repos"`
	PrivateRepos           []string           `json:"private_repos"`
	DayCapUSD              float64            `json:"day_cap_usd"`
	MonthCapUSD            float64            `json:"month_cap_usd"`
	BillingVerified        bool               `json:"billing_verified"`
	UnverifiedCapUSD       float64            `json:"unverified_cap_usd"`
	RunCapUSD              map[string]float64 `json:"run_cap_usd"`
	MaxConsecutiveFailures int                `json:"max_consecutive_failures"`
	CommitName             string             `json:"commit_name"`
	CommitEmail            string             `json:"commit_email"`
	AppID                  string             `json:"app_id"`
	Paths                  Paths              `json:"paths"`
}

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	appIDRe = regexp.MustCompile(`^[0-9]{1,12}$`)
)

// Load reads and validates the config at path.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse decodes a config strictly: unknown fields and trailing data are
// errors, because a misspelt cap silently left at zero is worse than a
// refusal to start.
func Parse(r io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if err != nil {
		return Config{}, err
	}
	if len(data) > 1<<20 {
		return Config{}, errors.New("config is larger than 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if dec.More() {
		return Config{}, errors.New("config: trailing data after the object")
	}
	return c, c.Validate()
}

// Validate checks every field a worker relies on for money or reach.
func (c Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if !ownerRe.MatchString(c.Owner) {
		add("owner %q is not a GitHub account name", c.Owner)
	}
	if len(c.Repos) == 0 {
		add("repos is empty")
	}
	seen := map[string]bool{}
	for _, r := range c.Repos {
		switch {
		case !repoRe.MatchString(r):
			add("repo %q is not a repository name", r)
		case isHardDenied(r):
			add("repo %q is hard-denied and may not be listed", r)
		case seen[strings.ToLower(r)]:
			add("repo %q is listed twice", r)
		}
		seen[strings.ToLower(r)] = true
	}
	for _, r := range c.PrivateRepos {
		if !slices.Contains(c.Repos, r) {
			add("private repo %q is not in repos", r)
		}
	}
	if !(c.DayCapUSD > 0) {
		add("day_cap_usd must be above zero")
	}
	if !(c.MonthCapUSD >= c.DayCapUSD) {
		add("month_cap_usd must be at least day_cap_usd")
	}
	if c.UnverifiedCapUSD < 0 || c.UnverifiedCapUSD > UnverifiedCeilingUSD {
		add("unverified_cap_usd must be between 0 and %.2f", UnverifiedCeilingUSD)
	}
	for _, m := range []string{models.Haiku, models.Sonnet} {
		if _, ok := c.RunCapUSD[m]; !ok {
			add("run_cap_usd has no entry for %s", m)
		}
	}
	for m, v := range c.RunCapUSD {
		hard, ok := models.HardMaxRunUSD[m]
		switch {
		case !ok:
			add("run_cap_usd names unknown model %q", m)
		case !(v > 0) || v > hard:
			add("run_cap_usd for %s must be above zero and at most %.2f", m, hard)
		}
	}
	if c.MaxConsecutiveFailures < 1 || c.MaxConsecutiveFailures > 10 {
		add("max_consecutive_failures must be between 1 and 10")
	}
	if c.AppID != "" && !appIDRe.MatchString(c.AppID) {
		add("app_id %q is not a number", c.AppID)
	}
	for name, p := range map[string]string{
		"state": c.Paths.State, "manual_stop": c.Paths.ManualStop, "claude": c.Paths.Claude,
		"secretscan": c.Paths.Secretscan, "bwrap": c.Paths.Bwrap, "git": c.Paths.Git,
		"go_root": c.Paths.GoRoot, "push_socket": c.Paths.PushSocket, "push_state": c.Paths.PushState,
	} {
		if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p) {
			add("paths.%s %q must be an absolute, clean path", name, p)
		}
	}
	if len(errs) > 0 {
		slices.Sort(errs)
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}

func isHardDenied(repo string) bool {
	for _, d := range HardDenied {
		if strings.EqualFold(d, repo) {
			return true
		}
	}
	return false
}

// RepoAllowed is true when repo is listed and not hard-denied.
func (c Config) RepoAllowed(repo string) bool {
	return !isHardDenied(repo) && slices.Contains(c.Repos, repo)
}

// IsPrivate is true for repositories that need a token even to read.
func (c Config) IsPrivate(repo string) bool {
	return slices.Contains(c.PrivateRepos, repo)
}

// EffectiveUnverifiedCap is the configured cap, never above the ceiling.
func (c Config) EffectiveUnverifiedCap() float64 {
	return min(c.UnverifiedCapUSD, UnverifiedCeilingUSD)
}
