package ui

// matrix/hardened: AI updates are explicit local operations. Installation and
// daemon activation remain separate, with no update polling or idle watcher.
import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type aiUpdatesManager interface {
	Inspect(context.Context) (aiupdate.Status, error)
	Check(context.Context) (aiupdate.Status, error)
	Install(context.Context) (aiupdate.Status, error)
	Restart(context.Context) (aiupdate.Status, error)
}

type aiUpdatesResult struct {
	panel      *AIUpdatesPanel
	generation uint64
	action     string
	status     aiupdate.Status
	err        error
}

// AIUpdatesPanel manages this machine's configured Codex shared daemon only.
type AIUpdatesPanel struct {
	visible, loaded bool
	width, height   int
	scroll          int
	manager         aiUpdatesManager
	scope           string
	status          aiupdate.Status
	generation      uint64
	busy            string
	confirmRestart  bool
	setupErr        string
	err, notice     string
}

func NewAIUpdatesPanel() *AIUpdatesPanel { return &AIUpdatesPanel{} }

func (p *AIUpdatesPanel) IsVisible() bool { return p != nil && p.visible }

func (p *AIUpdatesPanel) SetSize(width, height int) {
	if p != nil {
		p.width, p.height = width, height
	}
}

func (p *AIUpdatesPanel) Hide() {
	p.visible, p.confirmRestart = false, false
}

// Open does not replace a manager while its operation is in flight. Closing
// hides the panel while the command completes and retains its final result.
func (p *AIUpdatesPanel) Open(manager aiUpdatesManager, setupErr string) tea.Cmd {
	p.visible = true
	p.confirmRestart = false
	if p.busy != "" {
		return nil
	}
	p.manager, p.setupErr = manager, setupErr
	if manager == nil || setupErr != "" {
		return nil
	}
	return p.start("inspect")
}

