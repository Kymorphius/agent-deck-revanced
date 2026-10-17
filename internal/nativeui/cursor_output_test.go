package nativeui

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func renderCursorTestFrame(body string, frame cursorFrame) []byte {
	return []byte(ansi.CursorHomePosition + encodeCursorFrame(body, frame) + ansi.CursorPosition(0, frame.rows))
}

func TestCursorWriterStripsMetadataAndBindsPositionToSuccessfulFrame(t *testing.T) {
	var terminal bytes.Buffer
	writer := cursorFrameWriter{output: &terminal}
	frame := cursorFrame{anchor: cursorAnchor{2, 13}, width: 80, height: 24, rows: 3}
	first := renderCursorTestFrame("title\r\nfield\r\nfooter", frame)
	if n, err := writer.Write(first); n != len(first) || err != nil {
		t.Fatalf("write returned %d, %v", n, err)
	}
	wantTail := ansi.CursorPosition(0, 3) + ansi.CursorPosition(13, 2)
	if strings.Contains(terminal.String(), "agent-deck-native-cursor") || !strings.HasSuffix(terminal.String(), wantTail) {
		t.Fatalf("private metadata leaked or cursor did not follow renderer CUP: %q", terminal.String())
	}
	terminal.Reset()
	// The unchanged first line can be skipped by Bubble Tea's line diff.
	diff := []byte(ansi.CursorHomePosition + "\nchanged\r\n" + ansi.CursorPosition(0, 3))
	writer.Write(diff)
	if !strings.HasSuffix(terminal.String(), wantTail) {
		t.Fatal("line diff lost the same already-rendered anchor")
	}
	terminal.Reset()
	frame.anchor = cursorAnchor{}
	writer.Write(renderCursorTestFrame("list\r\nitem\r\nfooter", frame))
	if strings.HasSuffix(terminal.String(), ansi.CursorPosition(13, 2)) {
		t.Fatal("unfocused frame retained prior input positioning")
	}
}

func TestCursorWriterDoesNotPositionSetupTeardownOrMalformedWrites(t *testing.T) {
	valid := cursorFrame{anchor: cursorAnchor{2, 13}, width: 80, height: 24, rows: 3}
	for _, input := range [][]byte{
		[]byte(ansi.SetAltScreenSaveCursorMode), []byte(ansi.CursorHomePosition),
		[]byte(ansi.ResetAltScreenSaveCursorMode), []byte(ansi.ShowCursor),
		[]byte(encodeCursorFrame("", valid)),
		[]byte(ansi.CursorHomePosition + cursorFramePrefix + "bad\afooter" + ansi.CursorPosition(0, 3)),
		[]byte(ansi.CursorHomePosition + cursorFramePrefix + "2;13;80;24;3"),
		renderCursorTestFrame("bad", cursorFrame{anchor: cursorAnchor{4, 13}, width: 80, height: 24, rows: 3}),
		renderCursorTestFrame("bad", cursorFrame{anchor: cursorAnchor{2, 81}, width: 80, height: 24, rows: 3}),
		[]byte(ansi.CursorHomePosition + encodeCursorFrame(encodeCursorFrame("duplicate", valid), valid) + ansi.CursorPosition(0, 3)),
	} {
		var terminal bytes.Buffer
		writer := cursorFrameWriter{output: &terminal}
		writer.Write(renderCursorTestFrame("title\r\nfield\r\nfooter", valid))
		terminal.Reset()
		writer.Write(input)
		if strings.Contains(terminal.String(), "agent-deck-native-cursor") || strings.HasSuffix(terminal.String(), ansi.CursorPosition(13, 2)) {
			t.Fatalf("invalid/lifecycle write leaked or repositioned: %q", terminal.String())
		}
		terminal.Reset()
		writer.Write([]byte(ansi.CursorHomePosition + "\nchanged\r\n" + ansi.CursorPosition(0, 3)))
		if strings.HasSuffix(terminal.String(), ansi.CursorPosition(13, 2)) {
			t.Fatal("reset write retained a stale anchor")
		}
	}
}

