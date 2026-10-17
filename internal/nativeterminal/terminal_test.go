// matrix/hardened: verify portable native protocol fencing without creating a
// real manager, user account files, or an online AI client.
package nativeterminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalbackend"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/google/uuid"
)

var testPipe = winipc.PipePrefix + "offline-test"

func boundTerminal(t *testing.T, caller Caller) (*Terminal, string, string) {
	t.Helper()
	sessionID, runID := uuid.NewString(), uuid.NewString()
	terminal, err := NewWithCaller(testPipe, sessionID, runID, caller)
	if err != nil {
		t.Fatal(err)
	}
	return terminal, sessionID, runID
}

func reply(request terminalhost.Request) terminalhost.Response {
	return terminalhost.Response{OK: true, SessionID: request.SessionID, RunID: request.RunID, HostPID: 123, PID: 456}
}

func TestBindingValidation(t *testing.T) {
	sessionID, runID := uuid.NewString(), uuid.NewString()
	caller := Caller(func(_ context.Context, _ string, r terminalhost.Request) (terminalhost.Response, error) {
		return reply(r), nil
	})
	for _, pipe := range []string{"", `\\remote\pipe\agent-deck-native-test`, winipc.PipePrefix, winipc.PipePrefix + strings.Repeat("a", 129), winipc.PipePrefix + "test/other", winipc.PipePrefix + "中文", winipc.PipePrefix + "x\x00"} {
		if _, err := NewWithCaller(pipe, sessionID, runID, caller); !errors.Is(err, ErrInvalidBinding) {
			t.Fatalf("pipe %q accepted: %v", pipe, err)
		}
	}
	for _, id := range []string{"", "bad", uuid.Nil.String(), strings.ReplaceAll(sessionID, "-", ""), "{" + sessionID + "}", strings.ToUpper("abcdefab-1234-4567-89ab-abcdefabcdef")} {
		if _, err := NewWithCaller(testPipe, id, runID, caller); !errors.Is(err, ErrInvalidBinding) {
			t.Fatalf("session ID %q accepted: %v", id, err)
		}
		if _, err := NewWithCaller(testPipe, sessionID, id, caller); !errors.Is(err, ErrInvalidBinding) {
			t.Fatalf("run ID %q accepted: %v", id, err)
		}
	}
	var nilCaller Caller
	if _, err := NewWithCaller(testPipe, sessionID, runID, nilCaller); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("nil caller accepted: %v", err)
	}
	if _, err := New(testPipe, sessionID, runID); err != nil {
		t.Fatal(err)
	}
	var terminal *Terminal
	var backend terminalbackend.Backend = terminal
	if _, err := backend.Observe(context.Background()); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("typed nil backend: %v", err)
	}
}

func TestBoundRequestsAndObservation(t *testing.T) {
	var requests []terminalhost.Request
	exitCode := uint32(7)
	terminal, sessionID, runID := boundTerminal(t, func(_ context.Context, pipe string, request terminalhost.Request) (terminalhost.Response, error) {
		if pipe != testPipe {
			t.Errorf("pipe changed: %s", pipe)
		}
		requests = append(requests, request)
		r := reply(request)
		r.Running, r.HeadOffset, r.ExitCode = false, 99, &exitCode
		r.Screen, r.Render, r.Output = "当前屏幕", "wrong render", []byte("wrong output")
		return r, nil
	})
	observed, err := terminal.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observed.Alive || observed.PID != 456 || observed.HostPID != 123 || observed.RunID != runID || observed.OutputSequence != 99 || observed.ExitCode == nil || *observed.ExitCode != 7 {
		t.Fatalf("observation: %#v", observed)
	}
	exitCode = 8
	if *observed.ExitCode != 7 {
		t.Fatal("observation retained mutable wire pointer")
	}
	screen, err := terminal.Capture(context.Background(), false)
	if err != nil || screen != "当前屏幕" {
		t.Fatalf("screen %q: %v", screen, err)
	}
	payload := []byte("继续😀\r\x03\x1b[A\x00\xff")
	if err := terminal.Send(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'X'
	if err := terminal.Resize(context.Background(), 123, 35); err != nil {
		t.Fatal(err)
	}
	if err := terminal.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, op := range []string{"status", "capture", "send", "resize", "stop"} {
		request := requests[i]
		if request.Op != op || request.SessionID != sessionID || request.RunID != runID {
			t.Fatalf("request %d changed binding: %#v", i, request)
		}
	}
	if !bytes.Equal(requests[2].Data, []byte("继续😀\r\x03\x1b[A\x00\xff")) {
		t.Fatalf("input changed: %q", requests[2].Data)
	}
	if requests[3].Cols != 123 || requests[3].Rows != 35 {
		t.Fatal("resize lost dimensions")
	}
}

func TestResponseFencesEveryOperationWithoutRetry(t *testing.T) {
	for _, operation := range []string{"status", "capture", "send", "resize", "stop"} {
		for _, mismatch := range []string{"session", "run", "missing"} {
			t.Run(operation+"/"+mismatch, func(t *testing.T) {
				calls := 0
				terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
					calls++
					response := reply(request)
					switch mismatch {
					case "session":
						response.SessionID = uuid.NewString()
					case "run":
						response.RunID = uuid.NewString()
					case "missing":
						response.SessionID, response.RunID = "", ""
					}
					return response, nil
				})
				if err := exercise(terminal, operation, context.Background()); !errors.Is(err, ErrTargetMismatch) {
					t.Fatalf("mismatch accepted: %v", err)
				}
				if calls != 1 {
					t.Fatalf("retried/rebound %d calls", calls)
				}
			})
		}
	}
	stale := errors.New("native session run no longer matches")
	calls := 0
	terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		calls++
		response := reply(request)
		response.RunID, response.OK = uuid.NewString(), false
		return response, stale
	})
	if err := terminal.Stop(context.Background()); !errors.Is(err, ErrTargetMismatch) || !errors.Is(err, stale) {
		t.Fatalf("stale response: %v", err)
	}
	if calls != 1 {
		t.Fatalf("retried stale mutation %d times", calls)
	}
}

