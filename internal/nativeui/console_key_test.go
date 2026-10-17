package nativeui

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: exercise the console event boundaries which previously
// turned a pasted non-BMP character into two Unicode replacement characters.
func TestConsoleKeyDecoder(t *testing.T) {
	high := consoleRunes(0xD83D)
	low := consoleRunes(0xDE00)
	emoji := consoleRunes('😀')
	window := tea.WindowSizeMsg{Width: 120, Height: 30}
	poll := pollMsg{token: 7}
	control := tea.KeyMsg{Type: tea.KeyCtrlC}
	altHigh, altLow, altEmoji := high, low, emoji
	altHigh.Alt, altLow.Alt, altEmoji.Alt = true, true, true
	pasteHigh, pasteLow, pasteEmoji := high, low, emoji
	pasteHigh.Paste, pasteLow.Paste, pasteEmoji.Paste = true, true, true
	tests := []struct {
		name  string
		input []tea.Msg
		want  []tea.Msg
	}{
		{"two events", []tea.Msg{high, low}, []tea.Msg{emoji}},
		{"same event pairs", []tea.Msg{consoleRunes('a', 0xD83D, 0xDE00, '中', 0xD83D, 0xDE03)}, []tea.Msg{consoleRunes('a', '😀', '中', '😃')}},
		{"non-key events preserve pending", []tea.Msg{high, poll, window, low}, []tea.Msg{poll, window, emoji}},
		{"isolated low discarded", []tea.Msg{low, consoleRunes('中')}, []tea.Msg{consoleRunes('中')}},
		{"high followed by ASCII", []tea.Msg{high, consoleRunes('a'), low}, []tea.Msg{consoleRunes('a')}},
		{"high followed by control", []tea.Msg{high, control, low}, []tea.Msg{control}},
		{"high followed by space", []tea.Msg{high, tea.KeyMsg{Type: tea.KeySpace}, low}, []tea.Msg{tea.KeyMsg{Type: tea.KeySpace}}},
		{"repeated high replaces pending", []tea.Msg{high, high, low}, []tea.Msg{emoji}},
		{"Alt boundary", []tea.Msg{high, altLow, low}, nil},
		{"paste boundary", []tea.Msg{high, pasteLow, low}, nil},
		{"matching Alt pair", []tea.Msg{altHigh, altLow}, []tea.Msg{altEmoji}},
		{"matching paste pair", []tea.Msg{pasteHigh, pasteLow}, []tea.Msg{pasteEmoji}},
		{"valid Unicode preserved", []tea.Msg{consoleRunes('中', '文', ' ', '😀', '𠀀')}, []tea.Msg{consoleRunes('中', '文', ' ', '😀', '𠀀')}},
		{"prefix before split pair", []tea.Msg{consoleRunes('中', 0xD83D), consoleRunes(0xDE00, '文')}, []tea.Msg{consoleRunes('中'), consoleRunes('😀', '文')}},
		{"isolated units within text", []tea.Msg{consoleRunes(0xDE00, 'a', 0xD83D, 'b')}, []tea.Msg{consoleRunes('a', 'b')}},
		{"empty rune event preserves pending", []tea.Msg{high, consoleRunes(), low}, []tea.Msg{consoleRunes(), emoji}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := &consoleKeyDecoder{}
			var got []tea.Msg
			for _, input := range test.input {
				if output := decoder.filter(nil, input); output != nil {
					got = append(got, output)
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("filtered messages = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestConsoleKeyDecoderDoesNotSharePendingStateOrMutateInput(t *testing.T) {
	first, second := &consoleKeyDecoder{}, &consoleKeyDecoder{}
	high, low := consoleRunes(0xD83D), consoleRunes(0xDE00)
	if first.filter(nil, high) != nil || second.filter(nil, low) != nil {
		t.Fatal("isolated code unit escaped its own window")
	}
	if got := first.filter(nil, low); !reflect.DeepEqual(got, consoleRunes('😀')) {
		t.Fatalf("first window lost its pending pair: %#v", got)
	}
	input := consoleRunes('中', 0xD83D, 0xDE00, '文')
	want := append([]rune(nil), input.Runes...)
	first.filter(nil, input)
	if !reflect.DeepEqual(input.Runes, want) {
		t.Fatalf("filter changed the caller's rune buffer: %#v", input.Runes)
	}
}

func consoleRunes(runes ...rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: runes}
}
