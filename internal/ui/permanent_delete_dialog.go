package ui

// matrix/hardened: permanent deletion stays inside Bubble Tea so opening its
// read-only preview never releases terminal ownership or switches screens.
import (
	"fmt"
	"strings"
	"unicode"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type permanentDeleteDialogState uint8

const (
	permanentDeleteLoading permanentDeleteDialogState = iota
	permanentDeleteReady
	permanentDeleteBusy
	permanentDeleteFailed
)

// PermanentDeleteDialog owns only preview and confirmation state; Home runs
// the read-only planner and deletion commands on Bubble Tea's update loop.
type PermanentDeleteDialog struct {
	visible   bool
	width     int
	height    int
	requestID uint64
	sessionID string
	title     string
	state     permanentDeleteDialogState
	plan      session.PermanentDeletePlan
	err       string
	input     []rune
	scroll    int
}

func NewPermanentDeleteDialog() *PermanentDeleteDialog {
	return &PermanentDeleteDialog{width: 80, height: 24}
}

func (d *PermanentDeleteDialog) IsVisible() bool { return d != nil && d.visible }

func (d *PermanentDeleteDialog) Hide() {
	if d == nil {
		return
	}
	d.visible = false
	d.input = nil
}

func (d *PermanentDeleteDialog) SetSize(w, h int) {
	if d == nil {
		return
	}
	d.width, d.height = max(1, w), max(1, h)
}

func (d *PermanentDeleteDialog) ShowLoading(requestID uint64, sessionID, title string) {
	d.visible, d.requestID, d.sessionID, d.title = true, requestID, sessionID, title
	d.state, d.plan, d.err = permanentDeleteLoading, session.PermanentDeletePlan{}, ""
	d.input, d.scroll = nil, 0
}

func (d *PermanentDeleteDialog) RequestID() uint64 { return d.requestID }

func (d *PermanentDeleteDialog) SessionID() string { return d.sessionID }

// SetPreview rejects late results after cancellation or a newer opening. A
// preview cannot replace an approved plan while deletion is in progress.
func (d *PermanentDeleteDialog) SetPreview(requestID uint64, plan session.PermanentDeletePlan, err error) bool {
	if !d.IsVisible() || d.requestID != requestID || d.state != permanentDeleteLoading {
		return false
	}
	if err != nil {
		d.state, d.err = permanentDeleteFailed, err.Error()
		return true
	}
	if plan.SessionID != d.sessionID {
		return false
	}
	d.plan, d.state = permanentDeletePlanCopy(plan), permanentDeleteReady
	d.input, d.scroll = nil, 0
	return true
}

func (d *PermanentDeleteDialog) Plan() session.PermanentDeletePlan {
	return permanentDeletePlanCopy(d.plan)
}

func permanentDeletePlanCopy(plan session.PermanentDeletePlan) session.PermanentDeletePlan {
	plan.NativeIDs = append([]string(nil), plan.NativeIDs...)
	plan.Files = append([]string(nil), plan.Files...)
	return plan
}

// HandleKey returns true only for the exact confirmation text and Enter.
// Every other key remains in the dialog, including main-screen shortcuts.
func (d *PermanentDeleteDialog) HandleKey(msg tea.KeyMsg) bool {
	if !d.IsVisible() {
		return false
	}
	if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
		if d.state != permanentDeleteBusy {
			d.Hide()
		}
		return false
	}
	_, bodyRows, _, _ := d.geometry()
	switch msg.Type {
	case tea.KeyUp:
		d.scroll = max(0, d.scroll-1)
		return false
	case tea.KeyDown:
		d.scroll++
	case tea.KeyPgUp:
		d.scroll = max(0, d.scroll-max(1, bodyRows))
	case tea.KeyPgDown:
		d.scroll += max(1, bodyRows)
	case tea.KeyHome:
		d.scroll = 0
	case tea.KeyEnd:
		d.scroll = len(d.bodyLines())
	default:
		if d.state != permanentDeleteReady {
			return false
		}
		switch msg.Type {
		case tea.KeyEnter:
			return string(d.input) == "DELETE"
		case tea.KeyBackspace, tea.KeyDelete:
			if len(d.input) > 0 {
				d.input = d.input[:len(d.input)-1]
			}
		case tea.KeyCtrlU:
			d.input = nil
		case tea.KeySpace:
			if len(d.input) < 256 {
				d.input = append(d.input, ' ')
			}
		case tea.KeyRunes:
			for _, r := range msg.Runes {
				if !unicode.IsControl(r) && len(d.input) < 256 {
					d.input = append(d.input, r)
				}
			}
		}
		return false
	}
	d.clampScroll(bodyRows)
	return false
}

func (d *PermanentDeleteDialog) StartDeleting() {
	if d.IsVisible() && d.state == permanentDeleteReady && string(d.input) == "DELETE" {
		d.state = permanentDeleteBusy
		d.input = nil
	}
}

// A failed deletion may have partially completed. Keep its error visible and
// require closing and reopening for a fresh plan before another confirmation.
func (d *PermanentDeleteDialog) SetDeleteError(requestID uint64, err error) bool {
	if !d.IsVisible() || d.requestID != requestID || d.state != permanentDeleteBusy || err == nil {
		return false
	}
	d.state, d.err, d.input, d.scroll = permanentDeleteFailed, err.Error(), nil, 0
	return true
}

