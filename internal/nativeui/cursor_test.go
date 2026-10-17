package nativeui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestInputCursorProbeMeasuresWideAndRepeatedCellsWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name, value             string
		width, position, column int
	}{
		{"empty", "", 20, 0, 1},
		{"Chinese start", "中文🐱a", 20, 0, 1},
		{"Chinese middle", "中文🐱a", 20, 1, 3},
		{"emoji", "中文🐱a", 20, 2, 5},
		{"after emoji", "中文🐱a", 20, 3, 7},
		{"end", "中文🐱a", 20, 4, 8},
		{"repeated", "aaaaaa", 20, 3, 4},
		{"scroll ASCII", "aaaaaaaaaaaa", 4, 12, 5},
		{"scroll wide", "中文中文中文", 4, 6, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := textinput.New()
			input.Prompt, input.Width = "", test.width
			input.SetValue(test.value)
			input.SetCursor(test.position)
			input.Focus()
			for _, blink := range []bool{false, true} {
				input.Cursor.Blink = blink
				before, position := input.View(), input.Position()
				lines, anchor := clipCursorLines([]string{cursorInputView(input)}, 80, 1)
				if anchor != (cursorAnchor{1, test.column}) {
					t.Fatalf("blink=%v anchor=%+v want column %d view=%q", blink, anchor, test.column, lines)
				}
				if lines[0] != before || input.Position() != position || input.Cursor.Blink != blink || input.Value() != test.value || input.View() != before {
					t.Fatal("measurement changed input render, position, value or blink state")
				}
			}
		})
	}
}

func TestFormCursorTracksFocusAndShortViewport(t *testing.T) {
	for _, height := range []int{1, 2, 3, 4, 24} {
		m := modelWith(&fakeService{})
		press(m, "n")
		m.name.SetValue("")
		m.dir.SetValue("")
		m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
		for focus := 0; focus < 4; focus++ {
			view, frame := m.viewFrame()
			if focus == 2 {
				if frame.anchor != (cursorAnchor{}) {
					t.Fatal("tool choice gained a text cursor anchor")
				}
			} else {
				if frame.anchor.row < 1 || frame.anchor.row > height || frame.anchor.column != ansi.StringWidth("› 名称：")+1 {
					t.Fatalf("height=%d focus=%d anchor=%+v view=%q", height, focus, frame.anchor, view)
				}
				line := strings.Split(ansi.Strip(view), "\n")[frame.anchor.row-1]
				if !strings.HasPrefix(line, "› ") {
					t.Fatalf("cursor was on a different viewport line: %q", line)
				}
			}
			if strings.Contains(view, "agent-deck-native-cursor-probe") {
				t.Fatal("measurement probe escaped the view")
			}
			press(m, "tab")
		}
	}
}

func TestInvisibleAndClippedInputCursorHasNoAnchor(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {5, 2}, {8, 24}} {
		m := modelWith(&fakeService{})
		press(m, "n")
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.name.SetValue("中")
		m.name.SetCursor(0)
		_, frame := m.viewFrame()
		if frame.anchor != (cursorAnchor{}) {
			t.Fatalf("partial/clipped cursor was anchored for %v: %+v", size, frame)
		}
	}
	input := textinput.New()
	input.Prompt = ""
	input.Focus()
	_, anchor := clipCursorLines([]string{"hidden"}, 80, 1)
	if anchor != (cursorAnchor{}) {
		t.Fatal("hidden input gained an anchor")
	}
}

