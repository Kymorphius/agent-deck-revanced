// matrix/hardened: a rendered viewer never forwards queries to a second console.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

func TestPaintSnapshotOmitsRawDeviceQueries(t *testing.T) {
	r := terminalhost.Response{Render: "native screen", Output: []byte("\x1b[c\x1b[6n"), CursorX: 2, CursorY: 3}
	var output bytes.Buffer
	if err := paintSnapshot(&output, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "native screen") || !strings.Contains(output.String(), "\x1b[4;3H") {
		t.Fatalf("bad snapshot: %q", output.String())
	}
	if strings.Contains(output.String(), "\x1b[c") || strings.Contains(output.String(), "\x1b[6n") {
		t.Fatal("raw device query reached client console")
	}
}

// matrix/hardened: initial/reconnect paint sets exactly the allowlisted paste
// mode, with no replay of unrelated child modes or clipboard control sequences.
func TestPaintSnapshotAppliesParsedBracketedPasteMode(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		r := terminalhost.Response{
			Render: "paste screen", BracketedPaste: enabled,
			Output: []byte("\x1b[?1000h\x1b]52;c;c2VjcmV0\a\x1b[c\x1b[6n"),
		}
		var output bytes.Buffer
		if err := paintSnapshot(&output, r); err != nil {
			t.Fatal(err)
		}
		want, unwanted := "\x1b[?2004l", "\x1b[?2004h"
		if enabled {
			want, unwanted = unwanted, want
		}
		if strings.Count(output.String(), want) != 1 || strings.Contains(output.String(), unwanted) {
			t.Fatalf("paint paste mode=%v: %q", enabled, output.String())
		}
		for _, raw := range []string{"\x1b[?1000h", "\x1b]52", "\x1b[c", "\x1b[6n"} {
			if strings.Contains(output.String(), raw) {
				t.Fatalf("paint replayed raw child control sequence %q", raw)
			}
		}
	}
}

// matrix/hardened: the capture poll must paint a mode-only change, while new
// raw output with an unchanged parsed display must not cause a query replay.
func TestSnapshotDisplayChangedIncludesPasteModeOnlyChanges(t *testing.T) {
	before := terminalhost.Response{Render: "unchanged screen", CursorX: 3, CursorY: 4}
	for _, field := range []string{"render", "cursor_x", "cursor_y", "paste_enable", "paste_disable", "raw_output"} {
		t.Run(field, func(t *testing.T) {
			previous, current := before, before
			want := true
			switch field {
			case "render":
				current.Render = "new screen"
			case "cursor_x":
				current.CursorX++
			case "cursor_y":
				current.CursorY++
			case "paste_enable":
				current.BracketedPaste = true
			case "paste_disable":
				previous.BracketedPaste = true
			case "raw_output":
				current.Output, current.HeadOffset = []byte("\x1b[c"), 4
				want = false
			}
			if got := snapshotDisplayChanged(previous, current); got != want {
				t.Fatalf("snapshotDisplayChanged=%v, want %v", got, want)
			}
		})
	}
}
