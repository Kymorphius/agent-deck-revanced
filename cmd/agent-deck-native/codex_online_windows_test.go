//go:build windows

package main

// matrix/hardened: explicitly opt-in online acceptance owns one private Codex
// process, StateDB, pipe and project. Credentials are supplied in a separately
// protected QA CODEX_HOME by the operator; this test never opens default auth.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

const (
	// A fresh QA home has no model catalog or startup caches. Let Codex finish
	// configuration before sending any prompt; ordinary UI/IPC checks stay 15s.
	nativeCodexStartupTimeout = 90 * time.Second
	nativeCodexModelTimeout   = 120 * time.Second
)

func nativeCodexQAHome(t *testing.T) string {
	t.Helper()
	path := nativeCodexExistingQAHome(t)
	entries, err := os.ReadDir(filepath.Join(path, "sessions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cannot inspect QA rollout directory")
	}
	if len(entries) != 0 {
		t.Fatal("QA CODEX_HOME must have no pre-existing sessions; use a fresh config/auth-only clone")
	}
	return path
}

func nativeCodexExistingQAHome(t *testing.T) string {
	t.Helper()
	path := os.Getenv("CODEX_HOME")
	const prefix = "agent-deck-native-codex-online-"
	if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Base(path), prefix) {
		t.Fatal("CODEX_HOME must explicitly name a protected QA clone: agent-deck-native-codex-online-<UUID>")
	}
	if _, err := uuid.Parse(strings.TrimPrefix(filepath.Base(path), prefix)); err != nil {
		t.Fatal("CODEX_HOME QA clone must have a UUID suffix")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !strings.EqualFold(filepath.Clean(path), canonical) {
		t.Fatal("CODEX_HOME QA clone must be an existing canonical directory without links")
	}
	if info, err := os.Lstat(path); err != nil || !info.IsDir() {
		t.Fatal("CODEX_HOME QA clone directory is unavailable")
	}
	// Only inspect metadata. No configuration, authentication or token values
	// enter test output. The operator owns creation and removal of this clone.
	for _, name := range []string{"config.toml", "auth.json"} {
		info, err := os.Lstat(filepath.Join(path, name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("QA clone requires a regular %s supplied by the operator", name)
		}
	}
	return path
}

func nativeCodexStartUI(t *testing.T, cli string, config options, cwd string) *nativeFleetUI {
	t.Helper()
	// matrix/hardened: the isolated Windows runner may add a long TEMP prefix.
	// Keep the owned project and expanded picker metadata observable together.
	cols := max(160, ansi.StringWidth(cwd)+80)
	args := []string{"ui", "--tool", "codex", "--store", "statedb", "--pipe", config.Pipe, "--state-dir", config.StateDir, "--cwd", cwd}
	// matrix/hardened: online acceptance may explicitly select the installed
	// native CLI; do not silently fall back to an older npm shim/version.
	if config.Executable != "" {
		args = append(args, "--codex-executable", config.Executable)
	}
	process, err := conpty.Start(conpty.Options{
		Executable: cli,
		Args:       args,
		Env:        os.Environ(), Dir: cwd, Cols: cols, Rows: 45,
	})
	if err != nil {
		t.Fatalf("start shared-store native Codex UI: %v", err)
	}
	frame, err := terminalhost.New(process, cols, 45)
	if err != nil {
		_ = process.Close()
		t.Fatalf("decode native Codex UI: %v", err)
	}
	t.Cleanup(func() { nativeClose(t, "online Codex UI console", frame.Close) })
	return &nativeFleetUI{viewer: &nativeViewer{process: process}, frame: frame}
}

func nativeCodexFrame(t *testing.T, ui *nativeFleetUI, label string, wanted ...string) {
	t.Helper()
	nativeAwait(t, label, func() (bool, string) {
		screen := ui.screen()
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		for _, value := range wanted {
			if !strings.Contains(screen, value) {
				return false, fmt.Sprintf("missing expected fixture text %q; screen withheld", value)
			}
		}
		return true, "expected fixture text present"
	})
}

func nativeCodexBlockedScreen(screen string) string {
	lower := strings.ToLower(screen)
	for _, phrase := range []string{
		"sign in with chatgpt", "provide your own api key", "paste your api key",
		"authentication failed", "unauthorized", "invalid api key", "not logged in",
		"failed to refresh token", "quota exceeded", "usage limit reached",
	} {
		if strings.Contains(lower, phrase) {
			return "Codex login/provider setup or authentication failed (" + phrase + ")"
		}
	}
	if strings.Contains(lower, "windows sandbox") &&
		(strings.Contains(lower, "set up") || strings.Contains(lower, "setup") || strings.Contains(lower, "enable") || strings.Contains(lower, "elevat")) {
		return "Codex requires Windows sandbox setup; no setup choice or permission bypass was submitted"
	}
	if strings.Contains(lower, "create a sandbox") || strings.Contains(lower, "set up the sandbox") {
		return "Codex startup would create a sandbox; no setup choice was submitted"
	}
	return ""
}

func nativeCodexComposer(screen string) bool {
	lower := strings.ToLower(screen)
	if !strings.Contains(lower, "openai codex") || !strings.Contains(screen, "›") || nativeCodexBlockedScreen(screen) != "" {
		return false
	}
	modelPresent, modelLoading := nativeCodexModelHeader(screen)
	if !modelPresent || modelLoading {
		return false
	}
	// matrix/hardened: selection menus also use ›. Require the real composer
	// placeholder or its footer, and exclude every onboarding confirmation.
	for _, blocked := range []string{
		"press enter to continue", "do you trust the contents", "yes, continue",
		"resume a previous session", "fork a previous session", "waiting for startup",
		"stream disconnected", "error sending request", "failed to connect", "retrying", "reconnecting",
	} {
		if strings.Contains(lower, blocked) {
			return false
		}
	}
	composerRow, composerPlaceholder, composerBlank := false, false, false
	for _, line := range strings.Split(lower, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "›") {
			composerRow = true
			composerPlaceholder = nativeCodexComposerPlaceholder(line)
			composerBlank = strings.TrimSpace(strings.TrimPrefix(line, "›")) == ""
		}
	}
	// matrix/hardened: 0.162.1 replaces model: with its precise contextual
	// model/reasoning footer. A real prompt row remains required; neither
	// this footer alone nor a generic Codex brand grants permission to type.
	return composerRow && (composerPlaceholder ||
		composerBlank && strings.Contains(lower, "for shortcuts") && strings.Contains(lower, "context left"))
}

func nativeCodexComposerPlaceholder(line string) bool {
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "› ask codex to do anything" || line == "› ask a follow-up question"
}

func nativeCodexResolvedFooter(screen string) bool {
	composerSeen := false
	for _, line := range strings.Split(strings.ToLower(screen), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "›") {
			composerSeen = nativeCodexComposerPlaceholder(line)
			continue
		}
		if !composerSeen {
			continue
		}
		model, directory, ok := strings.Cut(line, " · ")
		fields := strings.Fields(model)
		if !ok || strings.TrimSpace(directory) == "" || len(fields) != 2 || fields[0] != "gpt-6.1-sol" {
			continue
		}
		// These are the explicit runtime reasoning labels, including the
		// observed default. No arbitrary model/provider/summary row is valid.
		switch fields[1] {
		case "default", "minimal", "low", "medium", "high", "xhigh", "ultra":
			return true
		}
	}
	return false
}

func nativeCodexModelHeader(screen string) (present, loading bool) {
	for _, line := range strings.Split(strings.ToLower(screen), "\n") {
		index := strings.Index(line, "model:")
		if index < 0 {
			continue
		}
		value := strings.TrimSpace(line[index+len("model:"):])
		if strings.Trim(value, " │") != "" {
			present = true
			loading = loading || strings.Contains(value, "loading")
		}
	}
	// matrix/hardened: the newer rich header deliberately omits model:.
	// Accept only the independently observed native GPT-6.1-Sol footer.
	present = present || nativeCodexResolvedFooter(screen)
	return present, loading
}

