package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/ingest"
	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
	"github.com/asheshgoplani/agent-deck/internal/recall/store"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The Recall search overlay (docs/recall.md, phase 3): the `G` key over
// recall.db. It replaced the in-memory global search index, which opened
// one directory watcher per project and loaded every transcript into
// memory (the OOM the comment at the old init site recorded). Nothing here
// walks a tree or parses a file on a keypress: every keystroke is an FTS
// query against the index as it stands, and the index is refreshed by a
// bounded sweep (150 ms / 32 MB) in a tea.Cmd after the overlay opens,
// continued in further bounded passes through the busy/load gate while
// anything was deferred. The header says how stale the index is.
//
// Everything the overlay can do has a CLI form: typing = `recall search`,
// the preview = `recall show`, Enter = `recall open`.

var (
	globalSearchBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(ColorCyan).
				Padding(0, 1)

	globalResultStyle = lipgloss.NewStyle().
				Padding(0, 2)

	globalSelectedStyle = lipgloss.NewStyle().
				Padding(0, 2).
				Background(ColorCyan).
				Foreground(ColorBg)

	globalSearchHeaderStyle = lipgloss.NewStyle().
				Foreground(ColorCyan).
				Bold(true)

	highlightStyle = lipgloss.NewStyle().
			Background(ColorYellow).
			Foreground(ColorBg).
			Bold(true)
)

// Overlay limits.
const (
	recallResultLimit  = 15
	recallPreviewTurns = 12
	recallDebounce     = 250 * time.Millisecond
	// recallMaxWidth caps the overlay on wide terminals; below it the
	// overlay takes the terminal width less a margin, so it fits 80 columns.
	recallMaxWidth = 160
)

// recallCatchUpDelay paces the background continuation of a deferred
// sweep: one bounded pass, then a pause, then the next through the gate,
// so a large backlog never holds a core while the user types. Tests
// shorten it.
var recallCatchUpDelay = time.Second

// GlobalSearchResult is one ranked session as the overlay shows it and as
// Home receives it on Enter.
type GlobalSearchResult struct {
	SessID    int64
	Harness   string
	Profile   string
	SessionID string // the harness conversation id
	DeckID    string // bound agent-deck session, when any
	Title     string
	Snippet   string
	CWD       string
	EndedAt   time.Time
	BodyHits  int
	CardHit   bool
	Missing   bool
	Sidechain bool
	// InAgentDeck / InstanceID: a registered session owns this conversation.
	InAgentDeck bool
	InstanceID  string
}

// globalSearchResultsMsg delivers async search results back to the UI.
type globalSearchResultsMsg struct {
	query      string
	generation uint64 // matrix/hardened: reject results from another query or scope.
	res        query.SearchResult
	err        error
}

// globalSearchDebounceMsg fires after the debounce interval.
type globalSearchDebounceMsg struct {
	query      string
	generation uint64 // matrix/hardened
}

// recallPreviewMsg delivers the preview of one session.
type recallPreviewMsg struct {
	epoch    uint64 // matrix/hardened: previews belong to one refresh cycle.
	revision uint64 // matrix/hardened: discard previews read before committed progress.
	sessID   int64
	detail   query.Detail
	err      error
}

// recallRefreshMsg reports one bounded sweep pass.
type recallRefreshMsg struct {
	epoch uint64 // matrix/hardened
	res   ingest.Result
	err   error
	gated bool
}

// recallStatusMsg delivers the index summary for the header.
type recallStatusMsg struct {
	epoch    uint64 // matrix/hardened
	revision uint64 // matrix/hardened: status reads can complete out of order.
	status   query.Status
	err      error
}

// recallCatchUpMsg fires after recallCatchUpDelay: run the next gated pass.
type recallCatchUpMsg struct {
	epoch   uint64
	ungated bool // matrix/hardened: bounded retry of the explicit opening/manual pass.
}

// GlobalSearch is the Recall search overlay.
type GlobalSearch struct {
	input         textinput.Model
	results       []*GlobalSearchResult
	cursor        int
	width         int
	height        int
	visible       bool
	switchToLocal bool
	previewScroll int
	query         string
	searching     bool
	searchErr     string
	searchDirty   bool // matrix/hardened: rerun after committed progress without starving a slow read.
	resultQuery   string
	candidates    int
	ceilingHit    bool
	// matrix/hardened: nil means every conversation; otherwise the current
	// profile's owners are an immutable scope for this opening of Recall.
	managed    *query.ManagedScope
	generation uint64
	// matrix/hardened: cancel abandoned work and isolate every refresh cycle,
	// independently of the query generation which changes while typing.
	epoch           uint64
	ctx             context.Context
	cancel          context.CancelFunc
	searchCancel    context.CancelFunc
	previewRevision uint64
	statusRevision  uint64
	lockRetries     int // matrix/hardened: let a canceled sweep release its lock before retrying.

	source   RecallSource
	status   query.Status
	hasStat  bool
	refresh  string // staleness line: what the last pass deferred or skipped
	sweeping bool
	preview  map[int64]query.Detail
	previewT map[int64]bool   // preview requested
	previewE map[int64]string // preview failed: what `recall show` answered
}

