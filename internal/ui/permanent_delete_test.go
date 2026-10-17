package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func permanentDeleteHomeFixture(t *testing.T) (*Home, *session.Instance, string) {
	t.Helper()
	root := setXDGTestHome(t)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	h := NewHome()
	t.Cleanup(func() { h.storage.Close() })
	h.width, h.height, h.initialLoading = 120, 40, false
	h.updateSizes()
	inst := session.NewInstanceWithTool("permanent preview fixture", root, "codex")
	inst.CodexSessionID = "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(root, ".codex", "sessions", "2026", "10", "12", "rollout-2026-10-12T00-00-00-"+inst.CodexSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("synthetic private transcript\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.storage.Save([]*session.Instance{inst}); err != nil {
		t.Fatal(err)
	}
	return h, inst, path
}

// matrix/hardened: the ordinary delete dialog exposes a separate permanent
// preview; it does not execute permanent deletion on y/Enter automatically.
func TestPermanentDeleteDialogEntry(t *testing.T) {
	h, inst, path := permanentDeleteHomeFixture(t)
	h.confirmDialog.ShowDeleteSession(inst.ID, inst.Title, false, false)
	if !strings.Contains(strings.ReplaceAll(h.confirmDialog.View(), "\u00a0", " "), "p permanent delete") {
		t.Fatal("permanent preview entry missing")
	}
	if h.confirmDialog.GetFocusedButton() != 1 {
		t.Fatal("ordinary delete no longer defaults to Cancel")
	}
	_, cmd := h.handleConfirmDialogKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if cmd == nil || h.confirmDialog.IsVisible() || !h.permanentDeleteDialog.IsVisible() || !h.hasModalVisible() {
		t.Fatal("p did not open the native loading preview")
	}
	if !strings.Contains(ansi.Strip(h.View()), "Preparing") {
		t.Fatal("preview opened with a blank frame")
	}
	msg, ok := cmd().(permanentDeletePreviewMsg)
	if !ok || msg.err != nil {
		t.Fatalf("p must plan directly without a terminal subprocess: %#v", msg)
	}
	h.updateInner(msg)
	if view := ansi.Strip(h.View()); !strings.Contains(view, inst.CodexSessionID) || !strings.Contains(view, "Exact local files/directories") {
		t.Fatal("exact native deletion targets missing from the in-app preview")
	}
	if files := h.permanentDeleteDialog.Plan().Files; len(files) != 1 || files[0] != path {
		t.Fatalf("exact native transcript missing from plan: %v", files)
	}
	cursor := h.cursor
	h.updateInner(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if h.cursor != cursor {
		t.Fatal("preview wheel moved the main list")
	}
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.permanentDeleteDialog.IsVisible() || h.View() == "" || h.isQuitting {
		t.Fatal("Esc failed to return to a rendered list")
	}
	h.updateInner(msg)
	if h.permanentDeleteDialog.IsVisible() {
		t.Fatal("late preview reopened a cancelled modal")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "synthetic private transcript\n" {
		t.Fatalf("preview/cancel changed native records: %q %v", data, err)
	}
}

// matrix/hardened: exercise the real native-dialog command boundary with
// synthetic stopped records; no production deletion is needed to validate it.
func TestPermanentDeleteHomeExplicitConfirmation(t *testing.T) {
	h, inst, path := permanentDeleteHomeFixture(t)
	msg := h.beginPermanentDelete(inst.ID, inst.Title)()
	h.updateInner(msg)
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || h.permanentDeleteDialog.state != permanentDeleteReady {
		t.Fatal("empty Enter submitted permanent deletion")
	}
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("DELETE")})
	_, cmd = h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || h.permanentDeleteDialog.state != permanentDeleteBusy {
		t.Fatal("typed DELETE did not start asynchronous deletion")
	}
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if !h.permanentDeleteDialog.IsVisible() || h.isQuitting {
		t.Fatal("busy modal leaked cancellation/quit keys")
	}
	result, ok := cmd().(permanentDeleteResultMsg)
	if !ok || result.err != nil {
		t.Fatalf("synthetic native deletion failed: %#v", result)
	}
	_, reload := h.updateInner(result)
	if h.permanentDeleteDialog.IsVisible() || reload == nil {
		t.Fatal("successful deletion did not return to list and refresh")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("synthetic transcript remained: %v", err)
	}
	if instances, err := h.storage.Load(); err != nil || len(instances) != 0 {
		t.Fatalf("synthetic registry deletion failed: %d %v", len(instances), err)
	}
}
