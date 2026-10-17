package ui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// matrix/hardened: load the true default and explicit retention preference,
// then exercise the same toggle and persistence path as the real panel.
func TestMatrixSettingsClearSearchOnOpen(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)

	panel := NewSettingsPanel()
	if !panel.clearSearchOnOpen {
		t.Fatal("new panel must default to clearing local search")
	}
	retention := false
	panel.LoadConfig(&session.UserConfig{UI: session.UISettings{ClearSearchOnOpen: &retention}})
	if panel.clearSearchOnOpen {
		t.Fatal("panel did not load explicit retention preference")
	}
	panel.LoadConfig(&session.UserConfig{})
	if !panel.clearSearchOnOpen {
		t.Fatal("empty configuration must restore the true default")
	}

	panel.Show()
	panel.cursor = int(SettingClearSearchOnOpen)
	for _, want := range []bool{false, true} {
		_, _, changed := panel.Update(tea.KeyMsg{Type: tea.KeySpace})
		if !changed || panel.clearSearchOnOpen != want || panel.NeedsRestart() {
			t.Fatalf("toggle did not apply immediately: changed=%v clear=%v restart=%v", changed, panel.clearSearchOnOpen, panel.NeedsRestart())
		}
		cfg := panel.GetConfig()
		if cfg.UI.ClearSearchOnOpen == nil || cfg.UI.GetClearSearchOnOpen() != want {
			t.Fatalf("panel did not return explicit %v: %+v", want, cfg.UI)
		}
		merged, err := session.MergePanelConfigOntoDisk(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.SaveUserConfig(merged); err != nil {
			t.Fatal(err)
		}
		session.ClearUserConfigCache()
		cfg, err = session.LoadUserConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.UI.ClearSearchOnOpen == nil || cfg.UI.GetClearSearchOnOpen() != want {
			t.Fatalf("panel save lost explicit %v: %+v", want, cfg.UI)
		}
	}
}

// matrix/hardened: short terminals must scroll the new checkbox into view,
// retain its focus highlight, and keep all later settings correctly mapped.
func TestMatrixSettingsClearSearchOnOpenScrollAndHighlight(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	oldProfile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(oldProfile) })

	panel := NewSettingsPanel()
	panel.SetSize(80, 20)
	panel.Show()
	panel.cursor = int(SettingClearSearchOnOpen)
	view := panel.View()
	plain := ansi.Strip(view)
	if !strings.Contains(plain, "LOCAL SEARCH") || !strings.Contains(plain, "[x] Clear search on open (/)") ||
		!strings.Contains(plain, "more above") || !strings.Contains(plain, "more below") {
		t.Fatalf("short window did not show the focused local-search setting:\n%s", view)
	}
	rowContaining := func(rendered, label string) string {
		for _, row := range strings.Split(rendered, "\n") {
			if strings.Contains(ansi.Strip(row), label) {
				return row
			}
		}
		return ""
	}
	focusedRow := rowContaining(view, "Clear search on open (/)")
	panel.cursor = int(SettingGlobalSearchEnabled)
	unfocusedRow := rowContaining(panel.View(), "Clear search on open (/)")
	if unfocusedRow == "" || ansi.Strip(focusedRow) != ansi.Strip(unfocusedRow) || focusedRow == unfocusedRow {
		t.Fatalf("checkbox must gain a focus highlight without changing its label:\nfocused=%q\nunfocused=%q", focusedRow, unfocusedRow)
	}

	for setting, label := range map[SettingType]string{
		SettingGlobalSearchEnabled:   "[x] Enabled",
		SettingShowOutput:            "Show Output",
		SettingShowSessionTimestamps: "Show session timestamps",
		SettingSidebarDensity:        "Sidebar density:",
		SettingPrivacy:               "Usage data:",
	} {
		panel.cursor = int(setting)
		if got := ansi.Strip(panel.View()); !strings.Contains(got, label) {
			t.Fatalf("setting %v no longer scrolls into view; missing %q:\n%s", setting, label, got)
		}
	}
}