// geometry reserves the confirmation and footer even in short terminals.
// In the smallest terminals there is no border or body, but keys still work.
func (d *PermanentDeleteDialog) geometry() (innerWidth, bodyRows int, border, header bool) {
	w, h := max(1, d.width), max(1, d.height)
	panelWidth := min(96, w)
	if w >= 24 {
		panelWidth = min(96, w-4)
	}
	panelHeight := min(30, h)
	if h >= 12 {
		panelHeight = min(30, h-2)
	}
	border = panelWidth >= 10 && panelHeight >= 6
	innerWidth = panelWidth
	if border {
		innerWidth -= 4 // two border columns and one padding column per side
		panelHeight -= 2
	}
	header = panelHeight >= 3
	fixedRows := min(2, panelHeight) // input/status and footer
	if header {
		fixedRows++
	}
	return max(1, innerWidth), max(0, panelHeight-fixedRows), border, header
}

func (d *PermanentDeleteDialog) bodyLines() []string {
	var raw []string
	switch d.state {
	case permanentDeleteLoading:
		raw = []string{
			"Preparing read-only deletion preview...",
			"Checking exact conversation IDs, local files and shared references.",
			"No records have been deleted. Esc cancels.",
		}
	case permanentDeleteFailed:
		raw = []string{"Deletion not completed:", permanentDeleteClean(d.err), "", "Close and reopen to prepare a fresh preview before trying again."}
	default:
		raw = []string{
			"This permanently removes local conversation records and cannot be undone.",
			"Session: " + permanentDeleteClean(d.title),
			"Deck ID: " + permanentDeleteClean(d.sessionID),
			"Harness: " + permanentDeleteClean(d.plan.Harness),
			"", "Exact native conversation IDs:",
		}
		for _, id := range d.plan.NativeIDs {
			raw = append(raw, "  "+permanentDeleteClean(id))
		}
		raw = append(raw, "", fmt.Sprintf("Exact local files/directories (%d):", len(d.plan.Files)))
		if len(d.plan.Files) == 0 {
			raw = append(raw, "  No remaining native log files.")
		}
		for _, file := range d.plan.Files {
			raw = append(raw, "  "+permanentDeleteClean(file))
		}
		raw = append(raw, "",
			"Also removes matching native history/database rows, Agent Deck registration, recall/subagent indexes, costs, recent items and notifications.",
			"Project and worktree files are preserved.",
			"Other forks, external backups and cloud records are outside this operation.")
	}
	width, _, _, _ := d.geometry()
	var lines []string
	for _, line := range raw {
		for _, wrapped := range strings.Split(ansi.Hardwrap(line, width, true), "\n") {
			// cellTruncate also covers terminals narrower than a wide rune.
			lines = append(lines, cellTruncate(wrapped, width, ""))
		}
	}
	return lines
}

func (d *PermanentDeleteDialog) clampScroll(bodyRows int) {
	d.scroll = min(max(0, d.scroll), max(0, len(d.bodyLines())-max(1, bodyRows)))
}

// permanentDeleteClean displays embedded controls literally, so a filename or
// error cannot inject terminal escape sequences into the deletion preview.
func permanentDeleteClean(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch r {
		case '\n':
			out.WriteString("\\n")
		case '\r':
			out.WriteString("\\r")
		case '\t':
			out.WriteString("\\t")
		default:
			if unicode.IsControl(r) {
				fmt.Fprintf(&out, "\\u%04x", r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func (d *PermanentDeleteDialog) View() string {
	if !d.IsVisible() {
		return ""
	}
	width, bodyRows, border, header := d.geometry()
	red := lipgloss.NewStyle().Foreground(ColorRed)
	muted := lipgloss.NewStyle().Foreground(ColorTextDim)
	var rows []string
	if header {
		rows = append(rows, red.Bold(true).Render(cellTruncate("Permanently delete local records", width, "…")))
	}
	lines := d.bodyLines()
	start := min(max(0, d.scroll), max(0, len(lines)-max(1, bodyRows)))
	for i := 0; i < bodyRows; i++ {
		line := ""
		if start+i < len(lines) {
			line = lines[start+i]
		}
		rows = append(rows, line)
	}
	status, footer := "", ""
	switch d.state {
	case permanentDeleteLoading:
		status, footer = "Preparing preview...", "Esc cancel"
	case permanentDeleteBusy:
		status, footer = "Deleting... please wait", "Do not close; waiting for result"
	case permanentDeleteFailed:
		status, footer = "Not completed; reopen to replan", "↑/↓ scroll · Esc close"
	default:
		status = "Type DELETE: " + permanentDeleteClean(string(d.input)) + "_"
		if width < 30 {
			status = "DELETE: " + permanentDeleteClean(string(d.input)) + "_"
		}
		footer = "Enter confirm · Esc cancel · ↑/↓ PgUp/PgDn scroll"
		if width < 48 {
			footer = "Esc cancel · ↑/↓ scroll"
		}
	}
	if len(lines) > bodyRows && bodyRows > 0 && width >= 48 {
		footer += fmt.Sprintf(" · %d-%d/%d", start+1, min(start+bodyRows, len(lines)), len(lines))
	}
	// A one-row terminal keeps the cancel/wait instruction available.
	if d.height > 1 {
		rows = append(rows, red.Render(cellTruncate(status, width, "…")))
	}
	rows = append(rows, muted.Render(cellTruncate(footer, width, "…")))
	content := strings.Join(rows, "\n")
	style := lipgloss.NewStyle().Width(width)
	if border {
		// Lip Gloss includes horizontal padding in Width; reserve it here to
		// avoid a second wrap expanding the fixed-height panel.
		style = style.Width(width+2).Border(lipgloss.RoundedBorder()).BorderForeground(ColorRed).Padding(0, 1)
	}
	panel := style.Render(content)
	return lipgloss.Place(max(1, d.width), max(1, d.height), lipgloss.Center, lipgloss.Center, panel)
}