func TestSearchCursorUsesClippedLongGroupPrefix(t *testing.T) {
	for _, size := range [][2]int{{40, 24}, {80, 3}, {80, 1}} {
		m := modelWith(&fakeService{})
		m.allGroups, m.filterGroup = false, strings.Repeat("长分组🐱", 40)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		press(m, "/")
		view, frame := m.viewFrame()
		if size[1] == 1 {
			if frame.anchor != (cursorAnchor{}) {
				t.Fatal("offscreen search cursor was anchored")
			}
			continue
		}
		wantColumn := ansi.StringWidth(m.searchPrefix()) + 1
		if frame.anchor.column != wantColumn || frame.anchor.row != 3 || frame.anchor.column > size[0] {
			t.Fatalf("size=%v anchor=%+v want column=%d view=%q", size, frame.anchor, wantColumn, view)
		}
		line := strings.Split(ansi.Strip(view), "\n")[frame.anchor.row-1]
		if !strings.Contains(line, "搜索：") || ansi.StringWidth(line) > size[0] {
			t.Fatal("long group hid or overflowed active search")
		}
	}
}

func TestInactiveAndQuittingViewsClearCursorAnchor(t *testing.T) {
	m := modelWith(&fakeService{})
	press(m, "n")
	press(m, "esc")
	_, frame := m.viewFrame()
	if frame.anchor != (cursorAnchor{}) {
		t.Fatal("leaving a form retained its anchor")
	}
	m.quitting = true
	view, frame := m.viewFrame()
	if view != "" || frame.anchor != (cursorAnchor{}) {
		t.Fatal("quit retained content or anchor")
	}
}

func TestOrdinaryModelViewsNeverContainPrivateCursorMetadata(t *testing.T) {
	m := modelWith(&fakeService{})
	for _, currentMode := range []mode{listMode, createMode, renameMode, moveMode, confirmStopMode, confirmRemoveMode, workingMode, attachMode} {
		m.mode = currentMode
		if strings.Contains(m.View(), "agent-deck-native-cursor") {
			t.Fatalf("mode=%v emitted private metadata without its paired output", currentMode)
		}
	}
}

func TestFocusedCursorRemainsVisibleAfterFieldWidthShrinks(t *testing.T) {
	for _, field := range []string{"name", "dir", "group", "search"} {
		for _, middle := range []bool{false, true} {
			t.Run(field+map[bool]string{false: " end", true: " middle"}[middle], func(t *testing.T) {
				m := modelWith(&fakeService{})
				var input *textinput.Model
				row := 3
				if field == "search" {
					press(m, "/")
					input = &m.search
				} else {
					press(m, "n")
					switch field {
					case "name":
						input, row = &m.name, 2
					case "dir":
						press(m, "tab")
						input = &m.dir
					case "group":
						press(m, "tab")
						press(m, "tab")
						press(m, "tab")
						input, row = &m.group, 5
					}
				}
				m.Update(tea.WindowSizeMsg{Width: 160, Height: 24})
				input.SetValue(strings.Repeat("中文🐱", 30))
				input.CursorEnd()
				if middle {
					input.SetCursor(input.Position() / 2)
				}
				position, value, blink := input.Position(), input.Value(), input.Cursor.Blink
				m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
				_, frame := m.viewFrame()
				if frame.anchor.row != row || frame.anchor.column < 9 || frame.anchor.column > 40 {
					t.Fatalf("resize clipped the focused input cursor: %+v", frame.anchor)
				}
				if input.Position() != position || input.Value() != value || input.Cursor.Blink != blink {
					t.Fatal("resizing changed field value, cursor position or blink state")
				}
			})
		}
	}
}

func TestRenameAndMoveCursorFollowsTheirShortViewport(t *testing.T) {
	for _, currentMode := range []mode{renameMode, moveMode} {
		for _, height := range []int{1, 2, 3, 4, 24} {
			m := modelWith(&fakeService{})
			m.mode = currentMode
			if currentMode == renameMode {
				m.name.Focus()
			} else {
				m.focus = 3
				m.group.Focus()
			}
			m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
			view, frame := m.viewFrame()
			if frame.anchor.row < 1 || frame.anchor.row > height || frame.anchor.column != 9 {
				t.Fatalf("mode=%v height=%d anchor=%+v", currentMode, height, frame.anchor)
			}
			line := strings.Split(ansi.Strip(view), "\n")[frame.anchor.row-1]
			if !strings.HasPrefix(line, "› ") {
				t.Fatalf("anchor did not follow short form's active field: %q", line)
			}
		}
	}
}
