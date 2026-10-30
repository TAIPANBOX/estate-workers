package budget

import (
	"strings"
	"testing"

	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/ledger"
)

func cfg(verified bool) config.Config {
	return config.Config{
		DayCapUSD: 6, MonthCapUSD: 80, BillingVerified: verified, UnverifiedCapUSD: 0.10,
		MaxConsecutiveFailures: 3,
	}
}

func TestAllowsWhenEverythingFits(t *testing.T) {
	d := Decide(cfg(true), ledger.Sums{Day: 1, Month: 10, Total: 50}, 2, false, "")
	if !d.Allow || d.Stop {
		t.Fatalf("decision %+v, want allow", d)
	}
}

func TestStopFileWins(t *testing.T) {
	d := Decide(cfg(true), ledger.Sums{}, 0.5, true, "manual: stop requested")
	if d.Allow || !strings.Contains(d.Reason, "manual") {
		t.Fatalf("decision %+v, want refused with the stop reason", d)
	}
}

func TestUnverifiedBillingCapsTotalSpendAtTenCents(t *testing.T) {
	// The probe itself fits: 0 spent + 0.09 reserved.
	if d := Decide(cfg(false), ledger.Sums{}, 0.09, false, ""); !d.Allow {
		t.Fatalf("probe refused: %+v", d)
	}
	// After the probe spent 0.06, nothing that could cost 0.09 more may start.
	d := Decide(cfg(false), ledger.Sums{Total: 0.06, Month: 0.06, Day: 0.06}, 0.09, false, "")
	if d.Allow || d.Stop || !strings.Contains(d.Reason, "billing") {
		t.Fatalf("decision %+v, want refused without a stop, naming billing", d)
	}
	// Spend from an earlier month still counts while billing is unverified.
	if d := Decide(cfg(false), ledger.Sums{Total: 0.08}, 0.05, false, ""); d.Allow {
		t.Fatalf("old spend ignored: %+v", d)
	}
}

func TestUnverifiedCapNeverExceedsTheCeilingEvenIfConfigSaysMore(t *testing.T) {
	c := cfg(false)
	c.UnverifiedCapUSD = 50
	if d := Decide(c, ledger.Sums{}, 0.5, false, ""); d.Allow {
		t.Fatalf("a $0.50 run allowed before billing is verified: %+v", d)
	}
}

func TestMonthCapRefusesAndStops(t *testing.T) {
	d := Decide(cfg(true), ledger.Sums{Day: 0, Month: 79, Total: 300}, 2, false, "")
	if d.Allow || !d.Stop || !strings.Contains(d.Reason, "month") {
		t.Fatalf("decision %+v, want refused with stop naming the month", d)
	}
}

func TestDayCapRefusesWithoutStopping(t *testing.T) {
	d := Decide(cfg(true), ledger.Sums{Day: 5.5, Month: 20}, 2, false, "")
	if d.Allow || d.Stop || !strings.Contains(d.Reason, "day") {
		t.Fatalf("decision %+v, want refused without stop naming the day", d)
	}
}

func TestExactFitIsAllowed(t *testing.T) {
	if d := Decide(cfg(true), ledger.Sums{Day: 4, Month: 78}, 2, false, ""); !d.Allow {
		t.Fatalf("exact fit refused: %+v", d)
	}
}

func TestMalformedLedgerStops(t *testing.T) {
	d := Decide(cfg(true), ledger.Sums{Malformed: 1}, 0.5, false, "")
	if d.Allow || !d.Stop || !strings.Contains(d.Reason, "ledger") {
		t.Fatalf("decision %+v, want refused with stop naming the ledger", d)
	}
}

func TestConsecutiveFailuresStop(t *testing.T) {
	if d := Decide(cfg(true), ledger.Sums{ConsecutiveFailures: 2}, 0.5, false, ""); !d.Allow {
		t.Fatalf("two failures refused: %+v", d)
	}
	d := Decide(cfg(true), ledger.Sums{ConsecutiveFailures: 3}, 0.5, false, "")
	if d.Allow || !d.Stop || !strings.Contains(d.Reason, "failures") {
		t.Fatalf("decision %+v, want refused with stop naming failures", d)
	}
}

func TestNonPositiveRunCapIsRefused(t *testing.T) {
	for _, c := range []float64{0, -1} {
		if d := Decide(cfg(true), ledger.Sums{}, c, false, ""); d.Allow {
			t.Errorf("run cap %v allowed", c)
		}
	}
}