// matrix/hardened: these fixtures reproduce the observed old/new native UI
// formats and reject nearby lookalikes before authorizing a single submission.
func TestWindowsNativeCodexComposerFormats(t *testing.T) {
	newUI := "  >_ OpenAI Codex (v0.162.1)\n     ~\\AppData\\Local\\Temp\\owned-project\n  /model - choose what model and reasoning effort to use\n› Ask Codex to do anything\n  GPT-6.1-Sol default · ~\\AppData\\Local\\Temp\\owned-project\n? for shortcuts   ⚠ 1 warning · f2 to view"
	oldUI := "OpenAI Codex (v0.154.0)\n│ model: gpt-6.1-sol high │\n› Ask Codex to do anything\n? for shortcuts   100% context left"
	for _, test := range []struct {
		name, screen string
		ready        bool
	}{
		{name: "observed 01621 footer", screen: newUI, ready: true},
		{name: "legacy native model header", screen: oldUI, ready: true},
		{name: "new followup", screen: strings.ReplaceAll(newUI, "Ask Codex to do anything", "Ask a follow-up question"), ready: true},
		{name: "high reasoning", screen: strings.ReplaceAll(newUI, "Sol default", "Sol high"), ready: true},
		{name: "unknown reasoning", screen: strings.ReplaceAll(newUI, "Sol default", "Sol unknown")},
		{name: "different model", screen: strings.ReplaceAll(newUI, "GPT-6.1-Sol", "GPT-other")},
		{name: "model prefix lookalike", screen: strings.ReplaceAll(newUI, "GPT-6.1-Sol", "GPT-6.1-Solish")},
		{name: "new model loading", screen: strings.ReplaceAll(newUI, "GPT-6.1-Sol", "loading")},
		{name: "legacy model loading", screen: strings.ReplaceAll(oldUI, "gpt-6.1-sol high", "loading")},
		{name: "missing footer cwd", screen: strings.ReplaceAll(newUI, "default · ~\\AppData\\Local\\Temp\\owned-project", "default · ")},
		{name: "footer before composer", screen: "OpenAI Codex\nGPT-6.1-Sol default · owned-project\n› Ask Codex to do anything\n? for shortcuts"},
		{name: "no real composer row", screen: strings.ReplaceAll(newUI, "› Ask", "Ask")},
		{name: "loading arrow with complete footer", screen: strings.ReplaceAll(newUI, "› Ask Codex to do anything", "› Loading")},
		{name: "selection arrow with complete footer", screen: strings.ReplaceAll(newUI, "› Ask Codex to do anything", "› Selected menu item")},
		{name: "loading line includes placeholder", screen: strings.ReplaceAll(newUI, "› Ask Codex to do anything", "› Loading: Ask Codex to do anything")},
		{name: "menu line includes placeholder", screen: strings.ReplaceAll(newUI, "› Ask Codex to do anything", "› Select: Ask a follow-up question")},
		{name: "blank new row with complete footer", screen: strings.ReplaceAll(newUI, "› Ask Codex to do anything", "›")},
		{name: "blank legacy composer", screen: strings.ReplaceAll(oldUI, "› Ask Codex to do anything", "›"), ready: true},
		{name: "loading legacy arrow", screen: strings.ReplaceAll(oldUI, "› Ask Codex to do anything", "› Loading")},
		{name: "generic brand", screen: strings.ReplaceAll(newUI, "OpenAI Codex", "Codex")},
		{name: "native brand alone", screen: "OpenAI Codex (v0.162.1)\n› Ask Codex to do anything"},
		{name: "history picker", screen: newUI + "\nResume a previous session"},
		{name: "fork picker", screen: newUI + "\nFork a previous session"},
		{name: "trust onboarding", screen: newUI + "\nDo you trust the contents of this directory?\n› Yes, continue"},
		{name: "other startup choice", screen: newUI + "\nPress Enter to continue"},
		{name: "pending startup", screen: newUI + "\nWaiting for startup"},
		{name: "authentication", screen: newUI + "\nSign in with ChatGPT"},
		{name: "provider transport", screen: newUI + "\nError sending request"},
		{name: "provider retry", screen: newUI + "\nReconnecting"},
		{name: "sandbox onboarding", screen: newUI + "\nSet up the Windows sandbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if ready := nativeCodexComposer(test.screen); ready != test.ready {
				t.Fatalf("native composer format ready=%v wanted=%v; screen contents withheld", ready, test.ready)
			}
		})
	}
}

func nativeCodexScreenCategory(screen string) string {
	if reason := nativeCodexBlockedScreen(screen); reason != "" {
		return reason
	}
	lower := strings.ToLower(screen)
	_, modelLoading := nativeCodexModelHeader(screen)
	switch {
	case strings.Contains(lower, "resume a previous session"):
		return "history-picker"
	case strings.Contains(lower, "trust") && strings.Contains(lower, "yes, continue"):
		return "directory-trust-choice"
	case strings.Contains(lower, "update available") && strings.Contains(lower, "press"):
		return "update-choice"
	case strings.Contains(lower, "press enter to continue"):
		return "startup-choice"
	case modelLoading:
		return "startup-model-loading"
	case strings.Contains(lower, "stream disconnected") || strings.Contains(lower, "error sending request") || strings.Contains(lower, "failed to connect"):
		return "provider-transport-error"
	case strings.Contains(lower, "retrying") || strings.Contains(lower, "reconnecting"):
		return "provider-retry"
	case nativeCodexComposer(screen):
		return "composer"
	case strings.Contains(lower, "codex"):
		return "codex-screen-without-confirmed-composer"
	default:
		return "unclassified-screen"
	}
}

// matrix/hardened: online history selection is never inferred from a process,
// a generic arrow, or an old conversation still painted on the screen. Codex
// 0.162.1's picker exposes a title, Enter/resume hint, selected row and count;
// Ctrl+E expands that row to reveal its thread and saved working directory.
func nativeCodexPickerFacts(screen, project, threadID string) (base, expanded bool, facts string) {
	lower := strings.ToLower(screen)
	title := strings.Contains(lower, "resume a previous session")
	footer, single, selected, thread, directory := false, false, false, false, false
	for _, line := range strings.Split(lower, "\n") {
		line = strings.TrimSpace(line)
		footer = footer || strings.Contains(line, "enter resume")
		single = single || strings.Contains(line, "1 / 1 ·")
		selected = selected || strings.HasPrefix(line, "› ") || strings.HasPrefix(line, "⌄ ")
		thread = thread || strings.Contains(line, "session:") && strings.Contains(line, strings.ToLower(threadID))
		// The directory renderer may abbreviate the user-home prefix. The
		// UUID basename remains unique and the rollout verifies the full cwd.
		directory = directory || strings.Contains(line, "directory:") && strings.Contains(line, strings.ToLower(filepath.Base(project)))
	}
	base = title && footer && single && selected && nativeCodexBlockedScreen(screen) == "" && !nativeCodexComposer(screen)
	expanded = base && thread && directory
	facts = fmt.Sprintf("picker_title=%v enter_resume_hint=%v one_selected_record=%v selected_row=%v owned_thread_visible=%v owned_directory_visible=%v", title, footer, single, selected, thread, directory)
	return base, expanded, facts
}

func nativeCodexSelectOwnedHistory(t *testing.T, ui *nativeFleetUI, pipe string, info terminalhost.SessionInfo, home, project string, original nativeCodexRollout) {
	t.Helper()
	// matrix/hardened: expanded directory metadata truncates to console width.
	// The Windows viewer may report its legacy buffer width despite a wider
	// outer fixture. Widen only this exact QA generation before selecting any
	// history; this is a geometry operation, never a model/sandbox override.
	inner := nativeCall(t, pipe, terminalhost.Request{Op: "capture", SessionID: info.ID, RunID: info.RunID})
	outer := ui.frame.Dispatch(terminalhost.Request{Op: "capture"})
	if inner.SessionID != info.ID || inner.RunID != info.RunID || inner.PID != info.PID || outer.PID != ui.viewer.process.PID() {
		t.Fatal("history geometry does not belong to the exact owned inner/outer process")
	}
	t.Logf("owned history geometry before selection: inner=%dx%d outer=%dx%d", inner.Cols, inner.Rows, outer.Cols, outer.Rows)
	cols, rows := max(inner.Cols, ansi.StringWidth(project)+80), max(inner.Rows, outer.Rows)
	if inner.Cols < cols || inner.Rows < rows {
		resized := nativeCall(t, pipe, terminalhost.Request{Op: "resize", SessionID: info.ID, RunID: info.RunID, Cols: cols, Rows: rows})
		if resized.SessionID != info.ID || resized.RunID != info.RunID || resized.PID != info.PID || resized.Cols != cols || resized.Rows != rows {
			t.Fatal("history geometry operation changed identity or did not expose its requested size")
		}
	}
	deadline := time.Now().Add(nativeCodexStartupTimeout)
	expandSent := false
	var readySince time.Time
	lastCategory, lastFacts := "not-yet-observed", "not-yet-observed"
	for time.Now().Before(deadline) {
		screen := ui.screen()
		lastCategory = nativeCodexScreenCategory(screen)
		base, expanded, facts := nativeCodexPickerFacts(screen, project, original.threadID)
		lastFacts = facts
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		if base && !expanded && !expandSent {
			// This only expands metadata in the verified single-row picker.
			// It neither accepts a history nor changes an account setting.
			nativeWrite(t, ui.viewer, "\x05")
			expandSent = true
		}
		if expanded {
			if readySince.IsZero() {
				readySince = time.Now()
			}
			if time.Since(readySince) >= time.Second {
				rollout, err := nativeCodexFindRollout(home, project)
				if err != nil || rollout.path != original.path || rollout.threadID != original.threadID || rollout.toolCalled {
					t.Fatal("history picker no longer has exactly the independently verified owned QA thread; no selection submitted")
				}
				// Recheck the current frame immediately before the sole Enter.
				if _, stillExpanded, _ := nativeCodexPickerFacts(ui.screen(), project, original.threadID); !stillExpanded {
					t.Fatal("owned history selection changed before its single Enter; no selection submitted")
				}
				nativeWrite(t, ui.viewer, "\r")
				return
			}
		} else {
			readySince = time.Time{}
		}
		time.Sleep(50 * time.Millisecond)
	}
	debug := nativeCodexDebugStartupScreen(home, ui.screen())
	t.Fatalf("Codex history did not expose its exact owned thread/project within the startup deadline; screen_category=%s expand_sent=%v %s debug_screen=%s; no selection retried and screen contents withheld", lastCategory, expandSent, lastFacts, debug)
}

