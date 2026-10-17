// matrix/hardened: lifecycle and cursor tests do not touch production sessions.
package terminalhost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeProcess struct {
	mu         sync.Mutex
	input      bytes.Buffer
	reader     *io.PipeReader
	writer     *io.PipeWriter
	done       chan struct{}
	once       sync.Once
	closeCount int
	cols, rows int
}

func newFakeProcess() *fakeProcess {
	r, w := io.Pipe()
	return &fakeProcess{reader: r, writer: w, done: make(chan struct{})}
}
func (p *fakeProcess) PID() uint32       { return 4242 }
func (p *fakeProcess) Input() io.Writer  { return fakeInput{p} }
func (p *fakeProcess) Output() io.Reader { return p.reader }
func (p *fakeProcess) Resize(cols, rows int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cols, p.rows = cols, rows
	return nil
}
func (p *fakeProcess) Wait() (uint32, error) { <-p.done; return 0, nil }
func (p *fakeProcess) Close() error {
	p.once.Do(func() { p.mu.Lock(); p.closeCount++; p.mu.Unlock(); _ = p.writer.Close(); close(p.done) })
	return nil
}

type fakeInput struct{ p *fakeProcess }

func (w fakeInput) Write(data []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.input.Write(data)
}

func testHost(t *testing.T) (*Server, *fakeProcess, string) {
	t.Helper()
	p := newFakeProcess()
	s, err := New(p, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0") // Test transport only; Windows production uses Named Pipe.
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.Serve(listener) }()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		select {
		case err := <-serveDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("host did not close")
		}
	})
	return s, p, listener.Addr().String()
}

func callTestHost(t *testing.T, address string, request Request) Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := Call(ctx, conn, request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func waitForOutput(t *testing.T, s *Server, expected uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.snapshot(0, false).HeadOffset >= expected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("output did not reach host")
}

func TestClientsDetachAndReconnectWithoutStoppingProcess(t *testing.T) {
	s, p, address := testHost(t)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close() // A disconnected viewer must not close the owned process.
	text := "first\r\n中文输出"
	if _, err = p.writer.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, s, uint64(len(text)))
	first := callTestHost(t, address, Request{Op: "capture"})
	if !first.Running || first.PID != 4242 || string(first.Output) != text || !strings.Contains(first.Screen, "中文输出") {
		t.Fatalf("bad initial capture: %+v", first)
	}
	if _, err = p.writer.Write([]byte("\r\nafter-detach")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, s, first.EndOffset+uint64(len("\r\nafter-detach")))
	second := callTestHost(t, address, Request{Op: "capture", After: first.EndOffset})
	if string(second.Output) != "\r\nafter-detach" || second.Truncated {
		t.Fatalf("bad reconnect delta: %+v", second)
	}
	callTestHost(t, address, Request{Op: "send", Data: []byte("hello\r")})
	p.mu.Lock()
	input := p.input.String()
	closes := p.closeCount
	p.mu.Unlock()
	if input != "hello\r" || closes != 0 {
		t.Fatalf("input=%q closes=%d", input, closes)
	}
	resized := callTestHost(t, address, Request{Op: "resize", Cols: 120, Rows: 40})
	if resized.Cols != 120 || resized.Rows != 40 {
		t.Fatalf("resize not observed: %+v", resized)
	}
	callTestHost(t, address, Request{Op: "stop"})
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not terminate owned process")
	}
}

func TestCaptureBoundsAndExplicitHistoryGap(t *testing.T) {
	s, p, _ := testHost(t)
	// Append directly to avoid making the VT parser process megabytes of filler.
	s.mu.Lock()
	s.history = bytes.Repeat([]byte{'x'}, HistoryBytes)
	s.head = HistoryBytes + 17
	s.mu.Unlock()
	r := s.snapshot(0, true)
	if !r.Truncated || r.StartOffset != 17 || len(r.Output) != MaxOutputBytes || r.EndOffset != 17+MaxOutputBytes || r.HeadOffset != HistoryBytes+17 {
		t.Fatalf("gap/chunk lost: %+v", r)
	}
	r = s.snapshot(r.EndOffset, true)
	if r.Truncated || len(r.Output) != MaxOutputBytes {
		t.Fatal("next contiguous capture should not report a gap")
	}
	r = s.snapshot(HistoryBytes+18, true)
	if r.OK || !strings.Contains(r.Error, "ahead") {
		t.Fatal("future cursor accepted")
	}
	_ = p.Close()
}

func TestNaturalExitRetainsFinalCaptureAndRejectsInput(t *testing.T) {
	s, p, address := testHost(t)
	_, _ = p.writer.Write([]byte("final output"))
	waitForOutput(t, s, uint64(len("final output")))
	_ = p.Close()
	deadline := time.Now().Add(time.Second)
	for s.snapshot(0, false).Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r := callTestHost(t, address, Request{Op: "capture"})
	if r.Running || r.ExitCode == nil || *r.ExitCode != 0 || !strings.Contains(r.Screen, "final output") {
		t.Fatalf("lost final state: %+v", r)
	}
	r, _ = s.handle(Request{Op: "send", Data: []byte("no")})
	if r.OK {
		t.Fatal("input accepted after process exit")
	}
}

