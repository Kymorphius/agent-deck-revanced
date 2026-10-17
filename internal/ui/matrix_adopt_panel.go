package ui

// matrix/hardened: alt+u lists the Claude processes running in tmux sessions
// agent-deck did not create, on this machine or on the remote under the
// cursor, and adopts the selected one after confirmation. Every read and
// write is `agent-deck session adopt` (see cmd/agent-deck/matrix_adopt_cmd.go),
// run locally or over the remote's SSH runner, so the TUI and the CLI cannot
// disagree about what is adoptable.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// adoptRunFunc runs one agent-deck command on the panel's target and returns
// its stdout, also on failure (the JSON error payload is printed there).
type adoptRunFunc func(ctx context.Context, args ...string) ([]byte, error)

const adoptCommandTimeout = 30 * time.Second

type adoptLoadedMsg struct {
	seq   int
	cands []session.AdoptCandidate
	err   error
}

type adoptDoneMsg struct {
	target string
	notice string
	err    error
}

// AdoptPanel is the alt+u overlay.
type AdoptPanel struct {
	visible bool
	width   int
	height  int

	target string // "" = this machine, otherwise a remote name
	run    adoptRunFunc

	cands      []session.AdoptCandidate
	cursor     int
	seq        int
	loading    bool
	busy       bool
	confirming bool
	notice     string
	err        string
}

func NewAdoptPanel() *AdoptPanel { return &AdoptPanel{} }

func (p *AdoptPanel) IsVisible() bool { return p != nil && p.visible }

func (p *AdoptPanel) SetSize(w, h int) {
	if p == nil {
		return
	}
	p.width, p.height = w, h
}

func (p *AdoptPanel) Hide() {
	if p == nil {
		return
	}
	p.visible = false
	p.confirming = false
}

// Open shows the panel for target and starts reading its candidates.
func (p *AdoptPanel) Open(target string, run adoptRunFunc) tea.Cmd {
	p.visible = true
	p.target, p.run = target, run
	p.cands, p.cursor = nil, 0
	p.confirming, p.busy = false, false
	p.notice, p.err = "", ""
	return p.load()
}

