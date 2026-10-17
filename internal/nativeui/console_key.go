package nativeui

import (
	"unicode/utf16"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: the pinned Windows console reader emits UTF-16 code units
// as separate KeyRunes. Keep at most one high surrogate in this window's filter
// and emit its Unicode scalar only with a low surrogate of the same semantics.
// Isolated surrogate code units are discarded; valid text and control keys
// remain unchanged. Non-key messages do not interrupt a pending pair.
type consoleKeyDecoder struct {
	high       rune
	alt, paste bool
}

func (d *consoleKeyDecoder) filter(_ tea.Model, msg tea.Msg) tea.Msg {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return msg
	}
	if key.Type != tea.KeyRunes {
		d.high = 0
		return msg
	}
	if d.high != 0 && (d.alt != key.Alt || d.paste != key.Paste) {
		d.high = 0
	}
	if len(key.Runes) == 0 {
		return msg
	}
	runes := make([]rune, 0, len(key.Runes))
	for _, char := range key.Runes {
		switch {
		case char >= 0xD800 && char <= 0xDBFF:
			d.high, d.alt, d.paste = char, key.Alt, key.Paste
		case char >= 0xDC00 && char <= 0xDFFF:
			if d.high != 0 {
				runes = append(runes, utf16.DecodeRune(d.high, char))
				d.high = 0
			}
		default:
			d.high = 0
			runes = append(runes, char)
		}
	}
	if len(runes) == 0 {
		return nil
	}
	key.Runes = runes
	return key
}
