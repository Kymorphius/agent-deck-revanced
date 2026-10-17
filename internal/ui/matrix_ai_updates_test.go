package ui

// matrix/hardened: all update actions are injected; these tests never inspect,
// install, or restart the real Codex daemon or contact a release service.
import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type fakeAIUpdatesManager struct {
	status aiupdate.Status
	err    error
	calls  []string
	limits map[string]time.Duration
}

func (f *fakeAIUpdatesManager) result(ctx context.Context, action string) (aiupdate.Status, error) {
	f.calls = append(f.calls, action)
	if deadline, ok := ctx.Deadline(); ok {
		if f.limits == nil {
			f.limits = make(map[string]time.Duration)
		}
		f.limits[action] = time.Until(deadline)
	}
	return f.status, f.err
}

func (f *fakeAIUpdatesManager) Inspect(ctx context.Context) (aiupdate.Status, error) {
	return f.result(ctx, "inspect")
}
func (f *fakeAIUpdatesManager) Check(ctx context.Context) (aiupdate.Status, error) {
	return f.result(ctx, "check")
}
func (f *fakeAIUpdatesManager) Install(ctx context.Context) (aiupdate.Status, error) {
	return f.result(ctx, "install")
}
func (f *fakeAIUpdatesManager) Restart(ctx context.Context) (aiupdate.Status, error) {
	return f.result(ctx, "restart")
}

func aiUpdatesKey(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func finishAIUpdates(t *testing.T, p *AIUpdatesPanel, cmd tea.Cmd) aiUpdatesResult {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected an explicit asynchronous operation")
	}
	result, ok := cmd().(aiUpdatesResult)
	if !ok {
		t.Fatal("command did not return an AI update result")
	}
	p.accept(result)
	return result
}

func readyAIUpdates(t *testing.T) (*AIUpdatesPanel, *fakeAIUpdatesManager) {
	t.Helper()
	f := &fakeAIUpdatesManager{status: aiupdate.Status{
		CLIVersion: "0.163.0", InstalledVersion: "0.163.0", RunningVersion: "0.163.0",
		DaemonStatus: "running", Home: "/test/codex", NativeAutoUpdate: true,
	}}
	p := NewAIUpdatesPanel()
	p.SetSize(100, 30)
	finishAIUpdates(t, p, p.Open(f, ""))
	return p, f
}

func TestMatrixAIUpdatesExplicitOperationsAndSeparateRestart(t *testing.T) {
	p := NewAIUpdatesPanel()
	if p.IsVisible() || p.View() != "" || p.busy != "" {
		t.Fatal("constructing the panel must be inert")
	}
	p, fake := readyAIUpdates(t)
	_, cmd := p.Update(aiUpdatesKey("r"))
	finishAIUpdates(t, p, cmd)
	if got := strings.Join(fake.calls, ","); got != "inspect,inspect" {
		t.Fatalf("opening and refreshing performed nonlocal work: %s", got)
	}
	fake.status.LatestVersion = "0.164.0"
	_, cmd = p.Update(aiUpdatesKey("c"))
	finishAIUpdates(t, p, cmd)
	fake.status.InstalledVersion = "0.164.0"
	fake.status.Pending, fake.status.Managed, fake.status.NativeAutoUpdate = true, true, false
	_, cmd = p.Update(aiUpdatesKey("i"))
	finishAIUpdates(t, p, cmd)
	if got := strings.Join(fake.calls, ","); got != "inspect,inspect,check,install" {
		t.Fatalf("installation invoked an implicit restart or check: %s", got)
	}
	if !p.status.Pending || p.status.RunningVersion != "0.163.0" || p.status.InstalledVersion != "0.164.0" {
		t.Fatalf("selected package and serving process were conflated: %+v", p.status)
	}
	frame := strings.Join(strings.Fields(ansi.Strip(p.View())), " ")
	for _, required := range []string{"waiting to take effect", "disables native automatic updates", "may reconnect", "ALL tasks and connections"} {
		if !strings.Contains(frame, required) {
			t.Fatalf("missing update lifecycle explanation %q:\n%s", required, frame)
		}
	}
	for action, limit := range map[string]time.Duration{"inspect": 10 * time.Second, "check": 30 * time.Second, "install": 15 * time.Minute} {
		if got := fake.limits[action]; got <= limit-time.Second || got > limit {
			t.Fatalf("%s command deadline %s, want about %s", action, got, limit)
		}
	}
}

