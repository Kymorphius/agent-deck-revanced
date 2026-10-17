package ui

// matrix/hardened: test query admission separately from real SQLite window
// routing; no timer, transcript read, fee import or native agent is involved.
import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/costs"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func TestMatrixCostTotalsOnlyDisplayedWindows(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		want           []string
	}{
		{"default", "{cost_today} today", []string{"cost_today"}},
		{"empty", "", nil},
		{"literal", "manual totals", nil},
		{"unknown", "{cost_unknown}", nil},
		{"unclosed", "{cost_today", nil},
		{"nested", "{{cost_today}}", nil},
		{"duplicate", "{cost_projected} / {cost_today} / {cost_projected}", []string{"cost_projected", "cost_today"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Home{costLineTemplate: tc.template}
			var calls []string
			h.refreshCostTotalsForTemplate(time.Unix(100, 0), func(variable string) (int64, error) {
				calls = append(calls, variable)
				return 123, nil
			})
			if !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("queries = %v, want %v", calls, tc.want)
			}
		})
	}
}

func TestMatrixCostTotalsCadenceAndTemplateChange(t *testing.T) {
	h := &Home{costLineTemplate: "{cost_today} today"}
	now := time.Unix(100, 0)
	var calls []string
	query := func(variable string) (int64, error) {
		calls = append(calls, variable)
		return int64(len(calls)) * 1_000_000, nil
	}
	h.refreshCostTotalsForTemplate(now, query)
	h.refreshCostTotalsForTemplate(now.Add(10*time.Second-time.Nanosecond), query)
	if len(calls) != 1 || h.costToday.Load() != 1_000_000 {
		t.Fatal("same-template cooldown changed")
	}
	h.refreshCostTotalsForTemplate(now.Add(10*time.Second), query)
	if len(calls) != 2 || h.costToday.Load() != 2_000_000 {
		t.Fatal("ten-second update did not land")
	}
	// A newly visible window must not retain a zero/stale value for the
	// previous template's remaining ten-second cooldown.
	h.costLineTemplate = "{cost_last_month} last month"
	h.refreshCostTotalsForTemplate(now.Add(11*time.Second), query)
	if len(calls) != 3 || calls[2] != "cost_last_month" || h.costLastMonth.Load() != 3_000_000 {
		t.Fatal("new template did not refresh immediately on its next tick")
	}
	h.refreshCostTotalsForTemplate(now.Add(20*time.Second), query)
	if len(calls) != 3 {
		t.Fatal("new template did not establish its own cooldown")
	}
	h.costLineTemplate = ""
	h.refreshCostTotalsForTemplate(now.Add(21*time.Second), query)
	if len(calls) != 3 {
		t.Fatal("disabled cost line queried a window")
	}
	h.costLineTemplate = "{cost_today}"
	h.refreshCostTotalsForTemplate(now.Add(22*time.Second), query)
	if len(calls) != 4 || h.costToday.Load() != 4_000_000 {
		t.Fatal("re-enabled cost line reused its previous cached amount")
	}
}

func TestMatrixCostTotalsBestEffortErrorsRemainWindowLocal(t *testing.T) {
	h := &Home{costLineTemplate: "{cost_today}{cost_projected}"}
	h.costToday.Store(999)
	h.costYesterday.Store(456)
	h.refreshCostTotalsForTemplate(time.Unix(100, 0), func(variable string) (int64, error) {
		if variable == "cost_today" {
			return 0, errors.New("query failed")
		}
		return 123, errors.New("partial value")
	})
	if h.costToday.Load() != 0 || h.costProjected.Load() != 123 || h.costYesterday.Load() != 456 {
		t.Fatal("best-effort query behavior or an unused window changed")
	}
}

func matrixCostTotalsStore(t *testing.T) *costs.Store {
	t.Helper()
	db, err := statedb.Open(filepath.Join(t.TempDir(), "cost-totals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	store := costs.NewStore(db.DB())
	now := time.Now().UTC()
	store.SetClock(func() time.Time { return now })
	for index, ago := range []time.Duration{0, 24 * time.Hour, 8 * 24 * time.Hour, 35 * 24 * time.Hour} {
		if err := store.WriteCostEvent(costs.CostEvent{ID: string(rune('a' + index)), SessionID: "totals-fixture", Timestamp: now.Add(-ago), Model: "fixture", CostMicrodollars: int64(index+1) * 1_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestMatrixCostTotalsExistingStoreWindowsAndDashboardRemainComplete(t *testing.T) {
	store := matrixCostTotalsStore(t)
	today, _ := store.TotalToday()
	yesterday, _ := store.TotalYesterday()
	week, _ := store.TotalThisWeek()
	lastWeek, _ := store.TotalLastWeek()
	month, _ := store.TotalThisMonth()
	lastMonth, _ := store.TotalLastMonth()
	projected, _ := store.ProjectedMonthly()
	h := &Home{costStore: store, costLineTemplate: "{cost_today}{cost_yesterday}{cost_this_week}{cost_last_week}{cost_this_month}{cost_last_month}{cost_projected}"}
	h.refreshCostTotals()
	want := []int64{today.TotalCostMicrodollars, yesterday.TotalCostMicrodollars, week.TotalCostMicrodollars, lastWeek.TotalCostMicrodollars, month.TotalCostMicrodollars, lastMonth.TotalCostMicrodollars, projected}
	got := []int64{h.costToday.Load(), h.costYesterday.Load(), h.costWeek.Load(), h.costLastWeek.Load(), h.costThisMonth.Load(), h.costLastMonth.Load(), h.costProjected.Load()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("existing window calculations changed: %v != %v", got, want)
	}
	// A default-only header leaves the other Home caches unused; opening $
	// still reads the store directly and presents every dashboard aggregate.
	defaultHome := &Home{costStore: store, costLineTemplate: "{cost_today} today"}
	defaultHome.refreshCostTotals()
	if defaultHome.costWeek.Load() != 0 || defaultHome.costThisMonth.Load() != 0 || defaultHome.costProjected.Load() != 0 {
		t.Fatal("default header queried unused windows")
	}
	dashboard := newCostDashboard(store, 80, 24)
	if dashboard.today.TotalCostMicrodollars != today.TotalCostMicrodollars || dashboard.week.TotalCostMicrodollars != week.TotalCostMicrodollars || dashboard.month.TotalCostMicrodollars != month.TotalCostMicrodollars || dashboard.projected != projected || len(dashboard.top) == 0 || len(dashboard.byModel) == 0 {
		t.Fatal("header optimization removed full dashboard data")
	}
}

func TestMatrixCostTotalsManualUpdateRetainsTenSecondVisibility(t *testing.T) {
	store := matrixCostTotalsStore(t)
	h := &Home{costStore: store, costLineTemplate: "{cost_today} today"}
	now := time.Now()
	h.refreshCostTotalsForTemplate(now, h.queryCostLineTotal)
	before := h.costToday.Load()
	if err := store.WriteCostEvent(costs.CostEvent{ID: "manual-update", SessionID: "totals-fixture", Timestamp: now.UTC(), Model: "fixture", CostMicrodollars: 123_456}); err != nil {
		t.Fatal(err)
	}
	h.refreshCostTotalsForTemplate(now.Add(9*time.Second), h.queryCostLineTotal)
	if h.costToday.Load() != before {
		t.Fatal("manual update bypassed the existing cooldown")
	}
	h.refreshCostTotalsForTemplate(now.Add(10*time.Second), h.queryCostLineTotal)
	if h.costToday.Load() != before+123_456 {
		t.Fatal("manual update missed the existing ten-second visibility limit")
	}
}