// NewGlobalSearch creates the overlay without a source; SetSource wires it.
func NewGlobalSearch() *GlobalSearch {
	ti := textinput.New()
	ti.Placeholder = "Search every conversation on this machine..."
	ti.Focus()
	ti.CharLimit = 200
	ti.Width = 60
	return &GlobalSearch{input: ti, results: []*GlobalSearchResult{}, preview: map[int64]query.Detail{}, previewT: map[int64]bool{}, previewE: map[int64]string{}}
}

// SetSource wires the index the overlay reads (nil disables it).
func (gs *GlobalSearch) SetSource(src RecallSource) { gs.source = src }

// HasSource reports whether the overlay can be opened.
func (gs *GlobalSearch) HasSource() bool { return gs.source != nil }

// SetSize sets the dimensions of the overlay.
func (gs *GlobalSearch) SetSize(width, height int) {
	gs.width = width
	gs.height = height
}

// Show opens the overlay and returns the commands that refresh the header
// and run the first bounded sweep pass.
func (gs *GlobalSearch) Show() tea.Cmd {
	return gs.show(nil)
}

// matrix/hardened: a dedicated shortcut opens Recall over the current
// registered sessions. An empty scope stays empty rather than becoming all.
func (gs *GlobalSearch) ShowManaged(scope query.ManagedScope) tea.Cmd {
	// matrix/hardened: own the snapshot even if the caller reuses its slices.
	scope.DeckIDs = append([]string(nil), scope.DeckIDs...)
	scope.ClaudeIDs = append([]string(nil), scope.ClaudeIDs...)
	scope.CodexIDs = append([]string(nil), scope.CodexIDs...)
	return gs.show(&scope)
}

func (gs *GlobalSearch) show(scope *query.ManagedScope) tea.Cmd {
	gs.restartRequests()
	gs.managed = scope
	gs.generation++
	gs.input.Placeholder = "Search every conversation on this machine..."
	if scope != nil {
		gs.input.Placeholder = "搜索已管理会话的内容..."
	}
	gs.visible = true
	gs.input.Focus()
	gs.input.SetValue("")
	gs.results = nil
	gs.cursor = 0
	gs.switchToLocal = false
	gs.previewScroll = 0
	gs.searching = false
	gs.searchErr = ""
	gs.searchDirty = false // matrix/hardened
	gs.query = ""
	gs.refresh = ""
	gs.preview = map[int64]query.Detail{}
	gs.previewT = map[int64]bool{}
	gs.previewE = map[int64]string{}
	if gs.source == nil {
		return nil
	}
	gs.sweeping = true
	return tea.Batch(gs.statusCmd(), gs.refreshCmd(false))
}

// WantsSwitchToLocal returns true if user pressed Tab to switch to local search.
func (gs *GlobalSearch) WantsSwitchToLocal() bool {
	if gs.switchToLocal {
		gs.switchToLocal = false
		return true
	}
	return false
}

// Hide hides the overlay and ends the catch-up chain: a tick that
// outlives the overlay finds sweeping false and runs nothing.
func (gs *GlobalSearch) Hide() {
	// matrix/hardened: a hidden overlay must not keep queued database work.
	if gs.cancel != nil {
		gs.cancel()
	}
	gs.visible = false
	gs.sweeping = false
	gs.input.Blur()
}

// IsVisible returns whether the overlay is visible.
func (gs *GlobalSearch) IsVisible() bool { return gs.visible }

// Selected returns the currently selected result.
func (gs *GlobalSearch) Selected() *GlobalSearchResult {
	if len(gs.results) == 0 {
		return nil
	}
	if gs.cursor >= len(gs.results) {
		gs.cursor = len(gs.results) - 1
	}
	return gs.results[gs.cursor]
}

