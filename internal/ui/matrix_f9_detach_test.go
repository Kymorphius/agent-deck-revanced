package ui

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// matrix/hardened: exercise the actual bytewise fd reader and generation
// cancellation, not just a synthetic F9 KeyMsg or a prefix matcher.
func TestMatrixF9RawRouterReturnsWithoutForwardingKey(t *testing.T) {
	for _, f9 := range []string{"\x1b[20~", "\x1b[20;1~", "\x1b[57372;1u"} {
		t.Run(f9, func(t *testing.T) {
			readFile, writeFile, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer readFile.Close()
			defer writeFile.Close()
			router := NewSessionInputRouter(readFile)
			defer router.Deactivate()
			child := newConnectingCapture()
			router.Activate(child, terminalCellRect{Width: 80, Height: 24}, 0)
			result := make(chan []byte, 1)
			go func() {
				buf := make([]byte, 32)
				for {
					n, err := router.Read(buf)
					if n > 0 || err != nil {
						result <- append([]byte(nil), buf[:n]...)
						return
					}
				}
			}()
			if _, err := io.WriteString(writeFile, "draft"); err != nil {
				t.Fatal(err)
			}
			awaitConnectingBytes(t, child, "draft")
			if _, err := writeFile.Write([]byte{0x1b}); err != nil {
				t.Fatal(err)
			}
			awaitConnectingBytes(t, child, "draft\x1b")
			// The native reader also reads one byte at a time. Separate writes
			// make split-sequence handling explicit without artificial sleeps.
			for _, b := range []byte(f9 + "tail") {
				if _, err := writeFile.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case got := <-result:
				if !bytes.Equal(got, []byte{0x11}) {
					t.Fatalf("dashboard signal = %q, want the existing Ctrl+Q detach signal", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("F9 did not return from the raw session input router")
			}
			awaitConnectingSignal(t, child.closed, "F9 did not cancel the generation writer")
			if got := child.snapshot(); got != "draft\x1b" {
				t.Fatalf("F9 or its coalesced suffix reached the session: %q", got)
			}
		})
	}
}

// matrix/hardened: terminal-string bodies stay streaming input instead of
// accumulating in the bounded connecting buffer while awaiting a terminator.
func TestMatrixF9RawRouterStreamsOpaqueStrings(t *testing.T) {
	router, child := newRoutingTestInput(terminalCellRect{Width: 80, Height: 24})
	input := "\x1b]title" + strings.Repeat("payload", 10000) + "\x1b[20~"
	if got := routeTestBytes(t, router, input); len(got) != 0 {
		t.Fatalf("opaque string emitted a dashboard key: %q", got)
	}
	if !router.active || child.String() != input || len(router.rawBuf) != 0 {
		t.Fatalf("opaque string did not stream: active=%v bytes=%d pending=%d", router.active, child.Len(), len(router.rawBuf))
	}
	if got := routeTestBytes(t, router, "\a\x1b[20~"); !bytes.Equal(got, []byte{0x11}) {
		t.Fatalf("F9 after the string terminator did not return: %q", got)
	}
	if router.active || child.String() != input+"\a" {
		t.Fatal("F9 leaked to the pane or left opaque-string routing active")
	}
}

// matrix/hardened: BEL is an OSC terminator, but remains literal payload in
// DCS/APC/PM/SOS. F9 after BEL in those strings cannot escape their ST boundary.
func TestMatrixF9RawRouterBELDoesNotTerminateOtherStrings(t *testing.T) {
	for name, introducer := range map[string]byte{"DCS": 'P', "APC": '_', "PM": '^', "SOS": 'X'} {
		t.Run(name, func(t *testing.T) {
			router, child := newRoutingTestInput(terminalCellRect{Width: 80, Height: 24})
			input := "\x1b" + string(introducer) + "payload\a\x1b[20~\x1b\\"
			for _, b := range []byte(input) {
				if got := routeTestBytes(t, router, string([]byte{b})); len(got) != 0 {
					t.Fatalf("%s payload returned to the dashboard: %q", name, got)
				}
			}
			if !router.active || child.String() != input {
				t.Fatalf("%s payload changed: active=%v child=%q", name, router.active, child.String())
			}
			if got := routeTestBytes(t, router, "\x1b[20~"); !bytes.Equal(got, []byte{0x11}) {
				t.Fatalf("F9 after %s ST terminator did not return: %q", name, got)
			}
		})
	}
}

// matrix/hardened: a pasted F9 encoding, a modified key, and bytes inside
// terminal strings cannot become return commands, even across parser reads.
func TestMatrixF9RawRouterPreservesNonReturnInput(t *testing.T) {
	for name, input := range map[string]string{
		"paste":      bracketedPasteStart + "text\x1b[20~\x1b[57372u" + bracketedPasteEnd,
		"Alt+F9":     "\x1b[20;3~",
		"Ctrl+F9":    "\x1b[20;5~",
		"AltPrefix":  "\x1b\x1b[20~",
		"release":    "\x1b[57372;1:3u",
		"other CSI":  "\x1b[120~",
		"OSC":        "\x1b]title\x1b[20~\a",
		"DCS":        "\x1bPdata\x1b[20~\x1b\\",
		"APC":        "\x1b_data\x1b[20~\x1b\\",
		"plain text": "f9 and ordinary input",
	} {
		t.Run(name, func(t *testing.T) {
			router, child := newRoutingTestInput(terminalCellRect{Width: 80, Height: 24})
			for _, b := range []byte(input) {
				if got := routeTestBytes(t, router, string([]byte{b})); len(got) != 0 {
					t.Fatalf("non-return input emitted dashboard signal: %q", got)
				}
			}
			if !router.active || child.String() != input {
				t.Fatalf("non-return input changed: active=%v child=%q want=%q", router.active, child.String(), input)
			}
		})
	}
}

// matrix/hardened: returning from a focused session must close its input
// generation through the existing detach path rather than sending F9 to it.
func TestMatrixF9KeyMsgReturnsFromEmbeddedSession(t *testing.T) {
	home, sender := armEmbeddedSessionWithFakeSender(t)
	before := home.embeddedGeneration
	model, cmd := home.Update(tea.KeyMsg{Type: tea.KeyF9})
	home = model.(*Home)
	if home.insertMode || home.embeddedMode {
		t.Fatal("F9 did not return focus to the session list")
	}
	if sender.closeCount.Load() != 1 || sender.namedKeyCount.Load() != 0 {
		t.Fatal("F9 did not close the session input without forwarding the key")
	}
	if home.embeddedGeneration <= before {
		t.Fatal("F9 did not cancel the embedded input generation")
	}
	if cmd == nil {
		t.Fatal("F9 did not schedule a fresh detached terminal preview")
	}
}

// matrix/hardened: F9 also leaves ordinary insert mode, while its existing
// Escape return behavior stays intact.
func TestMatrixF9KeyMsgReturnsFromInsertMode(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyF9, tea.KeyEsc} {
		t.Run(key.String(), func(t *testing.T) {
			home, sender := armInsertModeWithFakeKeySender(t)
			model, _ := home.Update(tea.KeyMsg{Type: key})
			home = model.(*Home)
			if home.insertMode || sender.closeCount.Load() != 1 {
				t.Fatalf("%s did not leave insert mode and close its sender", key)
			}
		})
	}
}

// matrix/hardened: only the unmodified, non-paste F9 event is a return key;
// embedded Escape and text remain owned by the focused terminal.
func TestMatrixF9KeyMsgPreservesSessionInput(t *testing.T) {
	for name, key := range map[string]tea.KeyMsg{
		"Alt+F9":   {Type: tea.KeyF9, Alt: true},
		"paste F9": {Type: tea.KeyF9, Paste: true},
		"Escape":   {Type: tea.KeyEsc},
		"text":     {Type: tea.KeyRunes, Runes: []rune("f9")},
	} {
		t.Run(name, func(t *testing.T) {
			home, sender := armEmbeddedSessionWithFakeSender(t)
			model, _ := home.Update(key)
			home = model.(*Home)
			if !home.insertMode || !home.embeddedMode || sender.closeCount.Load() != 0 {
				t.Fatalf("%s unexpectedly returned from the session", name)
			}
			if name != "text" && sender.namedKeyCount.Load() != 1 {
				t.Fatalf("%s was not forwarded to the session", name)
			}
			home.exitInsertMode()
		})
	}
}

// matrix/hardened: help and active-session hints expose F9 alongside the
// applicable compatibility keys, without inventing embedded Alt+Q support.
func TestMatrixF9ReturnHintsMatchLayout(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		help := NewHelpOverlay()
		help.SetSize(120, 50)
		help.SetEmbeddedLayout(embedded)
		help.Show()
		plain := ansi.Strip(help.View())
		if !strings.Contains(plain, "F9 / Ctrl+Q") {
			t.Fatalf("embedded=%v help did not advertise F9: %s", embedded, plain)
		}
		if embedded && strings.Contains(plain, "Ctrl+Q / Alt+Q") {
			t.Fatal("embedded help advertised unsupported Alt+Q detach")
		}
	}
	home, _ := armEmbeddedSessionWithFakeSender(t)
	if plain := ansi.Strip(home.renderInsertModeBar()); !strings.Contains(plain, "F9 / Ctrl+Q detach") {
		t.Fatalf("active embedded hint did not advertise F9: %s", plain)
	}
	home.exitInsertMode()
}
