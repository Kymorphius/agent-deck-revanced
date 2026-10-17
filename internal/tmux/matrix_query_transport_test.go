package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// matrix/hardened: all real clients below use the isolated runner's private
// named sockets, never production default/agentdeck or any AI process.
func matrixQueryFixture(t *testing.T, socket string) *Session {
	t.Helper()
	cmd := tmuxExec(socket, "new-session", "-d", "-s", "same", "-x", "101", "-y", "37", "printf '\\033[31mhello 中文\\033[0m\\n\\n'; sleep 120")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private server: %v %s", err, out)
	}
	t.Cleanup(func() { _ = tmuxExec(socket, "kill-server").Run() })
	return &Session{Name: "same", SocketName: socket}
}
func matrixNativeQuery(t *testing.T, socket string, args ...string) []byte {
	t.Helper()
	out, err := tmuxExec(socket, args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestMatrixQueryPrivateSockets(t *testing.T) {
	a := matrixQueryFixture(t, "query-a")
	b := matrixQueryFixture(t, "query-b")
	before := matrixNativeQuery(t, a.SocketName, "list-panes", "-a", "-F", "#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}|#{window_id}")
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{a, b})
	for _, s := range []*Session{a, b} {
		args := []string{"capture-pane", "-t", s.Name, "-p", "-e"}
		want := matrixNativeQuery(t, s.SocketName, args...)
		got, ok := m.output(context.Background(), s.SocketName, args)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("query %s ok=%v got=%q want=%q stats=%+v", s.SocketName, ok, got, want, m.Diagnostics())
		}
		missing := []string{"show-environment", "-t", s.Name, "MATRIX_MISSING_ENV_912312"}
		if _, ok := m.output(context.Background(), s.SocketName, missing); ok {
			t.Fatal("missing env accepted")
		}
		got, ok = m.output(context.Background(), s.SocketName, args)
		if !ok || !bytes.Equal(got, want) {
			t.Fatal("framed command error destroyed healthy query", m.Diagnostics())
		}
	}
	_, nativeErr := runBoundedOutput(a.SocketName, "show-environment", "-t", a.Name, "MATRIX_MISSING_ENV_912312")
	var exitErr *exec.ExitError
	if !errors.As(nativeErr, &exitErr) {
		t.Fatalf("native error identity lost: %T %v", nativeErr, nativeErr)
	}
	if m.Diagnostics().Starts != 2 {
		t.Fatal(m.Diagnostics())
	}
	after := matrixNativeQuery(t, a.SocketName, "list-panes", "-a", "-F", "#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}|#{window_id}")
	if !bytes.Equal(before, after) {
		t.Fatalf("geometry/process changed before=%s after=%s", before, after)
	}
	clients := string(matrixNativeQuery(t, a.SocketName, "list-clients", "-F", "#{client_flags}"))
	for _, flag := range []string{"ignore-size", "no-output", "read-only"} {
		if !strings.Contains(clients, flag) {
			t.Fatalf("missing %s: %s", flag, clients)
		}
	}
	starts := m.Diagnostics().Queries
	a.invalidateCache()
	if _, err := a.CapturePaneFresh(); err != nil {
		t.Fatal(err)
	}
	if m.Diagnostics().Queries != starts {
		t.Fatal("Fresh used transport")
	}
	m.Close()
	m.Close()
	if got := matrixNativeQuery(t, a.SocketName, "list-clients", "-F", "#{client_pid}"); len(got) != 0 {
		t.Fatalf("owned client survived %s", got)
	}
	if !bytes.Equal(before, matrixNativeQuery(t, a.SocketName, "list-panes", "-a", "-F", "#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}|#{window_id}")) {
		t.Fatal("closing manager changed AI pane")
	}
}
func TestMatrixQueryAllowlist(t *testing.T) {
	for _, args := range [][]string{{"send-keys", "-t", "s", "x"}, {"capture-pane", "-t", "s", "-p", ";", "kill-server"}, {"display-message", "-p", "#(touch /tmp/bad)"}, {"capture-pane", "-t", "s\nkill-server", "-p"}, {"capture-pane", "-t", "s", "-p", "-C"}, {"display-message", "-p", "#{client_width}"}, {"list-panes", "-a", "-F", "#{client_width}"}, {"list-panes", "-a", "-F", "#(echo bad)"}, {"show-environment"}, {"display-message", "-p", "__agentdeck_capture_x", ";"}} {
		if _, ok := matrixQueryCommands(args); ok {
			t.Fatalf("unsafe accepted %q", args)
		}
	}
	for _, args := range [][]string{{"capture-pane", "-t", "s", "-p", "-e"}, {"list-panes", "-a", "-F", "#{pane_pid}"}, {"show-environment", "-t", "s", "KEY"}, {"display-message", "-t", "s", "-p", "a '$ # 中文 \\"}} {
		if _, ok := matrixQueryCommands(args); !ok {
			t.Fatalf("safe rejected %q", args)
		}
	}
}
func matrixFramedConnection(lines string) *matrixQueryConnection {
	c := &matrixQueryConnection{lines: make(chan []byte, 100)}
	for _, l := range strings.SplitAfter(lines, "\n") {
		if l != "" {
			c.lines <- []byte(l)
		}
	}
	close(c.lines)
	return c
}
func TestMatrixQueryFrameStrict(t *testing.T) {
	for _, raw := range []string{"%begin 1 1 1\nhello\n%end 1 2 1\n", "%begin 1 1 1\n%begin 1 2 1\n", "%output %0 hello\n%begin 1 1 1\n%end 1 1 1\n", "%begin 1 1 1\nhello\n"} {
		if _, err := matrixFramedConnection(raw).frame(context.Background()); err == nil {
			t.Fatalf("bad frame accepted %q", raw)
		}
	}
	c := matrixFramedConnection("%session-changed $0 same\n%begin 1 1 1\n\n\x1b[31m中文\x1b[0m\n%output literal pane line\n\n%end 1 1 1\n")
	got, err := c.frame(context.Background())
	if err != nil || string(got) != "\n\x1b[31m中文\x1b[0m\n%output literal pane line\n\n" {
		t.Fatalf("raw preservation %q %v", got, err)
	}
	c = matrixFramedConnection("%begin 1 1 1\n%end 1 1 1\n%begin 1 1 1\n%end 1 1 1\n")
	_, _ = c.frame(context.Background())
	if _, err = c.frame(context.Background()); err == nil {
		t.Fatal("duplicate number accepted")
	}
}

