package ui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// matrix/hardened: exercise the group-only corpus independently of Home and sessions.
func TestMatrixGroupSearchShowStartsFreshAndOwnsItems(t *testing.T) {
	s := NewGroupSearch()
	if s.IsVisible() || s.View() != "" {
		t.Fatal("new locator should be hidden")
	}
	items := []GroupSearchItem{{Name: "Agent-Deck", Path: "Tools/Agent-Deck"}}
	s.Show(items)
	items[0].Name = "mutated"
	if !s.IsVisible() || !s.input.Focused() {
		t.Fatal("Show did not focus the visible locator")
	}
	if got, ok := s.Selected(); !ok || got.Name != "Agent-Deck" {
		t.Fatalf("caller mutation changed corpus: %+v, %v", got, ok)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("missing")})
	s.Hide()
	s.Show(items)
	if s.input.Value() != "" || len(s.results) != 1 {
		t.Fatalf("reopened query was not cleared: %q, %+v", s.input.Value(), s.results)
	}
}

func TestMatrixGroupSearchSeparatorsChineseAndAbbreviations(t *testing.T) {
	items := []GroupSearchItem{
		{Name: "Agent-Deck", Path: "Tools/Agent-Deck"},
		{Name: "中文项目", Path: "工作/中文项目"},
		{Name: "Other", Path: "Other"},
	}
	for _, tc := range []struct{ query, want string }{
		{"agent deck", "Agent-Deck"},
		{"AGENT_DECK", "Agent-Deck"},
		{"agent\u3000deck", "Agent-Deck"},
		{"agent—deck", "Agent-Deck"},
		{"adk", "Agent-Deck"},
		{"中文 项目", "中文项目"},
		{"中项", "中文项目"},
		{"工作/中文", "中文项目"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			s := NewGroupSearch()
			s.Show(items)
			s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.query)})
			if got, ok := s.Selected(); !ok || got.Name != tc.want || len(s.results) != 1 {
				t.Fatalf("query=%q: got %+v, want %q", tc.query, s.results, tc.want)
			}
		})
	}
}

func TestMatrixGroupSearchRankingAndStableTies(t *testing.T) {
	s := NewGroupSearch()
	s.Show([]GroupSearchItem{
		{Name: "a_long_distant_k", Path: "one"},
		{Name: "ADK", Path: "literal-first"},
		{Name: "a-d-k", Path: "normalized"},
		{Name: "adk", Path: "literal-second"},
		{Name: "a-de-k", Path: "better-fuzzy"},
	})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("adk")})
	got := make([]string, len(s.results))
	for i, item := range s.results {
		got[i] = item.Path
	}
	want := []string{"literal-first", "literal-second", "normalized", "better-fuzzy", "one"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("literal > normalized > fuzzy, stable ties: got %v, want %v", got, want)
	}
}

func TestMatrixGroupSearchStatusWordsAndPunctuationStayNames(t *testing.T) {
	items := []GroupSearchItem{
		{Name: "running", Path: "Tools/running"},
		{Name: "error", Path: "Tools/error"},
		{Name: "Agent-Deck", Path: "Tools/Agent-Deck"},
		{Name: "Agent_Deck", Path: "Tools/Agent_Deck"},
	}
	for query, want := range map[string]string{"running": "running", "error": "error", "-": "Agent-Deck", "_": "Agent_Deck"} {
		s := NewGroupSearch()
		s.Show(items)
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(query)})
		if got, ok := s.Selected(); !ok || got.Name != want || len(s.results) != 1 {
			t.Fatalf("%q matched %+v, want %q only", query, s.results, want)
		}
	}
	s := NewGroupSearch()
	s.Show(items)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!!!")})
	if _, ok := s.Selected(); ok || len(s.results) != 0 {
		t.Fatal("unmatched punctuation became a match-all query")
	}
}

