// Package budget decides, before a run starts, whether the money is there.
// Each run reserves its own cap: a run is allowed only if the spend so far
// plus the most this run could cost still fits under every ceiling.
package budget

import (
	"fmt"

	"github.com/TAIPANBOX/estate-workers/internal/config"
	"github.com/TAIPANBOX/estate-workers/internal/ledger"
)

type Decision struct {
	Allow  bool
	Stop   bool
	Reason string
}

// Decide checks, in order: the stop files, the ledger's own health, the
// failure streak, the unverified-billing ceiling, the month cap and the day
// cap. A month cap or a broken ledger also stops the workers; a day cap only
// defers the run to tomorrow.
func Decide(cfg config.Config, s ledger.Sums, runCapUSD float64, stopped bool, stopReason string) Decision {
	switch {
	case stopped:
		return Decision{Reason: "stopped: " + stopReason}
	case !(runCapUSD > 0):
		return Decision{Reason: fmt.Sprintf("run cap %.2f is not above zero", runCapUSD)}
	case s.Malformed > 0:
		return Decision{Stop: true, Reason: fmt.Sprintf("ledger has %d line(s) it cannot trust; spend is unknown", s.Malformed)}
	case s.ConsecutiveFailures >= cfg.MaxConsecutiveFailures:
		return Decision{Stop: true, Reason: fmt.Sprintf("%d consecutive failures", s.ConsecutiveFailures)}
	case !cfg.BillingVerified && s.Total+runCapUSD > cfg.EffectiveUnverifiedCap():
		return Decision{Reason: fmt.Sprintf("billing not verified: total %.4f plus this run's cap %.2f exceeds %.2f",
			s.Total, runCapUSD, cfg.EffectiveUnverifiedCap())}
	case s.Month+runCapUSD > cfg.MonthCapUSD:
		return Decision{Stop: true, Reason: fmt.Sprintf("month cap: spent %.2f plus this run's cap %.2f exceeds %.2f",
			s.Month, runCapUSD, cfg.MonthCapUSD)}
	case s.Day+runCapUSD > cfg.DayCapUSD:
		return Decision{Reason: fmt.Sprintf("day cap: spent %.2f plus this run's cap %.2f exceeds %.2f",
			s.Day, runCapUSD, cfg.DayCapUSD)}
	}
	return Decision{Allow: true}
}