// matrix/hardened: the keyboard gate must reject lookalike startup screens,
// multiple/unfinished records and a different row before any history selection.
func TestWindowsNativeCodexHistoryPickerGate(t *testing.T) {
	project := filepath.Join(t.TempDir(), "owned-project-"+uuid.NewString())
	threadID := uuid.NewString()
	collapsed := "Resume a previous session\n› Owned harmless QA history\n1 / 1 · 100%\nenter resume   esc start new   ⌃c quit\n⌃e expand"
	expanded := strings.ReplaceAll(collapsed, "› ", "⌄ ") + "\n  │ Session:    " + threadID + "\n  │ Directory:  " + project
	for _, test := range []struct {
		name, screen   string
		base, expanded bool
	}{
		{name: "collapsed exact single row", screen: collapsed, base: true},
		{name: "expanded exact owned row", screen: expanded, base: true, expanded: true},
		{name: "composer alone", screen: "Codex\nmodel: gpt-6.1-sol\n› Ask Codex to do anything"},
		{name: "fork picker", screen: strings.ReplaceAll(expanded, "Resume a previous session", "Fork a previous session")},
		{name: "ten rows", screen: strings.ReplaceAll(expanded, "1 / 1 ·", "1 / 10 ·")},
		{name: "still loading", screen: strings.ReplaceAll(expanded, "1 / 1 ·", "1 / 1… ·")},
		{name: "no selected row", screen: strings.ReplaceAll(expanded, "⌄ ", "  ")},
		{name: "different thread", screen: strings.ReplaceAll(expanded, threadID, uuid.NewString()), base: true},
		{name: "different project", screen: strings.ReplaceAll(expanded, filepath.Base(project), "different-project"), base: true},
		{name: "authentication choice", screen: expanded + "\nSign in with ChatGPT"},
		{name: "sandbox choice", screen: expanded + "\nSet up the Windows sandbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, expanded, facts := nativeCodexPickerFacts(test.screen, project, threadID)
			if base != test.base || expanded != test.expanded {
				t.Fatalf("history gate accepted the wrong screen: base=%v expanded=%v %s", base, expanded, facts)
			}
		})
	}
}

func nativeCodexDialogFacts(screen, project string) string {
	lower := strings.ToLower(screen)
	selectedYes := false
	for _, line := range strings.Split(lower, "\n") {
		line = strings.TrimSpace(line)
		selectedYes = selectedYes || (strings.HasPrefix(line, "›") && strings.Contains(line, "yes, continue"))
	}
	modelPresent, modelLoading := nativeCodexModelHeader(screen)
	// matrix/hardened: only allowlisted label presence leaves the private QA
	// terminal; no model/provider text, account data, or raw lines are logged.
	headerLabels := make([]string, 0, 4)
	for _, label := range []string{"model:", "directory:", "/model", "/status"} {
		if strings.Contains(lower, label) {
			headerLabels = append(headerLabels, label)
		}
	}
	return fmt.Sprintf("own_project_visible=%v trust_heading=%v yes_continue=%v selected_yes=%v continue_hint=%v model_header_present=%v model_loading=%v header_labels=%v composer_arrow=%v composer_placeholder=%v shortcuts_footer=%v context_footer=%v",
		strings.Contains(lower, strings.ToLower(project)), strings.Contains(lower, "do you trust"),
		strings.Contains(lower, "yes, continue"), selectedYes, strings.Contains(lower, "to continue"), modelPresent, modelLoading, headerLabels,
		strings.Contains(screen, "›"), strings.Contains(lower, "ask codex to do anything") || strings.Contains(lower, "ask a follow-up question"), strings.Contains(lower, "for shortcuts"), strings.Contains(lower, "context left"))
}

// matrix/hardened: an operator may explicitly retain the last QA startup frame
// in its already-validated private home. Exclusive creation rejects links or an
// existing destination; raw contents never enter test output or production.
func nativeCodexDebugStartupScreen(home, screen string) string {
	if os.Getenv("AGENTDECK_NATIVE_CODEX_DEBUG_SCREEN") != "1" {
		return "disabled"
	}
	if home == "" || home != os.Getenv("CODEX_HOME") || !filepath.IsAbs(home) {
		return "refused-home-identity"
	}
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil || !strings.EqualFold(filepath.Clean(home), canonical) {
		return "refused-home-link"
	}
	if info, err := os.Lstat(home); err != nil || !info.IsDir() || len(screen) > 1<<20 {
		return "refused-home-or-size"
	}
	file, err := os.OpenFile(filepath.Join(home, "startup-screen.txt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "refused-existing-or-unwritable-file"
	}
	_, writeErr := file.WriteString(screen)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return "write-or-close-failed"
	}
	return "written-private-qa-startup-screen"
}

func nativeCodexAwaitReady(t *testing.T, ui *nativeFleetUI, home, project string) {
	t.Helper()
	deadline := time.Now().Add(nativeCodexStartupTimeout)
	trusted := false
	lastCategory := "not-yet-observed"
	lastFacts := "not-yet-observed"
	lastScreen := ""
	var composerSince time.Time
	for time.Now().Before(deadline) {
		screen := ui.screen()
		lastScreen = screen
		lastCategory = nativeCodexScreenCategory(screen)
		lastFacts = nativeCodexDialogFacts(screen, project)
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		lower := strings.ToLower(screen)
		if strings.Contains(lower, "trust") && strings.Contains(lower, "yes, continue") && strings.Contains(lower, strings.ToLower(project)) {
			composerSince = time.Time{}
			if trusted {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			// The trust prompt must visibly identify this test's owned project.
			// This changes only the explicit QA clone, never the default config.
			if strings.Contains(lower, "yes, continue") {
				nativeWrite(t, ui.viewer, "\r")
				trusted = true
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		if nativeCodexComposer(screen) {
			if composerSince.IsZero() {
				composerSince = time.Now()
			}
			// Initial placeholders can precede asynchronous trust onboarding.
			// Observe a stable real composer before sending the first draft.
			if time.Since(composerSince) >= time.Second {
				return
			}
		} else {
			composerSince = time.Time{}
		}
		time.Sleep(50 * time.Millisecond)
	}
	debug := nativeCodexDebugStartupScreen(home, lastScreen)
	t.Fatalf("Codex did not expose its stable native composer within the startup deadline; screen_category=%s trusted_owned_project=%v %s debug_screen=%s; screen contents withheld", lastCategory, trusted, lastFacts, debug)
}

func nativeCodexSubmit(t *testing.T, ui *nativeFleetUI, prompt string) {
	t.Helper()
	// matrix/hardened: verify human typing separately from bracketed paste.
	// Codex classifies an 8 ms char burst as paste; space events by 30 ms.
	for _, char := range prompt {
		nativeWrite(t, ui.viewer, string(char))
		time.Sleep(30 * time.Millisecond)
	}
	nativeCodexCommitDraft(t, ui, prompt)
}

func nativeCodexSubmitPaste(t *testing.T, ui *nativeFleetUI, pipe string, info terminalhost.SessionInfo, prompt string) {
	t.Helper()
	nativeAwait(t, "exact inner Codex and reopened viewer both enable bracketed paste", func() (bool, string) {
		inner := nativeCall(t, pipe, terminalhost.Request{Op: "capture", SessionID: info.ID, RunID: info.RunID})
		outer := ui.frame.Dispatch(terminalhost.Request{Op: "capture"})
		if inner.SessionID != info.ID || inner.RunID != info.RunID || inner.PID != info.PID || outer.PID != ui.viewer.process.PID() {
			t.Fatal("bracketed-paste mode capture disagrees with exact inner/outer native process ownership")
		}
		return inner.BracketedPaste && outer.BracketedPaste,
			fmt.Sprintf("inner_bracketed_paste=%v outer_bracketed_paste=%v", inner.BracketedPaste, outer.BracketedPaste)
	})
	// The same one-Enter acknowledgement applies after real bracketed paste.
	// Never resend a prompt after uncertainty about its submission.
	nativeWrite(t, ui.viewer, "\x1b[200~"+prompt+"\x1b[201~")
	nativeCodexCommitDraft(t, ui, prompt)
}

func nativeCodexCommitDraft(t *testing.T, ui *nativeFleetUI, prompt string) {
	t.Helper()
	nativeAwait(t, "exact harmless prompt is visible in the last native composer row", func() (bool, string) {
		screen := ui.screen()
		return nativeCodexDraftContains(screen, prompt), nativeCodexDraftFacts(screen, prompt)
	})
	time.Sleep(time.Second)
	screen := ui.screen()
	if !nativeCodexDraftContains(screen, prompt) || nativeCodexScreenCategory(screen) == "directory-trust-choice" || nativeCodexBlockedScreen(screen) != "" {
		t.Fatalf("composer changed before the single Enter; no submission sent; %s %s", nativeCodexScreenCategory(screen), nativeCodexDraftFacts(screen, prompt))
	}
	nativeWrite(t, ui.viewer, "\r")
	nativeAwait(t, "single Codex Enter clears the exact draft rather than adding a newline", func() (bool, string) {
		screen := ui.screen()
		category := nativeCodexScreenCategory(screen)
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		if category == "directory-trust-choice" || category == "startup-choice" || category == "update-choice" {
			t.Fatalf("startup choice appeared after the single Enter; no retry submitted; %s", category)
		}
		return !nativeCodexDraftContains(screen, prompt), nativeCodexDraftFacts(screen, prompt)
	})
}

func nativeCodexDraftContains(screen, prompt string) bool {
	lastComposerRow := ""
	for _, line := range strings.Split(screen, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "›") {
			lastComposerRow = line
		}
	}
	return strings.Contains(lastComposerRow, prompt)
}

func nativeCodexDraftFacts(screen, prompt string) string {
	promptRows, arrowRows := 0, 0
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, prompt) {
			promptRows++
		}
		if strings.HasPrefix(strings.TrimSpace(line), "›") {
			arrowRows++
		}
	}
	return fmt.Sprintf("draft_has_prompt=%v prompt_rows=%d arrow_rows=%d", nativeCodexDraftContains(screen, prompt), promptRows, arrowRows)
}

