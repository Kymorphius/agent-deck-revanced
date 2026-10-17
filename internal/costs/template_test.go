package costs_test

import (
	"reflect"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/costs"
)

// matrix/hardened: query discovery must follow the renderer's placeholder
// boundaries, not find known strings inside unknown or unclosed placeholders.
func TestMatrixCostLineVariables(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		want           []string
	}{
		{"empty", "", nil},
		{"literal", "cost_today today", nil},
		{"unknown", "{cost_galaxy}", nil},
		{"unclosed", "{cost_today", nil},
		{"nested", "{{cost_today}}", nil},
		{"nested-known-tail", "{unknown{cost_today}}{cost_yesterday}", []string{"cost_yesterday"}},
		{"unclosed-tail", "{cost_today} {cost_this_week", []string{"cost_today"}},
		{"default", "{cost_today} today", []string{"cost_today"}},
		{"deduplicated-order", "{cost_last_week}{cost_today}{cost_last_week}{cost_today}", []string{"cost_last_week", "cost_today"}},
		{"unknown-next-to-known", "{cost_galaxy}{cost_this_month}", []string{"cost_this_month"}},
		{"literal-closing-brace", "}{cost_today}}", []string{"cost_today"}},
		{"all-windows", "{cost_today}{cost_yesterday}{cost_this_week}{cost_last_week}{cost_this_month}{cost_last_month}{cost_projected}", []string{"cost_today", "cost_yesterday", "cost_this_week", "cost_last_week", "cost_this_month", "cost_last_month", "cost_projected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := costs.CostLineVariables(tc.template)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("variables = %v, want %v", got, tc.want)
			}
			// Supplying only discovered values must produce exactly the same
			// output as all known values, including hide-when-zero behavior.
			all := map[string]int64{"cost_today": 1_000_000, "cost_yesterday": 2_000_000, "cost_this_week": 3_000_000, "cost_last_week": 4_000_000, "cost_this_month": 5_000_000, "cost_last_month": 6_000_000, "cost_projected": 7_000_000}
			needed := make(map[string]int64, len(got))
			for _, name := range got {
				needed[name] = all[name]
			}
			for _, hide := range []bool{false, true} {
				if full, selected := costs.RenderCostLine(tc.template, all, hide), costs.RenderCostLine(tc.template, needed, hide); full != selected {
					t.Fatalf("query discovery changed render: %q != %q", full, selected)
				}
			}
		})
	}
}

func TestRenderCostLine(t *testing.T) {
	vars := map[string]int64{
		"cost_today":      50_000,    // $0.05
		"cost_yesterday":  1_000_000, // $1.00
		"cost_this_week":  0,         // $0.00
		"cost_last_week":  2_500_000, // $2.50
		"cost_this_month": 0,         // $0.00
	}

	tests := []struct {
		name         string
		template     string
		hideWhenZero bool
		want         string
	}{
		{
			name:     "empty template",
			template: "",
			want:     "",
		},
		{
			name:     "single known var",
			template: "{cost_today}",
			want:     "$0.05",
		},
		{
			name:     "known var with literal suffix",
			template: "{cost_today} today",
			want:     "$0.05 today",
		},
		{
			name:     "multiple known vars with separator",
			template: "{cost_today} today | {cost_last_week} last wk",
			want:     "$0.05 today | $2.50 last wk",
		},
		{
			name:     "unknown var passes through literally",
			template: "{cost_galaxy}",
			want:     "{cost_galaxy}",
		},
		{
			name:     "known plus unknown",
			template: "{cost_today} today | {cost_galaxy}",
			want:     "$0.05 today | {cost_galaxy}",
		},
		{
			name:     "adjacent known vars",
			template: "{cost_today}{cost_yesterday}",
			want:     "$0.05$1.00",
		},
		{
			name:     "unclosed brace at end is literal",
			template: "{cost_today",
			want:     "{cost_today",
		},
		{
			name:     "literal with no placeholders is preserved",
			template: "static text",
			want:     "static text",
		},

		// hide_when_zero == true cases
		{
			name:         "hide-when-zero hides when single var is zero",
			template:     "{cost_this_week} wk",
			hideWhenZero: true,
			want:         "",
		},
		{
			name:         "hide-when-zero hides when all known vars are zero",
			template:     "{cost_this_week} wk | {cost_this_month} mo",
			hideWhenZero: true,
			want:         "",
		},
		{
			name:         "hide-when-zero shows when at least one var is non-zero",
			template:     "{cost_this_week} wk | {cost_today} today",
			hideWhenZero: true,
			want:         "$0.00 wk | $0.05 today",
		},
		{
			name:         "hide-when-zero hides template with only literal text (no recognized vars)",
			template:     "static text",
			hideWhenZero: true,
			want:         "",
		},
		{
			name:         "hide-when-zero hides template with only unknown vars",
			template:     "{cost_galaxy}",
			hideWhenZero: true,
			want:         "",
		},
		{
			name:         "hide-when-zero hides empty template",
			template:     "",
			hideWhenZero: true,
			want:         "",
		},

		// hide_when_zero == false cases
		{
			name:         "show-zero shows literal-only template",
			template:     "static text",
			hideWhenZero: false,
			want:         "static text",
		},
		{
			name:         "show-zero shows zero-valued var as $0.00",
			template:     "{cost_this_week} wk",
			hideWhenZero: false,
			want:         "$0.00 wk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := costs.RenderCostLine(tt.template, vars, tt.hideWhenZero)
			if got != tt.want {
				t.Errorf("RenderCostLine(%q) = %q, want %q", tt.template, got, tt.want)
			}
		})
	}
}

func TestRenderCostLine_NilVars(t *testing.T) {
	// Nil vars map: every placeholder is unknown.
	got := costs.RenderCostLine("{cost_today} today", nil, false)
	if got != "{cost_today} today" {
		t.Errorf("nil vars: got %q, want %q", got, "{cost_today} today")
	}
}

func TestRenderCostLine_HideWhenZero_NilVars(t *testing.T) {
	// Nil vars + hideWhenZero: zero recognized vars renders, hide.
	got := costs.RenderCostLine("{cost_today} today", nil, true)
	if got != "" {
		t.Errorf("nil vars hideWhenZero: got %q, want empty", got)
	}
}