func TestInvalidRequestsAndConcurrentClose(t *testing.T) {
	s, _, address := testHost(t)
	for _, request := range []Request{{Op: "resize", Cols: 0, Rows: 24}, {Op: "resize", Cols: 50000, Rows: 24}, {Op: "unknown"}, {Op: "send", Data: bytes.Repeat([]byte{'a'}, MaxInputBytes+1)}} {
		r, _ := s.handle(request)
		if r.OK {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A half-written JSON client cannot hold shutdown open.
	_, _ = io.WriteString(conn, "{\"op\":")
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Close() }()
	}
	wg.Wait()
}

func TestCallContextCancellationClosesOnlyClient(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Call(ctx, client, Request{Op: "status"}); done <- err }()
	var req Request
	if err := json.NewDecoder(server).Decode(&req); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled call succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled call stayed blocked")
	}
}

type delayedFinalProcess struct {
	*fakeProcess
	exited    chan struct{}
	allowTail chan struct{}
	finalOnce sync.Once
}

func (p *delayedFinalProcess) Wait() (uint32, error) { <-p.exited; return 7, nil }
func (p *delayedFinalProcess) Close() error {
	p.finalOnce.Do(func() { <-p.allowTail; _, _ = p.writer.Write([]byte("late final frame")); _ = p.fakeProcess.Close() })
	return nil
}

func TestExitPublishedOnlyAfterFinalOutputDrains(t *testing.T) {
	p := &delayedFinalProcess{fakeProcess: newFakeProcess(), exited: make(chan struct{}), allowTail: make(chan struct{})}
	s, err := New(p, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	close(p.exited)
	time.Sleep(10 * time.Millisecond)
	if r := s.snapshot(0, false); !r.Running || r.ExitCode != nil {
		t.Fatal("exit was published before final output")
	}
	close(p.allowTail)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r := s.snapshot(0, true)
		if !r.Running {
			if r.ExitCode == nil || *r.ExitCode != 7 || string(r.Output) != "late final frame" {
				t.Fatalf("incomplete final state: %+v", r)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("final state did not become available")
}

func TestHostAnswersQueriesButRenderDoesNotRepeatThem(t *testing.T) {
	s, p, _ := testHost(t)
	queries := []byte("\x1b[c\x1b[5n\x1b[6n")
	_, _ = p.writer.Write(queries)
	waitForOutput(t, s, uint64(len(queries)))
	p.mu.Lock()
	answers := p.input.String()
	p.mu.Unlock()
	if !strings.Contains(answers, "\x1b[0n") || !strings.Contains(answers, "\x1b[1;1R") {
		t.Fatalf("host did not answer VT queries: %q", answers)
	}
	r := s.snapshot(0, true)
	for _, query := range []string{"\x1b[c", "\x1b[5n", "\x1b[6n"} {
		if strings.Contains(r.Render, query) {
			t.Fatalf("render forwarded raw query %q", query)
		}
	}
}

// matrix/hardened: preserve the parsed paste mode across split child writes and
// reconnect captures without forwarding unrelated modes or terminal queries.
func TestCaptureBracketedPasteModeTracksSplitOutputAndReset(t *testing.T) {
	s, p, address := testHost(t)
	var written uint64
	write := func(data string) Response {
		t.Helper()
		if _, err := io.WriteString(p.writer, data); err != nil {
			t.Fatal(err)
		}
		written += uint64(len(data))
		waitForOutput(t, s, written)
		return callTestHost(t, address, Request{Op: "capture", After: written})
	}
	initial := callTestHost(t, address, Request{Op: "capture"})
	if initial.BracketedPaste {
		t.Fatal("new host enabled bracketed paste before the child requested it")
	}
	before := write("paste-ready")
	partial := write("\x1b[?20")
	if partial.BracketedPaste {
		t.Fatal("incomplete child mode sequence changed the capture mode")
	}
	enabled := write("04h")
	if !enabled.BracketedPaste || enabled.Render != before.Render || enabled.CursorX != before.CursorX || enabled.CursorY != before.CursorY || len(enabled.Output) != 0 {
		t.Fatalf("mode-only snapshot lost parsed state: %+v", enabled)
	}
	// A new viewer has no raw output delta but still needs the current mode.
	reconnected := callTestHost(t, address, Request{Op: "capture", After: written})
	if !reconnected.BracketedPaste || len(reconnected.Output) != 0 {
		t.Fatal("reconnect capture did not retain the child paste mode")
	}
	otherModes := write("\x1b[?1000h\x1b[?1000l\x1b[c\x1b[6n\x1b]52;c;c2VjcmV0\a")
	if !otherModes.BracketedPaste {
		t.Fatal("an unrelated child mode changed bracketed paste state")
	}
	for _, raw := range []string{"\x1b[?1000h", "\x1b[?1000l", "\x1b[c", "\x1b[6n", "\x1b]52"} {
		if strings.Contains(otherModes.Render, raw) {
			t.Fatalf("render replayed raw child control sequence %q", raw)
		}
	}
	write("\x1b[?200")
	if disabled := write("4l"); disabled.BracketedPaste {
		t.Fatal("split child disable did not clear bracketed paste")
	}
	if enabled := write("\x1b[?2004;1000h"); !enabled.BracketedPaste {
		t.Fatal("combined DEC mode sequence did not enable bracketed paste")
	}
	if reset := write("\x1bc"); reset.BracketedPaste {
		t.Fatal("RIS did not reset the captured bracketed paste mode")
	}
}