type matrixQueryWriteFunc func([]byte) (int, error)

func (f matrixQueryWriteFunc) Write(b []byte) (int, error) { return f(b) }
func (f matrixQueryWriteFunc) Close() error                { return nil }
func TestMatrixQueryExactEndCollisionNeedsAck(t *testing.T) {
	c := &matrixQueryConnection{lines: make(chan []byte, 20)}
	c.in = matrixQueryWriteFunc(func(b []byte) (int, error) {
		// Captured text forges exactly this request's closing identity. The actual
		// close remains and must fail before an unpredictable ack can be accepted.
		for _, line := range []string{"%begin 1 1 1\n", "prefix\n", "%end 1 1 1\n", "%end 1 1 1\n"} {
			c.lines <- []byte(line)
		}
		return len(b), nil
	})
	if out, err := c.query(context.Background(), [][]string{{"capture-pane", "-p", "-t", "s"}}); err == nil {
		t.Fatalf("collision returned truncated success %q", out)
	}
}
func TestMatrixQueryRegistrationBounds(t *testing.T) {
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	for p := 0; p < 40; p++ {
		var ss []*Session
		for i := 0; i < 50; i++ {
			ss = append(ss, &Session{Name: "s", SocketName: fmt.Sprintf("socket-%d", i)})
		}
		m.RegisterTargets(fmt.Sprint(p), ss)
	}
	if len(m.profiles) > 16 || len(m.sockets) > 32 {
		t.Fatal("unbounded inventory")
	}
	if _, ok := m.output(context.Background(), "unknown", []string{"capture-pane", "-p", "-t", "s"}); ok {
		t.Fatal("unknown anchor accepted")
	}
	m.ReleaseTargets("0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := m.output(ctx, "socket-0", []string{"capture-pane", "-p", "-t", "s"}); ok {
		t.Fatal("cancelled accepted")
	}
	if m.Diagnostics().Starts != 0 {
		t.Fatal("registration/cancellation started client")
	}
}
func TestMatrixQueryPrivateQuoteAndBatch(t *testing.T) {
	s := matrixQueryFixture(t, "query-quote")
	if out, err := tmuxExec(s.SocketName, "new-window", "-d", "-t", "same", "-n", "other", "printf second; sleep 120").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{s})
	args := []string{"display-message", "-t", "same", "-p", "literal apostrophe' dollar$ quote\" backslash\\ hash# 中文"}
	want := matrixNativeQuery(t, s.SocketName, args...)
	got, ok := m.output(context.Background(), s.SocketName, args)
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("quote mismatch ok=%v got=%q want=%q", ok, got, want)
	}
	tickets := []matrixCaptureTicket{{name: "same"}, {name: "same:other"}}
	token := "0123456789abcdef0123456789abcdef"
	args = matrixCaptureBatchArgs(token, tickets)
	want = matrixNativeQuery(t, s.SocketName, args...)
	got, ok = m.output(context.Background(), s.SocketName, args)
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("batch mismatch ok=%v got=%q want=%q", ok, got, want)
	}
	if _, ok := parseMatrixCaptureBatch(got, token, 2); !ok {
		t.Fatal("bad batch bytes")
	}

	a := &Session{Name: "same", SocketName: s.SocketName}
	b := &Session{Name: "same:other", SocketName: s.SocketName}
	count := m.Diagnostics().Successes
	PrefetchCapturePanes(context.Background(), []*Session{a, b})
	if m.Diagnostics().Successes != count+1 {
		t.Fatal("prefetch did not use query channel")
	}
	for _, session := range []*Session{a, b} {
		session.cacheMu.RLock()
		cached := session.cacheContent
		session.cacheMu.RUnlock()
		native := matrixNativeQuery(t, s.SocketName, "capture-pane", "-t", session.Name, "-p", "-e")
		if cached != string(native) {
			t.Fatal("prefetch cache differs from native")
		}
	}
	// Explicit session targets preserve native current-window semantics.
	if err := tmuxExec(s.SocketName, "select-window", "-t", "same:other").Run(); err != nil {
		t.Fatal(err)
	}
	args = []string{"capture-pane", "-t", "same", "-p", "-e"}
	want = matrixNativeQuery(t, s.SocketName, args...)
	got, ok = m.output(context.Background(), s.SocketName, args)
	if !ok || !bytes.Equal(got, want) {
		t.Fatal("current window changed semantics")
	}
}

