package tmux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// matrix/hardened: seed raw test facts without an actual independent command.
func (s *Session) reserveMatrixCapture(useCache bool) (matrixCaptureTicket, string, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if useCache && s.cacheContent != "" && time.Since(s.cacheTime) < matrixCaptureTTL {
		return matrixCaptureTicket{}, s.cacheContent, true
	}
	s.cacheGeneration++
	return matrixCaptureTicket{s, s.Name, s.SocketName, s.cacheGeneration}, "", false
}

// matrix/hardened: a scripted command preserves argv boundaries and allows
// batch failures/races without mutating any production tmux server.
func matrixBatchFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\0' "$@" >> "$MATRIX_BATCH_CALLS"
printf '\n' >> "$MATRIX_BATCH_CALLS"
if [ "$1" = -u ]; then shift; fi
if [ "$1" = -L ]; then shift 2; fi
batch=0
for arg in "$@"; do if [ "$arg" = ';' ]; then batch=1; fi; done
mode="$MATRIX_BATCH_MODE"
if [ "$batch" = 1 ]; then
 case "$mode" in
 wait) touch "$MATRIX_BATCH_READY"; while [ ! -e "$MATRIX_BATCH_RELEASE" ]; do sleep .002; done ;;
 slow) sleep .55 ;;
 fail) exit 1 ;;
 large) dd if=/dev/zero bs=1048576 count=9 2>/dev/null; exit 0 ;;
 esac
else
 case "$1" in
 has-session) exit 0 ;;
 list-panes) printf '0\n'; exit 0 ;;
 capture-pane)
 if [ "$MATRIX_BATCH_ORDINARY_WAIT" = 1 ]; then
  touch "$MATRIX_BATCH_INDEPENDENT_READY"
  while [ ! -e "$MATRIX_BATCH_INDEPENDENT_RELEASE" ]; do sleep .002; done
 fi
 printf 'ordinary:%s\n' "$3"; exit 0 ;;
 *) exit 1 ;;
 esac
fi
while [ "$#" -gt 0 ]; do
 case "$1" in
 ';') shift ;;
 display-message)
 marker="$3"
 if [ "$mode" != missing ] || [ "${marker%_1_end__}" = "$marker" ]; then printf '%s\n' "$marker"; fi
 if [ "$mode" = collision ] && [ "${marker%_0_begin__}" != "$marker" ]; then printf '%s\n' "$marker"; fi
 shift 3 ;;
 capture-pane)
 if [ "$3" = missing ]; then exit 1; fi
 printf '\n\033[31m%s\033[0m\n\n' "$3"
 shift 5 ;;
 *) exit 1 ;;
 esac
done
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MATRIX_BATCH_CALLS", calls)
	t.Setenv("MATRIX_BATCH_READY", filepath.Join(dir, "ready"))
	t.Setenv("MATRIX_BATCH_RELEASE", filepath.Join(dir, "release"))
	t.Setenv("MATRIX_BATCH_INDEPENDENT_READY", filepath.Join(dir, "independent-ready"))
	t.Setenv("MATRIX_BATCH_INDEPENDENT_RELEASE", filepath.Join(dir, "independent-release"))
	// macOS may spend over the production cache TTL validating a newly written
	// executable the first time it starts. Warm the private shim before measuring
	// the batch, so this fixture startup does not masquerade as capture latency.
	if err := commandRun(tmuxExec("fixture-warmup", "has-session", "-t", "warmup")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(calls, nil, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, calls
}

func matrixBatchCalls(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	var calls [][]string
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		var args []string
		for _, arg := range bytes.Split(bytes.TrimSuffix(line, []byte{0}), []byte{0}) {
			args = append(args, string(arg))
		}
		calls = append(calls, args)
	}
	return calls
}