type failingCursorOutput struct{ err error }

func (output failingCursorOutput) Write(p []byte) (int, error) {
	return len(p) / 2, output.err
}

func TestCursorWriterRejectsShortWritesAndErrorsWithoutAdvancingAnchor(t *testing.T) {
	frame := cursorFrame{anchor: cursorAnchor{2, 13}, width: 80, height: 24, rows: 3}
	for _, err := range []error{nil, errors.New("terminal closed")} {
		writer := cursorFrameWriter{output: failingCursorOutput{err: err}, previous: frame}
		_, got := writer.Write(renderCursorTestFrame("title\r\nfield\r\nfooter", frame))
		want := err
		if want == nil {
			want = io.ErrShortWrite
		}
		if !errors.Is(got, want) || writer.previous != (cursorFrame{}) {
			t.Fatalf("failed write retained successful state: err=%v previous=%+v", got, writer.previous)
		}
	}
}

type cursorCapture struct {
	mu     sync.Mutex
	writes [][]byte
	notify chan struct{}
}

func (capture *cursorCapture) Write(p []byte) (int, error) {
	capture.mu.Lock()
	capture.writes = append(capture.writes, bytes.Clone(p))
	capture.mu.Unlock()
	select {
	case capture.notify <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (capture *cursorCapture) waitFor(t *testing.T, body, tail string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		capture.mu.Lock()
		found := false
		for _, p := range capture.writes {
			if bytes.Contains(p, []byte(body)) && bytes.HasSuffix(p, []byte(tail)) {
				found = true
			}
		}
		capture.mu.Unlock()
		if found {
			return
		}
		select {
		case <-capture.notify:
		case <-deadline.C:
			t.Fatalf("real renderer did not position %q at %q", body, tail)
		}
	}
}

type cursorRendererModel struct{ view string }
type cursorRendererView string

func (m cursorRendererModel) Init() tea.Cmd { return nil }
func (m cursorRendererModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if view, ok := msg.(cursorRendererView); ok {
		m.view = string(view)
	}
	return m, nil
}
func (m cursorRendererModel) View() string { return m.view }

func TestRealBubbleTeaRendererPositionsAfterItsFinalCUPAndLineDiff(t *testing.T) {
	capture := &cursorCapture{notify: make(chan struct{}, 1)}
	writer := &cursorFrameWriter{output: capture}
	frame := cursorFrame{anchor: cursorAnchor{2, 13}, width: 80, height: 24, rows: 3}
	view := encodeCursorFrame("title\nfield first\nfooter", frame)
	program := tea.NewProgram(cursorRendererModel{view}, tea.WithInput(nil), tea.WithOutput(writer), tea.WithAltScreen(), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	defer func() {
		program.Quit()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("renderer run: %v", err)
			}
		case <-time.After(5 * time.Second):
			program.Kill()
			t.Error("renderer did not finish")
		}
	}()
	tail := ansi.CursorPosition(0, 3) + ansi.CursorPosition(13, 2)
	capture.waitFor(t, "field first", tail)
	program.Send(cursorRendererView(encodeCursorFrame("title\nfield changed\nfooter", frame)))
	capture.waitFor(t, "field changed", tail)
	frame.anchor = cursorAnchor{3, 9}
	program.Send(cursorRendererView(encodeCursorFrame("title\nfield changed\nfooter focused", frame)))
	capture.waitFor(t, "footer focused", ansi.CursorPosition(0, 3)+ansi.CursorPosition(9, 3))
	capture.mu.Lock()
	defer capture.mu.Unlock()
	for _, p := range capture.writes {
		if bytes.Contains(p, []byte("agent-deck-native-cursor")) {
			t.Fatal("private metadata reached the real renderer output")
		}
	}
}
