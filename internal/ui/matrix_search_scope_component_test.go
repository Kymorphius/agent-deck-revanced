package ui

import (
	"fmt"
	"slices"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestMatrixSearchScopeComponent_ViewRefreshKeepsScopeAndQuery(t *testing.T) {
	setXDGTestHome(t)
	s := NewSearch()
	visible := map[string]bool{"inside": true, "moved": true}
	s.SetViewScope("已归档", func(inst *session.Instance) bool { return visible[inst.ID] })
	s.SetItems([]*session.Instance{
		{ID: "inside", Title: "needle", GroupPath: "工作"},
		{ID: "moved", Title: "needle", GroupPath: "生活"},
		{ID: "outside", Title: "needle", GroupPath: "工作"},
		nil,
	})
	s.Show()
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("needle")})
	assertMatrixSearchScopeComponentIDs(t, s.allItems, "inside", "moved")
	assertMatrixSearchScopeComponentIDs(t, s.results, "inside", "moved")

	// The predicate reads the current view again on background refresh; a
	// session that leaves the view must not survive just because it matched.
	delete(visible, "moved")
	visible["new"] = true
	s.SetItems([]*session.Instance{
		{ID: "inside", Title: "needle", GroupPath: "工作"},
		{ID: "moved", Title: "needle", GroupPath: "生活"},
		{ID: "new", Title: "needle", GroupPath: "其他"},
		{ID: "outside", Title: "needle", GroupPath: "工作"},
	})
	assertMatrixSearchScopeComponentIDs(t, s.allItems, "inside", "new")
	assertMatrixSearchScopeComponentIDs(t, s.results, "inside", "new")
	if s.input.Value() != "needle" || !s.IsVisible() || s.scopeLabel != "已归档" || s.viewFilter == nil {
		t.Fatal("background refresh must retain the view scope, query and overlay")
	}
}

func TestMatrixSearchScopeComponent_ScopesReplaceOneAnother(t *testing.T) {
	s := NewSearch()
	items := []*session.Instance{
		{ID: "empty-group", Title: "needle"},
		{ID: "default", Title: "needle", GroupPath: session.DefaultGroupPath},
		{ID: "parent", Title: "needle", GroupPath: "工作"},
		{ID: "child", Title: "needle", GroupPath: "工作/子组"},
		nil,
	}
	s.SetViewScope("空视图", func(*session.Instance) bool { return false })
	s.SetScopedGroup(session.DefaultGroupPath)
	s.SetItems(items)
	assertMatrixSearchScopeComponentIDs(t, s.allItems, "empty-group", "default")
	if s.viewFilter != nil || s.scopeLabel != "" {
		t.Fatal("a group search must clear the former view scope")
	}

	s.SetScopedGroup("工作")
	s.SetItems(items)
	assertMatrixSearchScopeComponentIDs(t, s.allItems, "parent")

	s.SetViewScope("当前可见", func(inst *session.Instance) bool { return inst.ID == "child" })
	s.SetItems(items)
	assertMatrixSearchScopeComponentIDs(t, s.allItems, "child")
	if s.scopedGroup != "" {
		t.Fatal("a view search must clear the former exact-group scope")
	}

	s.SetScopedGroup("")
	s.SetItems(items)
	assertMatrixSearchScopeComponentIDs(t, s.allItems, "empty-group", "default", "parent", "child")
}

func TestMatrixSearchScopeComponent_EmptyScopeStaysEmpty(t *testing.T) {
	setXDGTestHome(t)
	s := NewSearch()
	s.SetViewScope("已归档", func(*session.Instance) bool { return false })
	s.SetItems([]*session.Instance{nil, {ID: "hidden", Title: "needle"}})
	s.Show()
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("needle")})
	s.SetItems([]*session.Instance{{ID: "another-hidden", Title: "needle"}})
	if len(s.allItems) != 0 || len(s.results) != 0 || s.Selected() != nil {
		t.Fatal("an empty view must not fall back to the complete registry")
	}
	if s.input.Value() != "needle" {
		t.Fatal("an empty refresh must keep the query")
	}
	s.SetSize(60, 15)
	assertDialogFits(t, s.View(), 60, 15, "当前视图 · 已归档", "No results", "[Esc] Cancel")
}

func TestMatrixSearchScopeComponent_HideRetainsSelectionAndClearsScope(t *testing.T) {
	for _, scope := range []string{"group", "view"} {
		t.Run(scope, func(t *testing.T) {
			setXDGTestHome(t)
			s := NewSearch()
			inside := &session.Instance{ID: "inside", Title: "needle", GroupPath: "工作"}
			outside := &session.Instance{ID: "outside", Title: "other", GroupPath: "生活"}
			items := []*session.Instance{inside, outside}
			if scope == "group" {
				s.SetScopedGroup("工作")
			} else {
				s.SetViewScope("当前可见", func(inst *session.Instance) bool { return inst == inside })
			}
			s.SetItems(items)
			s.SetNotice("temporary notice")
			s.Show()
			s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("needle")})
			s.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if s.IsVisible() || s.Selected() != inside {
				t.Fatal("Enter must leave its selected result available to the parent")
			}
			if s.scopedGroup != "" || s.viewFilter != nil || s.scopeLabel != "" || s.notice != "" {
				t.Fatal("Hide must clear all scope and notice state")
			}

			// Home provides the complete registry on each open. The cleared
			// scope must allow the next ordinary search to include every row.
			s.SetItems(items)
			s.Show()
			assertMatrixSearchScopeComponentIDs(t, s.allItems, "inside", "outside")
			assertMatrixSearchScopeComponentIDs(t, s.results, "inside", "outside")
			if s.input.Value() != "" {
				t.Fatal("a fresh search must honor the default clear-on-open setting")
			}
		})
	}
}

func TestMatrixSearchScopeComponent_ScopeLabelsFit(t *testing.T) {
	for _, width := range []int{30, 60, 80, 120} {
		for _, scope := range []string{"all", "view", "group"} {
			t.Run(fmt.Sprintf("%s_%d_columns", scope, width), func(t *testing.T) {
				setXDGTestHome(t)
				s := NewSearch()
				s.SetSize(width, 12)
				path := "工具/研发/很长的中文分组名称/agent-deck"
				want := "全部本机会话"
				switch scope {
				case "view":
					s.SetViewScope("已归档 · 很长的中文视图条件", func(*session.Instance) bool { return true })
					want = "当前视图"
				case "group":
					s.SetScopedGroup(path)
					want = "当前分组"
				}
				s.SetItems([]*session.Instance{{ID: "target", Title: "needle", Tool: "codex", GroupPath: path}})
				s.Show()
				assertDialogFits(t, s.View(), width, 12, want, "needle", "[Enter] Select", "[Tab] Recall", "[Esc] Cancel")
			})
		}
	}
}

func assertMatrixSearchScopeComponentIDs(t *testing.T, items []*session.Instance, want ...string) {
	t.Helper()
	got := make([]string, len(items))
	for i, inst := range items {
		if inst == nil {
			t.Fatal("scope stored a nil session")
		}
		got[i] = inst.ID
	}
	if !slices.Equal(got, want) {
		t.Fatalf("session IDs = %v, want %v", got, want)
	}
}
