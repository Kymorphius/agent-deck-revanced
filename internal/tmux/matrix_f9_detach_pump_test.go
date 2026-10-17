//go:build !windows

package tmux

import (
	"context"
	"testing"
	"time"
)

// matrix/hardened: test the actual poll/read pump, including the 32-byte read
// boundary and earlier switch/control-key precedence in the same input batch.
func TestMatrixF9DetachPump(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chunks  []string
		want    string
		outcome SwitchIntent
	}{
		{"whole", []string{"hello\x1b[20~tail"}, "hello", SwitchNone},
		{"split", []string{"hello\x1b", "[20", "~tail"}, "hello", SwitchNone},
		{"read_boundary", []string{"abcdefghijklmnopqrstuvwxyz1234\x1b[20~tail"}, "abcdefghijklmnopqrstuvwxyz1234", SwitchNone},
		{"switch_before_f9", []string{"a\x13b\x1b[20~"}, "a", SwitchRequested},
		{"f9_before_switch", []string{"a\x1b[20~b\x13"}, "a", SwitchNone},
		{"ctrl_before_f9", []string{"a\x11b\x1b[20~"}, "a", SwitchNone},
		{"rebound_ctrl", []string{"a\x04b\x1b[20~"}, "a", SwitchNone},
		{"alt_q_original", []string{"\x1bq"}, "", SwitchNone},
		{"alt_q_split", []string{"\x1b", "q", "\x1b[20~"}, "\x1bq", SwitchNone},
		{"paste", []string{"\x1b[200~pasted", "\x1b[20~", "\x1b[201~", "\x1b[20~"}, "\x1b[200~pasted\x1b[20~\x1b[201~", SwitchNone},
		{"esc_then_paste", []string{"\x1b\x1b[200~pasted", "\x1b[20~", "\x1b[201~", "\x1b[20~"}, "\x1b\x1b[200~pasted\x1b[20~\x1b[201~", SwitchNone},
		{"alt_f9", []string{"\x1b", "\x1b[20~", "\x1b[20~"}, "\x1b\x1b[20~", SwitchNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := AttachOptions{DetachByte: 17, SwitchKeyByte: 19}
			if tc.name == "rebound_ctrl" {
				opts.DetachByte = 4
			}
			pump, w, out := newTestPump(t, opts)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runPump(ctx, pump)
			for _, chunk := range tc.chunks {
				if _, err := w.Write([]byte(chunk)); err != nil {
					t.Fatal(err)
				}
				// Separate reads without expiring a standalone ESC prefix.
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case result := <-done:
				if !result.interrupted || result.outcome != tc.outcome {
					t.Fatalf("result %+v want interrupt with %v", result, tc.outcome)
				}
			case <-time.After(time.Second):
				t.Fatal("F9 did not return from the input pump")
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("forwarded %q want %q", got, tc.want)
			}
		})
	}
}

// matrix/hardened: the F9 parser must not swallow standalone Esc or survive
// teardown as another reader. Observe all output after the pump has stopped.
func TestMatrixF9DetachPumpLoneEscape(t *testing.T) {
	for _, armed := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_quarantine", true: "during_quarantine"}[armed], func(t *testing.T) {
			pump, w, out := newTestPump(t, AttachOptions{})
			if armed {
				pump.startTime = time.Now()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runPump(ctx, pump)
			if _, err := w.Write([]byte{0x1b}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(3 * AttachStdinPollInterval)
			if _, err := w.Write([]byte("\x1b[20~")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("F9 did not return after lone Esc")
			}
			if out.String() != "\x1b" {
				t.Fatalf("lone Esc forwarded %q", out.String())
			}
		})
	}
}