func (gs *GlobalSearch) statusCmd() tea.Cmd {
	src, parent, epoch := gs.source, gs.ctx, gs.epoch
	gs.statusRevision++ // matrix/hardened: retain only the latest status request.
	revision := gs.statusRevision
	return func() tea.Msg {
		// matrix/hardened: busy reads report a failure instead of waiting forever.
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		st, err := src.Status(ctx)
		return recallStatusMsg{epoch: epoch, revision: revision, status: st, err: err}
	}
}

func (gs *GlobalSearch) refreshCmd(gated bool) tea.Cmd {
	src, parent, epoch := gs.source, gs.ctx, gs.epoch
	return func() tea.Msg {
		// matrix/hardened: the ingest budget also needs a cancellable outer limit.
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		defer cancel()
		res, err := src.Refresh(ctx, gated)
		return recallRefreshMsg{epoch: epoch, res: res, err: err, gated: gated}
	}
}

func (gs *GlobalSearch) searchCmd(q string) tea.Cmd {
	src := gs.source
	// matrix/hardened: capture the opening/query scope before the async command.
	scope, generation := gs.managed, gs.generation
	// matrix/hardened: obsolete searches must release their read connection.
	if gs.searchCancel != nil {
		gs.searchCancel()
	}
	ctx, cancel := context.WithCancel(gs.ctx)
	gs.searchCancel = cancel
	return func() tea.Msg {
		ctx, timeoutCancel := context.WithTimeout(ctx, 5*time.Second)
		defer timeoutCancel()
		defer cancel()
		var res query.SearchResult
		var err error
		if scope != nil {
			res, err = src.SearchManaged(ctx, q, recallResultLimit, *scope)
		} else {
			res, err = src.Search(ctx, q, recallResultLimit)
		}
		return globalSearchResultsMsg{query: q, generation: generation, res: res, err: err}
	}
}

func (gs *GlobalSearch) previewCmd(sessID int64) tea.Cmd {
	src, parent, epoch := gs.source, gs.ctx, gs.epoch
	revision := gs.previewRevision // matrix/hardened
	return func() tea.Msg {
		// matrix/hardened
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		d, err := src.Show(ctx, sessID, recallPreviewTurns)
		return recallPreviewMsg{epoch: epoch, revision: revision, sessID: sessID, detail: d, err: err}
	}
}

// catchUpCmd schedules the next gated pass after recallCatchUpDelay.
func (gs *GlobalSearch) catchUpCmd() tea.Cmd {
	return gs.refreshAfterCmd(true)
}

// matrix/hardened: a manual retry that races cancellation may briefly find the
// old sweep holding its lock. Retry that one pass without entering a busy loop.
func (gs *GlobalSearch) refreshAfterCmd(gated bool) tea.Cmd {
	epoch := gs.epoch // matrix/hardened: an old timer cannot restart a new chain.
	return tea.Tick(recallCatchUpDelay, func(time.Time) tea.Msg { return recallCatchUpMsg{epoch: epoch, ungated: !gated} })
}

// matrix/hardened: manual refresh replaces pending work while retaining the
// query, managed scope and visible results so retries do not lose the user's place.
func (gs *GlobalSearch) restartRequests() {
	if gs.cancel != nil {
		gs.cancel()
	}
	gs.epoch++
	gs.ctx, gs.cancel = context.WithCancel(context.Background())
	gs.searchCancel = nil
	gs.lockRetries = 0 // matrix/hardened
}

func (gs *GlobalSearch) manualRefresh() tea.Cmd {
	if gs.source == nil {
		return nil
	}
	gs.restartRequests()
	gs.generation++
	gs.refresh, gs.searchErr = "", ""
	gs.searchDirty = false // matrix/hardened
	gs.sweeping = true
	gs.preview = map[int64]query.Detail{}
	gs.previewT = map[int64]bool{}
	gs.previewE = map[int64]string{}
	cmds := []tea.Cmd{gs.statusCmd(), gs.refreshCmd(false)}
	gs.searching = gs.query != ""
	if gs.searching {
		cmds = append(cmds, gs.searchCmd(gs.query))
	}
	return tea.Batch(cmds...)
}

// requestPreview asks for the selected session's preview once.
func (gs *GlobalSearch) requestPreview() tea.Cmd {
	sel := gs.Selected()
	if sel == nil || gs.source == nil || gs.previewT[sel.SessID] {
		return nil
	}
	gs.previewT[sel.SessID] = true
	return gs.previewCmd(sel.SessID)
}