type nativeCodexRollout struct {
	path       string
	threadID   string
	assistant  []string
	user       []string
	submitted  []string
	completed  []nativeCodexUserReceipt
	toolCalled bool
}

type nativeCodexUserReceipt struct {
	threadID, turnID, itemID, message string
}

// matrix/hardened: the exact Alpha11 single-local-environment serialization is
// defined by core/src/context/world_state/environment.rs and
// core/src/context/environment_context.rs in rust-v0.163.0-alpha.11. This
// acceptance fixture permits only the observed 188 PowerShell/managed profile
// and timezone, with the owned project replacing every variable path. It does
// not allow general XML, additional text, optional agents/network or profiles.
func nativeCodexStartupContext(project, date string) string {
	escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace
	owned := escape(project)
	filesystem := `<filesystem><workspace_roots><root>` + owned + `</root></workspace_roots><permission_profile type="managed"><file_system type="restricted">` +
		`<entry access="read"><special>:root</special></entry>` +
		`<entry access="write"><path>` + owned + `</path></entry>` +
		`<entry access="write"><special>:slash_tmp</special></entry>` +
		`<entry access="write"><special>:tmpdir</special></entry>`
	for _, directory := range []string{".git", ".agents", ".codex", ".aws"} {
		filesystem += `<entry access="read"><path>` + escape(filepath.Join(project, directory)) + `</path></entry>`
	}
	filesystem += `</file_system></permission_profile></filesystem>`
	return "<environment_context>\n  <cwd>" + owned + "</cwd>\n  <shell>powershell</shell>\n  <current_date>" + date + "</current_date>\n  <timezone>America/Los_Angeles</timezone>\n  " + filesystem + "\n</environment_context>"
}

func nativeCodexOwnedStartupContext(context, project string) bool {
	lines := strings.Split(context, "\n")
	if len(lines) != 7 || !strings.HasPrefix(lines[3], "  <current_date>") || !strings.HasSuffix(lines[3], "</current_date>") {
		return false
	}
	date := strings.TrimSuffix(strings.TrimPrefix(lines[3], "  <current_date>"), "</current_date>")
	parsed, err := time.Parse("2006-01-02", date)
	return err == nil && parsed.Format("2006-01-02") == date && context == nativeCodexStartupContext(project, date)
}

// matrix/hardened: Codex records one initial environment-context response item
// with role=user before any submitted prompt. It is not an additional request.
// Permit only that first, exact owned-cwd wrapper; every subsequent user/model
// message must still match the three single submissions in order.
func nativeCodexExactReplyPairs(rollout nativeCodexRollout, project string, prompts, replies []string) error {
	users := rollout.user
	if len(prompts) != 3 || len(replies) != 3 || rollout.toolCalled || len(rollout.submitted) == 0 && len(rollout.completed) == 0 {
		return errors.New("harmless lifecycle must prove three requests/replies without tools")
	}
	// matrix/hardened: the newer app-server journal uses typed completed
	// UserMessage items instead of legacy user_message events. Require exact
	// thread ownership and three unique turn/item identities, never dedupe or
	// infer a submission from task_started or a context response item.
	if len(rollout.completed) > 0 {
		_, threadErr := uuid.Parse(rollout.threadID)
		if len(rollout.completed) != 3 || threadErr != nil {
			return errors.New("modern journal must contain exactly three completed user receipts")
		}
		turns, items := make(map[uuid.UUID]bool), make(map[uuid.UUID]bool)
		for index, receipt := range rollout.completed {
			turn, turnErr := uuid.Parse(receipt.turnID)
			item, itemErr := uuid.Parse(receipt.itemID)
			if receipt.threadID != rollout.threadID || turnErr != nil || itemErr != nil || turns[turn] || items[item] || receipt.message != prompts[index] {
				return errors.New("modern user receipt does not identify its exact owned thread/unique turn/input")
			}
			turns[turn], items[item] = true, true
		}
	}
	if len(rollout.submitted) > 0 {
		if len(rollout.submitted) != 3 {
			return errors.New("legacy journal must contain exactly three submitted user events")
		}
		for index, message := range rollout.submitted {
			if message != prompts[index] {
				return errors.New("legacy user event does not match its exact harmless single submission")
			}
		}
	}
	if len(users) == 4 {
		if !nativeCodexOwnedStartupContext(users[0], project) {
			return errors.New("additional user item is not the first exact owned startup environment context")
		}
		users = users[1:]
	}
	if len(users) != 3 || len(rollout.assistant) != 3 {
		return fmt.Errorf("owned history has unexpected submitted message counts: user=%d assistant=%d", len(users), len(rollout.assistant))
	}
	for index := range prompts {
		if users[index] != prompts[index] || rollout.assistant[index] != replies[index] {
			return fmt.Errorf("owned history message pair %d does not exactly match its harmless single submission/reply", index+1)
		}
	}
	return nil
}

func TestWindowsNativeCodexExactReplyPairs(t *testing.T) {
	// matrix/hardened: startup context is allowed only in its real protocol
	// position. Duplicate, modified, extra and stale writes remain failures.
	project := filepath.Join(t.TempDir(), "owned-project")
	prompts, replies := []string{"first prompt", "second prompt", "third prompt"}, []string{"FIRST", "SECOND", "THIRD"}
	context := nativeCodexStartupContext(project, "2026-10-14")
	withContext := append([]string{context}, prompts...)
	for _, test := range []struct {
		name       string
		users      []string
		assistants []string
		events     []string
		tools, ok  bool
	}{
		{name: "three exact pairs", users: prompts, assistants: replies, ok: true},
		{name: "one initial owned context", users: withContext, assistants: replies, ok: true},
		{name: "unknown initial user", users: append([]string{"unknown request"}, prompts...), assistants: replies},
		{name: "wrong context cwd", users: append([]string{strings.ReplaceAll(context, project, "other-project")}, prompts...), assistants: replies},
		{name: "multiple context cwd tags", users: append([]string{strings.ReplaceAll(context, "</cwd>", "</cwd><cwd>other-project</cwd>")}, prompts...), assistants: replies},
		{name: "auth in context", users: append([]string{strings.ReplaceAll(context, "</environment_context>", "<auth>fixture</auth></environment_context>")}, prompts...), assistants: replies},
		{name: "instruction tag in context", users: append([]string{strings.ReplaceAll(context, "</environment_context>", "<user_instructions>fixture</user_instructions></environment_context>")}, prompts...), assistants: replies},
		{name: "stale data in context", users: append([]string{strings.ReplaceAll(context, "</environment_context>", "stale viewer must not reach resumed Codex</environment_context>")}, prompts...), assistants: replies},
		{name: "unknown system tag", users: append([]string{strings.ReplaceAll(context, "</environment_context>", "<system>hidden request</system></environment_context>")}, prompts...), assistants: replies},
		{name: "unknown plain context text", users: append([]string{strings.ReplaceAll(context, "</environment_context>", "hidden request</environment_context>")}, prompts...), assistants: replies},
		{name: "unknown shell", users: append([]string{strings.ReplaceAll(context, "powershell", "hidden request")}, prompts...), assistants: replies},
		{name: "unknown timezone", users: append([]string{strings.ReplaceAll(context, "America/Los_Angeles", "hidden request")}, prompts...), assistants: replies},
		{name: "invalid date", users: append([]string{strings.ReplaceAll(context, "2026-10-14", "2026-02-31")}, prompts...), assistants: replies},
		{name: "extra filesystem path", users: append([]string{strings.ReplaceAll(context, "</file_system>", `<entry access="write"><path>hidden request</path></entry></file_system>`)}, prompts...), assistants: replies},
		{name: "changed filesystem permission", users: append([]string{strings.Replace(context, `entry access="read"`, `entry access="write"`, 1)}, prompts...), assistants: replies},
		{name: "unknown filesystem attribute", users: append([]string{strings.ReplaceAll(context, `<filesystem>`, `<filesystem hidden="request">`)}, prompts...), assistants: replies},
		{name: "context CRLF framing", users: append([]string{strings.ReplaceAll(context, "\n", "\r\n")}, prompts...), assistants: replies},
		{name: "context leading whitespace", users: append([]string{" " + context}, prompts...), assistants: replies},
		{name: "context after prompt", users: []string{prompts[0], context, prompts[1], prompts[2]}, assistants: replies},
		{name: "duplicate context", users: append([]string{context}, withContext...), assistants: replies},
		{name: "extra stale write", users: append(append([]string{}, prompts...), "stale viewer must not reach resumed Codex"), assistants: replies},
		{name: "leading whitespace input", users: []string{" " + prompts[0], prompts[1], prompts[2]}, assistants: replies},
		{name: "trailing whitespace input", users: []string{prompts[0], prompts[1], prompts[2] + "\n"}, assistants: replies},
		{name: "leading whitespace receipt", users: prompts, assistants: replies, events: []string{" " + prompts[0], prompts[1], prompts[2]}},
		{name: "trailing whitespace receipt", users: prompts, assistants: replies, events: []string{prompts[0], prompts[1], prompts[2] + "\n"}},
		{name: "altered exact input", users: []string{prompts[0], prompts[1] + " leaked text", prompts[2]}, assistants: replies},
		{name: "leaked context CSI", users: append([]string{context + "\x1b[200~"}, prompts...), assistants: replies},
		{name: "reordered model replies", users: prompts, assistants: []string{replies[1], replies[0], replies[2]}},
		{name: "leading whitespace assistant", users: prompts, assistants: []string{" " + replies[0], replies[1], replies[2]}},
		{name: "trailing whitespace assistant", users: prompts, assistants: []string{replies[0], replies[1], replies[2] + "\n"}},
		{name: "extra assistant", users: prompts, assistants: append(append([]string{}, replies...), "EXTRA")},
		{name: "tool invocation", users: withContext, assistants: replies, tools: true},
		{name: "missing user events", users: withContext, assistants: replies, events: []string{}},
		{name: "extra user event", users: withContext, assistants: replies, events: append(append([]string{}, prompts...), "stale viewer must not reach resumed Codex")},
		{name: "altered user event", users: withContext, assistants: replies, events: []string{prompts[0], prompts[1] + " leaked CSI", prompts[2]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := test.events
			if events == nil {
				events = prompts
			}
			err := nativeCodexExactReplyPairs(nativeCodexRollout{user: test.users, submitted: events, assistant: test.assistants, toolCalled: test.tools}, project, prompts, replies)
			if (err == nil) != test.ok {
				t.Fatalf("owned reply-pair proof accepted the wrong protocol fixture: %v", err)
			}
		})
	}
}

