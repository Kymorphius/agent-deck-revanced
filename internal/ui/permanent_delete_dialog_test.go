package ui

// matrix/hardened: guard native preview state, explicit destructive consent,
// and bounded rendering without ever touching real conversation records.
import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func permanentDeleteDialogFixture() session.PermanentDeletePlan {
	return session.PermanentDeletePlan{
		SessionID: "fixture", Title: "fixture title", Harness: "codex",
		NativeIDs: []string{"01234567-89ab-cdef-0123-456789abcdef"},
		Files:     []string{"/synthetic/codex/sessions/rollout-01234567-89ab-cdef-0123-456789abcdef.jsonl"},
	}
}

func readyPermanentDeleteDialog(t *testing.T) *PermanentDeleteDialog {
	t.Helper()
	d := NewPermanentDeleteDialog()
	d.ShowLoading(1, "fixture", "fixture title")
	if !d.SetPreview(1, permanentDeleteDialogFixture(), nil) {
		t.Fatal("matching preview was rejected")
	}
	return d
}

func dialogType(d *PermanentDeleteDialog, text string) {
	d.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}

func TestPermanentDeleteDialogCancellationAndStalePreview(t *testing.T) {
	d := NewPermanentDeleteDialog()
	d.ShowLoading(1, "fixture", "fixture title")
	if !strings.Contains(ansi.Strip(d.View()), "Preparing") {
		t.Fatal("loading state has no visible progress")
	}
	d.HandleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if d.IsVisible() || d.SetPreview(1, permanentDeleteDialogFixture(), nil) {
		t.Fatal("cancelled loading preview accepted a late result")
	}
	d.ShowLoading(2, "fixture", "fixture title")
	if d.RequestID() != 2 || d.SessionID() != "fixture" || d.SetPreview(1, permanentDeleteDialogFixture(), nil) {
		t.Fatal("new opening accepted a stale preview")
	}
	wrong := permanentDeleteDialogFixture()
	wrong.SessionID = "neighbor"
	if d.SetPreview(2, wrong, nil) {
		t.Fatal("preview from a different session was accepted")
	}
	if !d.SetPreview(2, permanentDeleteDialogFixture(), nil) {
		t.Fatal("current preview was rejected")
	}
	d.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if d.IsVisible() || d.View() != "" {
		t.Fatal("Ctrl+C failed to close the ready preview")
	}
}

func TestPermanentDeleteDialogRequiresExactConfirmation(t *testing.T) {
	d := readyPermanentDeleteDialog(t)
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	if d.HandleKey(enter) {
		t.Fatal("empty confirmation was accepted")
	}
	for _, text := range []string{"delete", "DELET", "DELETE ", "q", "n"} {
		d.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
		dialogType(d, text)
		if d.HandleKey(enter) || !d.IsVisible() {
			t.Fatalf("%q confirmed or escaped the modal", text)
		}
	}
	d.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	dialogType(d, "DELETEX")
	d.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if !d.HandleKey(enter) {
		t.Fatal("exact DELETE plus Enter was rejected")
	}
	// Copying a returned plan cannot change the baseline held for execution.
	plan := d.Plan()
	plan.Files[0], plan.NativeIDs[0] = "neighbor", "neighbor"
	if d.Plan().Files[0] == "neighbor" || d.Plan().NativeIDs[0] == "neighbor" {
		t.Fatal("returned plan aliases the approved preview")
	}
}