// Update handles messages for the overlay.
func (gs *GlobalSearch) Update(msg tea.Msg) (*GlobalSearch, tea.Cmd) {
	if !gs.visible {
		return gs, nil
	}
	switch msg := msg.(type) {
	case recallStatusMsg:
		if msg.epoch != gs.epoch || msg.revision != gs.statusRevision {
			return gs, nil // matrix/hardened: superseded refresh cycle.
		}
		if msg.err == nil {
			gs.status, gs.hasStat = msg.status, true
		}
		return gs, nil

	case recallRefreshMsg:
		if msg.epoch != gs.epoch {
			return gs, nil // matrix/hardened
		}
		gs.sweeping = false
		cmds := []tea.Cmd{gs.statusCmd()}
		// matrix/hardened: cached or late previews from before this commit
		// must not hide newly indexed turns of the same conversation.
		changed := msg.res.Parsed > 0 || msg.res.Missing > 0 || msg.res.Sessions > 0
		if changed {
			gs.previewRevision++
			gs.preview = map[int64]query.Detail{}
			gs.previewT = map[int64]bool{}
			gs.previewE = map[int64]string{}
		}
		switch {
		case !msg.gated && errors.Is(msg.err, store.ErrLocked) && gs.lockRetries < 3:
			gs.lockRetries++
			gs.sweeping = true
			gs.refresh = "index busy; retrying refresh shortly..."
			cmds = append(cmds, gs.refreshAfterCmd(false))
		case msg.err != nil && recallSkipped(msg.err):
			gs.refresh = "index not refreshed: " + msg.err.Error()
		case msg.err != nil:
			gs.refresh = "index refresh failed: " + msg.err.Error()
		case msg.res.Deferred > 0:
			gs.refresh = fmt.Sprintf("index behind by %d source(s) / %s; catching up in the background", msg.res.Deferred, humanBytes(msg.res.DeferredBytes))
			// Continue in bounded passes, paced and through the gate,
			// while the overlay is open.
			gs.sweeping = true
			cmds = append(cmds, gs.catchUpCmd())
		default:
			gs.refresh = ""
		}
		// matrix/hardened: committed progress is searchable before the entire
		// backlog finishes. Do not keep cancelling a slow query on empty passes.
		if gs.query != "" {
			if gs.searching {
				gs.searchDirty = gs.searchDirty || changed
			} else {
				gs.searching = true
				gs.generation++
				cmds = append(cmds, gs.searchCmd(gs.query))
			}
		}
		return gs, tea.Batch(cmds...)

	case recallCatchUpMsg:
		if msg.epoch != gs.epoch || !gs.sweeping || gs.source == nil {
			return gs, nil // closed and reopened meanwhile: Show restarted the chain
		}
		return gs, gs.refreshCmd(!msg.ungated)

	case globalSearchDebounceMsg:
		if msg.generation == gs.generation && msg.query == strings.TrimSpace(gs.input.Value()) && msg.query != "" && gs.source != nil {
			gs.searching = true
			return gs, gs.searchCmd(msg.query)
		}
		return gs, nil

	case globalSearchResultsMsg:
		if msg.generation != gs.generation || msg.query != strings.TrimSpace(gs.input.Value()) {
			return gs, nil
		}
		gs.searching = false
		if msg.err != nil {
			// matrix/hardened: committed progress still needs its follow-up
			// even if the older snapshot failed or timed out.
			if gs.searchDirty {
				gs.searchDirty = false
				gs.searching = true
				gs.searchErr = ""
				gs.generation++
				return gs, gs.searchCmd(gs.query)
			}
			gs.searchErr = msg.err.Error()
			gs.results = nil
			return gs, nil
		}
		gs.searchErr = ""
		gs.applySearchResults(msg.res)
		// matrix/hardened: a sweep may have committed while this read was
		// running. Finish it first, then query the newer snapshot once.
		if gs.searchDirty {
			gs.searchDirty = false
			gs.searching = true
			gs.generation++
			return gs, tea.Batch(gs.requestPreview(), gs.searchCmd(gs.query))
		}
		return gs, gs.requestPreview()

	case recallPreviewMsg:
		if msg.epoch != gs.epoch || msg.revision != gs.previewRevision {
			return gs, nil // matrix/hardened
		}
		if msg.err != nil {
			gs.previewE[msg.sessID] = msg.err.Error()
		} else {
			gs.preview[msg.sessID] = msg.detail
		}
		return gs, nil

	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			return gs, gs.move(-1)
		case tea.MouseButtonWheelDown:
			return gs, gs.move(1)
		}
		return gs, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+r", "f5": // matrix/hardened: retry without closing Recall.
			return gs, gs.manualRefresh()
		case "esc":
			gs.Hide()
			return gs, nil
		case "enter":
			if len(gs.results) > 0 {
				gs.Hide() // Home handles the selection
			}
			return gs, nil
		case "up":
			return gs, gs.move(-1)
		case "down":
			return gs, gs.move(1)
		case "[", "pgup":
			gs.previewScroll = max(gs.previewScroll-5, 0)
			return gs, nil
		case "]", "pgdown":
			gs.previewScroll += 5
			return gs, nil
		case "tab":
			gs.switchToLocal = true
			gs.Hide()
			return gs, nil
		default:
			var cmd tea.Cmd
			gs.input, cmd = gs.input.Update(msg)
			q := strings.TrimSpace(gs.input.Value())
			if q == gs.query {
				return gs, cmd
			}
			gs.query = q
			gs.searchDirty = false // matrix/hardened
			// matrix/hardened: cancel before the debounce, including an empty query.
			if gs.searchCancel != nil {
				gs.searchCancel()
			}
			// matrix/hardened: even returning to the same text must reject an
			// older in-flight result from before the intervening edits.
			gs.generation++
			if q == "" {
				gs.results = nil
				gs.searching = false
				gs.searchErr = ""
				return gs, cmd
			}
			gs.searching = true
			generation := gs.generation
			return gs, tea.Batch(cmd, tea.Tick(recallDebounce, func(time.Time) tea.Msg {
				return globalSearchDebounceMsg{query: q, generation: generation}
			}))
		}
	}
	return gs, nil
}

