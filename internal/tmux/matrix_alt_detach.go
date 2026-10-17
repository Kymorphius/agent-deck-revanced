package tmux

import "bytes"

// matrix/hardened: Alt+Q detaches too (Ctrl+Q is awkward to reach).
//
// Terminals send Alt+Q in several ways: "ESC q" (Option as Meta/Esc+), the
// xterm modifyOtherKeys and kitty CSI-u forms, or, with macOS Option left at
// "Normal" (iTerm2's default), the character the layout puts on Option+Q,
// "œ". A match only counts when the keypress is the whole read: a key arrives
// as its own read, so typing Esc then q, or pasting text that contains "œ"
// (bracketed and read in 32-byte chunks), never detaches.
var matrixAltQDetachSeqs = [][]byte{
	[]byte("\x1bq"),
	[]byte("\x1b[27;3;113~"),
	[]byte("\x1b[113;3u"),
	[]byte("œ"),
}

// MatrixIsAltQDetach reports whether an input read is exactly one Alt+Q press.
func MatrixIsAltQDetach(chunk []byte) bool {
	for _, seq := range matrixAltQDetachSeqs {
		if bytes.Equal(chunk, seq) {
			return true
		}
	}
	return false
}
