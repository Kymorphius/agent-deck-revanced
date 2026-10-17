package ui

// matrix/hardened: Home-side glue for the alt+w web service panel and the
// header's "web" mark. Kept out of home.go so upstream merges only meet
// one-line hooks there.

import (
	"context"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

const webServiceWatchInterval = 30 * time.Second

// webServiceWatch keeps a background reading of this machine's web service,
// so the header never runs launchctl/systemctl or an HTTP probe on a frame.
type webServiceWatch struct {
	mu       sync.RWMutex
	status   session.WebServiceStatus
	have     bool
	settings session.WebServiceSettings
	kick     chan struct{}
	stop     chan struct{}
	once     sync.Once
}

func newWebServiceWatch(settings session.WebServiceSettings) *webServiceWatch {
	return &webServiceWatch{settings: settings, kick: make(chan struct{}, 1), stop: make(chan struct{})}
}

func (w *webServiceWatch) start() {
	go func() {
		ticker := time.NewTicker(webServiceWatchInterval)
		defer ticker.Stop()
		for {
			w.collect()
			select {
			case <-ticker.C:
			case <-w.kick:
			case <-w.stop:
				return
			}
		}
	}()
}

func (w *webServiceWatch) collect() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := session.CheckWebService(ctx, w.settings)
	w.mu.Lock()
	w.status, w.have = st, true
	w.mu.Unlock()
}

// set records a reading taken elsewhere (the panel's own command).
func (w *webServiceWatch) set(st session.WebServiceStatus) {
	w.mu.Lock()
	w.status, w.have = st, true
	w.mu.Unlock()
}

// refresh asks for a new reading now; never blocks.
func (w *webServiceWatch) refresh() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *webServiceWatch) get() (session.WebServiceStatus, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status, w.have
}

func (w *webServiceWatch) close() { w.once.Do(func() { close(w.stop) }) }

// startWebServiceWatch begins watching this machine's service when
// [webservice] is configured; otherwise the header shows nothing.
func (h *Home) startWebServiceWatch() {
	cfg, _ := session.LoadUserConfig()
	if cfg == nil || !cfg.WebService.Enabled() || h.webServiceWatch != nil {
		return
	}
	h.webServiceWatch = newWebServiceWatch(cfg.WebService)
	h.webServiceWatch.start()
}

func (h *Home) stopWebServiceWatch() {
	if h.webServiceWatch != nil {
		h.webServiceWatch.close()
	}
}

// webServiceHeaderSegment is the header's "web ●" mark for this machine's
// service, "" when none is configured.
func (h *Home) webServiceHeaderSegment() string {
	if h.webServiceWatch == nil {
		return ""
	}
	dim := lipgloss.NewStyle().Foreground(ColorComment)
	st, ok := h.webServiceWatch.get()
	if !ok {
		return dim.Render("web …")
	}
	mark, style := webServiceMark(st)
	word := ""
	switch {
	case st.ManagerError != "":
		word = " unknown"
	case !st.Running:
		word = " stopped"
	case st.Probed && !st.Healthy:
		word = " no reply"
	}
	return dim.Render("web ") + style.Render(mark+word)
}

// openWebServicePanel shows the web service on the remote under the cursor,
// or on this machine otherwise.
func (h *Home) openWebServicePanel() tea.Cmd {
	if h.webServicePanel == nil {
		h.webServicePanel = NewWebServicePanel()
	}
	h.webServicePanel.SetSize(h.width, h.height)

	target := ""
	if h.cursor >= 0 && h.cursor < len(h.flatItems) {
		if item := h.flatItems[h.cursor]; item.Type == session.ItemTypeRemoteGroup || item.Type == session.ItemTypeRemoteSession {
			target = item.RemoteName
		}
	}
	return h.webServicePanel.Open(target, h.adoptRunner(target))
}

// handleWebServiceMsg consumes the panel's async results. ok is false for
// any other message.
func (h *Home) handleWebServiceMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case webServiceLoadedMsg:
		if h.webServicePanel != nil {
			h.webServicePanel.handleLoaded(m)
			if h.webServicePanel.target == "" && m.err == nil && h.webServiceWatch != nil {
				h.webServiceWatch.set(m.status)
			}
		}
		return nil, true
	case webServiceDoneMsg:
		if h.webServicePanel != nil {
			h.webServicePanel.handleDone(m)
		}
		if m.target == "" && h.webServiceWatch != nil {
			if !m.status.CheckedAt.IsZero() {
				h.webServiceWatch.set(m.status)
			} else {
				h.webServiceWatch.refresh()
			}
		}
		if h.webServicePanel != nil && h.webServicePanel.IsVisible() && m.status.CheckedAt.IsZero() && m.target == h.webServicePanel.target {
			return h.webServicePanel.load(), true
		}
		return nil, true
	}
	return nil, false
}