var _ io.Writer = matrixQueryWriteFunc(nil)

func TestMatrixQueryPrivateReplacementAndErrorBatch(t *testing.T) {
	s := matrixQueryFixture(t, "query-replace")
	if err := tmuxExec(s.SocketName, "new-session", "-d", "-s", "replacement", "sleep 120").Run(); err != nil {
		t.Fatal(err)
	}
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{s})
	args := []string{"capture-pane", "-t", s.Name, "-p", "-e"}
	if _, ok := m.output(context.Background(), s.SocketName, args); !ok {
		t.Fatal("initial")
	}
	pid := m.Diagnostics().ClientPIDs[0]
	bad := []string{"show-environment", "-t", s.Name, "MATRIX_MISSING_ENV_912312", ";", "capture-pane", "-t", s.Name, "-p", "-e"}
	if _, ok := m.output(context.Background(), s.SocketName, bad); ok {
		t.Fatal("failed batch success")
	}
	if _, ok := m.output(context.Background(), s.SocketName, args); !ok || m.Diagnostics().Starts != 1 {
		t.Fatal("framed error should preserve connection", m.Diagnostics())
	}
	m.RegisterTargets("fixture", []*Session{{Name: "replacement", SocketName: s.SocketName}})
	if _, ok := m.output(context.Background(), s.SocketName, args); !ok {
		t.Fatal("replacement")
	}
	if d := m.Diagnostics(); d.Connections != 1 || d.ClientPIDs[0] == pid {
		t.Fatal("old client reused", d)
	}
	m.ReleaseTargets("fixture")
	if d := m.Diagnostics(); d.Connections != 0 {
		t.Fatal("lease retained connection", d)
	}
}
func TestMatrixQueryCancellationQueue(t *testing.T) {
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{{Name: "same", SocketName: "never-start"}})
	s := m.sockets[matrixQuerySocketKey("never-start")]
	s.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := m.output(ctx, "never-start", []string{"capture-pane", "-t", "same", "-p"})
		done <- ok
	}()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("cancel success")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel queued blocked")
	}
	<-s.gate
	if m.Diagnostics().Starts != 0 {
		t.Fatal("cancel spawned")
	}
}
func TestMatrixQueryOversizedAndNotificationAck(t *testing.T) {
	c := &matrixQueryConnection{lines: make(chan []byte, 20)}
	c.in = matrixQueryWriteFunc(func(b []byte) (int, error) {
		command := string(b)
		start := strings.Index(command, "__agentdeck_query_")
		end := strings.Index(command[start:], "'")
		ack := command[start : start+end]
		for _, line := range []string{"%begin 1 1 1\n", "\n", "hello\n", "\n", "%end 1 1 1\n", "%layout-change @0 x y\n", "%begin 1 2 1\n", ack + "\n", "%end 1 2 1\n"} {
			c.lines <- []byte(line)
		}
		return len(b), nil
	})
	if out, err := c.query(context.Background(), [][]string{{"capture-pane", "-t", "s", "-p"}}); err != nil || string(out) != "\nhello\n\n" {
		t.Fatalf("ack %q %v", out, err)
	}
	large := "%begin 1 1 1\n" + strings.Repeat("x", matrixQueryLimit) + "\n%end 1 1 1\n"
	if _, err := matrixFramedConnection(large).frame(context.Background()); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestMatrixQueryTimeoutReapsBeforeReconnect(t *testing.T) {
	// A private fake tmux delays the first response beyond the transport attempt.
	// Its late bytes must never satisfy a request on a newly created connection.
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "starts")
	script := `#!` + python + `
import os,sys,time,shlex
if '--warmup' in sys.argv: sys.exit(0)
path=os.environ['MATRIX_QUERY_STARTS']
try: n=int(open(path).read())+1
except FileNotFoundError: n=1
open(path,'w').write(str(n))
number=1
def frame(text):
 global number
 print('%%begin 1 %d 1'%number,flush=True)
 if text: print(text,flush=True)
 print('%%end 1 %d 1'%number,flush=True)
 number+=1
frame('')
for line in sys.stdin:
 args=shlex.split(line)
 if not args: continue
 if args[0]=='capture-pane':
  if n==1: time.sleep(1)
  frame('connection-%d'%n)
 elif args[0]=='display-message': frame(args[-1])
 else: sys.exit(2)
`
	if err = os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MATRIX_QUERY_STARTS", count)
	// macOS executable validation can exceed the production deadline on the
	// first launch of a freshly written shim. Warm only the private fixture.
	if err := exec.Command(filepath.Join(dir, "tmux"), "--warmup").Run(); err != nil {
		t.Fatal(err)
	}
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{{Name: "same", SocketName: "query-script"}})
	args := []string{"capture-pane", "-t", "same", "-p"}
	start := time.Now()
	if out, ok := m.output(context.Background(), "query-script", args); ok {
		t.Fatalf("late response success %q", out)
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout not bounded")
	}
	if d := m.Diagnostics(); d.Connections != 0 || d.Timeouts != 1 {
		t.Fatal("timed out client retained", d)
	}
	if _, ok := m.output(context.Background(), "query-script", args); ok {
		t.Fatal("backoff bypassed")
	}
	if raw, _ := os.ReadFile(count); string(raw) != "1" {
		t.Fatalf("backoff spawned %q", raw)
	}
	socket := m.sockets[matrixQuerySocketKey("query-script")]
	socket.gate <- struct{}{}
	socket.retry = time.Time{}
	<-socket.gate
	out, ok := m.output(context.Background(), "query-script", args)
	if !ok || string(out) != "connection-2\n" {
		t.Fatalf("reconnect accepted stale/missing output ok=%v out=%q stats=%+v", ok, out, m.Diagnostics())
	}
	c := socket.observed.Load()
	m.Close()
	select {
	case <-c.readerDone:
	default:
		t.Fatal("reader survived")
	}
	select {
	case <-c.waitDone:
	default:
		t.Fatal("client reaper survived")
	}
}