func TestMatrixCaptureBatchBytesAndReadOnlyArgv(t *testing.T) {
	_, calls := matrixBatchFixture(t)
	sessions := []*Session{{Name: "one", SocketName: "batch-fixture"}, {Name: "two", SocketName: "batch-fixture"}}
	PrefetchCapturePanes(context.Background(), sessions)
	for _, s := range sessions {
		got, err := s.CapturePane()
		want := "\n\x1b[31m" + s.Name + "\x1b[0m\n\n"
		if err != nil || got != want {
			t.Fatalf("capture %q = %q, %v; want byte-identical %q", s.Name, got, err, want)
		}
	}
	commands := matrixBatchCalls(t, calls)
	if len(commands) != 1 {
		t.Fatalf("two cache readers spawned %d commands, want one", len(commands))
	}
	args := commands[0]
	if args[0] != "-u" || args[1] != "-L" || args[2] != "batch-fixture" {
		t.Fatalf("global argv = %v", args)
	}
	args = args[3:]
	for _, name := range []string{"one", "two"} {
		if len(args) < 13 || args[0] != "display-message" || args[1] != "-p" || args[3] != ";" ||
			args[4] != "capture-pane" || args[5] != "-t" || args[6] != name || args[7] != "-p" || args[8] != "-e" ||
			args[9] != ";" || args[10] != "display-message" || args[11] != "-p" {
			t.Fatalf("batch must contain only framed original visible captures: %v", args)
		}
		args = args[13:]
		if len(args) != 0 {
			if args[0] != ";" {
				t.Fatal("missing command separator")
			}
			args = args[1:]
		}
	}
	if len(args) != 0 {
		t.Fatalf("unexpected mutation/extra command: %v", args)
	}
}

func TestMatrixCaptureBatchRejectsAllPartialOutput(t *testing.T) {
	for _, mode := range []string{"fail", "missing", "collision", "large", "slow"} {
		t.Run(mode, func(t *testing.T) {
			_, calls := matrixBatchFixture(t)
			t.Setenv("MATRIX_BATCH_MODE", mode)
			sessions := []*Session{{Name: "one", SocketName: "partial"}, {Name: "two", SocketName: "partial"}}
			started := time.Now()
			PrefetchCapturePanes(context.Background(), sessions)
			if time.Since(started) > 3*time.Second+tmuxSubprocessWaitDelay+time.Second {
				t.Fatal("batch exceeded bounded capture lifecycle")
			}
			for _, s := range sessions {
				s.cacheMu.RLock()
				seeded := !s.cacheTime.IsZero()
				s.cacheMu.RUnlock()
				if seeded {
					t.Fatalf("%s partially seeded %s", mode, s.Name)
				}
				got, err := s.CapturePane()
				if err != nil || got != "ordinary:"+s.Name+"\n" {
					t.Fatalf("individual fallback = %q %v", got, err)
				}
			}
			if n := len(matrixBatchCalls(t, calls)); n != 3 {
				t.Fatalf("batch + two fallbacks = %d calls, want 3", n)
			}
		})
	}
}

func TestMatrixCaptureBatchMissingTargetDoesNotContaminateSibling(t *testing.T) {
	_, calls := matrixBatchFixture(t)
	sessions := []*Session{{Name: "one", SocketName: "missing-target"}, {Name: "missing", SocketName: "missing-target"}}
	PrefetchCapturePanes(context.Background(), sessions)
	got, err := sessions[0].CapturePane()
	if err != nil || got != "ordinary:one\n" || len(matrixBatchCalls(t, calls)) != 2 {
		t.Fatalf("sibling reused a partial batch: %q %v", got, err)
	}
}

func TestMatrixCaptureBatchParserStrictFraming(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	frame := "\n\x1b[31mtext\x1b[0m\n\n"
	var b strings.Builder
	for i := 0; i < 2; i++ {
		b.WriteString(matrixCaptureMarker(token, i, "begin") + "\n")
		b.WriteString(frame)
		b.WriteString(matrixCaptureMarker(token, i, "end") + "\n")
	}
	good := b.String()
	if frames, ok := parseMatrixCaptureBatch([]byte(good), token, 2); !ok || len(frames) != 2 || frames[0] != frame || frames[1] != frame {
		t.Fatalf("valid frame = %q %v", frames, ok)
	}
	for _, bad := range []string{good + "extra\n", "extra\n" + good, strings.Replace(good, "text", token, 1), strings.Replace(good, "_0_end__", "_1_end__", 1), strings.Replace(good, "\n\n__agentdeck", "x__agentdeck", 1), good[:len(good)-1]} {
		if _, ok := parseMatrixCaptureBatch([]byte(bad), token, 2); ok {
			t.Fatalf("accepted malformed/colliding marker output %q", bad)
		}
	}
}