func (p *AdoptPanel) load() tea.Cmd {
	p.seq++
	p.loading = true
	seq, run := p.seq, p.run
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), adoptCommandTimeout)
		defer cancel()
		out, err := run(ctx, "session", "adopt", "--list", "--json")
		if err != nil {
			return adoptLoadedMsg{seq: seq, err: adoptCommandError(out, err)}
		}
		var resp struct {
			Candidates []session.AdoptCandidate `json:"candidates"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return adoptLoadedMsg{seq: seq, err: fmt.Errorf("unexpected reply (is agent-deck on the target older than adopt?): %w", err)}
		}
		return adoptLoadedMsg{seq: seq, cands: resp.Candidates}
	}
}

// adoptCommandError prefers the command's own JSON error message.
func adoptCommandError(out []byte, err error) error {
	var resp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(out, &resp) == nil && resp.Error != "" {
		return errors.New(resp.Error)
	}
	return err
}

func (p *AdoptPanel) handleLoaded(m adoptLoadedMsg) {
	if m.seq != p.seq {
		return
	}
	p.loading = false
	if m.err != nil {
		p.err = m.err.Error()
		return
	}
	p.err = ""
	p.cands = m.cands
	if p.cursor >= len(p.cands) {
		p.cursor = max(0, len(p.cands)-1)
	}
}

func (p *AdoptPanel) handleDone(m adoptDoneMsg) tea.Cmd {
	p.busy = false
	if m.err != nil {
		p.err = m.err.Error()
		p.notice = ""
	} else {
		p.err = ""
		p.notice = m.notice
	}
	return p.load()
}

func (p *AdoptPanel) selected() *session.AdoptCandidate {
	if p.cursor < 0 || p.cursor >= len(p.cands) {
		return nil
	}
	return &p.cands[p.cursor]
}

func (p *AdoptPanel) adopt(c session.AdoptCandidate) tea.Cmd {
	p.busy = true
	p.notice, p.err = "", ""
	target, run := p.target, p.run
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), adoptCommandTimeout)
		defer cancel()
		out, err := run(ctx, "session", "adopt", c.TmuxSession, "--socket", c.Socket, "--json")
		if err != nil {
			return adoptDoneMsg{target: target, err: adoptCommandError(out, err)}
		}
		var resp struct {
			Title       string `json:"title"`
			Group       string `json:"group"`
			TmuxSession string `json:"tmux_session"`
		}
		_ = json.Unmarshal(out, &resp)
		return adoptDoneMsg{target: target, notice: fmt.Sprintf("Adopted %q as session %q in group %q (tmux session is now %s).", c.TmuxSession, resp.Title, resp.Group, resp.TmuxSession)}
	}
}

func (p *AdoptPanel) Update(msg tea.Msg) (*AdoptPanel, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || p.busy {
		return p, nil
	}
	if p.confirming {
		switch key.String() {
		case "y", "Y", "enter":
			p.confirming = false
			if c := p.selected(); c != nil && c.Adoptable {
				return p, p.adopt(*c)
			}
		case "n", "N", "esc", "q":
			p.confirming = false
		}
		return p, nil
	}
	switch key.String() {
	case "esc", "q":
		p.Hide()
	case "j", "down":
		if p.cursor < len(p.cands)-1 {
			p.cursor++
		}
	case "k", "up":
		if p.cursor > 0 {
			p.cursor--
		}
	case "r":
		p.notice = ""
		return p, p.load()
	case "enter", "a":
		c := p.selected()
		switch {
		case c == nil:
		case !c.Adoptable:
			p.notice = "Not adoptable: " + c.Reason
		default:
			p.notice = ""
			p.confirming = true
		}
	}
	return p, nil
}

func (p *AdoptPanel) View() string {
	if p == nil || !p.visible {
		return ""
	}
	width := max(40, p.width-2)
	inner := width - 2
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ColorBorder).Padding(0, 1).Width(width)
	title := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
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
	case p.loading:
		head += " · reading…"
	case p.busy:
		head += " · adopting…"
	}
	var sb strings.Builder
	sb.WriteString(title.Render("ADOPT CLAUDE") + "  " + dim.Render(kanbanFit(head, inner-14)) + "\n" + rule + "\n")

	// Rows: border 2, header 2, detail 4, rule 1, footer 1.
	bodyH := max(3, p.height-10)
	lines := 0
	if len(p.cands) == 0 {
		msg := "No Claude processes found in tmux."
		if p.loading {
			msg = "Looking for Claude processes in tmux…"
		}
		sb.WriteString(dim.Render(msg) + "\n")
		lines++
	}
	start := 0
	if p.cursor >= bodyH {
		start = p.cursor - bodyH + 1
	}
	for i := start; i < len(p.cands) && lines < bodyH; i++ {
		c := p.cands[i]
		mark, style := "✕", dim
		if c.Adoptable {
			mark, style = "✓", green
		}
		row := fmt.Sprintf("%s %s  [%s] pid %d %s  %s", mark, c.TmuxSession, c.Socket, c.ClaudePID, c.ClaudeStatus, c.Path)
		if i == p.cursor {
			sb.WriteString(sel.Render(kanbanFit("▸ "+row, inner)) + "\n")
		} else {
			sb.WriteString(style.Render(kanbanFit("  "+row, inner)) + "\n")
		}
		lines++
	}
	sb.WriteString(strings.Repeat("\n", max(0, bodyH-lines)))
	sb.WriteString(rule + "\n")

	var detail []string
	if c := p.selected(); c != nil {
		detail = append(detail, "conversation "+c.SessionID)
		if c.Reason != "" {
			detail = append(detail, "not adoptable: "+c.Reason)
		} else {
			detail = append(detail, "Adopting keeps Claude running: the tmux session is renamed and registered. Removing the session in agent-deck later kills it.")
		}
	}
	for len(detail) < 2 {
		detail = append(detail, "")
	}
	for _, line := range detail[:2] {
		sb.WriteString(dim.Render(kanbanFit(line, inner)) + "\n")
	}
	switch {
	case p.err != "":
		sb.WriteString(red.Render(kanbanFit(p.err, inner)) + "\n")
	case p.notice != "":
		sb.WriteString(green.Render(kanbanFit(p.notice, inner)) + "\n")
	default:
		sb.WriteString("\n")
	}
	sb.WriteString(rule + "\n")
	if p.confirming {
		c := p.selected()
		sb.WriteString(sel.Render(kanbanFit(fmt.Sprintf("Adopt %q (pid %d) without restarting it? [y] yes  [n] no", c.TmuxSession, c.ClaudePID), inner)))
	} else {
		sb.WriteString(dim.Render(kanbanFit("[↑↓/jk] Move  [Enter] Adopt  [r] Refresh  [Esc] Close", inner)))
	}
	return box.Render(sb.String())
}
