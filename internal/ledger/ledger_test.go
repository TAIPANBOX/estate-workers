package ledger

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func line(t *testing.T, e Entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "l.jsonl")
	if err := Append(p, e); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppendThenSumRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	entries := []Entry{
		{ID: "a", Outcome: OutcomeSpent, CostUSD: 1.25, At: now.Add(-40 * 24 * time.Hour)}, // last month
		{ID: "b", Outcome: OutcomeSpent, CostUSD: 2.00, At: now.Add(-3 * 24 * time.Hour)},  // this month
		{ID: "c", Outcome: OutcomeSpent, CostUSD: 0.50, At: now.Add(-2 * time.Hour)},       // today
		{ID: "c", Outcome: OutcomeOK, At: now.Add(-time.Hour)},
		{ID: "d", Outcome: OutcomeSpent, CostUSD: 0.25, At: now.Add(-30 * time.Minute)},
	}
	for _, e := range entries {
		if err := Append(p, e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := Sum(f, now)
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if !near(s.Day, 0.75) || !near(s.Month, 2.75) || !near(s.Total, 4.00) {
		t.Errorf("sums day=%v month=%v total=%v, want 0.75 2.75 4.00", s.Day, s.Month, s.Total)
	}
	if !s.Spent["c"] || !s.Spent["d"] || s.Spent["zzz"] {
		t.Errorf("spent ids wrong: %v", s.Spent)
	}
	if s.Malformed != 0 {
		t.Errorf("malformed %d", s.Malformed)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDayAndMonthAreUTC(t *testing.T) {
	// 23:30 on the 14th in London is still the 14th in UTC: not today.
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no tzdata")
	}
	at := time.Date(2026, 11, 14, 23, 30, 0, 0, london)
	s, err := Sum(strings.NewReader(line(t, Entry{ID: "x", Outcome: OutcomeSpent, CostUSD: 1, At: at})), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Day != 0 || s.Month != 1 {
		t.Errorf("day=%v month=%v, want 0 and 1", s.Day, s.Month)
	}
}

func TestConsecutiveFailuresCountTheTailOnly(t *testing.T) {
	var b strings.Builder
	for i, o := range []string{OutcomeFailed, OutcomeOK, OutcomeFailed, OutcomeSpent, OutcomeError, OutcomeNoChange, OutcomeFailed, OutcomeError} {
		b.WriteString(line(t, Entry{ID: string(rune('a' + i)), Outcome: o, At: now.Add(-time.Hour)}))
	}
	s, err := Sum(strings.NewReader(b.String()), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", s.ConsecutiveFailures)
	}
}

func TestMalformedLinesAreCountedNotSkippedSilently(t *testing.T) {
	good := line(t, Entry{ID: "g", Outcome: OutcomeSpent, CostUSD: 1, At: now.Add(-time.Hour)})
	bad := []string{
		"{not json}\n",
		`{"id":"","outcome":"spent","cost_usd":1,"at":"2026-11-15T10:00:00Z"}` + "\n",
		`{"id":"n","outcome":"spent","cost_usd":-1,"at":"2026-11-15T10:00:00Z"}` + "\n",
		`{"id":"z","outcome":"spent","cost_usd":1}` + "\n",
		`{"id":"f","outcome":"spent","cost_usd":1,"at":"2026-11-16T10:00:00Z"}` + "\n",
		`{"id":"o","outcome":"weird","cost_usd":0,"at":"2026-11-15T10:00:00Z"}` + "\n",
		`{"id":"c","outcome":"ok","cost_usd":3,"at":"2026-11-15T10:00:00Z"}` + "\n",
	}
	for _, b := range bad {
		s, err := Sum(strings.NewReader(good+b), now)
		if err != nil {
			t.Fatalf("Sum(%q): %v", b, err)
		}
		if s.Malformed != 1 {
			t.Errorf("Sum(%q).Malformed = %d, want 1", b, s.Malformed)
		}
	}
}

func TestBlankLinesAreNotMalformed(t *testing.T) {
	s, err := Sum(strings.NewReader("\n\n"+line(t, Entry{ID: "g", Outcome: OutcomeOK, At: now})), now)
	if err != nil || s.Malformed != 0 {
		t.Errorf("err=%v malformed=%d", err, s.Malformed)
	}
}

func TestAnOverlongLineIsAnError(t *testing.T) {
	if _, err := Sum(strings.NewReader(strings.Repeat("x", 2<<20)), now); err == nil {
		t.Error("an overlong line was accepted")
	}
}

func TestAppendRefusesWhatSumWouldCallMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l.jsonl")
	for _, e := range []Entry{
		{ID: "", Outcome: OutcomeOK, At: now},
		{ID: "x", Outcome: "weird", At: now},
		{ID: "x", Outcome: OutcomeSpent, CostUSD: -1, At: now},
		{ID: "x", Outcome: OutcomeSpent, CostUSD: math.NaN(), At: now},
		{ID: "x", Outcome: OutcomeOK, At: time.Time{}},
	} {
		if err := Append(p, e); err == nil {
			t.Errorf("Append(%+v) succeeded", e)
		}
	}
	if err := Append(filepath.Join(t.TempDir(), "missing", "l.jsonl"), Entry{ID: "x", Outcome: OutcomeOK, At: now}); err == nil {
		t.Error("Append into a missing directory succeeded")
	}
}

func TestSumFileTreatsAMissingFileAsEmptyAndReadsAFile(t *testing.T) {
	dir := t.TempDir()
	s, err := SumFile(filepath.Join(dir, "none.jsonl"), now)
	if err != nil || s.Total != 0 || s.Spent == nil {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	p := filepath.Join(dir, "l.jsonl")
	if err := Append(p, Entry{ID: "a", Outcome: OutcomeSpent, CostUSD: 0.5, At: now}); err != nil {
		t.Fatal(err)
	}
	if s, err := SumFile(p, now); err != nil || s.Total != 0.5 {
		t.Errorf("file: %+v %v", s, err)
	}
	if _, err := SumFile(dir, now); err == nil {
		t.Error("a directory read as a ledger")
	}
}

// The failure count is read from the ledger, so after repeated failures only
// a line in the ledger can reset it; clearing the stop files alone left the
// workers stopped for ever (measured on the host 2026-10-30).
func TestResumeResetsTheFailureCount(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	var b strings.Builder
	for i, o := range []string{OutcomeError, OutcomeFailed, OutcomeError, OutcomeResumed, OutcomeError} {
		line, _ := json.Marshal(Entry{ID: fmt.Sprintf("x%d", i), Outcome: o, At: now})
		b.Write(line)
		b.WriteString("\n")
	}
	s, err := Sum(strings.NewReader(b.String()), now)
	if err != nil || s.Malformed != 0 {
		t.Fatalf("sum %+v err %v", s, err)
	}
	if s.ConsecutiveFailures != 1 {
		t.Errorf("failures after a resume and one error = %d, want 1", s.ConsecutiveFailures)
	}
}
