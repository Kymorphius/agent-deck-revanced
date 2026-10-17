package tmux

// matrix/hardened: tests for Alt+Q detach (matrix_alt_detach.go).

import "testing"

func TestMatrixAltQ_EveryEncodingDetaches(t *testing.T) {
	for _, in := range []string{"\x1bq", "\x1b[27;3;113~", "\x1b[113;3u", "œ"} {
		idx, outcome := resolveAttachInterrupt([]byte(in), 17, AttachOptions{})
		if idx != 0 || outcome != SwitchNone {
			t.Errorf("%q: idx=%d outcome=%v, want detach at 0", in, idx, outcome)
		}
	}
}

func TestMatrixAltQ_OnlyAWholeReadCounts(t *testing.T) {
	for _, in := range []string{
		"\x1b", "q", // Esc then q typed as two reads
		"cœur",                // typed or pasted text containing œ
		"\x1b[200~œ\x1b[201~", // a bracketed paste of just œ
		"\x1bQ", "Œ",          // Alt+Shift+Q is not Alt+Q
		"\x1bqx", // Alt+Q coalesced with more input
	} {
		if idx, _ := resolveAttachInterrupt([]byte(in), 17, AttachOptions{}); idx >= 0 {
			t.Errorf("%q: detached at %d, want forwarded", in, idx)
		}
	}
}

func TestMatrixAltQ_CtrlQStillDetaches(t *testing.T) {
	if idx, _ := resolveAttachInterrupt([]byte("ab\x11"), 17, AttachOptions{}); idx != 2 {
		t.Fatalf("ctrl+q: idx=%d, want 2", idx)
	}
}