func TestMatrixGroupSearchRefreshRetainsIdentityAndClamps(t *testing.T) {
	s := NewGroupSearch()
	first := GroupSearchItem{Name: "Ops", Path: "Ops"}
	second := GroupSearchItem{Name: "Ops", Path: "Other/Ops"}
	remote := GroupSearchItem{Name: "Ops", Path: "Ops", Remote: "srv"}
	s.Show([]GroupSearchItem{first, second, remote, first})
	if len(s.results) != 3 {
		t.Fatalf("deduplication lost distinct group identities: %+v", s.results)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ops")})
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	s.SetItems([]GroupSearchItem{remote, first, second})
	if got, ok := s.Selected(); !ok || got != remote || s.cursor != 0 || s.input.Value() != "ops" {
		t.Fatalf("refresh changed query or selected identity: got %+v, index %d, query %q", got, s.cursor, s.input.Value())
	}
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	s.SetItems([]GroupSearchItem{remote})
	if got, ok := s.Selected(); !ok || got != remote || s.cursor != 0 {
		t.Fatalf("removed selection was not clamped: %+v, index %d", got, s.cursor)
	}
	s.SetItems(nil)
	if _, ok := s.Selected(); ok || s.cursor != 0 {
		t.Fatal("empty refresh kept a selection")
	}
}

func TestMatrixGroupSearchNavigationAndSelection(t *testing.T) {
	s := NewGroupSearch()
	s.Show([]GroupSearchItem{{Name: "One", Path: "one"}, {Name: "Two", Path: "two"}})
	for _, key := range []tea.KeyType{tea.KeyUp, tea.KeyCtrlK} {
		s.Update(tea.KeyMsg{Type: key})
		if s.cursor != 0 {
			t.Fatal("navigation went below zero")
		}
	}
	s.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got, _ := s.Selected(); got.Path != "two" {
		t.Fatal("down navigation failed or passed the last result")
	}
	s.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.IsVisible() || cmd != nil {
		t.Fatal("Enter should close without launching external actions")
	}
	if got, ok := s.Selected(); !ok || got.Path != "two" {
		t.Fatal("Home cannot read selected identity after Enter")
	}
	s.Show(nil)
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !s.IsVisible() {
		t.Fatal("Enter closed an empty locator")
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if s.IsVisible() || s.input.Focused() {
		t.Fatal("Esc did not close and blur")
	}
}

func TestMatrixGroupSearchTypingAndBlinkMessages(t *testing.T) {
	s := NewGroupSearch()
	s.Show([]GroupSearchItem{{Name: "groups", Path: "groups"}})
	for _, r := range "gs" {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if s.input.Value() != "gs" || !s.IsVisible() {
		t.Fatal("ordinary main-list shortcuts leaked instead of typing")
	}
	if _, cmd := s.Update(cursor.Blink()); cmd == nil {
		t.Fatal("non-key cursor blink message was dropped")
	}
	s.Hide()
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ignored")})
	if s.input.Value() != "gs" {
		t.Fatal("hidden locator accepted typing")
	}
}

func TestMatrixGroupSearchNamesAndHostsAreVisible(t *testing.T) {
	s := NewGroupSearch()
	s.SetSize(120, 40)
	s.Show([]GroupSearchItem{
		{Name: "Ops", Path: "Tools/Ops"},
		{Name: "Ops", Path: "Other/Ops"},
		{Name: "Ops", Path: "remotes/srv/Tools/Ops", Remote: "srv"},
	})
	view := strings.ReplaceAll(stripAnsi(s.View()), "\u00a0", " ")
	for _, want := range []string{"Group Search", "本机 · Tools/Ops", "1/3 groups", "[Enter] Locate", "[Esc] Cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("group identity or controls missing %q:\n%s", want, view)
		}
	}
	for _, identity := range [][3]string{{"Ops", "Tools", "本机"}, {"Ops", "Other", "本机"}, {"Ops", "Tools", "srv"}} {
		if !matrixGroupSearchHasRow(view, identity[:]...) {
			t.Errorf("group name, parent and device must share one row: %v:\n%s", identity, view)
		}
	}
	if strings.Contains(view, "remotes/srv/") {
		t.Fatalf("remote namespace should not duplicate the device column:\n%s", view)
	}
}

func matrixGroupSearchHasRow(view string, fields ...string) bool {
	for _, line := range strings.Split(stripAnsi(view), "\n") {
		if strings.Contains(line, " · ") {
			continue // The fixed identity is separate from the table itself.
		}
		all := true
		for _, field := range fields {
			all = all && strings.Contains(line, field)
		}
		if all {
			return true
		}
	}
	return false
}

// matrix/hardened: wider screens should use their height for results instead
// of repeating each identity on two rows or retaining the old ten-item cap.
func TestMatrixGroupSearchWideScreenShowsTwentyGroups(t *testing.T) {
	items := make([]GroupSearchItem, 40)
	for i := range items {
		items[i] = GroupSearchItem{Name: fmt.Sprintf("entry%02d", i), Path: fmt.Sprintf("Tools/entry%02d", i)}
	}
	s := NewGroupSearch()
	s.SetSize(120, 40)
	s.Show(items)
	view := stripAnsi(s.View())
	visible := 0
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "entry") && strings.Contains(line, "本机") && !strings.Contains(line, " · ") {
			visible++
		}
	}
	if visible < 20 {
		t.Fatalf("120x40 should show at least twenty complete group rows, got %d:\n%s", visible, view)
	}
}