func TestMatrixAIUpdatesRestartNeedsSeparateExplicitConfirmation(t *testing.T) {
	p, fake := readyAIUpdates(t)
	_, cmd := p.Update(aiUpdatesKey("R"))
	if cmd != nil || !p.confirmRestart {
		t.Fatal("R must show a confirmation before restarting")
	}
	frame := strings.Join(strings.Fields(ansi.Strip(p.View())), " ")
	for _, required := range []string{"ALL running tasks", "ALL clients", "outside Agent Deck", "Codex home:", "ready to interrupt"} {
		if !strings.Contains(frame, required) {
			t.Fatalf("restart warning omits %q:\n%s", required, frame)
		}
	}
	_, cmd = p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || len(fake.calls) != 1 {
		t.Fatal("Enter must not confirm a shared-daemon interruption")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if p.confirmRestart || !p.IsVisible() {
		t.Fatal("Esc must cancel the confirmation and retain the panel")
	}
	p.Update(aiUpdatesKey("R"))
	_, cmd = p.Update(aiUpdatesKey("y"))
	finishAIUpdates(t, p, cmd)
	if got := strings.Join(fake.calls, ","); got != "inspect,restart" {
		t.Fatalf("explicit confirmation did not run exactly one restart: %s", got)
	}
}

func TestMatrixAIUpdatesBusyHiddenCompletionAndStaleResults(t *testing.T) {
	p, fake := readyAIUpdates(t)
	_, installation := p.Update(aiUpdatesKey("i"))
	for _, key := range []string{"i", "r", "c", "R"} {
		_, duplicate := p.Update(aiUpdatesKey(key))
		if duplicate != nil || p.confirmRestart {
			t.Fatalf("busy panel accepted overlapping %q", key)
		}
	}
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if p.IsVisible() || p.busy != "install" {
		t.Fatal("closing must leave the operation owned by the same panel")
	}
	replacement := &fakeAIUpdatesManager{}
	if p.Open(replacement, "") != nil || p.manager != fake {
		t.Fatal("reopening replaced an in-flight update or scheduled a new one")
	}
	p.Hide()
	fake.status.Pending, fake.status.InstalledVersion = true, "0.164.0"
	result := installation().(aiUpdatesResult)
	h := &Home{aiUpdatesPanel: p}
	if cmd, handled := h.handleAIUpdatesMsg(result); !handled || cmd != nil {
		t.Fatal("Home did not consume hidden completion")
	}
	if p.IsVisible() || p.busy != "" || !p.status.Pending || p.notice == "" {
		t.Fatalf("hidden completion reopened or lost its result: %+v", p)
	}
	notice := p.notice
	refresh := p.Open(fake, "")
	if p.notice != notice || !p.status.Pending {
		t.Fatal("reopening discarded installation completion before local readback")
	}
	p.accept(result) // late duplicate of the previous generation
	if p.busy != "inspect" {
		t.Fatal("old installation result cleared a newer read operation")
	}
	finishAIUpdates(t, p, refresh)
	p.accept(aiUpdatesResult{panel: NewAIUpdatesPanel(), generation: p.generation, status: aiupdate.Status{Home: "wrong"}})
	if p.status.Home != "/test/codex" {
		t.Fatal("result belonging to a different panel replaced current state")
	}
}

func TestMatrixAIUpdatesViewportAndLongErrors(t *testing.T) {
	p, fake := readyAIUpdates(t)
	fake.err = errors.New(strings.Repeat("下载失败 invalid package ", 40) + "ERROR-END\x1b[2J")
	_, cmd := p.Update(aiUpdatesKey("c"))
	finishAIUpdates(t, p, cmd)
	for _, size := range [][2]int{{100, 30}, {80, 20}, {30, 12}, {15, 6}, {2, 2}, {1, 1}} {
		p.SetSize(size[0], size[1])
		p.scroll = 0
		frame := p.View()
		if lipgloss.Width(frame) > size[0] || lipgloss.Height(frame) > size[1] {
			t.Fatalf("panel overflowed %dx%d: got %dx%d", size[0], size[1], lipgloss.Width(frame), lipgloss.Height(frame))
		}
	}
	p.SetSize(30, 12)
	p.scroll = 100000
	frame := ansi.Strip(p.View())
	if !strings.Contains(frame, "ERROR-END") || !strings.Contains(frame, "Esc Close") || strings.Contains(frame, "\x1b[2J") {
		t.Fatalf("long error tail, safe controls or close hint missing:\n%s", frame)
	}
	p.Update(aiUpdatesKey("R"))
	for range 12 {
		p.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if p.scroll == 0 {
		t.Fatal("restart warning cannot scroll in a small terminal")
	}
}

func TestMatrixAIUpdatesSettingsEntryAndHomeModalRouting(t *testing.T) {
	setXDGTestHome(t)
	if int(SettingPrivacy) != 39 || int(SettingAIUpdates) != 40 || settingsCount != 41 {
		t.Fatal("AI update row renumbered existing settings")
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeySpace}} {
		settings := NewSettingsPanel()
		settings.SetSize(80, 20)
		settings.Show()
		settings.cursor = int(SettingAIUpdates)
		if frame := ansi.Strip(settings.View()); !strings.Contains(frame, "Manage AI updates") {
			t.Fatalf("short settings viewport does not reveal AI updates row:\n%s", frame)
		}
		_, cmd, changed := settings.Update(key)
		if cmd != nil || changed || settings.IsVisible() || !settings.ConsumeAIUpdatesRequest() || settings.ConsumeAIUpdatesRequest() {
			t.Fatal("settings action did not consume exactly once without saving configuration")
		}
	}
	h := newMatrixLocalSearchMatchingHome(t, 80, []*session.Instance{{ID: "one", Title: "one", Tool: "codex", GroupPath: "tools"}, {ID: "two", Title: "two", Tool: "codex", GroupPath: "tools"}})
	h.aiUpdatesPanel, _ = readyAIUpdates(t)
	oldCursor := h.cursor
	if !h.hasModalVisible() {
		t.Fatal("AI updates did not register as a modal")
	}
	if _, cmd := h.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown}); cmd != nil || h.cursor != oldCursor || h.aiUpdatesPanel.scroll != 1 {
		t.Fatal("modal wheel event escaped to the underlying list")
	}
	h.Update(aiUpdatesKey("j"))
	if h.cursor != oldCursor || h.aiUpdatesPanel.scroll != 2 {
		t.Fatal("modal key event escaped to the underlying list")
	}
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.aiUpdatesPanel.IsVisible() {
		t.Fatal("Esc did not close through the actual Home route")
	}
}

