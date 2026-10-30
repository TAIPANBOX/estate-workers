// Package ledger is the append-only record of every run and what it cost.
// The budget reads it before each run, so a ledger it cannot trust stops
// the workers instead of letting them spend blind.
package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"time"
)

// Outcomes. OutcomeSpent is written right after the model returns, with the
// cost, so money is recorded even if a later step crashes. The others close
// a run and carry no cost.
const (
	OutcomeSpent    = "spent"
	OutcomeOK       = "ok"
	OutcomeNoChange = "no-change"
	OutcomeRefused  = "refused"
	OutcomeFailed   = "failed"
	OutcomeError    = "error"
	OutcomeStopped  = "stopped"
)

var outcomes = []string{OutcomeSpent, OutcomeOK, OutcomeNoChange, OutcomeRefused, OutcomeFailed, OutcomeError, OutcomeStopped}

// MaxLine bounds one ledger line; a longer line is an error, not a skip.
const MaxLine = 1 << 20

// Clock skew tolerated before a future-dated entry counts as malformed.
const futureSlack = time.Hour

type Entry struct {
	ID           string    `json:"id"`
	Template     string    `json:"template,omitempty"`
	Repo         string    `json:"repo,omitempty"`
	Model        string    `json:"model,omitempty"`
	Outcome      string    `json:"outcome"`
	CostUSD      float64   `json:"cost_usd"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	CacheRead    int64     `json:"cache_read_tokens,omitempty"`
	CacheWrite   int64     `json:"cache_write_tokens,omitempty"`
	PRURL        string    `json:"pr_url,omitempty"`
	Note         string    `json:"note,omitempty"`
	At           time.Time `json:"at"`
}

type Sums struct {
	Day                 float64
	Month               float64
	Total               float64
	ConsecutiveFailures int
	Malformed           int
	Spent               map[string]bool
}

func (e Entry) check() error {
	switch {
	case e.ID == "":
		return errors.New("entry has no id")
	case !slices.Contains(outcomes, e.Outcome):
		return fmt.Errorf("entry %s has unknown outcome %q", e.ID, e.Outcome)
	case math.IsNaN(e.CostUSD) || math.IsInf(e.CostUSD, 0) || e.CostUSD < 0:
		return fmt.Errorf("entry %s has cost %v", e.ID, e.CostUSD)
	case e.Outcome != OutcomeSpent && e.CostUSD != 0:
		return fmt.Errorf("entry %s carries a cost on outcome %q", e.ID, e.Outcome)
	case e.At.IsZero():
		return fmt.Errorf("entry %s has no time", e.ID)
	}
	return nil
}

// Append writes one entry and syncs it to disk before returning.
func Append(path string, e Entry) error {
	if err := e.check(); err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Sum totals spend for the UTC day and month of now and for all time, and
// counts the failures at the tail. Lines it cannot trust are counted, not
// skipped, so the budget can refuse to run on them.
func Sum(r io.Reader, now time.Time) (Sums, error) {
	s := Sums{Spent: map[string]bool{}}
	now = now.UTC()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil || e.check() != nil || e.At.After(now.Add(futureSlack)) {
			s.Malformed++
			continue
		}
		at := e.At.UTC()
		if e.Outcome == OutcomeSpent {
			s.Total += e.CostUSD
			s.Spent[e.ID] = true
			if at.Year() == now.Year() && at.Month() == now.Month() {
				s.Month += e.CostUSD
				if at.Day() == now.Day() {
					s.Day += e.CostUSD
				}
			}
			continue
		}
		switch e.Outcome {
		case OutcomeFailed, OutcomeError:
			s.ConsecutiveFailures++
		case OutcomeOK, OutcomeNoChange:
			s.ConsecutiveFailures = 0
		}
	}
	if err := sc.Err(); err != nil {
		return s, fmt.Errorf("ledger: %w", err)
	}
	return s, nil
}

// SumFile is Sum over a file; a missing file is an empty ledger.
func SumFile(path string, now time.Time) (Sums, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Sums{Spent: map[string]bool{}}, nil
	}
	if err != nil {
		return Sums{}, err
	}
	defer f.Close()
	return Sum(f, now)
}