func (p *AIUpdatesPanel) start(action string) tea.Cmd {
	if p.busy != "" || p.manager == nil || p.setupErr != "" {
		return nil
	}
	p.generation++
	p.busy, p.err = action, ""
	if action != "inspect" {
		p.notice = ""
	}
	manager, generation := p.manager, p.generation
	return func() tea.Msg {
		timeout := 10 * time.Second
		switch action {
		case "check":
			timeout = 30 * time.Second
		case "install":
			timeout = 15 * time.Minute
		case "restart":
			timeout = 6 * time.Minute // matrix/hardened: cover the native configured shutdown deadline.
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var status aiupdate.Status
		var err error
		switch action {
		case "check":
			status, err = manager.Check(ctx)
		case "install":
			status, err = manager.Install(ctx)
		case "restart":
			status, err = manager.Restart(ctx)
		default:
			status, err = manager.Inspect(ctx)
		}
		return aiUpdatesResult{panel: p, generation: generation, action: action, status: status, err: err}
	}
}

func (p *AIUpdatesPanel) accept(result aiUpdatesResult) {
	if result.panel != p || result.generation != p.generation || result.action != p.busy {
		return
	}
	p.busy = ""
	// Failed operations can still return a useful independent local readback.
	if result.err == nil || result.status.Home != "" {
		latest := p.status.LatestVersion
		p.status, p.loaded = result.status, true
		if p.status.LatestVersion == "" {
			p.status.LatestVersion = latest
		}
	}
	if result.err != nil {
		p.err = result.err.Error()
		p.scroll = 0
		return
	}
	p.err = ""
	switch result.action {
	case "check":
		p.notice = "Latest stable version checked. No installation or restart performed."
	case "install":
		p.notice = "Installation finished; the shared daemon was not restarted."
	case "restart":
		p.notice = "Restart command finished. Running version below is the local readback."
	}
}

func (p *AIUpdatesPanel) Update(msg tea.Msg) (*AIUpdatesPanel, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || !p.visible {
		return p, nil
	}
	if p.confirmRestart {
		switch key.String() {
		case "y", "Y":
			p.confirmRestart = false
			return p, p.start("restart")
		case "esc", "n", "N":
			p.confirmRestart = false
			p.scroll = 0
		case "up", "k":
			p.scroll = max(0, p.scroll-1)
		case "down", "j":
			p.scroll++
		case "pgup", "ctrl+u":
			p.scroll = max(0, p.scroll-max(1, p.height/2))
		case "pgdown", "ctrl+d":
			p.scroll += max(1, p.height/2)
		}
		return p, nil
	}
	switch key.String() {
	case "esc", "q":
		p.Hide()
	case "up", "k":
		p.scroll = max(0, p.scroll-1)
	case "down", "j":
		p.scroll++
	case "pgup", "ctrl+u":
		p.scroll = max(0, p.scroll-max(1, p.height/2))
	case "pgdown", "ctrl+d":
		p.scroll += max(1, p.height/2)
	case "r":
		return p, p.start("inspect")
	case "c":
		return p, p.start("check")
	case "i":
		return p, p.start("install")
	case "R", "shift+r":
		if p.busy == "" && p.loaded && p.manager != nil && p.setupErr == "" {
			p.confirmRestart = true
			p.scroll = 0
		}
	}
	return p, nil
}

func (p *AIUpdatesPanel) lines() []string {
	value := func(s string) string {
		if s == "" {
			return "unknown"
		}
		return permanentDeleteClean(s)
	}
	lines := []string{
		"Scope: this machine · configured Codex shared daemon",
		"Codex home: " + value(p.status.Home),
		"CLI version:       " + value(p.status.CLIVersion),
		"Installed version: " + value(p.status.InstalledVersion),
		"Running version:   " + value(p.status.RunningVersion),
		"Latest stable:     " + value(p.status.LatestVersion) + " (c checks online)",
		"Daemon status:     " + value(p.status.DaemonStatus),
	}
	if p.status.Pending {
		lines = append(lines, "Installed; waiting to take effect. The running daemon still uses its current version.")
	}
	if p.loaded {
		lines = append(lines, fmt.Sprintf("Managed installation: %t · Native auto-update: %t", p.status.Managed, p.status.NativeAutoUpdate))
	}
	lines = append(lines, "",
		"[i] Install the latest stable shared-daemon package without interrupting current tasks.",
		"Installing selects the package for the next daemon start and disables native automatic updates; you control when it takes effect.",
		"Opening Codex again may reconnect to the existing, older daemon. The new version takes effect when the shared daemon actually starts again.",
		"[R] Restart and apply now is a separate action that may interrupt ALL tasks and connections sharing this Codex home.",
		"No automatic checks, timers, or idle switching.")
	if p.busy != "" {
		lines = append(lines, "", "In progress: "+p.busy+"… Esc closes the panel; the operation continues.")
	}
	if p.notice != "" {
		lines = append(lines, "", permanentDeleteClean(p.notice))
	}
	if p.setupErr != "" {
		lines = append(lines, "", "Configuration: "+permanentDeleteClean(p.setupErr))
	}
	if p.err != "" {
		lines = append(lines, "", "Error: "+permanentDeleteClean(p.err))
	}
	return lines
}

func (p *AIUpdatesPanel) View() string {
	if !p.IsVisible() {
		return ""
	}
	// Count wrapped rows before windowing, including long paths and errors.
	// The plain compact frame also fits terminals narrower than dialog chrome.
	width := max(1, min(p.width, 100))
	height := max(1, p.height)
	title := "AI UPDATES · Codex"
	footer := renderDialogFooterRows(width, 3, " · ", []string{"r Local", "c Check", "i Install", "R Restart", "↑/↓ Scroll", "Esc Close"})
	if p.confirmRestart {
		title = "RESTART SHARED CODEX DAEMON?"
		footer = renderDialogFooterRows(width, 2, " · ", []string{"y Restart now", "↑/↓ Scroll", "Esc Cancel"})
	}
	title = cellTruncate(title, width, "")
	raw := p.lines()
	if p.confirmRestart {
		raw = []string{
			"Restarting may interrupt ALL running tasks and disconnect ALL clients sharing this Codex home, including clients outside Agent Deck.",
			"Codex home: " + permanentDeleteClean(p.status.Home),
			"Only continue when you are ready to interrupt that shared background service.",
			"Press y to restart now. Press Esc to cancel.",
		}
	}
	var rows []string
	for _, line := range raw {
		for _, wrapped := range strings.Split(ansi.Wrap(line, width, ""), "\n") {
			rows = append(rows, cellTruncate(wrapped, width, ""))
		}
	}
	if height == 1 {
		return title
	}
	footerRows := strings.Split(footer, "\n")
	if len(footerRows) >= height {
		footerRows = footerRows[:height-1]
	}
	bodyHeight := max(0, height-1-len(footerRows))
	scroll := min(max(0, p.scroll), max(0, len(rows)-bodyHeight))
	p.scroll = scroll
	end := min(len(rows), scroll+bodyHeight)
	visible := append([]string(nil), rows[scroll:end]...)
	if len(visible) > 0 && scroll > 0 {
		visible[0] = cellTruncate("↑ more above", width, "")
	}
	if len(visible) > 1 && end < len(rows) {
		visible[len(visible)-1] = cellTruncate("↓ more below", width, "")
	}
	frame := append([]string{lipgloss.NewStyle().Bold(true).Foreground(ColorAccent).Render(title)}, visible...)
	frame = append(frame, footerRows...)
	return lipgloss.Place(p.width, p.height, lipgloss.Center, lipgloss.Center, strings.Join(frame, "\n"))
}