func TestMatrixAIUpdatesConfigurationMatchesCodexHomeScope(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	cfg := &session.UserConfig{Codex: session.CodexSettings{Command: "codex-local", ConfigDir: "/test/global"}, Profiles: map[string]session.ProfileSettings{
		"work": {Codex: session.ProfileCodexSettings{ConfigDir: "/test/profile"}},
	}}
	binary, home, err := aiUpdatesConfiguration(cfg, "work")
	if err != nil || binary != "codex-local" || home != "/test/profile" {
		t.Fatalf("profile home resolution: %q %q %v", binary, home, err)
	}
	t.Setenv("CODEX_HOME", "/test/ambient")
	_, home, err = aiUpdatesConfiguration(cfg, "work")
	if err != nil || home != "/test/ambient" {
		t.Fatalf("ambient home must retain native precedence: %q %v", home, err)
	}
	for _, command := range []string{"codex --foo", "CODEX_HOME=/else codex", "codex; reboot", "env codex", "'codex'"} {
		cfg.Codex.Command = command
		if _, _, err := aiUpdatesConfiguration(cfg, "work"); err == nil {
			t.Fatalf("accepted ambiguous shell command %q", command)
		}
	}
	cfg.Codex.Command = filepath.Join(t.TempDir(), "Codex CLI")
	if binary, _, err := aiUpdatesConfiguration(cfg, "work"); err != nil || binary != cfg.Codex.Command {
		t.Fatalf("literal absolute executable path with spaces rejected: %q %v", binary, err)
	}
}
