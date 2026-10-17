package ui

// matrix/hardened: alt+w shows the user's own `agent-deck web` service on
// this machine, or on the remote under the cursor, and starts, stops or
// restarts it. Every read and write is `agent-deck webservice` (see
// cmd/agent-deck/matrix_webservice_cmd.go), run locally or over the remote's
// SSH runner, so the TUI and the CLI cannot disagree.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

const webServiceCommandTimeout = 45 * time.Second

type webServiceLoadedMsg struct {
	seq    int
	status session.WebServiceStatus
	err    error
}

type webServiceDoneMsg struct {
	target string
	action string
	status session.WebServiceStatus
	err    error
}

// WebServicePanel is the alt+w overlay.
type WebServicePanel struct {
	visible bool
	width   int
	height  int

	target string // "" = this machine, otherwise a remote name
	run    adoptRunFunc

	status     session.WebServiceStatus
	loaded     bool
	seq        int
	loading    bool
	busy       string // action in flight
	confirming string // action awaiting y/n
	notice     string
	err        string
}

func NewWebServicePanel() *WebServicePanel { return &WebServicePanel{} }

func (p *WebServicePanel) IsVisible() bool { return p != nil && p.visible }

func (p *WebServicePanel) SetSize(w, h int) {
	if p == nil {
		return
	}
	p.width, p.height = w, h
}

func (p *WebServicePanel) Hide() {
	if p == nil {
		return
	}
	p.visible = false
	p.confirming = ""
}

// Open shows the panel for target and starts reading the service's state.
func (p *WebServicePanel) Open(target string, run adoptRunFunc) tea.Cmd {
	p.visible = true
	p.target, p.run = target, run
	p.status, p.loaded = session.WebServiceStatus{}, false
	p.busy, p.confirming = "", ""
	p.notice, p.err = "", ""
	return p.load()
}

