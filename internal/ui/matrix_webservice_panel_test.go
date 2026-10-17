package ui

// matrix/hardened: alt+w panel flow with a fake `agent-deck webservice`.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

type fakeWebServiceCLI struct {
	calls  [][]string
	status session.WebServiceStatus
	fail   string
}

func (f *fakeWebServiceCLI) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.fail != "" && len(args) > 1 && args[1] != "status" {
		out, _ := json.Marshal(map[string]any{"success": false, "error": f.fail, "status": f.status})
		return out, errors.New("exit status 1")
	}
	switch args[1] {
	case session.WebServiceStop:
		f.status.Running, f.status.Healthy, f.status.PID = false, false, 0
	case session.WebServiceStart, session.WebServiceRestart:
		f.status.Running, f.status.Healthy, f.status.PID = true, true, f.status.PID+1
	}
	f.status.CheckedAt = time.Now()
	out, _ := json.Marshal(map[string]any{"success": true, "status": f.status})
	return out, nil
}

func wsKey(s string) tea.KeyMsg {
	if s == "esc" {
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// drive runs cmd and feeds its message back into the panel.
func drivePanel(t *testing.T, p *WebServicePanel, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	switch m := cmd().(type) {
	case webServiceLoadedMsg:
		p.handleLoaded(m)
	case webServiceDoneMsg:
		p.handleDone(m)
	default:
		t.Fatalf("unexpected message %T", m)
	}
}

func TestWebServicePanel_StopNeedsConfirmation(t *testing.T) {
	cli := &fakeWebServiceCLI{status: session.WebServiceStatus{
		Configured: true, Manager: "launchd", Service: "my.web", URL: "http://127.0.0.1:8420",
		Loaded: true, Running: true, PID: 7, Probed: true, Healthy: true, State: "running",
	}}
	p := NewWebServicePanel()
	p.SetSize(100, 30)
	drivePanel(t, p, p.Open("", cli.run))
	if !p.loaded || !p.status.Running {
		t.Fatalf("status not loaded: %+v", p.status)
	}
	view := p.View()
	for _, want := range []string{"WEB SERVICE", "this machine", "my.web (launchd)", "running (pid 7)", "[x] Stop"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}

	_, cmd := p.Update(wsKey("x"))
	if cmd != nil || p.confirming != session.WebServiceStop {
		t.Fatalf("x should ask first (cmd=%v confirming=%q)", cmd != nil, p.confirming)
	}
	if !strings.Contains(p.View(), "Stop my.web?") {
		t.Fatalf("no confirmation prompt:\n%s", p.View())
	}
	_, cmd = p.Update(wsKey("n"))
	if cmd != nil || p.confirming != "" || len(cli.calls) != 1 {
		t.Fatalf("n should cancel without running anything: %q", cli.calls)
	}

	_, _ = p.Update(wsKey("x"))
	_, cmd = p.Update(wsKey("y"))
	if p.busy != session.WebServiceStop {
		t.Fatalf("busy = %q", p.busy)
	}
	drivePanel(t, p, cmd)
	if got := cli.calls[len(cli.calls)-1]; strings.Join(got, " ") != "webservice stop --json" {
		t.Fatalf("ran %q", got)
	}
	if p.busy != "" || p.status.Running || p.notice != "Stopped." {
		t.Fatalf("after stop: busy=%q status=%+v notice=%q", p.busy, p.status, p.notice)
	}

	// Start runs without a confirmation.
	_, cmd = p.Update(wsKey("s"))
	drivePanel(t, p, cmd)
	if !p.status.Running || p.notice != "Started." {
		t.Fatalf("after start: %+v %q", p.status, p.notice)
	}
}

func TestWebServicePanel_ShowsFailureAndRemoteTarget(t *testing.T) {
	cli := &fakeWebServiceCLI{
		status: session.WebServiceStatus{Configured: true, Manager: "systemd", Service: "agent-deck-web", Loaded: true, Running: true, PID: 3},
		fail:   "sudo systemctl restart: sudo: a password is required",
	}
	p := NewWebServicePanel()
	p.SetSize(100, 30)
	drivePanel(t, p, p.Open("lab", cli.run))
	if !strings.Contains(p.View(), "remote lab") {
		t.Fatalf("target not shown:\n%s", p.View())
	}
	_, _ = p.Update(wsKey("R"))
	_, cmd := p.Update(wsKey("y"))
	drivePanel(t, p, cmd)
	if !strings.Contains(p.err, "password is required") || !p.status.Running {
		t.Fatalf("err=%q status=%+v", p.err, p.status)
	}
}

func TestWebServicePanel_NotConfigured(t *testing.T) {
	cli := &fakeWebServiceCLI{}
	p := NewWebServicePanel()
	p.SetSize(100, 30)
	drivePanel(t, p, p.Open("", cli.run))
	if !strings.Contains(p.View(), "No [webservice] configured") {
		t.Fatalf("view:\n%s", p.View())
	}
	for _, k := range []string{"s", "x", "R"} {
		if _, cmd := p.Update(wsKey(k)); cmd != nil || p.confirming != "" {
			t.Fatalf("%s acted on an unconfigured service", k)
		}
	}
	_, _ = p.Update(wsKey("esc"))
	if p.IsVisible() {
		t.Fatal("esc did not close")
	}
}

func TestWebServiceHeaderSegment(t *testing.T) {
	h := &Home{}
	if h.webServiceHeaderSegment() != "" {
		t.Fatal("segment shown without [webservice]")
	}
	h.webServiceWatch = newWebServiceWatch(session.WebServiceSettings{})
	if got := h.webServiceHeaderSegment(); !strings.Contains(got, "web …") {
		t.Fatalf("before first reading: %q", got)
	}
	cases := []struct {
		st   session.WebServiceStatus
		want string
	}{
		{session.WebServiceStatus{Configured: true, Running: true, Probed: true, Healthy: true}, "●"},
		{session.WebServiceStatus{Configured: true, Running: true}, "●"},
		{session.WebServiceStatus{Configured: true, Running: true, Probed: true}, "◐ no reply"},
		{session.WebServiceStatus{Configured: true}, "○ stopped"},
		{session.WebServiceStatus{Configured: true, ManagerError: "boom"}, "? unknown"},
	}
	for _, c := range cases {
		h.webServiceWatch.set(c.st)
		if got := h.webServiceHeaderSegment(); !strings.Contains(got, c.want) {
			t.Errorf("%+v: segment %q lacks %q", c.st, got, c.want)
		}
	}
}