// move shifts the cursor and requests the new selection's preview.
func (gs *GlobalSearch) move(delta int) tea.Cmd {
	if len(gs.results) == 0 {
		return nil
	}
	next := gs.cursor + delta
	if next < 0 || next >= len(gs.results) {
		return nil
	}
	gs.cursor = next
	gs.previewScroll = 0
	return gs.requestPreview()
}

// applySearchResults converts ranked hits into rows. The order is the
// query layer's (card hit, body hits, recency); the overlay never re-ranks.
func (gs *GlobalSearch) applySearchResults(res query.SearchResult) {
	// matrix/hardened: keep the selected conversation through background refresh.
	var selectedID int64
	if selected := gs.Selected(); selected != nil && gs.resultQuery == gs.query {
		selectedID = selected.SessID
	}
	gs.resultQuery = gs.query
	gs.results = make([]*GlobalSearchResult, 0, len(res.Hits))
	for _, h := range res.Hits {
		r := &GlobalSearchResult{
			SessID: h.SessID, Harness: h.Harness, Profile: h.Profile, SessionID: h.NativeID, DeckID: h.DeckID,
			Title: h.Title, Snippet: h.Snippet, CWD: h.CWD, BodyHits: h.BodyHits, CardHit: h.CardHit,
			Missing: h.Missing, Sidechain: h.Sidechain,
		}
		r.EndedAt = unixOrZeroTime(firstNonZero(h.EndedAt, h.StartedAt))
		gs.results = append(gs.results, r)
	}
	gs.candidates, gs.ceilingHit = res.Candidates, res.CeilingHit
	gs.cursor = 0
	for i, result := range gs.results {
		if result.SessID == selectedID {
			gs.cursor = i
			break
		}
	}
	gs.previewScroll = 0
}

