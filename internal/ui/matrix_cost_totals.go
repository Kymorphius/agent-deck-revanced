package ui

// matrix/hardened: header queries are independent of the full cost dashboard,
// budget checks and Web readers. Keep their original Store calculations.
import (
	"time"

	"github.com/asheshgoplani/agent-deck/internal/costs"
)

func (h *Home) queryCostLineTotal(variable string) (int64, error) {
	var summary costs.CostSummary
	var err error
	switch variable {
	case "cost_today":
		summary, err = h.costStore.TotalToday()
	case "cost_yesterday":
		summary, err = h.costStore.TotalYesterday()
	case "cost_this_week":
		summary, err = h.costStore.TotalThisWeek()
	case "cost_last_week":
		summary, err = h.costStore.TotalLastWeek()
	case "cost_this_month":
		summary, err = h.costStore.TotalThisMonth()
	case "cost_last_month":
		summary, err = h.costStore.TotalLastMonth()
	case "cost_projected":
		return h.costStore.ProjectedMonthly()
	}
	return summary.TotalCostMicrodollars, err
}

// matrix/hardened: accepting the clock/query at this private scheduling seam
// makes the exact cadence and unnecessary queries observable without timers.
func (h *Home) refreshCostTotalsForTemplate(now time.Time, query func(string) (int64, error)) {
	if h.costRefreshTemplate == h.costLineTemplate && now.Sub(h.costRefreshTime) < 10*time.Second {
		return
	}
	h.costRefreshTime, h.costRefreshTemplate = now, h.costLineTemplate
	for _, variable := range costs.CostLineVariables(h.costLineTemplate) {
		// Preserve the old best-effort error/value behavior for each queried
		// window; an unused window cannot trigger work or alter another value.
		value, _ := query(variable)
		switch variable {
		case "cost_today":
			h.costToday.Store(value)
		case "cost_yesterday":
			h.costYesterday.Store(value)
		case "cost_this_week":
			h.costWeek.Store(value)
		case "cost_last_week":
			h.costLastWeek.Store(value)
		case "cost_this_month":
			h.costThisMonth.Store(value)
		case "cost_last_month":
			h.costLastMonth.Store(value)
		case "cost_projected":
			h.costProjected.Store(value)
		}
	}
}