func TestMatrixQuerySocketAliases(t *testing.T) {
	t.Setenv("TMUX", "")
	if matrixQuerySocketKey("") != matrixQuerySocketKey("default") {
		t.Fatal("equivalent default sockets duplicated")
	}
	inherited := filepath.Join(t.TempDir(), "another-socket")
	t.Setenv("TMUX", inherited+",123,0")
	if matrixQuerySocketKey("") != inherited || matrixQuerySocketKey("") == matrixQuerySocketKey("default") {
		t.Fatal("inherited server isolation lost")
	}
}

func TestMatrixQueryProtocolPaneFallsBack(t *testing.T) {
	s := matrixQueryFixture(t, "query-protocol")
	if out, err := tmuxExec(s.SocketName, "new-window", "-d", "-t", "same", "-n", "protocol", "printf '%s\\n' '%end 1 999 1' 'preserve the following line'; sleep 120").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	args := []string{"capture-pane", "-t", "same:protocol", "-p", "-e"}
	want := matrixNativeQuery(t, s.SocketName, args...)
	deadline := time.Now().Add(time.Second)
	for !bytes.Contains(want, []byte("preserve the following line")) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		want = matrixNativeQuery(t, s.SocketName, args...)
	}
	if !bytes.Contains(want, []byte("%end 1 999 1")) {
		t.Fatal("fixture not ready")
	}
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{s})
	target := &Session{Name: "same:protocol", SocketName: s.SocketName}
	got, err := target.CapturePane()
	if err != nil || got != string(want) {
		t.Fatalf("native fallback bytes altered err=%v got=%q want=%q", err, got, want)
	}
	if m.Diagnostics().FramingFailures != 1 || m.Diagnostics().Successes != 0 {
		t.Fatal("ambiguous frame accepted", m.Diagnostics())
	}
}

