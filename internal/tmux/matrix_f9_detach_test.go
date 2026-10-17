package tmux

import (
	"bytes"
	"strings"
	"testing"
)

// matrix/hardened: exercise terminal encodings and every read boundary rather
// than assuming a complete function-key sequence arrives in one stdin read.
func TestMatrixF9DetachInput(t *testing.T) {
	for _, seq := range matrixF9DetachSeqs {
		if !MatrixIsF9Detach(seq) {
			t.Fatalf("complete F9 %q not recognized", seq)
		}
		for split := 0; split <= len(seq); split++ {
			var input MatrixF9DetachInput
			var output []byte
			detached := false
			for _, chunk := range [][]byte{append([]byte("before"), seq[:split]...), append(seq[split:], []byte("after")...)} {
				forward, stop := input.Consume(chunk)
				output = append(output, forward...)
				if stop {
					detached = true
					break
				}
			}
			if !detached || string(output) != "before" {
				t.Fatalf("F9 %q split at %d: detached=%v forwarded=%q", seq, split, detached, output)
			}
		}
	}
}

// matrix/hardened: modifiers, release events, escape-string bodies, bracketed
// paste and ordinary ANSI input remain child input even when read byte by byte.
func TestMatrixF9DetachInputPassthrough(t *testing.T) {
	for _, text := range []string{
		"ordinary text 中文", "\x1b[A\x1b[1;5D\x1b[19~\x1bOP", "\x1b(0",
		"\x1b[20;2~", "\x1b[20;3~", "\x1b[20;5~", "\x1b[20;1:3~",
		"\x1b[57372;2u", "\x1b[57372;3u", "\x1b[57372;5u", "\x1b[57372;1:3u",
		"\x1b\x1b[20~", "\x1b\x1b[57372u", "\x1bq",
		"\x1b[200~pasted\x1b[20~\x1b[57372;1u\x1b[201~ordinary",
		"\x1b\x1b[200~pasted\x1b[20~\x1b[201~ordinary",
		"\x1b]title\x1b[20~\x07", "\x1bPdata\x1b[20~\x1b\\",
		"\x1b_payload\x1b[57372u\x1b\\", "\x1b^message\x1b[20~\x1b\\",
		"\x1bXstring\x1b[20~\x1b\\", "\x1b[" + strings.Repeat("1", 70) + "~",
		"\x1bPdata\x07\x1b[20~\x1b\\", "\x1b_data\x07\x1b[20~\x1b\\",
		"\x1b^data\x07\x1b[20~\x1b\\", "\x1bXdata\x07\x1b[20~\x1b\\",
	} {
		for _, bytewise := range []bool{false, true} {
			var input MatrixF9DetachInput
			var output []byte
			chunks := [][]byte{[]byte(text)}
			if bytewise {
				chunks = nil
				for i := range len(text) {
					chunks = append(chunks, []byte{text[i]})
				}
			}
			for _, chunk := range chunks {
				forward, detached := input.Consume(chunk)
				if detached {
					t.Fatalf("ordinary input detached: %q (bytewise=%v)", text, bytewise)
				}
				output = append(output, forward...)
			}
			output = append(output, input.Flush()...)
			if !bytes.Equal(output, []byte(text)) {
				t.Fatalf("input changed: got %q want %q (bytewise=%v)", output, text, bytewise)
			}
		}
	}
}

// matrix/hardened: paste/end and string/end state spans reads and idle gaps;
// the very next real F9 becomes active again after the protected region ends.
func TestMatrixF9DetachInputProtectedBoundaries(t *testing.T) {
	for _, text := range []string{
		"\x1b[200~\x1b[20~\x1b[201~",
		"\x1b\x1b[200~\x1b[20~\x1b[201~",
		"\x1b]title\x1b[20~\x07",
		"\x1bPdata\x1b[20~\x1b\\",
		"\x1bPdata\x07\x1b[20~\x1b\\", "\x1b_data\x07\x1b[20~\x1b\\",
		"\x1b^data\x07\x1b[20~\x1b\\", "\x1bXdata\x07\x1b[20~\x1b\\",
	} {
		var input MatrixF9DetachInput
		var output []byte
		for i := range len(text) {
			forward, detached := input.Consume([]byte{text[i]})
			if detached {
				t.Fatalf("protected input detached at %d: %q", i, text)
			}
			output = append(output, forward...)
		}
		if input.Protected() || string(output) != text {
			t.Fatalf("protected region not finished or changed: %+v, %q", input, output)
		}
		forward, detached := input.Consume([]byte("\x1b[20~"))
		if !detached || len(forward) != 0 {
			t.Fatalf("F9 after protected region = %q, %v", forward, detached)
		}
	}
	var input MatrixF9DetachInput
	input.Consume([]byte("\x1b[200~"))
	input.Flush()
	if _, detached := input.Consume([]byte("\x1b[20~")); detached {
		t.Fatal("idle flush unprotected an active paste")
	}
}

// matrix/hardened: exact matching never accepts only a prefix or extra Alt ESC.
func TestMatrixF9DetachExactTokens(t *testing.T) {
	for _, seq := range matrixF9DetachSeqs {
		for _, token := range [][]byte{seq[:len(seq)-1], append([]byte{0x1b}, seq...), append(append([]byte(nil), seq...), 'x')} {
			if MatrixIsF9Detach(token) {
				t.Fatalf("non-F9 complete token matched %q", token)
			}
		}
	}
	var input MatrixF9DetachInput
	input.Consume([]byte{0x1b})
	if !input.HasPendingEscape() || string(input.Flush()) != "\x1b" || input.Protected() {
		t.Fatalf("standalone Esc flush failed: %+v", input)
	}
}

// matrix/hardened: embedded routing uses the same complete-token boundaries.
func TestMatrixInputTokenLength(t *testing.T) {
	for _, token := range []string{"\x1b[20~", "\x1b\x1b[20~", "\x1b[57372;1u", "\x1b[1;5D", "\x1bOP", "\x1b(0", "\x1b]a\x1b[20~\x07", "\x1bPa\x1b[20~\x1b\\", "\x1bPa\x07\x1b[20~\x1b\\", "\x1b_a\x07\x1b[20~\x1b\\", "\x1b^a\x07\x1b[20~\x1b\\", "\x1bXa\x07\x1b[20~\x1b\\"} {
		if n, partial := MatrixInputTokenLength([]byte(token+"tail"), false); partial || n != len(token) {
			t.Fatalf("token %q = %d, %v", token, n, partial)
		}
		for i := 1; i < len(token); i++ {
			if n, partial := MatrixInputTokenLength([]byte(token[:i]), false); !partial || n != 0 {
				t.Fatalf("partial %q = %d, %v", token[:i], n, partial)
			}
		}
	}
}
