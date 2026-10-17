package session

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// matrix/hardened: SSH and mosh's shared forwarding path recognizes F9 across
// reads and preserves every byte in paste, modified keys and ANSI strings.
func TestMatrixF9RemoteDetach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"whole", []string{"prefix\x1b[20~discard"}, "prefix"},
		{"split", []string{"prefix\x1b", "[20", "~discard"}, "prefix"},
		{"kitty", []string{"\x1b[573", "72;1u"}, ""},
		{"paste", []string{"\x1b[200~", "\x1b[20~", "\x1b[201~", "\x1b[20~"}, "\x1b[200~\x1b[20~\x1b[201~"},
		{"opaque", []string{"\x1bPpayload", "\x1b[20~", "\x1b\\", "\x1b[20~"}, "\x1bPpayload\x1b[20~\x1b\\"},
		{"dcs_bel", []string{"\x1bPpayload\x07", "\x1b[20~", "\x1b\\", "\x1b[20~"}, "\x1bPpayload\x07\x1b[20~\x1b\\"},
		{"modified", []string{"\x1b[20;2~\x1b[20;3~\x1b[20;5~\x1b[57372;1:3u", "\x1b[20~"}, "\x1b[20;2~\x1b[20;3~\x1b[20;5~\x1b[57372;1:3u"},
		{"alt_prefix", []string{"\x1b", "\x1b[20~", "\x1b[20~"}, "\x1b\x1b[20~"},
		{"alt_q_split", []string{"\x1b", "q", "\x1b[20~"}, "\x1bq"},
		{"ctrl_first", []string{"prefix\x11middle\x1b[20~"}, "prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			input := sshAttachInput{writer: &output}
			for i, chunk := range tc.chunks {
				detached, err := input.forward([]byte(chunk))
				if err != nil || detached != (i == len(tc.chunks)-1) {
					t.Fatalf("chunk %d: detached=%v error=%v", i, detached, err)
				}
			}
			if !input.detachRequested.Load() || output.String() != tc.want || input.result(errors.New("transport stopped")) != nil {
				t.Fatalf("detach=%v output=%q want %q", input.detachRequested.Load(), output.String(), tc.want)
			}
		})
	}
}

// matrix/hardened: F9 intent must be observable before a prefix write blocks,
// otherwise an SSH/mosh exit racing that write is misreported as failure.
func TestMatrixF9RemoteDetachOrdering(t *testing.T) {
	writer := &sshAttachBarrierWriter{entered: make(chan struct{}), release: make(chan struct{})}
	input := sshAttachInput{writer: writer}
	done := make(chan bool, 1)
	go func() { detached, _ := input.forward([]byte("prefix\x1b[20~discard")); done <- detached }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		close(writer.release)
		t.Fatal("forwarding never reached the writer")
	}
	if got := input.result(errors.New("SSH exited")); got != nil {
		t.Errorf("intentional F9 detach reported failure: %v", got)
	}
	close(writer.release)
	select {
	case detached := <-done:
		if !detached || writer.bytes.String() != "prefix" {
			t.Fatalf("detached=%v output=%q", detached, writer.bytes.String())
		}
	case <-time.After(time.Second):
		t.Fatal("prefix writer never completed")
	}
}

// matrix/hardened: a timeout forwards standalone Esc without applying Alt+Q
// detection to the next read or ending an active bracketed paste.
func TestMatrixF9RemoteLoneEscape(t *testing.T) {
	var output bytes.Buffer
	input := sshAttachInput{writer: &output}
	input.forward([]byte{0x1b})
	if err := input.flushEscape(); err != nil {
		t.Fatal(err)
	}
	if detached, err := input.forward([]byte("q")); detached || err != nil || output.String() != "\x1bq" {
		t.Fatalf("standalone Esc/q: detached=%v err=%v output=%q", detached, err, output.String())
	}
	input.forward([]byte("\x1b[200~"))
	input.flushEscape()
	if detached, _ := input.forward([]byte("\x1b[20~")); detached {
		t.Fatal("idle gap unprotected active paste")
	}
}