// unixOrZeroTime is time.Unix(ts, 0), with the zero time (not 1970) for a
// missing timestamp.
func unixOrZeroTime(ts int64) time.Time {
	if ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// matrix/hardened: resolve the owner the same way for labels and Enter.
// A bound deck id takes precedence over a harness conversation id, and
// empty native ids must never match an instance's unset fields.
func findGlobalSearchOwner(result *GlobalSearchResult, instances []*session.Instance) *session.Instance {
	if result.DeckID != "" {
		for _, inst := range instances {
			if inst.ID == result.DeckID {
				return inst
			}
		}
	}
	if result.SessionID == "" {
		return nil
	}
	for _, inst := range instances {
		if (result.Harness == reader.HarnessClaude && inst.ClaudeSessionID == result.SessionID) ||
			(result.Harness == reader.HarnessCodex && inst.CodexSessionID == result.SessionID) {
			return inst
		}
	}
	return nil
}

// MarkInAgentDeck marks results owned by a session in the current list.
// matrix/hardened: recompute on every render so imports/removals update the
// label, including Codex threads without a persisted deck link.
func (gs *GlobalSearch) MarkInAgentDeck(instances []*session.Instance) {
	for _, r := range gs.results {
		r.InAgentDeck, r.InstanceID = false, ""
		if inst := findGlobalSearchOwner(r, instances); inst != nil {
			r.InAgentDeck, r.InstanceID = true, inst.ID
		}
	}
	// matrix/hardened: an owner may be removed while this opening is on
	// screen. Never leave that result visible in the managed-only list.
	if gs.managed != nil {
		managed := gs.results[:0]
		for _, r := range gs.results {
			if r.InAgentDeck {
				managed = append(managed, r)
			}
		}
		gs.results = managed
		gs.cursor = min(gs.cursor, max(len(gs.results)-1, 0))
	}
}

// View renders the overlay with split-pane layout.
func (gs *GlobalSearch) View() string {
	if !gs.visible {
		return ""
	}
	// Two panes plus four border cells must fit the terminal: the overlay
	// is the terminal width less a margin, capped for wide screens, and
	// never wider than the screen it is drawn on.
	totalWidth := min(max(gs.width-4, 40), recallMaxWidth)
	leftWidth := totalWidth * 38 / 100
	rightWidth := totalWidth - leftWidth - 4
	previewHeight := max(gs.height-12, 6)
	gs.input.Width = max(leftWidth-8, 10)

	var left strings.Builder
	var resultRows []string
	left.WriteString(globalSearchHeaderStyle.Render(gs.headerLine()) + "\n")
	left.WriteString(lipgloss.NewStyle().Foreground(ColorComment).Render(gs.staleLine()) + "\n\n")
	left.WriteString(globalSearchBoxStyle.Width(leftWidth-4).Render(gs.input.View()) + "\n\n")

	switch {
	case gs.searchErr != "":
		left.WriteString(lipgloss.NewStyle().Foreground(ColorRed).Render("  " + gs.searchErr))
	case gs.searching && len(gs.results) == 0:
		left.WriteString(lipgloss.NewStyle().Foreground(ColorYellow).Render("  Searching..."))
	case len(gs.results) == 0 && gs.input.Value() != "":
		left.WriteString(lipgloss.NewStyle().Foreground(ColorComment).Render("  No results"))
	case len(gs.results) == 0:
		prompt := "  Type to search; titles, hints and tags rank first"
		if gs.managed != nil {
			prompt = "  仅搜索当前 profile 的已管理会话"
		}
		left.WriteString(lipgloss.NewStyle().Foreground(ColorComment).Italic(true).Render(prompt))
	default:
		summary := fmt.Sprintf("  %d matching message(s)", gs.candidates)
		if gs.ceilingHit {
			summary += " (capped; narrow the query)"
		}
		left.WriteString(lipgloss.NewStyle().Foreground(ColorComment).Render(summary))
		for i, r := range gs.results {
			// matrix/hardened: a text label survives selection, unlike the old
			// bullet. Put it below the title to preserve titles on narrow panes.
			title := clipCells(firstNonEmpty(r.Title, r.Snippet, r.SessionID), max(leftWidth-8, 1))
			label := "[未管理]"
			if r.InAgentDeck {
				label = "[已管理]"
			}
			meta := label + " " + clipCells(gs.resultMeta(r), max(leftWidth-19, 1))
			if i == gs.cursor {
				resultRows = append(resultRows, globalSelectedStyle.Render("› "+title)+"\n"+
					lipgloss.NewStyle().Foreground(ColorPurple).Render("    "+meta))
			} else {
				resultRows = append(resultRows, globalResultStyle.Render("  "+title)+"\n"+
					lipgloss.NewStyle().Foreground(ColorComment).Render("    "+meta))
			}
		}
	}
	// The key hints stay two rows at every width: on a narrow pane the
	// less important hints drop out instead of wrapping.
	keyWidth := leftWidth - 2 // the pane's Padding(0, 1)
	enterHint := "[Enter] Open"
	if gs.managed != nil {
		enterHint = "[Enter] Locate"
	}
	// matrix/hardened: keep a usable refresh hint at 60/80 columns.
	refreshHint := "[Ctrl+R/F5] Refresh"
	refreshSep := "  "
	if keyWidth < 40 {
		refreshHint = "[F5] Refresh"
	}
	if keyWidth < 28 {
		refreshHint, refreshSep = "[F5]", " "
	}
	keys := lipgloss.NewStyle().Foreground(ColorComment).Render(
		renderDialogFooter(keyWidth, "  ", []string{"[↑↓] Select", enterHint, "[Tab] Local"}, 0) + "\n" +
			strings.TrimSpace(renderDialogFooter(keyWidth, refreshSep, []string{refreshHint, "[Esc] Cancel", "[ ] scroll preview"}, 2)))
	// The results scroll inside the pane around the cursor; the header, the
	// query box and the key hints stay put.
	sections := dialogSections{head: []string{left.String()}, foot: []string{"", keys}}
	if resultRows != nil {
		sections.body, sections.focus = resultRows, gs.cursor
	} else {
		sections.foot = []string{keys}
	}
	leftContent := fitDialogRows(keyWidth, previewHeight+6, sections)

	var right strings.Builder
	if sel := gs.Selected(); sel != nil {
		right.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("📄 "+firstNonEmpty(sel.Title, "(untitled)")) + "\n")
		meta := sel.Harness
		if sel.Profile != "" {
			meta += "/" + sel.Profile
		}
		meta += "  " + shortNative(sel.SessionID)
		if sel.DeckID != "" {
			meta += "  deck:" + sel.DeckID
		}
		right.WriteString(lipgloss.NewStyle().Foreground(ColorComment).Render(meta) + "\n")
		if sel.CWD != "" {
			right.WriteString(lipgloss.NewStyle().Foreground(ColorComment).Render("📁 "+clipLeft(sel.CWD, rightWidth-5)) + "\n")
		}
		// matrix/hardened: the index spans profiles; these labels describe
		// ownership in the current list, not a possibly stale recall deck id.
		management := "未管理（当前列表）"
		if sel.InAgentDeck {
			management = "已管理（当前列表）"
		}
		for _, line := range gs.wrapText(management, rightWidth-2) {
			right.WriteString(lipgloss.NewStyle().Foreground(ColorCyan).Render(line) + "\n")
		}
		if sel.InAgentDeck {
			for _, line := range gs.wrapText("管理会话："+sel.InstanceID, rightWidth-2) {
				right.WriteString(lipgloss.NewStyle().Foreground(ColorComment).Render(line) + "\n")
			}
		}
		right.WriteString("\n")
		lines := gs.previewLines(sel, rightWidth-2)
		// matrix/hardened: reserve the actual header height, including the
		// management label, so the preview never pushes out the footer.
		visible := max(previewHeight+6-strings.Count(right.String(), "\n")-2, 1)
		start := min(gs.previewScroll, max(len(lines)-visible, 0))
		gs.previewScroll = start
		end := min(start+visible, len(lines))
		for _, l := range lines[start:end] {
			right.WriteString(l + "\n")
		}
		if len(lines) > visible {
			right.WriteString("\n" + lipgloss.NewStyle().Foreground(ColorComment).Render(fmt.Sprintf("─── %d/%d lines ───", start+1, len(lines))))
		}
	} else {
		right.WriteString(lipgloss.NewStyle().Foreground(ColorComment).Italic(true).Render("Select a result to preview"))
	}

	leftStyle := lipgloss.NewStyle().Width(leftWidth).Height(previewHeight+6).
		BorderStyle(lipgloss.RoundedBorder()).BorderForeground(ColorAccent).Padding(0, 1)
	rightStyle := lipgloss.NewStyle().Width(rightWidth).Height(previewHeight+6).
		BorderStyle(lipgloss.RoundedBorder()).BorderForeground(ColorCyan).Padding(0, 1)
	combined := lipgloss.JoinHorizontal(lipgloss.Top, leftStyle.Render(leftContent), rightStyle.Render(right.String()))
	return centerInScreen(combined, gs.width, gs.height)
}