func TestWindowsNativeCodexModernUserReceipts(t *testing.T) {
	// matrix/hardened: reproduce the actual capitalized UserMessage journal
	// schema and independent turn/item fences, without accounts or a process.
	project, thread := t.TempDir(), uuid.NewString()
	prompts, replies := []string{"first prompt", "second prompt", "third prompt"}, []string{"FIRST", "SECOND", "THIRD"}
	for _, test := range []string{"exact modern", "wrong thread", "duplicate turn", "duplicate item", "alternate turn representation", "alternate item representation", "altered input", "leading whitespace", "trailing whitespace", "extra receipt", "missing receipts", "dual exact legacy", "dual altered legacy"} {
		t.Run(test, func(t *testing.T) {
			rollout := nativeCodexRollout{threadID: thread, user: prompts, assistant: replies}
			for _, prompt := range prompts {
				rollout.completed = append(rollout.completed, nativeCodexUserReceipt{thread, uuid.NewString(), uuid.NewString(), prompt})
			}
			switch test {
			case "wrong thread":
				rollout.completed[1].threadID = uuid.NewString()
			case "duplicate turn":
				rollout.completed[1].turnID = rollout.completed[0].turnID
			case "duplicate item":
				rollout.completed[1].itemID = rollout.completed[0].itemID
			case "alternate turn representation":
				rollout.completed[1].turnID = "urn:uuid:" + rollout.completed[0].turnID
			case "alternate item representation":
				rollout.completed[1].itemID = "urn:uuid:" + rollout.completed[0].itemID
			case "altered input":
				rollout.completed[1].message += " leaked text"
			case "leading whitespace":
				rollout.completed[0].message = " " + rollout.completed[0].message
			case "trailing whitespace":
				rollout.completed[2].message += "\n"
			case "extra receipt":
				rollout.completed = append(rollout.completed, rollout.completed[0])
			case "missing receipts":
				rollout.completed = nil
			case "dual exact legacy":
				rollout.submitted = prompts
			case "dual altered legacy":
				rollout.submitted = []string{prompts[0], "stale", prompts[2]}
			}
			want := test == "exact modern" || test == "dual exact legacy"
			if err := nativeCodexExactReplyPairs(rollout, project, prompts, replies); (err == nil) != want {
				t.Fatalf("modern receipt proof accepted wrong identities/input/count: %v", err)
			}
		})
	}
	var journal strings.Builder
	appendEvent := func(kind string, payload any) {
		encoded, err := json.Marshal(map[string]any{"type": kind, "payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		journal.Write(encoded)
		journal.WriteByte('\n')
	}
	appendEvent("session_meta", map[string]any{"id": thread, "cwd": project})
	for index, prompt := range prompts {
		appendEvent("event_msg", map[string]any{"type": "item_completed", "thread_id": thread, "turn_id": uuid.NewString(), "item": map[string]any{"type": "UserMessage", "id": uuid.NewString(), "content": []any{map[string]any{"type": "text", "text": prompt}}}})
		appendEvent("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}})
		appendEvent("response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": replies[index]}}})
	}
	path := filepath.Join(t.TempDir(), "rollout-fixture.jsonl")
	if err := os.WriteFile(path, []byte(journal.String()), 0600); err != nil {
		t.Fatal(err)
	}
	readback, err := nativeCodexReadRollout(path, project)
	if err != nil || len(readback.submitted) != 0 || len(readback.completed) != 3 || nativeCodexExactReplyPairs(readback, project, prompts, replies) != nil {
		t.Fatalf("actual modern UserMessage journal schema was not independently decoded: %v", err)
	}
}

func nativeCodexReadRollout(path, project string) (nativeCodexRollout, error) {
	result := nativeCodexRollout{path: path}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return result, errors.New("QA rollout is unavailable, linked or exceeds its size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return result, errors.New("cannot open owned QA rollout")
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, (8<<20)+1))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	line, total := 0, 0
	for scanner.Scan() {
		line++
		total += len(scanner.Bytes()) + 1
		if total > 8<<20 {
			return result, errors.New("QA rollout exceeded its total read limit")
		}
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		// matrix/hardened: an incomplete live tail is retried by the polling
		// caller. Never skip a damaged row and accept later rows: the damaged
		// row could be an additional user submission or tool invocation.
		if !nativeCodexCompleteJSON(scanner.Bytes()) {
			return result, fmt.Errorf("QA rollout row %d has incomplete, duplicate-field or malformed JSON", line)
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Type == "" || len(event.Payload) == 0 || event.Payload[0] != '{' {
			return result, fmt.Errorf("QA rollout row %d has an invalid event envelope", line)
		}
		if line == 1 {
			var meta struct {
				ID           string `json:"id"`
				SessionID    string `json:"session_id"`
				CWD          string `json:"cwd"`
				ThreadSource string `json:"thread_source"`
				ParentID     string `json:"parent_thread_id"`
			}
			if event.Type != "session_meta" || json.Unmarshal(event.Payload, &meta) != nil ||
				!strings.EqualFold(filepath.Clean(meta.CWD), filepath.Clean(project)) || meta.ParentID != "" || meta.ThreadSource == "subagent" {
				return result, errors.New("QA rollout header does not identify this exact project/user thread")
			}
			result.threadID = meta.ID
			if result.threadID == "" {
				result.threadID = meta.SessionID
			}
			if _, err := uuid.Parse(result.threadID); err != nil || (meta.SessionID != "" && meta.SessionID != result.threadID) {
				return result, errors.New("QA rollout header has invalid or conflicting thread identity")
			}
			continue
		}
		// matrix/hardened: the canonical harmless journal contains only these
		// inert bookkeeping types plus separately parsed messages/events. Other
		// SDK types (compaction, realtime, retained or inter-agent content) can
		// carry requests, so this acceptance must fail closed on their presence.
		switch event.Type {
		case "event_msg", "response_item", "world_state", "turn_context", "token_usage_record":
		default:
			return result, fmt.Errorf("QA rollout row %d has an unexpected journal type", line)
		}
		// matrix/hardened: actual submission events are distinct from the
		// model's startup environment-context response item with role=user.
		if event.Type == "event_msg" {
			var receipt struct {
				Type     string `json:"type"`
				Message  string `json:"message"`
				ThreadID string `json:"thread_id"`
				TurnID   string `json:"turn_id"`
				Item     struct {
					Type    string `json:"type"`
					ID      string `json:"id"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"item"`
			}
			if json.Unmarshal(event.Payload, &receipt) != nil || receipt.Type == "" || receipt.Type == "item_completed" && receipt.Item.Type == "" {
				return result, fmt.Errorf("QA rollout row %d has an invalid event payload", line)
			}
			switch receipt.Type {
			case "user_message", "agent_message", "task_started", "thread_settings_applied", "item_completed", "token_count", "task_complete":
			default:
				return result, errors.New("owned harmless journal has an unknown event kind")
			}
			if receipt.Type == "item_completed" && receipt.Item.Type != "UserMessage" && receipt.Item.Type != "AgentMessage" {
				return result, errors.New("owned harmless journal has an unexpected completed item kind")
			}
			if receipt.Type == "user_message" {
				if strings.TrimSpace(receipt.Message) == "" {
					return result, errors.New("owned user submission event has empty input")
				}
				result.submitted = append(result.submitted, receipt.Message)
			} else if receipt.Type == "item_completed" && receipt.Item.Type == "UserMessage" {
				if len(receipt.Item.Content) == 0 {
					return result, errors.New("owned user receipt has empty content")
				}
				var message strings.Builder
				for _, part := range receipt.Item.Content {
					if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
						return result, errors.New("owned harmless user receipt contains unexpected non-text/empty input")
					}
					message.WriteString(part.Text)
				}
				result.completed = append(result.completed, nativeCodexUserReceipt{receipt.ThreadID, receipt.TurnID, receipt.Item.ID, message.String()})
			}
			continue
		}
		if event.Type != "response_item" {
			continue
		}
		var response struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(event.Payload, &response) != nil || response.Type == "" {
			return result, fmt.Errorf("QA rollout row %d has an invalid response payload", line)
		}
		if strings.HasSuffix(response.Type, "_call") || strings.HasSuffix(response.Type, "_call_output") {
			result.toolCalled = true
			continue
		}
		if response.Type != "message" {
			if response.Type == "reasoning" {
				continue
			}
			return result, errors.New("owned harmless journal has an unknown response kind")
		}
		if response.Role != "assistant" && response.Role != "user" && response.Role != "developer" && response.Role != "system" {
			return result, errors.New("owned history message has an unknown role")
		}
		if len(response.Content) == 0 {
			return result, errors.New("owned history message has empty content")
		}
		var text strings.Builder
		for _, content := range response.Content {
			want := "input_text"
			if response.Role == "assistant" {
				want = "output_text"
			}
			if content.Type != want || strings.TrimSpace(content.Text) == "" {
				return result, errors.New("owned history message contains an unknown/non-text/empty content part")
			}
			text.WriteString(content.Text)
		}
		if response.Role == "assistant" {
			result.assistant = append(result.assistant, text.String())
		} else if response.Role == "user" {
			result.user = append(result.user, text.String())
		}
	}
	if err := scanner.Err(); err != nil {
		return result, errors.New("QA rollout exceeded its bounded JSON line reader")
	}
	if result.threadID == "" {
		return result, errors.New("QA rollout header is not complete yet")
	}
	return result, nil
}