type matrixBrokenEntropy struct{}

func (matrixBrokenEntropy) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestMatrixCaptureBatchFreshSkipDedupAndEntropyFailure(t *testing.T) {
	_, calls := matrixBatchFixture(t)
	one := &Session{Name: "one", SocketName: "fresh"}
	two := &Session{Name: "two", SocketName: "fresh"}
	three := &Session{Name: "three", SocketName: "fresh"}
	ticket, _, _ := one.reserveMatrixCapture(false)
	ticket.publish("fresh-one\n", time.Now())
	PrefetchCapturePanes(context.Background(), []*Session{one, two, two, three, nil})
	if got, _ := one.CapturePane(); got != "fresh-one\n" {
		t.Fatalf("fresh capture overwritten: %q", got)
	}
	if n := len(matrixBatchCalls(t, calls)); n != 1 {
		t.Fatalf("fresh/deduplicated group spawned %d", n)
	}
	two.invalidateCache()
	three.invalidateCache()
	prefetchMatrixCapturePanes(context.Background(), []*Session{two, three}, matrixBrokenEntropy{})
	if n := len(matrixBatchCalls(t, calls)); n != 1 {
		t.Fatal("entropy failure started a batch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	PrefetchCapturePanes(ctx, []*Session{two, three})
	if n := len(matrixBatchCalls(t, calls)); n != 1 {
		t.Fatal("canceled prefetch started a command")
	}
}

func TestMatrixCaptureBatchTicketFencesInvalidateFreshAndNewerOrdinary(t *testing.T) {
	for _, action := range []string{"invalidate", "fresh", "ordinary", "identity"} {
		t.Run(action, func(t *testing.T) {
			dir, _ := matrixBatchFixture(t)
			t.Setenv("MATRIX_BATCH_MODE", "wait")
			one := &Session{Name: "one", SocketName: "fenced"}
			two := &Session{Name: "two", SocketName: "fenced"}
			done := make(chan struct{})
			go func() { PrefetchCapturePanes(context.Background(), []*Session{one, two}); close(done) }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("batch did not start")
				}
				time.Sleep(time.Millisecond)
			}
			switch action {
			case "invalidate":
				one.invalidateCache()
			case "fresh":
				if got, err := one.CapturePaneFresh(); err != nil || got != "ordinary:one\n" {
					t.Fatalf("fresh capture was not direct: %q %v", got, err)
				}
			case "ordinary":
				if got, err := one.CapturePane(); err != nil || got != "ordinary:one\n" {
					t.Fatalf("ordinary acquisition = %q %v", got, err)
				}
			case "identity":
				// Production Session identity is immutable. Exercise the seed guard
				// directly below; avoid a raced unsynchronized identity mutation.
				one.invalidateCache()
			}
			if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			<-done
			one.cacheMu.RLock()
			got, at := one.cacheContent, one.cacheTime
			one.cacheMu.RUnlock()
			if action == "fresh" || action == "ordinary" {
				if got != "ordinary:one\n" {
					t.Fatalf("late batch replaced newer %s: %q", action, got)
				}
			} else if got != "" || !at.IsZero() {
				t.Fatal("late batch restored invalidated capture")
			}
		})
	}
	s := &Session{Name: "old", SocketName: "old-socket"}
	ticket, _, _ := s.reserveMatrixCapture(false)
	s.Name = "new"
	if ticket.publish("wrong name", time.Now()) {
		t.Fatal("ticket accepted changed Name")
	}
	s.Name = "old"
	s.SocketName = "new-socket"
	if ticket.publish("wrong socket", time.Now()) {
		t.Fatal("ticket accepted changed SocketName")
	}
	older, _, _ := s.reserveMatrixCapture(false)
	newer, _, _ := s.reserveMatrixCapture(false)
	if !newer.publish("new", time.Now()) || older.publish("old", time.Now()) {
		t.Fatal("ordinary capture reservation did not fence older publication")
	}
}