// headerLine names the overlay and the index size.
func (gs *GlobalSearch) headerLine() string {
	// matrix/hardened: the index's total count is not this scoped search's size.
	if gs.managed != nil {
		return "🔍 Recall · 已管理"
	}
	if !gs.hasStat {
		return "🔍 Recall"
	}
	return fmt.Sprintf("🔍 Recall (%d sessions, %d messages)", gs.status.Sessions, gs.status.Messages)
}

// staleLine is the staleness indicator: what the bounded sweep left
// behind, or when the index was last swept.
func (gs *GlobalSearch) staleLine() string {
	switch {
	case gs.sweeping && gs.refresh == "":
		return "refreshing the index (bounded: 150 ms / 32 MB per pass)..."
	case gs.refresh != "":
		return gs.refresh
	case gs.hasStat && gs.status.LastSweep > 0:
		return "index swept " + humanizeSince(time.Since(time.Unix(gs.status.LastSweep, 0)))
	}
	return "index as on disk"
}

// resultMeta is the line under the selected row: date, harness, hits.
func (gs *GlobalSearch) resultMeta(r *GlobalSearchResult) string {
	parts := []string{}
	if !r.EndedAt.IsZero() {
		parts = append(parts, humanizeSince(time.Since(r.EndedAt)))
	}
	parts = append(parts, r.Harness)
	if r.CardHit {
		parts = append(parts, "title/hint")
	}
	if r.BodyHits > 0 {
		parts = append(parts, fmt.Sprintf("%d in body", r.BodyHits))
	}
	if r.Missing {
		parts = append(parts, "file missing")
	}
	return strings.Join(parts, " • ")
}

