package ui

import (
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: exercise the real Home input router without sending input
// to an AI process. Local, window and remote rows must open the same target.
func TestMatrixOpenSession_MainKeys(t *testing.T) {
	for _, row := range []string{"local", "window", "remote"} {
		for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'o'}}, {Type: tea.KeyEnter}} {
			t.Run(row+"/"+key.String(), func(t *testing.T) {
				h, source := matrixOpenSessionHome(t, row)
				_, cmd := h.Update(key)
				if cmd == nil || !h.embeddedMode || !h.insertMode {
					t.Fatalf("%q did not open %s row: cmd nil=%v, error=%v", key.String(), row, cmd == nil, h.err)
				}
				if h.promptInputDialog.IsVisible() {
					t.Fatal("opening a session displayed the prompt dialog")
				}
				if row == "remote" {
					if h.insertModeRemoteName != "lab" || h.insertModeRemoteID != "remote-1" {
						t.Fatalf("wrong remote target: %q/%q", h.insertModeRemoteName, h.insertModeRemoteID)
					}
				} else if h.insertModeSessionID != source.ID {
					t.Fatalf("opened %q, want %q", h.insertModeSessionID, source.ID)
				}
			})
		}
	}
}

// matrix/hardened: remapping or disabling o must affect both dispatch and
// hints, while the Enter compatibility path remains available.
func TestMatrixOpenSession_RebindAndDisable(t *testing.T) {
	for _, binding := range []string{"ctrl+o", ""} {
		t.Run(binding, func(t *testing.T) {
			h, _ := matrixOpenSessionHome(t, "local")
			h.setHotkeys(resolveHotkeys(map[string]string{hotkeyOpenSession: binding}))
			if _, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}}); cmd != nil || h.embeddedMode || h.promptInputDialog.IsVisible() {
				t.Fatal("unbound o still opened a session or prompt dialog")
			}
			label, key := "Enter", tea.KeyMsg{Type: tea.KeyEnter}
			if binding != "" {
				label, key = binding, tea.KeyMsg{Type: tea.KeyCtrlO}
			}
			footer := strings.Join(strings.Fields(tmux.StripANSI(h.renderHelpBarFull())), " ")
			displayLabel := label
			if label == "Enter" {
				displayLabel = "⏎"
			}
			if h.sessionOpenKeyLabel() != label || !strings.Contains(footer, displayLabel+" Attach") {
				t.Fatalf("opening hint does not advertise %q: %s", label, footer)
			}
			_, cmd := h.Update(key)
			if cmd == nil || !h.embeddedMode {
				t.Fatalf("%q did not open the selected session", key.String())
			}
		})
	}
}

// matrix/hardened: the compact and minimal fallbacks also advertise o.
func TestMatrixOpenSession_FooterWidths(t *testing.T) {
	for _, width := range []int{60, 80, 120, 200} {
		for _, mode := range []string{session.FooterFull, session.FooterCompact, session.FooterMinimal, session.FooterCurated} {
			h, _ := matrixOpenSessionHome(t, "local")
			h.width, h.footerMode = width, mode
			footer := strings.Join(strings.Fields(tmux.StripANSI(h.renderHelpBar())), " ")
			if !strings.Contains(footer, " o ") || strings.Contains(footer, "⏎") {
				t.Fatalf("%s/%d footer lost the primary opening key: %s", mode, width, footer)
			}
		}
	}
}

// matrix/hardened: preserve automatic recovery on the classic opening path.
func TestMatrixOpenSession_StoppedSessionRestarts(t *testing.T) {
	h, source := matrixOpenSessionHome(t, "local")
	h.embeddedLayout = false
	h.sessionExists = func(*session.Instance) bool { return false }
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if cmd == nil || h.resumingSessions[source.ID].IsZero() || h.promptInputDialog.IsVisible() {
		t.Fatal("o did not use the existing stopped-session recovery path")
	}
}

func matrixOpenSessionHome(t *testing.T, row string) (*Home, *session.Instance) {
	t.Helper()
	withTempAgentDeckHome(t, "")
	source := &session.Instance{ID: "open-source", Title: "Open session", ProjectPath: t.TempDir(), Tool: "claude", Status: session.StatusRunning}
	source.SetTmuxSessionForTest(tmux.NewSession("open-session-test", source.ProjectPath))
	h := NewGateHome(GateHomeOptions{Width: 120, Height: 40, Instances: []*session.Instance{source}}).home
	h.embeddedLayout = true
	h.sessionExists = func(*session.Instance) bool { return true }
	h.insertOpenKeySender = func(insertTargetRef) (insertKeySender, error) { return &fakeInsertKeySender{}, nil }
	item := session.Item{Type: session.ItemTypeSession, Session: source}
	switch row {
	case "window":
		item = session.Item{Type: session.ItemTypeWindow, WindowSessionID: source.ID, WindowIndex: 2}
	case "remote":
		item = session.Item{Type: session.ItemTypeRemoteSession, RemoteName: "lab", RemoteSession: &session.RemoteSessionInfo{ID: "remote-1", Title: "Remote", Status: "running", Tool: "claude"}}
	}
	h.flatItems = []session.Item{item}
	h.cursor = 0
	return h, source
}
