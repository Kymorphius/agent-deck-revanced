// matrix/hardened: preview capture must not replay raw terminal commands and
// bounded history paging must remain tied to one immutable native generation.
package nativeterminal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

func TestCaptureUsesSafeScreenAndBoundedRetainedOutput(t *testing.T) {
	prefix := uint64(12345)
	retained := []byte(strings.Repeat("x", terminalhost.MaxOutputBytes-1) + "中文😀\r\n\x1b[6n\x1b]52;c;c2VjcmV0\a\x1b[31m红\x1b[0m\x07\x00\x7f")
	var requests []terminalhost.Request
	terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		requests = append(requests, request)
		response := reply(request)
		response.Screen = "屏幕\x1b[6n\x1b]52;c;clipboard\a\x00\x07"
		response.Render = "WRONG_RENDER\x1b[6n"
		response.StartOffset = max(request.After, prefix)
		response.HeadOffset = prefix + uint64(len(retained))
		response.EndOffset = min(response.HeadOffset, response.StartOffset+terminalhost.MaxOutputBytes)
		response.Output = retained[response.StartOffset-prefix : response.EndOffset-prefix]
		response.Truncated = request.After < prefix
		return response, nil
	})
	screen, err := terminal.Capture(context.Background(), false)
	if err != nil || screen != "屏幕" {
		t.Fatalf("screen %q: %v", screen, err)
	}
	requests = nil
	text, err := terminal.Capture(context.Background(), true)
	want := strings.Repeat("x", terminalhost.MaxOutputBytes-1) + "中文😀\n红"
	if err != nil || text != want || !utf8.ValidString(text) {
		t.Fatalf("history safe/boundary capture got len=%d want=%d suffix=%q: %v", len(text), len(want), text[max(0, len(text)-80):], err)
	}
	if len(requests) != 2 || requests[0].After != 0 || requests[1].After != prefix+terminalhost.MaxOutputBytes {
		t.Fatalf("pages: %#v", requests)
	}
}

func TestCaptureSnapshotsInitialHeadWhileOutputGrows(t *testing.T) {
	initial := strings.Repeat("x", terminalhost.MaxOutputBytes) + "initial"
	data := []byte(initial + "later")
	calls := 0
	terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		calls++
		response := reply(request)
		response.StartOffset = request.After
		response.HeadOffset = uint64(len(data))
		if calls == 1 {
			response.HeadOffset = uint64(len(initial))
		}
		response.EndOffset = min(response.HeadOffset, request.After+terminalhost.MaxOutputBytes)
		response.Output = data[response.StartOffset:response.EndOffset]
		return response, nil
	})
	text, err := terminal.Capture(context.Background(), true)
	if err != nil || text != initial || calls != 2 {
		t.Fatalf("snapshot grew: len=%d calls=%d err=%v", len(text), calls, err)
	}
}

func TestCaptureRejectsMalformedOrChangedHistory(t *testing.T) {
	for _, fault := range []string{"ahead", "reversed", "length", "oversized", "bound", "no-progress", "regressed", "evicted", "stale"} {
		t.Run(fault, func(t *testing.T) {
			calls := 0
			terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
				calls++
				response := reply(request)
				response.StartOffset = request.After
				response.HeadOffset = 2
				response.EndOffset = request.After + 1
				response.Output = []byte("x")
				switch fault {
				case "ahead":
					response.EndOffset = 3
				case "reversed":
					response.StartOffset, response.EndOffset = 1, 0
				case "length":
					response.Output = nil
				case "oversized":
					response.Output = make([]byte, terminalhost.MaxOutputBytes+1)
					response.EndOffset = uint64(len(response.Output))
					response.HeadOffset = response.EndOffset
				case "bound":
					response.HeadOffset = terminalhost.HistoryBytes + 1
				case "no-progress":
					response.EndOffset = request.After
					response.Output = nil
				case "regressed":
					if calls > 1 {
						response.HeadOffset = 1
						response.EndOffset = 1
						response.Output = nil
					}
				case "evicted":
					if calls > 1 {
						response.StartOffset, response.EndOffset, response.HeadOffset = 2, 2, 2
						response.Output = nil
						response.Truncated = true
					}
				case "stale":
					if calls > 1 {
						response.RunID = uuid.NewString()
					}
				}
				return response, nil
			})
			text, err := terminal.Capture(context.Background(), true)
			if err == nil || text != "" {
				t.Fatalf("fault %s accepted: %q %v", fault, text, err)
			}
			if fault == "evicted" && !errors.Is(err, ErrHistoryUnavailable) {
				t.Fatalf("eviction: %v", err)
			}
			if fault == "stale" && !errors.Is(err, ErrTargetMismatch) {
				t.Fatalf("stale: %v", err)
			}
			if calls > 2 {
				t.Fatalf("unexpected retries/pages: %d", calls)
			}
		})
	}
}

func TestCapturePageCountIsBounded(t *testing.T) {
	calls := 0
	terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		calls++
		response := reply(request)
		response.StartOffset, response.EndOffset, response.HeadOffset = request.After, request.After+1, terminalhost.HistoryBytes
		response.Output = []byte("x")
		return response, nil
	})
	if _, err := terminal.Capture(context.Background(), true); !errors.Is(err, ErrInvalidResponse) {
		t.Fatal(err)
	}
	if calls > terminalhost.HistoryBytes/terminalhost.MaxOutputBytes+1 {
		t.Fatalf("unbounded paging: %d", calls)
	}
}

func TestObserveRejectsInventedLivenessAndFailedReplies(t *testing.T) {
	for _, fault := range []string{"zero-pid", "zero-host", "failed"} {
		t.Run(fault, func(t *testing.T) {
			terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
				response := reply(request)
				response.Running = true
				switch fault {
				case "zero-pid":
					response.PID = 0
				case "zero-host":
					response.HostPID = 0
				case "failed":
					response.OK = false
					response.Error = "cleanup uncertain"
				}
				return response, nil
			})
			observed, err := terminal.Observe(context.Background())
			if err == nil || observed.Alive {
				t.Fatalf("inferred liveness %#v: %v", observed, err)
			}
		})
	}
}

func TestStopCannotAcknowledgeLiveProcess(t *testing.T) {
	terminal, _, _ := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		response := reply(request)
		response.Running = true
		return response, nil
	})
	if err := terminal.Stop(context.Background()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("live stop reported success: %v", err)
	}
}

func TestObservePreservesFactsAndHostDiagnostic(t *testing.T) {
	terminal, _, runID := boundTerminal(t, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		response := reply(request)
		response.Running = true
		response.Error = "owned process output could not be read"
		return response, nil
	})
	observation, err := terminal.Observe(context.Background())
	if !errors.Is(err, ErrObservation) || !observation.Alive || observation.PID != 456 || observation.RunID != runID {
		t.Fatalf("host diagnostic or observed identity lost: %+v %v", observation, err)
	}
}
