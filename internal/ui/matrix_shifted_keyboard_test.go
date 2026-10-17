package ui

import (
	"fmt"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: CSI u reports base keys, while modifyOtherKeys reports the
// produced character. Normalize each protocol without changing Ctrl shortcuts.
func TestMatrixShiftedKeyboardASCII(t *testing.T) {
	type keyCase struct {
		name     string
		key      rune
		modifier int
		want     rune
		wantAlt  bool
	}
	commonCases := []keyCase{
		{"unshifted_digit", '1', 1, '1', false},
		{"already_shifted_exclamation", '!', 2, '!', false},
		{"already_shifted_underscore", '_', 2, '_', false},
		{"alt_unshifted_digit", '1', 3, '1', true},
		{"shift_enter", 13, 2, shiftEnterMarker, false},
		{"shift_alt_enter", 13, 4, shiftEnterMarker, true},
		{"shift_ctrl_letter", 'a', 6, 1, false},
	}
	csiuCases := []keyCase{
		{"shift_alt_digit", '1', 4, '!', true},
		{"shift_alt_punctuation", '-', 4, '_', true},
		{"shift_ctrl_punctuation", '-', 6, '-', false},
	}
	modifyOtherKeysCases := []keyCase{
		{"shift_alt_exclamation", '!', 4, '!', true},
		{"shift_alt_digit_preserved", '1', 4, '1', true},
		{"shift_alt_punctuation_preserved", '-', 4, '-', true},
	}
	for _, pair := range []struct{ base, shifted rune }{
		{'1', '!'}, {'2', '@'}, {'3', '#'}, {'4', '$'}, {'5', '%'},
		{'6', '^'}, {'7', '&'}, {'8', '*'}, {'9', '('}, {'0', ')'},
		{'`', '~'}, {'-', '_'}, {'=', '+'}, {'[', '{'}, {']', '}'},
		{'\\', '|'}, {';', ':'}, {'\'', '"'}, {',', '<'}, {'.', '>'}, {'/', '?'},
	} {
		csiuCases = append(csiuCases, keyCase{
			fmt.Sprintf("U+%04X_to_U+%04X", pair.base, pair.shifted), pair.base, 2, pair.shifted, false,
		})
		modifyOtherKeysCases = append(modifyOtherKeysCases,
			keyCase{fmt.Sprintf("base_U+%04X_preserved", pair.base), pair.base, 2, pair.base, false},
			keyCase{fmt.Sprintf("shifted_U+%04X_preserved", pair.shifted), pair.shifted, 2, pair.shifted, false},
		)
		if pair.base >= '0' && pair.base <= '9' {
			commonCases = append(commonCases,
				keyCase{fmt.Sprintf("shift_ctrl_digit_%c", pair.base), pair.base, 6, pair.base, false},
				keyCase{fmt.Sprintf("shift_ctrl_alt_digit_%c", pair.base), pair.base, 8, pair.base, true},
			)
		}
	}
	protocols := []struct {
		name   string
		format string
		parse  func([]byte) *tea.KeyMsg
		cases  []keyCase
	}{
		{"CSIu", "\x1b[%d;%du", ParseCSIu, csiuCases},
		{"ModifyOtherKeys", "\x1b[27;%[2]d;%[1]d~", ParseModifyOtherKeys, modifyOtherKeysCases},
	}
	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			for _, tc := range append(protocol.cases, commonCases...) {
				t.Run(tc.name, func(t *testing.T) {
					sequence := fmt.Sprintf(protocol.format, tc.key, tc.modifier)
					msg := protocol.parse([]byte(sequence))
					if msg == nil {
						t.Fatalf("parse(%q) = nil", sequence)
					}
					if msg.Type != tea.KeyRunes || string(msg.Runes) != string(tc.want) || msg.Alt != tc.wantAlt {
						t.Errorf("parse(%q) = %+v, want rune %q with Alt=%v", sequence, msg, tc.want, tc.wantAlt)
					}
					out, err := io.ReadAll(NewCSIuReader(strings.NewReader(sequence)))
					if err != nil {
						t.Fatal(err)
					}
					want := string(tc.want)
					if tc.wantAlt {
						want = "\x1b" + want
					}
					if string(out) != want {
						t.Errorf("reader(%q) = %q, want %q", sequence, out, want)
					}
				})
			}
		})
	}
}