func (p *WebServicePanel) load() tea.Cmd {
	p.seq++
	p.loading = true
	seq, run := p.seq, p.run
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), adoptCommandTimeout)
		defer cancel()
		out, err := run(ctx, "webservice", "status", "--json")
		if err != nil {
			return webServiceLoadedMsg{seq: seq, err: adoptCommandError(out, err)}
		}
		var resp struct {
			Status session.WebServiceStatus `json:"status"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return webServiceLoadedMsg{seq: seq, err: fmt.Errorf("unexpected reply (is agent-deck on the target older than webservice?): %w", err)}
		}
		return webServiceLoadedMsg{seq: seq, status: resp.Status}
	}
}

func (p *WebServicePanel) control(action string) tea.Cmd {
	p.busy = action
	p.notice, p.err = "", ""
	target, run := p.target, p.run
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), webServiceCommandTimeout)
		defer cancel()
		out, err := run(ctx, "webservice", action, "--json")
		var resp struct {
			Status session.WebServiceStatus `json:"status"`
		}
		_ = json.Unmarshal(out, &resp)
		if err != nil {
			return webServiceDoneMsg{target: target, action: action, status: resp.Status, err: adoptCommandError(out, err)}
		}
		return webServiceDoneMsg{target: target, action: action, status: resp.Status}
	}
}

func (p *WebServicePanel) handleLoaded(m webServiceLoadedMsg) {
	if m.seq != p.seq {
		return
	}
	p.loading = false
	if m.err != nil {
		p.err = m.err.Error()
		return
	}
	p.err = ""
	p.status, p.loaded = m.status, true
}

func (p *WebServicePanel) handleDone(m webServiceDoneMsg) {
	if m.target != p.target {
		return
	}
	p.busy = ""
	if m.status.CheckedAt.IsZero() {
		// No reading came back with the result: read it now.
		if m.err != nil {
			p.err = m.err.Error()
		}
		return
	}
	p.status, p.loaded = m.status, true
	if m.err != nil {
		p.err, p.notice = m.err.Error(), ""
		return
	}
	p.err = ""
	p.notice = webServiceActionNotice(m.action, m.status)
}

// webServiceActionNotice says whether the action took, from the reading
// taken after it.
func webServiceActionNotice(action string, st session.WebServiceStatus) string {
	switch action {
	case session.WebServiceStop:
		if st.Running {
			return "Asked to stop, but it is still running."
		}
		return "Stopped."
	default:
		switch {
		case !st.Running:
			return "Asked to " + action + ", but it is not running (check its log)."
		case st.Probed && !st.Healthy:
			return "Running, but not answering yet: refresh in a moment."
		case action == session.WebServiceRestart:
			return "Restarted."
		}
		return "Started."
	}
}

func (p *WebServicePanel) Update(msg tea.Msg) (*WebServicePanel, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	if p.busy != "" {
		if s := key.String(); s == "esc" || s == "q" {
			p.Hide() // the command keeps running; its result lands in the header
		}
		return p, nil
	}
	if p.confirming != "" {
		switch key.String() {
		case "y", "Y", "enter":
			action := p.confirming
			p.confirming = ""
			return p, p.control(action)
		case "n", "N", "esc", "q":
			p.confirming = ""
		}
		return p, nil
	}
	switch key.String() {
	case "esc", "q":
		p.Hide()
	case "r":
		p.notice = ""
		return p, p.load()
	case "s":
		if p.loaded && p.status.Configured {
			p.notice = ""
			return p, p.control(session.WebServiceStart)
		}
	case "x":
		if p.loaded && p.status.Configured {
			p.notice, p.confirming = "", session.WebServiceStop
		}
	case "R", "shift+r":
		if p.loaded && p.status.Configured {
			p.notice, p.confirming = "", session.WebServiceRestart
		}
	}
	return p, nil
}

// webServiceMark is the state dot shared by the panel and the header.
func webServiceMark(st session.WebServiceStatus) (string, lipgloss.Style) {
	switch {
	case st.ManagerError != "":
		return "?", lipgloss.NewStyle().Foreground(ColorYellow)
	case !st.Running:
		return "○", lipgloss.NewStyle().Foreground(ColorRed)
	case st.Probed && !st.Healthy:
		return "◐", lipgloss.NewStyle().Foreground(ColorYellow)
	}
	return "●", lipgloss.NewStyle().Foreground(ColorGreen)
}

func (p *WebServicePanel) View() string {
	if p == nil || !p.visible {
		return ""
	}
	width := min(max(40, p.width-2), 96)
	inner := width - 2
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ColorBorder).Padding(0, 1).Width(width)
	title := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	text := lipgloss.NewStyle().Foreground(ColorText)
	red := lipgloss.NewStyle().Foreground(ColorRed)
	green := lipgloss.NewStyle().Foreground(ColorGreen)
	sel := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	rule := dim.Render(strings.Repeat("─", inner))

	where := "this machine"
	if p.target != "" {
		where = "remote " + p.target
	}
	head := where
	switch {
	case p.busy != "":
		head += " · " + p.busy + "ing…"
	case p.loading:
		head += " · reading…"
	}
	var sb strings.Builder
	sb.WriteString(title.Render("WEB SERVICE") + "  " + dim.Render(kanbanFit(head, inner-13)) + "\n" + rule + "\n")

	st := p.status
	var body []string
	switch {
	case !p.loaded:
		body = append(body, dim.Render("Reading the service's state…"))
	case !st.Configured:
		body = append(body,
			text.Render(kanbanFit("No [webservice] configured on "+where+".", inner)),
			dim.Render(kanbanFit("Set manager, service and url in its config.toml; see `agent-deck webservice --help`.", inner)))
	default:
		mark, style := webServiceMark(st)
		body = append(body,
			text.Render(kanbanFit(fmt.Sprintf("%s (%s)", st.Service, st.Manager), inner)),
			style.Render(mark+" ")+text.Render(kanbanFit(st.Summary(), inner-2)))
		if st.State != "" {
			body = append(body, dim.Render(kanbanFit("state   "+st.State, inner)))
		}
		if st.URL != "" {
			body = append(body, dim.Render(kanbanFit("url     "+st.URL, inner)))
		}
		if st.HealthError != "" {
			body = append(body, red.Render(kanbanFit("health  "+st.HealthError, inner)))
		}
		if st.ManagerError != "" {
			body = append(body, red.Render(kanbanFit("error   "+st.ManagerError, inner)))
		}
		body = append(body, dim.Render(kanbanFit("checked "+st.CheckedAt.Local().Format("15:04:05"), inner)))
	}
	for _, line := range body {
		sb.WriteString(line + "\n")
	}
	sb.WriteString(rule + "\n")
	switch {
	case p.err != "":
		sb.WriteString(red.Render(kanbanFit(p.err, inner)) + "\n")
	case p.notice != "":
		sb.WriteString(green.Render(kanbanFit(p.notice, inner)) + "\n")
	default:
		sb.WriteString("\n")
	}
	switch {
	case p.confirming == session.WebServiceStop:
		sb.WriteString(sel.Render(kanbanFit(fmt.Sprintf("Stop %s? The web UI is down until it is started again. [y] yes  [n] no", st.Service), inner)))
	case p.confirming == session.WebServiceRestart:
		sb.WriteString(sel.Render(kanbanFit(fmt.Sprintf("Restart %s? Open web pages reconnect. [y] yes  [n] no", st.Service), inner)))
	case p.busy != "":
		sb.WriteString(dim.Render(kanbanFit("Waiting for the service manager…  [Esc] Close", inner)))
	case p.loaded && st.Configured:
		sb.WriteString(dim.Render(kanbanFit("[s] Start  [x] Stop  [R] Restart  [r] Refresh  [Esc] Close", inner)))
	default:
		sb.WriteString(dim.Render(kanbanFit("[r] Refresh  [Esc] Close", inner)))
	}
	return lipgloss.Place(p.width, p.height, lipgloss.Center, lipgloss.Center, box.Render(sb.String()))
}
