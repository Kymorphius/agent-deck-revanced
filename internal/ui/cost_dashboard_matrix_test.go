package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// matrix/hardened: repaint order is stable and every model remains reachable.
func TestMatrixCostDashboardStableOrderAndScroll(t *testing.T) {
	d := costDashboard{width: 80, height: 24, byModel: map[string]int64{}}
	for i := 0; i < 40; i++ {
		d.byModel[fmt.Sprintf("model-%02d", i)] = int64(i + 1)
	}
	content := d.content()
	for i := 0; i < 20; i++ {
		if got := d.content(); got != content {
			t.Fatal("model rows changed order on repaint")
		}
	}
	if strings.Index(content, "model-00") > strings.Index(content, "model-39") || strings.Index(content, "model-39") > strings.Index(content, "Top Sessions") {
		t.Fatal("models must be sorted and precede top sessions")
	}
	if lipgloss.Height(d.View()) != 24 {
		t.Fatalf("viewport height = %d", lipgloss.Height(d.View()))
	}
	d.scroll("end")
	if !strings.Contains(d.View(), "model-39") || !strings.Contains(d.View(), "q/Esc/Alt+$ return") {
		t.Fatal("last model / footer unreachable")
	}
	d.scroll("home")
	d.scroll("pgdown")
	if d.scrollOffset != 23 {
		t.Fatalf("page step: %d", d.scrollOffset)
	}
	d.scroll("pgup")
	if d.scrollOffset != 0 {
		t.Fatal("page up did not return to start")
	}
	// Resize a scrolled viewport; narrow wrapping must stay within the terminal.
	d.width, d.height = 35, 10
	d.scroll("end")
	if lipgloss.Height(d.View()) != 10 || lipgloss.Width(d.View()) > 35 {
		t.Fatalf("narrow viewport %dx%d", lipgloss.Width(d.View()), lipgloss.Height(d.View()))
	}
}
