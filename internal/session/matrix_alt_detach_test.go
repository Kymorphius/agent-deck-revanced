package session

// matrix/hardened: Alt+Q detaches remote (ssh/mosh) attaches too.

import (
	"bytes"
	"testing"
)

func TestMatrixSSHAttachInput_AltQDetaches(t *testing.T) {
	for _, in := range []string{"\x1bq", "œ"} {
		var w bytes.Buffer
		input := sshAttachInput{writer: &w}
		detached, err := input.forward([]byte(in))
		if !detached || err != nil || !input.detachRequested.Load() || w.Len() != 0 {
			t.Errorf("%q: detached=%v err=%v requested=%v forwarded=%q; want detach, nothing forwarded",
				in, detached, err, input.detachRequested.Load(), w.String())
		}
	}
}

func TestMatrixSSHAttachInput_TextWithOeIsForwarded(t *testing.T) {
	var w bytes.Buffer
	input := sshAttachInput{writer: &w}
	detached, err := input.forward([]byte("cœur"))
	if detached || err != nil || w.String() != "cœur" {
		t.Fatalf("cœur: detached=%v err=%v forwarded=%q; want forwarded as is", detached, err, w.String())
	}
}