func TestMatrixCaptureBatchSkipsEarlierIndependentCaptureUntilItFinishes(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "fresh"}[fresh], func(t *testing.T) {
			dir, calls := matrixBatchFixture(t)
			t.Setenv("MATRIX_BATCH_MODE", "wait")
			t.Setenv("MATRIX_BATCH_ORDINARY_WAIT", "1")
			one := &Session{Name: "one", SocketName: "earlier-independent"}
			two := &Session{Name: "two", SocketName: "earlier-independent"}
			three := &Session{Name: "three", SocketName: "earlier-independent"}
			independent := make(chan error, 1)
			go func() {
				capture := one.CapturePane
				if fresh {
					capture = one.CapturePaneFresh
				}
				_, err := capture()
				independent <- err
			}()
			waitFor := func(name string) {
				t.Helper()
				deadline := time.Now().Add(2 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("%s acquisition did not start", name)
					}
					time.Sleep(time.Millisecond)
				}
			}
			waitFor("independent-ready")
			// The child already inherited the waiting flag. Batch does not use it.
			done := make(chan struct{})
			go func() { PrefetchCapturePanes(context.Background(), []*Session{one, two, three}); close(done) }()
			waitFor("ready")
			if err := os.WriteFile(filepath.Join(dir, "independent-release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := <-independent; err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			<-done
			if got, err := one.CapturePane(); err != nil || got != "ordinary:one\n" {
				t.Fatalf("later batch superseded earlier-started independent completion: %q %v", got, err)
			}
			commands := matrixBatchCalls(t, calls)
			if len(commands) != 2 {
				t.Fatalf("commands = %v, want independent + one batch", commands)
			}
			for _, arg := range commands[1] {
				if arg == "one" {
					t.Fatal("batch reserved a target whose independent capture was in flight")
				}
			}
		})
	}
}

func TestMatrixCaptureBatchCanceledCommandPublishesNothing(t *testing.T) {
	_, calls := matrixBatchFixture(t)
	t.Setenv("MATRIX_BATCH_MODE", "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	sessions := []*Session{{Name: "one", SocketName: "canceled"}, {Name: "two", SocketName: "canceled"}}
	started := time.Now()
	PrefetchCapturePanes(ctx, sessions)
	if time.Since(started) > time.Second || ctx.Err() == nil {
		t.Fatal("canceled batch did not return promptly")
	}
	for _, s := range sessions {
		s.cacheMu.RLock()
		at := s.cacheTime
		s.cacheMu.RUnlock()
		if !at.IsZero() {
			t.Fatal("canceled command published a raw capture")
		}
	}
	if n := len(matrixBatchCalls(t, calls)); n > 1 {
		t.Fatalf("canceled command multiplied starts = %d", n)
	}
}

func TestMatrixCaptureBatchTTLAndIdenticalFreshObservations(t *testing.T) {
	_, _ = matrixBatchFixture(t)
	one := &Session{Name: "one", SocketName: "ttl", Command: "claude", detectedTool: "claude"}
	two := &Session{Name: "two", SocketName: "ttl"}
	PrefetchCapturePanes(context.Background(), []*Session{one, two})
	one.cacheMu.RLock()
	firstAt := one.cacheTime
	one.cacheMu.RUnlock()
	if firstAt.IsZero() || time.Since(firstAt) >= matrixCaptureTTL {
		t.Fatal("batch did not retain conservative fresh timestamp")
	}
	// Classification still runs on each successful observation, even when a
	// later batch returns byte-identical text. Use the existing completion frame.
	ticket, _, _ := one.reserveMatrixCapture(false)
	ticket.publish(realisticClaudeDoneContent, time.Now())
	one.GetSubstateWithLiveSpinner()
	_, firstSample := one.CachedCompletedTurnSample()
	ticket, _, _ = one.reserveMatrixCapture(false)
	ticket.publish(realisticClaudeDoneContent, time.Now())
	one.GetSubstateWithLiveSpinner()
	_, secondSample := one.CachedCompletedTurnSample()
	if firstSample.IsZero() || !secondSample.After(firstSample) {
		t.Fatal("same-byte successful capture lost fresh completion evidence")
	}
	// Publishing an old raw timestamp cannot create a new 500ms lease.
	ticket, _, _ = one.reserveMatrixCapture(false)
	ticket.publish("expired batch", time.Now().Add(-matrixCaptureTTL))
	if got, err := one.CapturePane(); err != nil || got != "ordinary:one\n" {
		t.Fatalf("expired raw capture was reused: %q %v", got, err)
	}
}

func TestMatrixCaptureBatchConcurrentCachePublication(t *testing.T) {
	s := &Session{Name: "concurrent", SocketName: "cache-only"}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				ticket, _, _ := s.reserveMatrixCapture(false)
				ticket.publish("new frame\n", time.Now())
				s.invalidateCache()
			}
		}()
	}
	wg.Wait()
}