func exercise(terminal *Terminal, op string, ctx context.Context) error {
	switch op {
	case "status":
		_, err := terminal.Observe(ctx)
		return err
	case "capture":
		_, err := terminal.Capture(ctx, false)
		return err
	case "send":
		return terminal.Send(ctx, []byte("x"))
	case "resize":
		return terminal.Resize(ctx, 80, 24)
	default:
		return terminal.Stop(ctx)
	}
}

func TestInputValidationDoesNotCallHost(t *testing.T) {
	var calls atomic.Int32
	terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, r terminalhost.Request) (terminalhost.Response, error) {
		calls.Add(1)
		return reply(r), nil
	})
	if err := terminal.Send(context.Background(), make([]byte, terminalbackend.MaxInputBytes+1)); err == nil {
		t.Fatal("oversized input accepted")
	}
	for _, size := range [][2]int{{0, 1}, {501, 1}, {1, 0}, {1, 301}, {-1, -1}} {
		if err := terminal.Resize(context.Background(), size[0], size[1]); err == nil {
			t.Fatalf("size accepted: %v", size)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := terminal.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := terminal.Observe(nil); err == nil {
		t.Fatal("nil context accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid/cancelled request reached host")
	}
}

func TestConcurrentRequestsStayBound(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var requests []terminalhost.Request
	terminal, sessionID, runID := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		calls.Add(1)
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		if len(request.Data) > 0 {
			request.Data[0] = 'X'
		}
		return reply(request), nil
	})
	payload := []byte("继续")
	var group sync.WaitGroup
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := terminal.Send(context.Background(), payload); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if string(payload) != "继续" || calls.Load() != 100 {
		t.Fatalf("mutation/count: %q %d", payload, calls.Load())
	}
	for _, request := range requests {
		if request.SessionID != sessionID || request.RunID != runID {
			t.Fatalf("binding changed: %#v", request)
		}
	}
}

func TestCancellationClosesClientOnly(t *testing.T) {
	requestRead, clientClosed := make(chan struct{}), make(chan struct{})
	var op string
	terminal, _, _ := boundTerminal(t, func(ctx context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			defer server.Close()
			var decoded terminalhost.Request
			_ = json.NewDecoder(server).Decode(&decoded)
			op = decoded.Op
			close(requestRead)
			_, _ = io.Copy(io.Discard, server)
			close(clientClosed)
		}()
		return terminalhost.Call(ctx, client, request)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- terminal.Send(ctx, []byte("pending")) }()
	select {
	case <-requestRead:
	case <-time.After(time.Second):
		t.Fatal("request blocked")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not cancel")
	}
	select {
	case <-clientClosed:
	case <-time.After(time.Second):
		t.Fatal("connection left open")
	}
	if op != "send" {
		t.Fatalf("cancellation sent %q", op)
	}
}
