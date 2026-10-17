package nativeui

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
)

const cursorFramePrefix = "\x1b]777;agent-deck-native-cursor;v1;"

// encodeCursorFrame attaches zero-cell metadata to the first line of its
// actual View. A changed focus/geometry changes that line, so the renderer's
// line-diff cannot omit a newly selected anchor.
func encodeCursorFrame(view string, frame cursorFrame) string {
	return fmt.Sprintf("%s%d;%d;%d;%d;%d\a%s", cursorFramePrefix, frame.anchor.row, frame.anchor.column, frame.width, frame.height, frame.rows, view)
}

// cursorFrameWriter only adjusts complete alt-screen renderer frames. It does
// not move the cursor for setup, teardown, child-process, or reset writes.
// State is advanced only after a complete successful terminal write.
type cursorFrameWriter struct {
	mu       sync.Mutex
	output   io.Writer
	previous cursorFrame
}

func (w *cursorFrameWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	clean, frame, markers := stripCursorFrames(p)
	rows, complete := rendererFrameRows(clean)
	if !complete || markers > 1 {
		frame = cursorFrame{}
	} else if markers == 0 {
		// A line-diff may skip the unchanged metadata line. Only an unchanged
		// frame height can retain its previous already-rendered anchor.
		frame = w.previous
	}
	valid := complete && markers <= 1 && frame.valid(rows)
	if !valid {
		frame = cursorFrame{}
	}
	output := clean
	if valid && frame.anchor.row != 0 {
		output = append(output, ansi.CursorPosition(frame.anchor.column, frame.anchor.row)...)
	}
	if len(output) > 0 {
		n, err := w.output.Write(output)
		if err == nil && n != len(output) {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.previous = cursorFrame{}
			// No complete source frame was consumed on a failed output. Bubble
			// Tea does not retry renderer writes; do not claim successful state.
			return 0, err
		}
	}
	w.previous = frame
	return len(p), nil
}

func (frame cursorFrame) valid(rows int) bool {
	if frame.width < 1 || frame.width > 32767 || frame.height < 1 || frame.height > 32767 || frame.rows != rows || rows > frame.height {
		return false
	}
	if frame.anchor.row == 0 || frame.anchor.column == 0 {
		return frame.anchor.row == 0 && frame.anchor.column == 0
	}
	return frame.anchor.row > 0 && frame.anchor.row <= rows && frame.anchor.column > 0 && frame.anchor.column <= frame.width
}

// stripCursorFrames strips even malformed or incomplete private metadata.
// These private OSC sequences must never reach Windows Terminal.
func stripCursorFrames(p []byte) ([]byte, cursorFrame, int) {
	clean := make([]byte, 0, len(p))
	frame, count := cursorFrame{}, 0
	for {
		start := bytes.Index(p, []byte(cursorFramePrefix))
		if start < 0 {
			return append(clean, p...), frame, count
		}
		clean = append(clean, p[:start]...)
		p = p[start+len(cursorFramePrefix):]
		count++
		end := bytes.IndexByte(p, '\a')
		if end < 0 {
			return clean, cursorFrame{}, count
		}
		if count == 1 {
			frame = parseCursorFrame(string(p[:end]))
		}
		p = p[end+1:]
	}
}

func parseCursorFrame(value string) cursorFrame {
	parts := strings.Split(value, ";")
	if len(parts) != 5 {
		return cursorFrame{}
	}
	var numbers [5]int
	for i, part := range parts {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return cursorFrame{}
		}
		number, err := strconv.Atoi(part)
		if err != nil {
			return cursorFrame{}
		}
		numbers[i] = number
	}
	return cursorFrame{anchor: cursorAnchor{numbers[0], numbers[1]}, width: numbers[2], height: numbers[3], rows: numbers[4]}
}

func rendererFrameRows(p []byte) (int, bool) {
	if !bytes.HasPrefix(p, []byte(ansi.CursorHomePosition)) || len(p) <= len(ansi.CursorHomePosition)*2 || p[len(p)-1] != 'H' {
		return 0, false
	}
	start := bytes.LastIndex(p, []byte("\x1b["))
	if start <= 0 {
		return 0, false
	}
	position := string(p[start+2 : len(p)-1])
	if position == "" {
		return 1, true
	}
	parts := strings.Split(position, ";")
	if len(parts) != 2 || (parts[1] != "" && parts[1] != "1") || parts[0] == "" || strings.IndexFunc(parts[0], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	rows, err := strconv.Atoi(parts[0])
	return rows, err == nil && rows > 0 && rows <= 32767
}