func TestMatrixCaptureBatchLiveTmuxCurrentWindowAndSockets(t *testing.T) {
	if os.Getenv("TMUX_TMPDIR") == "" {
		t.Fatal("this test requires scripts/run-isolated.py")
	}
	run := func(socket string, args ...string) string {
		t.Helper()
		out, err := commandOutput(tmuxExec(socket, args...))
		if err != nil {
			t.Fatalf("private tmux %v: %v", args, err)
		}
		return string(out)
	}
	const socketA, socketB = "matrix-capture-live-a", "matrix-capture-live-b"
	for _, socket := range []string{socketA, socketB} {
		run(socket, "new-session", "-d", "-s", "same", "-x", "80", "-y", "12", "printf '\\n\\033[31m"+socket+" zero\\033[0m\\n\\n'; exec sleep 60")
		t.Cleanup(func() { _ = commandRun(tmuxExec(socket, "kill-server")) })
		run(socket, "new-session", "-d", "-s", "sibling", "-x", "80", "-y", "12", "printf '\\n"+socket+" sibling\\n\\n'; exec sleep 60")
		run(socket, "new-window", "-d", "-t", "same:1", "-n", "selected", "printf '\\n\\033[32m"+socket+" current-window\\033[0m\\n\\n'; exec sleep 60")
		run(socket, "select-window", "-t", "same:1")
	}
	var sessions []*Session
	for _, socket := range []string{socketA, socketB} {
		sessions = append(sessions, &Session{Name: "same", SocketName: socket}, &Session{Name: "sibling", SocketName: socket})
	}
	// Read captures until child output has arrived; no real AI input is sent.
	var originals []string
	deadline := time.Now().Add(2 * time.Second)
	for _, s := range sessions {
		for {
			got, err := s.CapturePaneFresh()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, s.SocketName) {
				if s.Name == "same" && (!strings.Contains(got, "current-window") || strings.Contains(got, " zero")) {
					t.Fatal("fixture did not select window 1")
				}
				originals = append(originals, got)
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("private fixture output did not arrive")
			}
			time.Sleep(5 * time.Millisecond)
		}
		s.invalidateCache()
	}
	geometry := make(map[string]string)
	for _, socket := range []string{socketA, socketB} {
		geometry[socket] = run(socket, "list-panes", "-a", "-F", "#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}")
	}
	before := SubprocessStarts()
	PrefetchCapturePanes(context.Background(), sessions)
	for i, s := range sessions {
		got, err := s.CapturePane()
		if err != nil || got != originals[i] {
			t.Fatalf("%s/%s bytes differ: %q vs %q, %v", s.SocketName, s.Name, got, originals[i], err)
		}
	}
	if delta := SubprocessStarts() - before; delta != 2 {
		t.Fatalf("four captures on two sockets spawned %d commands, want 2", delta)
	}
	for _, socket := range []string{socketA, socketB} {
		if got := run(socket, "list-panes", "-a", "-F", "#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}"); got != geometry[socket] {
			t.Fatal("read-only batch changed a pane process or geometry")
		}
		if clients := run(socket, "list-clients", "-F", "#{client_name}"); clients != "" {
			t.Fatal("batch created an attached client")
		}
	}
}

var _ io.Reader = matrixBrokenEntropy{}