func TestMatrixGroupSearchRootAndLegacyAliasesKeepIdentity(t *testing.T) {
	s := NewGroupSearch()
	s.SetSize(120, 40)
	s.Show([]GroupSearchItem{
		{Name: "RootOnly", Path: "RootOnly"},
		{Name: "Ops", Path: "Tools/LegacyA"},
		{Name: "Ops", Path: "Tools/LegacyB"},
		{Name: "Ops", Path: "remotes/srv/Tools/LegacyA", Remote: "srv"},
		{Name: "Agent Deck", Path: "Tools/Agent-Deck"},
	})
	view := stripAnsi(s.View())
	for _, fields := range [][]string{{"RootOnly", "—", "本机"}, {"Ops", "Tools/LegacyA", "本机"}, {"Ops", "Tools/LegacyB", "本机"}, {"Ops", "Tools/LegacyA", "srv"}, {"Agent Deck", "Tools", "本机"}} {
		if !matrixGroupSearchHasRow(view, fields...) {
			t.Fatalf("distinct group identity lost from its table row: %v:\n%s", fields, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "RootOnly") && !strings.Contains(line, " · ") && strings.Count(line, "RootOnly") != 1 {
			t.Fatalf("root name repeated as its path:\n%s", line)
		}
		if strings.Contains(line, "Agent Deck") && strings.Contains(line, "Agent-Deck") {
			t.Fatalf("equivalent punctuation should not repeat the leaf:\n%s", line)
		}
	}
	if strings.Contains(view, "remotes/srv/") {
		t.Fatalf("remote prefix leaked into display paths:\n%s", view)
	}
}

func TestMatrixGroupSearchUnicodeColumnsStayAligned(t *testing.T) {
	s := NewGroupSearch()
	s.SetSize(120, 40)
	s.Show([]GroupSearchItem{
		{Name: "中文🚀", Path: "父组/中文🚀", Remote: "srv"},
		{Name: "👨‍👩‍👧‍👦家庭与很长很长很长很长很长很长的分组", Path: "父组/👨‍👩‍👧‍👦家庭与很长很长很长很长很长很长的分组", Remote: "srv"},
		{Name: "1️⃣关键项目很长很长很长很长很长很长很长", Path: "父组/1️⃣关键项目很长很长很长很长很长很长很长", Remote: "srv"},
	})
	view := stripAnsi(s.View())
	var pathColumn, hostColumn int
	rows := 0
	for _, line := range strings.Split(view, "\n") {
		if !utf8.ValidString(line) || cellWidth(line) > 120 {
			t.Fatalf("invalid or overflowing Unicode row:\n%s", line)
		}
		if !strings.Contains(line, "父组") || strings.Contains(line, " · ") {
			continue
		}
		pathIndex, hostIndex := strings.Index(line, "父组"), strings.Index(line, "srv")
		if hostIndex < 0 {
			t.Fatalf("device lost after a long Unicode name:\n%s", line)
		}
		pathCell, hostCell := cellWidth(line[:pathIndex]), cellWidth(line[:hostIndex])
		if rows == 0 {
			pathColumn, hostColumn = pathCell, hostCell
		} else if pathCell != pathColumn || hostCell != hostColumn {
			t.Fatalf("Unicode shifted table columns: path %d/%d host %d/%d:\n%s", pathCell, pathColumn, hostCell, hostColumn, view)
		}
		rows++
	}
	if rows != 3 || !strings.Contains(view, "…") {
		t.Fatalf("expected all three rows and truncation for long names:\n%s", view)
	}
}

func TestMatrixGroupSearchNarrowFooterRetainsSelectedPathSuffix(t *testing.T) {
	s := NewGroupSearch()
	s.SetSize(30, 12)
	s.Show([]GroupSearchItem{
		{Name: "组一", Path: "remotes/srv/很长的共同路径/第一/组一", Remote: "srv"},
		{Name: "组二", Path: "remotes/srv/很长的共同路径/第二/组二", Remote: "srv"},
	})
	for _, suffix := range []string{"第一/组一", "第二/组二"} {
		view := stripAnsi(s.View())
		if !strings.Contains(view, "srv · ") || !strings.Contains(view, suffix) || !strings.Contains(view, "…") || !strings.Contains(view, "[Esc]") {
			t.Fatalf("narrow footer lost selected device/path identity or controls (%s):\n%s", suffix, view)
		}
		if matrixGroupSearchHasRow(view, "分组", "路径", "设备") {
			t.Fatalf("narrow table should give name and device the available width:\n%s", view)
		}
		s.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
}

func TestMatrixGroupSearchFitsAndAllResultsStayReachable(t *testing.T) {
	items := make([]GroupSearchItem, 31)
	for i := range items {
		items[i] = GroupSearchItem{Name: fmt.Sprintf("组%02d", i), Path: fmt.Sprintf("相当长的分组路径/非常长的名字/组%02d", i)}
	}
	for _, dimensions := range [][2]int{{30, 12}, {30, 24}, {60, 18}, {80, 24}, {120, 40}} {
		t.Run(fmt.Sprint(dimensions), func(t *testing.T) {
			s := NewGroupSearch()
			s.SetSize(dimensions[0], dimensions[1])
			s.Show(items)
			for i := range items {
				got, ok := s.Selected()
				if !ok || got != items[i] {
					t.Fatalf("result %d unreachable: %+v", i, got)
				}
				view := s.View()
				for _, line := range strings.Split(view, "\n") {
					if width := lipgloss.Width(line); width > dimensions[0] {
						t.Fatalf("viewport %v row overflow %d:\n%s", dimensions, width, view)
					}
				}
				if height := lipgloss.Height(view); height > dimensions[1] {
					t.Fatalf("viewport %v height overflow %d:\n%s", dimensions, height, view)
				}
				if !strings.Contains(stripAnsi(view), "› "+items[i].Name) {
					t.Fatalf("viewport %v hides selected group %d:\n%s", dimensions, i, view)
				}
				s.Update(tea.KeyMsg{Type: tea.KeyDown})
			}
		})
	}
}