// previewLines renders the selected session's preview: the snippet first,
// then the card and the first turns once `recall show` answered.
func (gs *GlobalSearch) previewLines(sel *GlobalSearchResult, width int) []string {
	var lines []string
	if sel.Snippet != "" {
		for _, l := range gs.wrapText(sel.Snippet, width) {
			lines = append(lines, gs.highlightMatches(l, gs.query))
		}
		lines = append(lines, "")
	}
	d, ok := gs.preview[sel.SessID]
	if !ok {
		if why, failed := gs.previewE[sel.SessID]; failed {
			return append(lines, lipgloss.NewStyle().Foreground(ColorRed).Render("preview failed: "+why))
		}
		return append(lines, lipgloss.NewStyle().Foreground(ColorComment).Italic(true).Render("loading turns..."))
	}
	s := d.Session
	lines = append(lines, lipgloss.NewStyle().Foreground(ColorComment).Render(
		fmt.Sprintf("%d turns • %d tool calls • %d errors • model %s", s.Turns, s.ToolCalls, s.Errors, firstNonEmpty(s.Model, "-"))))
	if s.Hints != "" || s.Tags != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorComment).Render("hints: "+firstNonEmpty(s.Hints, "-")+"  tags: "+firstNonEmpty(s.Tags, "-")))
	}
	lines = append(lines, "")
	for _, m := range d.Messages {
		prefix, color := "🤖 ", ColorCyan
		if m.Role == "user" {
			prefix, color = "👤 ", ColorGreen
		}
		text := strings.Join(strings.Fields(m.Text), " ")
		for i, w := range gs.wrapText(text, width-len(prefix)) {
			if i == 0 {
				lines = append(lines, lipgloss.NewStyle().Foreground(color).Render(prefix)+gs.highlightMatches(w, gs.query))
			} else {
				lines = append(lines, "   "+gs.highlightMatches(w, gs.query))
			}
		}
	}
	if d.Truncated > 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorComment).Render(fmt.Sprintf("… %d more; agent-deck recall show %s", d.Truncated, query.Ref(sel.SessID))))
	}
	return lines
}

// wrapText wraps text at word boundaries to fit within maxWidth.
func (gs *GlobalSearch) wrapText(text string, maxWidth int) []string {
	if maxWidth < 10 {
		maxWidth = 10
	}
	if len(text) <= maxWidth {
		return []string{text}
	}
	var lines []string
	var cur strings.Builder
	for _, word := range strings.Fields(text) {
		switch {
		case cur.Len() == 0:
			cur.WriteString(word)
		case cur.Len()+1+len(word) <= maxWidth:
			cur.WriteString(" ")
			cur.WriteString(word)
		default:
			lines = append(lines, cur.String())
			cur.Reset()
			cur.WriteString(word)
		}
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

// highlightMatches highlights every query term in text (case-insensitive).
func (gs *GlobalSearch) highlightMatches(text, q string) string {
	if q == "" || text == "" {
		return text
	}
	out := text
	for _, term := range strings.Fields(q) {
		term = strings.Trim(term, `"*`)
		if term == "" || term == "AND" || term == "OR" || term == "NOT" {
			continue
		}
		out = highlightTerm(out, term)
	}
	return out
}

func highlightTerm(text, term string) string {
	lower := strings.ToLower(text)
	needle := strings.ToLower(term)
	var b strings.Builder
	last := 0
	for {
		i := strings.Index(lower[last:], needle)
		if i < 0 {
			b.WriteString(text[last:])
			return b.String()
		}
		at := last + i
		b.WriteString(text[last:at])
		b.WriteString(highlightStyle.Render(text[at : at+len(term)]))
		last = at + len(term)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clipCells(s string, n int) string {
	rs := []rune(strings.Join(strings.Fields(s), " "))
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n-1]) + "…"
}

func clipLeft(s string, n int) string {
	if n < 8 || len(s) <= n {
		return s
	}
	return "..." + s[len(s)-(n-3):]
}

// shortNative is the conversation id as the preview shows it.
func shortNative(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