func TestPermanentDeleteDialogBusyAndErrorGating(t *testing.T) {
	d := readyPermanentDeleteDialog(t)
	d.StartDeleting()
	if d.state != permanentDeleteReady {
		t.Fatal("deletion started without exact confirmation")
	}
	dialogType(d, "DELETE")
	if !d.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}) {
		t.Fatal("fixture could not confirm")
	}
	d.StartDeleting()
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyCtrlC}, {Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune("DELETE")}} {
		if d.HandleKey(key) || !d.IsVisible() {
			t.Fatal("busy deletion was cancelled or confirmed again")
		}
	}
	if !strings.Contains(ansi.Strip(d.View()), "please wait") {
		t.Fatal("busy deletion has no wait instruction")
	}
	if d.SetPreview(1, permanentDeleteDialogFixture(), nil) || d.SetDeleteError(2, errors.New("stale")) {
		t.Fatal("busy state accepted an unrelated result")
	}
	if !d.SetDeleteError(1, errors.New("synthetic failure")) {
		t.Fatal("current deletion error was rejected")
	}
	if !strings.Contains(ansi.Strip(d.View()), "synthetic failure") {
		t.Fatal("deletion error was hidden")
	}
	dialogType(d, "DELETE")
	if d.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}) {
		t.Fatal("failed deletion retried without a fresh plan")
	}
	d.HandleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if d.IsVisible() {
		t.Fatal("failed deletion cannot be closed")
	}
	d.ShowLoading(3, "fixture", "fixture title")
	if !d.SetPreview(3, session.PermanentDeletePlan{}, errors.New("shared native conversation")) {
		t.Fatal("planner error was rejected")
	}
	dialogType(d, "DELETE")
	if d.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}) || !strings.Contains(ansi.Strip(d.View()), "shared native conversation") {
		t.Fatal("planner error was not retained or allowed confirmation")
	}
}

func TestPermanentDeleteDialogLongPreviewAndSmallTerminal(t *testing.T) {
	d := NewPermanentDeleteDialog()
	d.ShowLoading(1, "fixture", "fixture title")
	plan := permanentDeleteDialogFixture()
	for i := 0; i < 80; i++ {
		plan.Files = append(plan.Files, fmt.Sprintf("/synthetic/path/%s/file-%02d.jsonl", strings.Repeat("long-segment/", 8), i))
	}
	if !d.SetPreview(1, plan, nil) {
		t.Fatal("long preview was rejected")
	}
	d.SetSize(38, 10)
	first := ansi.Strip(d.View())
	d.HandleKey(tea.KeyMsg{Type: tea.KeyPgDown})
	if d.scroll == 0 || ansi.Strip(d.View()) == first {
		t.Fatal("PageDown did not scroll preview details")
	}
	d.HandleKey(tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(ansi.Strip(d.View()), "cloud records") {
		t.Fatal("end of long preview cannot be read")
	}
	d.HandleKey(tea.KeyMsg{Type: tea.KeyHome})
	if d.scroll != 0 {
		t.Fatal("Home did not return to the first details line")
	}
	for _, size := range [][2]int{{100, 40}, {80, 24}, {38, 10}, {20, 8}, {10, 6}, {5, 4}, {2, 2}, {1, 1}} {
		d.SetSize(size[0], size[1])
		view := d.View()
		if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
			t.Fatalf("preview exceeds %dx%d terminal: %dx%d", size[0], size[1], lipgloss.Width(view), lipgloss.Height(view))
		}
		if size[0] >= 20 && size[1] >= 4 {
			plain := ansi.Strip(view)
			if !strings.Contains(plain, "DELETE:") || !strings.Contains(plain, "Esc cancel") {
				t.Fatalf("input/footer disappeared at %dx%d", size[0], size[1])
			}
		}
	}
}

func TestPermanentDeleteDialogDisplaysControlsLiterally(t *testing.T) {
	d := NewPermanentDeleteDialog()
	d.ShowLoading(1, "fixture", "title\x1b[2J\nnext")
	plan := permanentDeleteDialogFixture()
	plan.Files = []string{"/synthetic/file\x1b[2J\nname"}
	d.SetPreview(1, plan, nil)
	plain := ansi.Strip(d.View())
	if !strings.Contains(plain, "\\u001b[2J\\n") || strings.Contains(plain, "\x1b[2J") {
		t.Fatal("preview does not safely show embedded terminal controls")
	}
}