func nativeCodexCompleteJSON(raw []byte) bool {
	// Reject duplicate keys at any depth, including casing aliases accepted
	// by encoding/json's struct decoder. The final value must never erase an
	// earlier submission/content discriminator.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON exceeds its nesting bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		open, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		if open != '{' && open != '[' {
			return errors.New("unexpected JSON closing token")
		}
		keys := make(map[string]bool)
		for decoder.More() {
			if open == '{' {
				token, err := decoder.Token()
				key, ok := token.(string)
				folded := strings.ToLower(key)
				if err != nil || !ok || keys[folded] {
					return errors.New("invalid or duplicate JSON field")
				}
				keys[folded] = true
			}
			if err := value(depth + 1); err != nil {
				return err
			}
		}
		close, err := decoder.Token()
		if err != nil || open == '{' && close != json.Delim('}') || open == '[' && close != json.Delim(']') {
			return errors.New("incomplete JSON object/array")
		}
		return nil
	}
	if value(0) != nil {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

func TestWindowsNativeCodexStrictRollout(t *testing.T) {
	// matrix/hardened: malformed evidence must fail even when valid matching
	// messages follow it. A poll may retry a partial tail, never skip it.
	project, thread := t.TempDir(), uuid.NewString()
	encode := func(kind string, payload any) string {
		t.Helper()
		encoded, err := json.Marshal(map[string]any{"type": kind, "payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	header := encode("session_meta", map[string]any{"id": thread, "cwd": project})
	message := func(role string, parts ...any) string {
		return encode("response_item", map[string]any{"type": "message", "role": role, "content": parts})
	}
	input := map[string]any{"type": "input_text", "text": "owned exact prompt"}
	output := map[string]any{"type": "output_text", "text": "OWNED"}
	valid := message("user", input) + "\n" + message("assistant", output)
	for _, test := range []struct {
		name, row string
		ok, tools bool
	}{
		{name: "valid text parts", row: valid, ok: true},
		{name: "damaged middle row", row: "{invalid json\n" + valid},
		{name: "partial tail", row: valid + "\n{\"type\":\"response_item\""},
		{name: "missing envelope type", row: `{"payload":{"type":"message"}}` + "\n" + valid},
		{name: "duplicate envelope kind", row: `{"type":"response_item","type":"event_msg","payload":{"type":"task_started"}}` + "\n" + valid},
		{name: "casing duplicate envelope kind", row: `{"type":"response_item","Type":"event_msg","payload":{"type":"task_started"}}` + "\n" + valid},
		{name: "duplicate content type", row: `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","type":"input_text","text":"owned exact prompt"}]}}` + "\n" + valid},
		{name: "casing duplicate content type", row: `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","Type":"input_text","text":"owned exact prompt"}]}}` + "\n" + valid},
		{name: "casing duplicate receipt text", row: `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"hidden request","Text":"owned exact prompt"}]}}}` + "\n" + valid},
		{name: "null payload", row: `{"type":"response_item","payload":null}` + "\n" + valid},
		{name: "array payload", row: `{"type":"event_msg","payload":[]}` + "\n" + valid},
		{name: "unknown outer type", row: encode("unknown", map[string]any{"type": "message", "role": "user", "content": []any{input}}) + "\n" + valid},
		{name: "duplicate session header", row: header + "\n" + valid},
		{name: "compacted hidden request", row: encode("compacted", map[string]any{"message": "fixture", "replacement_history": []any{map[string]any{"type": "message", "role": "user", "content": []any{input}}}}) + "\n" + valid},
		{name: "realtime hidden request", row: encode("realtime_item", map[string]any{"type": "transcript_segment", "role": "user", "text": "hidden request"}) + "\n" + valid},
		{name: "malformed event fields", row: encode("event_msg", map[string]any{"type": 7}) + "\n" + valid},
		{name: "missing event kind", row: encode("event_msg", map[string]any{"message": "hidden request"}) + "\n" + valid},
		{name: "malformed response fields", row: encode("response_item", map[string]any{"type": "message", "content": "hidden request"}) + "\n" + valid},
		{name: "missing response kind", row: encode("response_item", map[string]any{"role": "user", "content": []any{input}}) + "\n" + valid},
		{name: "unknown response kind", row: encode("response_item", map[string]any{"type": "unknown", "role": "user", "content": []any{input}}) + "\n" + valid},
		{name: "function call output proves tools", row: encode("response_item", map[string]any{"type": "function_call_output", "output": "fixture"}) + "\n" + valid, ok: true, tools: true},
		{name: "custom tool output proves tools", row: encode("response_item", map[string]any{"type": "custom_tool_call_output", "output": "fixture"}) + "\n" + valid, ok: true, tools: true},
		{name: "unknown role", row: message("hidden", input) + "\n" + valid},
		{name: "hidden input image", row: message("user", input, map[string]any{"type": "input_image", "image_url": "fixture"}) + "\n" + valid},
		{name: "unknown input part", row: message("user", input, map[string]any{"type": "unknown", "text": "hidden request"}) + "\n" + valid},
		{name: "user output text", row: message("user", output) + "\n" + valid},
		{name: "assistant input text", row: message("assistant", input) + "\n" + valid},
		{name: "unknown assistant part", row: message("assistant", output, map[string]any{"type": "unknown", "text": "hidden reply"}) + "\n" + valid},
		{name: "empty user content", row: message("user") + "\n" + valid},
		{name: "empty assistant content", row: message("assistant") + "\n" + valid},
		{name: "empty text part", row: message("user", map[string]any{"type": "input_text", "text": ""}) + "\n" + valid},
		{name: "empty legacy submission", row: encode("event_msg", map[string]any{"type": "user_message", "message": ""}) + "\n" + valid},
		{name: "missing completed item type", row: encode("event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"id": uuid.NewString()}}) + "\n" + valid},
		{name: "empty modern submission", row: encode("event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "content": []any{}}}) + "\n" + valid},
		{name: "hidden modern image", row: encode("event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "content": []any{map[string]any{"type": "text", "text": "owned exact prompt"}, map[string]any{"type": "image", "image_url": "fixture"}}}}) + "\n" + valid},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout-fixture.jsonl")
			if err := os.WriteFile(path, []byte(header+"\n"+test.row+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			readback, err := nativeCodexReadRollout(path, project)
			if (err == nil) != test.ok || err == nil && readback.toolCalled != test.tools {
				t.Fatalf("strict reader accepted the wrong protocol fixture: %v", err)
			}
		})
	}
}

func TestWindowsNativeCodexRetainedCanonical(t *testing.T) {
	// matrix/hardened: explicit read-only revalidation of one stopped online
	// fixture, anchored to the operator's held SHA/thread/three nonce receipts.
	// This never launches a model or claims a new lifecycle from a stored log.
	if os.Getenv("AGENTDECK_NATIVE_CODEX_CANONICAL_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_CODEX_CANONICAL_TEST=1 for a retained owned QA rollout")
	}
	home := nativeCodexExistingQAHome(t)
	project := os.Getenv("AGENTDECK_NATIVE_CODEX_CANONICAL_PROJECT")
	if !filepath.IsAbs(project) || !strings.HasPrefix(filepath.Base(filepath.Dir(filepath.Dir(project))), "TestWindowsNativeCodexOnlineLifecycle") {
		t.Fatal("canonical readback must name the exact owned online fixture project")
	}
	if _, err := uuid.Parse(filepath.Base(project)); err != nil {
		t.Fatal("canonical project must retain its owned UUID identity")
	}
	rollout, err := nativeCodexFindRollout(home, project)
	if err != nil {
		t.Fatalf("retained canonical reader rejected evidence: %v", err)
	}
	if rollout.path != os.Getenv("AGENTDECK_NATIVE_CODEX_CANONICAL_PATH") || rollout.threadID != os.Getenv("AGENTDECK_NATIVE_CODEX_CANONICAL_THREAD") {
		t.Fatal("canonical path/thread changed from the held completed lifecycle")
	}
	data, err := os.ReadFile(rollout.path)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != strings.ToLower(os.Getenv("AGENTDECK_NATIVE_CODEX_CANONICAL_SHA256")) {
		t.Fatal("canonical bytes changed from the held completed lifecycle")
	}
	nonces := strings.Split(os.Getenv("AGENTDECK_NATIVE_CODEX_CANONICAL_NONCES"), ",")
	if len(nonces) != 3 {
		t.Fatal("canonical readback requires exactly three held nonce receipts")
	}
	var prompts, replies []string
	seen := make(map[string]bool)
	for _, nonce := range nonces {
		if len(nonce) != 12 || seen[nonce] || strings.Trim(nonce, "abcdefghijklmnopqrstuvwxyz") != "" {
			t.Fatal("canonical receipt is not a unique 12-letter harmless nonce")
		}
		seen[nonce] = true
		prompts = append(prompts, "Reply with only the uppercase form of "+nonce+". Do not use tools or modify any files.")
		replies = append(replies, strings.ToUpper(nonce))
	}
	if err := nativeCodexExactReplyPairs(rollout, project, prompts, replies); err != nil {
		t.Fatalf("retained canonical exact evidence was rejected: %v", err)
	}
	t.Log("held stopped lifecycle canonical bytes revalidated: exact startup grammar, three byte-exact submissions/model replies, unique owned turn/item identities, no tools")
}

func nativeCodexFindRollout(home, project string) (nativeCodexRollout, error) {
	var result nativeCodexRollout
	count, matches := 0, 0
	err := filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 128 || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("QA rollout traversal rejected a link or exceeded its entry limit")
		}
		relative, err := filepath.Rel(filepath.Join(home, "sessions"), path)
		if err != nil || len(strings.Split(relative, string(filepath.Separator))) > 5 {
			return errors.New("QA rollout traversal exceeded its depth limit")
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		candidate, err := nativeCodexReadRollout(path, project)
		if err != nil {
			return err
		}
		matches++
		result = candidate
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return result, errors.New("QA rollout has not been created yet")
	}
	if err != nil {
		return result, err
	}
	if matches != 1 {
		return result, errors.New("QA rollout discovery requires exactly one owned user thread")
	}
	return result, nil
}

func nativeCodexAwaitReply(t *testing.T, ui *nativeFleetUI, home, project, priorThread, prompt, expected string) nativeCodexRollout {
	t.Helper()
	deadline := time.Now().Add(nativeCodexModelTimeout)
	var lastError error
	lastCategory := "not-yet-observed"
	lastFacts := "not-yet-observed"
	lastUsers, lastAssistants := 0, 0
	lastExactUsers := 0
	promptVisible, replyVisible := false, false
	lastDraftFacts := "not-yet-observed"
	for time.Now().Before(deadline) {
		screen := ui.screen()
		lastCategory, promptVisible = nativeCodexScreenCategory(screen), strings.Contains(screen, prompt)
		replyVisible, lastDraftFacts = strings.Contains(screen, expected), nativeCodexDraftFacts(screen, prompt)
		lastFacts = nativeCodexDialogFacts(screen, project)
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		rollout, err := nativeCodexFindRollout(home, project)
		lastError = err
		if err == nil {
			lastUsers, lastAssistants = len(rollout.user), len(rollout.assistant)
			if priorThread != "" && rollout.threadID != priorThread {
				t.Fatal("reattached Codex changed its native rollout thread identity")
			}
			if rollout.toolCalled {
				t.Fatal("harmless reply-only Codex prompt unexpectedly invoked a tool")
			}
			userSeen, assistantSeen := false, false
			lastExactUsers = 0
			for _, text := range rollout.user {
				// A correct model reply alone cannot prove paste framing: the
				// model might ignore leaked CSI markers. Require exact user text.
				if strings.TrimSpace(text) == prompt {
					userSeen = true
					lastExactUsers++
				}
			}
			for _, text := range rollout.assistant {
				assistantSeen = assistantSeen || text == expected
			}
			if userSeen && assistantSeen && strings.Contains(screen, expected) {
				return rollout
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real Codex reply was not independently confirmed within %s; rollout_reader=%v user_messages=%d exact_user_matches=%d assistant_messages=%d screen_category=%s prompt_visible=%v reply_visible=%v %s %s; no auth/config or raw screen contents logged", nativeCodexModelTimeout, lastError, lastUsers, lastExactUsers, lastAssistants, lastCategory, promptVisible, replyVisible, lastFacts, lastDraftFacts)
	return nativeCodexRollout{}
}

func nativeCodexReadState(t *testing.T, db *statedb.StateDB, info terminalhost.SessionInfo, image string) {
	t.Helper()
	rows, err := db.LoadInstances()
	if err != nil || len(rows) != 1 {
		t.Fatalf("original StateDB must contain exactly the private Codex descriptor: count=%d error=%v", len(rows), err)
	}
	row := rows[0]
	backend, err := nativestatestore.DecodeBackend(row.ToolData)
	if err != nil || backend == nil || row.ID != info.ID || row.Title != info.Name || row.Tool != "codex" || row.GroupPath != nativestatestore.DefaultGroup || row.TmuxSession != "" || !strings.EqualFold(row.ProjectPath, info.Dir) {
		t.Fatal("original StateDB readback disagrees with the exact Codex session identity")
	}
	if !strings.EqualFold(filepath.Clean(backend.Launch.Executable), filepath.Clean(image)) || len(backend.Launch.Args) != 0 {
		t.Fatal("Codex descriptor did not launch the actual native executable with its default bare arguments")
	}
	if len(backend.Launch.Env) != 0 {
		t.Fatal("UI-created Codex descriptor persisted inherited environment")
	}
}

func TestWindowsNativeCodexOnlineLifecycle(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" || os.Getenv("AGENTDECK_NATIVE_CODEX_ONLINE_TEST") != "1" {
		t.Skip("set both AGENTDECK_NATIVE_TEST=1 and AGENTDECK_NATIVE_CODEX_ONLINE_TEST=1 to authorize three harmless real model turns")
	}
	cli := nativeBootstrapCLI(t)
	home := nativeCodexQAHome(t)
	// matrix/hardened: this preference is only a regular absolute codex.exe,
	// never a command string or authentication/provider/sandbox override.
	codexExecutable, err := validatedCodexExecutable(os.Getenv("AGENTDECK_NATIVE_CODEX_EXE"))
	if err != nil {
		t.Fatal("AGENTDECK_NATIVE_CODEX_EXE must identify a regular absolute native codex.exe")
	}
	project := filepath.Join(t.TempDir(), uuid.NewString())
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	// Windows TempDir may use an ADMINI~1 alias, while Codex displays the long
	// directory name in its trust prompt and rollout header. Supply one spelling
	// to the native UI, descriptor, process cwd and independent readbacks.
	shortPath, err := windows.UTF16PtrFromString(project)
	if err != nil {
		t.Fatal(err)
	}
	longPath := make([]uint16, 32768)
	longLength, err := windows.GetLongPathName(shortPath, &longPath[0], uint32(len(longPath)))
	if err != nil || longLength == 0 || longLength >= uint32(len(longPath)) {
		t.Fatalf("normalize exact owned project path: %v", err)
	}
	project = windows.UTF16ToString(longPath[:longLength])
	sentinel := []byte("owned harmless Codex lifecycle fixture\n")
	sentinelPath := filepath.Join(project, "keep.txt")
	if err := os.WriteFile(sentinelPath, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	config := options{Store: "statedb", StateDir: filepath.Join(t.TempDir(), "manager-"+uuid.NewString()), Pipe: winipc.PipePrefix + "codex-online-" + uuid.NewString(), Executable: codexExecutable}
	var identity terminalhost.Response
	nativeBootstrapCleanup(t, config, &identity)
	ui := nativeCodexStartUI(t, cli, config, project)
	nativeCodexFrame(t, ui, "private online Codex list", "Windows 原生会话", "没有会话")
	identity = nativeBootstrapIdentity(t, config)
	managerHandle := nativeProcessHandle(t, uint32(identity.HostPID))
	name := filepath.Base(project)
	nativeWrite(t, ui.viewer, "n")
	nativeCodexFrame(t, ui, "new-session form defaults to Codex and the owned project", "新建会话", "› 名称：", name, project, "[Codex]", "留空为默认分组")
	nativeWrite(t, ui.viewer, "\r")
	info := nativeFleetFind(t, config.Pipe, name, "running")
	if info.Tool != "codex" || info.Group != nativestatestore.DefaultGroup || info.ID == "" || info.RunID == "" || info.PID == 0 || info.HostPID != identity.HostPID || !strings.EqualFold(info.Dir, project) {
		t.Fatal("UI-created Codex does not belong to the exact shared-store background manager")
	}
	codexHandle := nativeProcessHandle(t, info.PID)
	imageBuffer := make([]uint16, 32768)
	imageLength := uint32(len(imageBuffer))
	if err := windows.QueryFullProcessImageName(codexHandle, 0, &imageBuffer[0], &imageLength); err != nil {
		t.Fatalf("query exact native Codex process image: %v", err)
	}
	image := windows.UTF16ToString(imageBuffer[:imageLength])
	if !strings.EqualFold(filepath.Base(image), "codex.exe") {
		t.Fatal("managed Codex PID is not the actual native codex.exe")
	}
	if codexExecutable != "" && !strings.EqualFold(filepath.Clean(image), codexExecutable) {
		t.Fatal("managed Codex PID did not use the explicitly selected native executable")
	}
	db, err := statedb.OpenReadOnlyLive(filepath.Join(config.StateDir, nativestatestore.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nativeClose(t, "online StateDB readback", db.Close) })
	nativeCodexReadState(t, db, info, image)
	if _, err := os.Stat(filepath.Join(config.StateDir, "registry.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("online shared-store manager created a duplicate prototype registry")
	}
	nativeCodexAwaitReady(t, ui, home, project)
	makePrompt := func() (string, string) {
		// matrix/hardened: test real I/O rather than random-string reversal
		// ability. The transformed proof is still absent from typed input.
		entropy := uuid.New()
		letters := make([]byte, 12)
		for index := range letters {
			letters[index] = 'a' + entropy[index]%26
		}
		nonce := string(letters)
		prompt := "Reply with only the uppercase form of " + nonce + ". Do not use tools or modify any files."
		expected := strings.ToUpper(nonce)
		if strings.Contains(prompt, expected) {
			t.Fatal("reply proof must not already appear in typed input")
		}
		return prompt, expected
	}
	firstPrompt, firstReply := makePrompt()
	nativeCodexSubmit(t, ui, firstPrompt)
	first := nativeCodexAwaitReply(t, ui, home, project, "", firstPrompt, firstReply)
	nativeFleetSameRun(t, info, nativeFleetFind(t, config.Pipe, name, "running"))
	nativeWrite(t, ui.viewer, "\x11")
	nativeCodexFrame(t, ui, "leave Codex after real reply", "Windows 原生会话", name, "已返回列表")
	nativeWrite(t, ui.viewer, "q")
	nativeWaitExit(t, ui.viewer.process, 0)
	nativeAssertProcess(t, managerHandle, true)
	nativeAssertProcess(t, codexHandle, true)
	nativeFleetSameRun(t, info, nativeFleetFind(t, config.Pipe, name, "running"))
	reopened := nativeCodexStartUI(t, cli, config, project)
	nativeCodexFrame(t, reopened, "reopen original running Codex list", "Windows 原生会话", name)
	after := nativeBootstrapIdentity(t, config)
	if after.RunID != identity.RunID || after.HostPID != identity.HostPID {
		t.Fatal("reopening online UI changed the background manager identity")
	}
	nativeWrite(t, reopened.viewer, "\r")
	nativeCodexFrame(t, reopened, "reattach restores independently verified assistant reply", firstReply)
	nativeFleetSameRun(t, info, nativeFleetFind(t, config.Pipe, name, "running"))
	secondPrompt, secondReply := makePrompt()
	nativeCodexSubmitPaste(t, reopened, config.Pipe, info, secondPrompt)
	second := nativeCodexAwaitReply(t, reopened, home, project, first.threadID, secondPrompt, secondReply)
	if second.path != first.path {
		t.Fatal("reattached online Codex wrote its second reply to a different rollout")
	}
	nativeWrite(t, reopened.viewer, "\x11")
	nativeCodexFrame(t, reopened, "return after second real Codex reply", "Windows 原生会话", "已返回列表", name)
	nativeWrite(t, reopened.viewer, "s")
	nativeCodexFrame(t, reopened, "confirm exact online Codex stop", "停止此会话？", name)
	nativeWrite(t, reopened.viewer, "\r")
	nativeCodexFrame(t, reopened, "exact online Codex run stopped", "会话已停止。", "已停止")
	stopped := nativeFleetFind(t, config.Pipe, name, "stopped")
	if stopped.ID != info.ID || stopped.RunID != info.RunID || stopped.PID != info.PID {
		t.Fatal("stop changed the original Codex descriptor/run identity")
	}
	nativeAssertProcess(t, codexHandle, false)
	nativeAssertProcess(t, managerHandle, true)
	nativeCodexReadState(t, db, stopped, image)
	nativeFleetProjectUnchanged(t, sentinelPath, sentinel)
	// matrix/hardened: only h selects history after stop. It must create a new
	// owned generation without changing the native descriptor or the manager.
	nativeWrite(t, reopened.viewer, "h")
	resumed := nativeFleetFind(t, config.Pipe, name, "running")
	if resumed.ID != info.ID || resumed.RunID == "" || resumed.RunID == info.RunID || resumed.PID == 0 || resumed.PID == info.PID || resumed.HostPID != identity.HostPID || resumed.Tool != "codex" || !strings.EqualFold(resumed.Dir, project) {
		t.Fatal("explicit Codex history did not create exactly a new process generation for the original descriptor/manager")
	}
	resumedHandle := nativeProcessHandle(t, resumed.PID)
	resumedImageBuffer := make([]uint16, 32768)
	resumedImageLength := uint32(len(resumedImageBuffer))
	if err := windows.QueryFullProcessImageName(resumedHandle, 0, &resumedImageBuffer[0], &resumedImageLength); err != nil {
		t.Fatalf("query exact resumed Codex process image: %v", err)
	}
	if !strings.EqualFold(windows.UTF16ToString(resumedImageBuffer[:resumedImageLength]), image) {
		t.Fatal("history replaced the saved native Codex executable")
	}
	afterResume := nativeBootstrapIdentity(t, config)
	if afterResume.RunID != identity.RunID || afterResume.HostPID != identity.HostPID {
		t.Fatal("explicit Codex history changed its background manager identity")
	}
	nativeAssertProcess(t, codexHandle, false)
	nativeAssertProcess(t, resumedHandle, true)
	nativeAssertProcess(t, managerHandle, true)
	nativeCodexReadState(t, db, resumed, image)
	for _, op := range []string{"send", "fleet-stop"} {
		ctx, cancel := context.WithTimeout(context.Background(), nativeAttachTimeout)
		stale, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: op, SessionID: info.ID, RunID: info.RunID, Data: []byte("stale viewer must not reach resumed Codex")})
		cancel()
		if err == nil || stale.OK || !strings.Contains(stale.Error, "run changed") {
			t.Fatalf("%s did not reject the former Codex generation", op)
		}
		nativeAssertProcess(t, resumedHandle, true)
		nativeFleetSameRun(t, resumed, nativeFleetFind(t, config.Pipe, name, "running"))
	}
	nativeCodexSelectOwnedHistory(t, reopened, config.Pipe, resumed, home, project, second)
	nativeCodexAwaitReady(t, reopened, home, project)
	nativeCodexFrame(t, reopened, "explicit history restored the prior verified conversation", secondReply)
	nativeFleetSameRun(t, resumed, nativeFleetFind(t, config.Pipe, name, "running"))
	thirdPrompt, thirdReply := makePrompt()
	nativeCodexSubmitPaste(t, reopened, config.Pipe, resumed, thirdPrompt)
	third := nativeCodexAwaitReply(t, reopened, home, project, first.threadID, thirdPrompt, thirdReply)
	if third.path != first.path {
		t.Fatal("explicitly resumed Codex wrote its third real reply into a different rollout")
	}
	// matrix/hardened: three single submissions must remain exactly three
	// owned user/model pairs; an old-generation write cannot hide in history.
	if err := nativeCodexExactReplyPairs(third, project, []string{firstPrompt, secondPrompt, thirdPrompt}, []string{firstReply, secondReply, thirdReply}); err != nil {
		t.Fatal(err)
	}
	nativeFleetSameRun(t, resumed, nativeFleetFind(t, config.Pipe, name, "running"))
	nativeWrite(t, reopened.viewer, "\x11")
	nativeCodexFrame(t, reopened, "return after resumed third real Codex reply", "Windows 原生会话", "已返回列表", name)
	nativeWrite(t, reopened.viewer, "s")
	nativeCodexFrame(t, reopened, "confirm exact resumed Codex stop", "停止此会话？", name)
	nativeWrite(t, reopened.viewer, "\r")
	nativeCodexFrame(t, reopened, "exact resumed Codex generation stopped", "会话已停止。", "已停止")
	finalStopped := nativeFleetFind(t, config.Pipe, name, "stopped")
	if finalStopped.ID != resumed.ID || finalStopped.RunID != resumed.RunID || finalStopped.PID != resumed.PID {
		t.Fatal("final stop changed the exact resumed Codex descriptor/generation")
	}
	nativeAssertProcess(t, resumedHandle, false)
	nativeAssertProcess(t, codexHandle, false)
	nativeAssertProcess(t, managerHandle, true)
	nativeCodexReadState(t, db, finalStopped, image)
	nativeFleetProjectUnchanged(t, sentinelPath, sentinel)
	nativeWrite(t, reopened.viewer, "q")
	nativeWaitExit(t, reopened.viewer.process, 0)
	ctx, cancel := context.WithTimeout(context.Background(), nativeAttachTimeout)
	defer cancel()
	final, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: "fleet-list"})
	if err != nil || len(final.Sessions) != 1 || final.Sessions[0].ID != info.ID || final.Sessions[0].RunID != resumed.RunID || final.Sessions[0].State != "stopped" {
		t.Fatal("Codex stop did not retain exactly the private shared-store descriptor")
	}
	t.Logf("human typing, bracketed paste, three real replies, same-thread reattach/history restore and stale-run rejection verified; manager_pid=%d first_codex_pid=%d resumed_codex_pid=%d thread_id=%s; headless console evidence, no visible desktop assertion", identity.HostPID, info.PID, resumed.PID, first.threadID)
}
