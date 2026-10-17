package tmux

import "bytes"

// matrix/hardened: reserve an unmodified F9 press for returning to the list.
// The tilde form is the existing Bubble Tea/vt F9 encoding; 57372 is F9 in
// the existing ultraviolet kitty-key table. Explicit press/repeat events are
// accepted, but modifiers and key-release events belong to the child program.
var matrixF9DetachSeqs = [][]byte{
	[]byte("\x1b[20~"),
	[]byte("\x1b[20;1~"),
	[]byte("\x1b[20;1:1~"),
	[]byte("\x1b[20;1:2~"),
	[]byte("\x1b[57372u"),
	[]byte("\x1b[57372;1u"),
	[]byte("\x1b[57372;1:1u"),
	[]byte("\x1b[57372;1:2u"),
}

// MatrixIsF9Detach matches one complete, unmodified F9 press, never a paste,
// another key's prefix, a modifier encoding, or an Alt-prefixed F9 token.
func MatrixIsF9Detach(token []byte) bool {
	for _, seq := range matrixF9DetachSeqs {
		if bytes.Equal(token, seq) {
			return true
		}
	}
	return false
}

// MatrixInputTokenLength identifies the first raw terminal input token. The
// caller must handle bracketed paste before using it to recognize hotkeys.
// matrix/hardened: keeping an entire ANSI token opaque prevents a nested F9
// byte sequence inside an escape string, or an extra ESC for Alt+F9, detaching.
// final releases incomplete input literally (for a lone-Esc timeout or EOF).
func MatrixInputTokenLength(data []byte, final bool) (length int, incomplete bool) {
	if len(data) == 0 {
		return 0, false
	}
	if data[0] != 0x1b {
		return 1, false
	}
	i := 1
	for i < len(data) && data[i] == 0x1b {
		i++
	}
	if i == len(data) {
		if final {
			return i, false
		}
		return 0, true
	}
	switch data[i] {
	case '[', 'O':
		for j := i + 1; j < len(data); j++ {
			if data[j] >= 0x40 && data[j] <= 0x7e {
				return j + 1, false
			}
			if data[j] < 0x20 || data[j] > 0x3f {
				return j, false
			}
		}
	case ']', 'P', '_', '^', 'X':
		for j := i + 1; j < len(data); j++ {
			if data[i] == ']' && data[j] == 0x07 {
				return j + 1, false
			}
			if data[j] == 0x1b && j+1 < len(data) && data[j+1] == '\\' {
				return j + 2, false
			}
		}
	default:
		for j := i; j < len(data); j++ {
			if data[j] < 0x20 || data[j] > 0x2f {
				return j + 1, false
			}
		}
	}
	if final {
		return len(data), false
	}
	return 0, true
}

// MatrixF9DetachInput is the zero-value-ready, per-attach F9 input parser.
// matrix/hardened: read boundaries are not key boundaries. Only bounded ANSI
// prefixes are retained; paste and escape-string bodies stream verbatim while
// their context suppresses F9. Consume returns only input preceding F9 and
// drops the rest of that read, like the existing control-key detach path.
type MatrixF9DetachInput struct {
	pending    []byte
	paste      bool
	pasteEnd   int
	stringMode bool
	stringEsc  bool
	stringBEL  bool
}

const matrixInputTokenLimit = 64

var matrixPasteStart = []byte("\x1b[200~")
var matrixPasteEnd = []byte("\x1b[201~")

// Protected reports whether Alt+Q's exact original read is inside another
// token/paste. Callers must test Alt+Q before Consume, never on rejoined output.
func (input *MatrixF9DetachInput) Protected() bool {
	return len(input.pending) > 0 || input.paste || input.stringMode
}

// HasPendingEscape permits a short idle timeout to forward a standalone Esc.
// Partial CSI tokens remain buffered for subsequent reads, without a timer.
func (input *MatrixF9DetachInput) HasPendingEscape() bool {
	if len(input.pending) == 0 {
		return false
	}
	for _, b := range input.pending {
		if b != 0x1b {
			return false
		}
	}
	return true
}

// Flush releases an incomplete prefix as literal input. It leaves active
// paste/escape-string contexts intact so an idle gap cannot unprotect a paste.
func (input *MatrixF9DetachInput) Flush() []byte {
	out := input.pending
	input.pending = nil
	return out
}

// Consume processes input in order and reports an unmodified F9 outside paste.
func (input *MatrixF9DetachInput) Consume(data []byte) ([]byte, bool) {
	out := make([]byte, 0, len(data))
	for _, b := range data {
		if input.paste {
			out = append(out, b)
			if b == matrixPasteEnd[input.pasteEnd] {
				input.pasteEnd++
			} else if b == matrixPasteEnd[0] {
				input.pasteEnd = 1
			} else {
				input.pasteEnd = 0
			}
			if input.pasteEnd == len(matrixPasteEnd) {
				input.paste = false
				input.pasteEnd = 0
			}
			continue
		}
		if input.stringMode {
			out = append(out, b)
			if (input.stringBEL && b == 0x07) || (input.stringEsc && b == '\\') {
				input.stringMode = false
			}
			input.stringEsc = b == 0x1b
			continue
		}
		if len(input.pending) == 0 && b != 0x1b {
			out = append(out, b)
			continue
		}
		input.pending = append(input.pending, b)
		// Escape strings stream immediately, but their body stays opaque.
		if b == ']' || b == 'P' || b == '_' || b == '^' || b == 'X' {
			prefix := input.pending[:len(input.pending)-1]
			onlyEsc := len(prefix) > 0
			for _, c := range prefix {
				onlyEsc = onlyEsc && c == 0x1b
			}
			if onlyEsc {
				out = append(out, input.pending...)
				input.pending = nil
				input.stringMode = true
				input.stringEsc = false
				// matrix/hardened: BEL terminates OSC only; DCS/APC/PM/SOS
				// payloads containing BEL stay opaque until the ST delimiter.
				input.stringBEL = b == ']'
				continue
			}
		}
		length, incomplete := MatrixInputTokenLength(input.pending, false)
		if incomplete && len(input.pending) < matrixInputTokenLimit {
			continue
		}
		if incomplete {
			// A malformed/unbounded prefix is literal input, never a hotkey.
			out = append(out, input.Flush()...)
			continue
		}
		token := input.pending[:length]
		if MatrixIsF9Detach(token) {
			input.pending = nil
			return out, true
		}
		// matrix/hardened: a bare Esc immediately followed by paste may
		// share a read; an extra Esc prefix cannot unprotect that paste.
		if bytes.HasSuffix(token, matrixPasteStart) {
			input.paste = true
		}
		out = append(out, token...)
		remaining := append([]byte(nil), input.pending[length:]...)
		input.pending = nil
		// An invalid CSI may end immediately before a fresh ESC/control byte.
		// Re-feed that byte so it starts its own token rather than being lost.
		if len(remaining) > 0 {
			forward, detached := input.Consume(remaining)
			out = append(out, forward...)
			if detached {
				return out, true
			}
		}
	}
	return out, false
}