func TestMatrixQueryMissingAnchorBackoff(t *testing.T) {
	s := matrixQueryFixture(t, "query-missing-anchor")
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{{Name: "missing", SocketName: s.SocketName}})
	args := []string{"capture-pane", "-t", s.Name, "-p"}
	if _, ok := m.output(context.Background(), s.SocketName, args); ok {
		t.Fatal("missing anchor accepted")
	}
	socket := m.sockets[matrixQuerySocketKey(s.SocketName)]
	if socket.retry.IsZero() || socket.conn != nil {
		t.Fatal("failed attach must back off")
	}
	retry := socket.retry
	if _, ok := m.output(context.Background(), s.SocketName, args); ok || socket.retry != retry {
		t.Fatal("backoff retried attach")
	}
	m.RegisterTargets("fixture", []*Session{s})
	if _, ok := m.output(context.Background(), s.SocketName, args); !ok {
		t.Fatal("replacement valid anchor blocked")
	}
}

func TestMatrixQueryConcurrentClose(t *testing.T) {
	s := matrixQueryFixture(t, "query-close-race")
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{s})
	args := []string{"capture-pane", "-t", s.Name, "-p", "-e"}
	if _, ok := m.output(context.Background(), s.SocketName, args); !ok {
		t.Fatal("initial")
	}
	c := m.sockets[matrixQuerySocketKey(s.SocketName)].observed.Load()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, _ = m.output(context.Background(), s.SocketName, args) }()
	}
	close(start)
	m.Close()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued queries survived manager close")
	}
	select {
	case <-c.readerDone:
	default:
		t.Fatal("reader survived close")
	}
	select {
	case <-c.waitDone:
	default:
		t.Fatal("reaper survived close")
	}
	if out := matrixNativeQuery(t, s.SocketName, "list-panes", "-a", "-F", "#{pane_pid}"); len(out) == 0 {
		t.Fatal("manager closed AI pane")
	}
}
func TestMatrixQueryRejectsUnsupportedBytesAndFormats(t *testing.T) {
	for _, args := range [][]string{{"capture-pane", "-t", string([]byte{255}), "-p"}, {"capture-pane", "-t", strings.Repeat("a", 16385), "-p"}, {"list-panes", "-a", "-F", "#{E:pane_title}"}, {"list-panes", "-a", "-F", "#{pane_id}\t#{pane_title}"}, {"display-message", "-t", "same", "-p", "#{@arbitrary}"}} {
		if _, ok := matrixQueryCommands(args); ok {
			t.Fatalf("unsupported bytes/format accepted %q", args)
		}
	}
}
