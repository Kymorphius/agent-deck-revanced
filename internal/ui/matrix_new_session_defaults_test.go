package ui

import (
	"os"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: test durable cold-open defaults and the exclusion boundary,
// not just values left in the reused dialog singleton.
func defaultsConfigForTest(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	cfg, err := session.LoadUserConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultTool = "claude"
	cfg.DefaultPath = "/existing/global/path"
	cfg.Claude.DangerousMode = new(bool)
	*cfg.Claude.DangerousMode = true
	if err := session.SaveUserConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestMatrixNewSessionDefaultsColdOpen(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			defaultsConfigForTest(t)
			d := NewNewDialog()
			d.SetDefaultTool("codex")
			d.SetSize(100, 30)
			d.ShowInGroup("first", "first", "/session/path/not/a/default", nil, "")
			d.nameInput.SetValue("DO_NOT_PERSIST_SESSION_NAME")
			d.branchInput.SetValue("DO_NOT_PERSIST_BRANCH")
			d.modelInput.SetValue("gpt-5.5")
			d.reasoningEffort = "high"
			d.codexOptions.SetDefaults(enabled)
			d.worktreeEnabled, d.sandboxEnabled = enabled, enabled
			d.focusIndex = d.indexOf(focusSaveDefaults)
			d.updateFocus()
			h := &Home{newDialog: d}
			_, cmd := h.handleNewDialogKey(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil || !d.IsVisible() || d.defaultsStatus == "" {
				t.Fatal("saving defaults must keep form open without creating a session")
			}
			path, _ := session.GetUserConfigPath()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"/session/path/not/a/default", "DO_NOT_PERSIST_SESSION_NAME", "DO_NOT_PERSIST_BRANCH"} {
				if strings.Contains(string(data), forbidden) {
					t.Fatalf("defaults persisted session data %q", forbidden)
				}
			}
			cfg, err := session.ReloadUserConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DefaultTool != "codex" || cfg.DefaultPath != "/existing/global/path" || !*cfg.Claude.DangerousMode {
				t.Fatal("save changed unrelated configuration")
			}
			next := NewNewDialog()
			next.SetSize(100, 30)
			next.ShowInGroup("second", "second", "/fresh/group/path", nil, "")
			if next.GetSelectedCommand() != "codex" || next.GetLaunchModelID() != "gpt-5.5" || next.GetLaunchReasoningEffort() != "high" {
				t.Fatalf("cold open lost tool/model/effort: %s %s %s", next.GetSelectedCommand(), next.GetLaunchModelID(), next.GetLaunchReasoningEffort())
			}
			if next.GetCodexYoloMode() != enabled || next.worktreeEnabled != enabled || next.sandboxEnabled != enabled {
				t.Fatal("cold open lost explicitly enabled/disabled launch controls")
			}
			if next.pathInput.Value() != "/fresh/group/path" || next.nameInput.Value() != "" || next.branchInput.Value() != "" {
				t.Fatal("cold open restored session identity or old path")
			}
			next.ResetRemoteDefaults()
			if next.GetLaunchModelID() != "" || next.GetCodexYoloMode() || next.sandboxEnabled || next.worktreeEnabled {
				t.Fatal("local defaults leaked to remote target")
			}
			if err := next.SaveDefaults(); err == nil {
				t.Fatal("remote must not overwrite local defaults")
			}
		})
	}
}

func TestMatrixNewSessionDefaultsCtrlDAndClaude(t *testing.T) {
	defaultsConfigForTest(t)
	d := NewNewDialog()
	d.SetDefaultTool("claude")
	d.Show()
	d.claudeOptions.SetFromOptions(&session.ClaudeOptions{SessionMode: "continue", SkipPermissions: true, UseChrome: true, UseTeammateMode: true})
	d.claudeOptions.SetExtraArgs([]string{"--verbose"})
	d.claudeOptions.SetStartQuery("Explain the project")
	d, _ = d.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if d.defaultsStatus == "" {
		t.Fatalf("Ctrl+D failed: %s", d.validationErr)
	}
	fresh := NewNewDialog()
	fresh.Show()
	opts := fresh.GetClaudeOptions()
	if opts == nil || opts.SessionMode != "continue" || !opts.SkipPermissions || !opts.UseChrome || !opts.UseTeammateMode {
		t.Fatal("Claude launch controls not restored")
	}
	if fresh.GetClaudeStartQuery() != "Explain the project" || len(fresh.GetClaudeExtraArgs()) != 1 {
		t.Fatal("Claude text controls not restored")
	}
}

func TestMatrixGroupPathCandidatesDoNotCoverFooter(t *testing.T) {
	for _, size := range [][2]int{{64, 28}, {80, 24}, {60, 15}, {100, 30}} {
		g := NewGroupDialog()
		g.SetSize(size[0], size[1])
		g.ShowCreateWithContext("Tools/Agent-Deck", "Agent-Deck")
		g.focusPath()
		g.SetPathSuggestions(matrixDropdownPaths())
		g.pathInput.SetValue("VISIBLE_PATH")
		view := stripAnsi(g.View())
		lines := strings.Split(view, "\n")
		input := matrixDropdownLine(t, lines, "VISIBLE_PATH")
		lastMenuBorder := -1
		for i, line := range lines {
			if i > input && strings.Count(line, "╰") > 0 {
				lastMenuBorder = i
				break
			}
		}
		footer := -1
		for i, line := range lines {
			if i > lastMenuBorder && strings.Contains(line, "Tab/Enter accept") {
				footer = i
				break
			}
		}
		if lastMenuBorder <= input || footer <= lastMenuBorder {
			t.Fatalf("candidate box overlaps footer at %dx%d:\n%s", size[0], size[1], view)
		}
		if !strings.Contains(lines[footer], "Esc dismiss") {
			t.Fatalf("footer clipped dismiss hint:\n%s", view)
		}
	}
}
