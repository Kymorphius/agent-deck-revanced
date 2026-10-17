package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: the alt+u panel reads and adopts only through
// `agent-deck session adopt`, asks before adopting, and never offers a
// refused candidate.

const adoptPanelList = `{"success":true,"candidates":[
 {"socket":"default","tmux_session":"gd","claude_pid":42,"claude_session_id":"6a1f0c2e-9b7d-4e3a-8c5f-2d4b6e8a0c1f","path":"/srv/work","claude_status":"busy","adoptable":true},
 {"socket":"default","tmux_session":"dg","claude_pid":43,"claude_session_id":"7b2e1d3f-0a8c-4f4b-9d6e-3e5c7f9b1d2a","path":"/srv/work","adoptable":false,"reason":"the conversation is also open in Claude process 7"}]}`

func adoptKey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestMatrixAdoptPanel_ConfirmsThenAdopts(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if len(args) > 2 && args[2] == "--list" {
			return []byte(adoptPanelList), nil
		}
		return []byte(`{"success":true,"title":"gd","group":"work","tmux_session":"agentdeck_gd_1"}`), nil
	}
	p := NewAdoptPanel()
	p.SetSize(120, 30)
	p.handleLoaded(p.Open("lab", run)().(adoptLoadedMsg))
	if len(p.cands) != 2 || !strings.Contains(p.View(), "remote lab") || !strings.Contains(p.View(), "gd") {
		t.Fatalf("list not shown: %d\n%s", len(p.cands), p.View())
	}

	// A refused candidate only explains itself.
	p.Update(adoptKey("down"))
	if _, cmd := p.Update(adoptKey("enter")); cmd != nil || p.confirming || !strings.Contains(p.notice, "also open") {
		t.Fatalf("refused candidate offered: confirming=%v notice=%q", p.confirming, p.notice)
	}

	// The adoptable one asks first; n cancels without running anything.
	p.Update(adoptKey("k"))
	p.Update(adoptKey("enter"))
	if !p.confirming || !strings.Contains(p.View(), "without restarting") {
		t.Fatalf("no confirmation:\n%s", p.View())
	}
	p.Update(adoptKey("n"))
	if p.confirming || len(calls) != 1 {
		t.Fatalf("cancel ran something: %v", calls)
	}

	p.Update(adoptKey("enter"))
	_, cmd := p.Update(adoptKey("y"))
	if cmd == nil {
		t.Fatal("y did not adopt")
	}
	done := cmd().(adoptDoneMsg)
	want := []string{"session", "adopt", "gd", "--socket", "default", "--json"}
	if got := calls[len(calls)-1]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("adopt ran %v, want %v", got, want)
	}
	if done.err != nil || done.target != "lab" || !strings.Contains(done.notice, "agentdeck_gd_1") {
		t.Fatalf("done: %+v", done)
	}
	if reload := p.handleDone(done); reload == nil || !strings.Contains(p.View(), "Adopted") {
		t.Fatalf("no reload/notice after adopt:\n%s", p.View())
	}
}

func TestMatrixAdoptPanel_ShowsCommandError(t *testing.T) {
	run := func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"success":false,"error":"rename tmux session \"gd\": tmux session name already in use"}`), errors.New("exit status 1")
	}
	p := NewAdoptPanel()
	p.SetSize(120, 30)
	p.handleLoaded(p.Open("", run)().(adoptLoadedMsg))
	if !strings.Contains(p.View(), "already in use") || !strings.Contains(p.View(), "this machine") {
		t.Fatalf("command error not shown:\n%s", p.View())
	}
}

func TestMatrixAdoptPanel_Hotkey(t *testing.T) {
	if defaultHotkeyBindings[hotkeyAdoptPanel] != "alt+u" {
		t.Fatalf("adopt_panel = %q", defaultHotkeyBindings[hotkeyAdoptPanel])
	}
}
