// matrix/hardened: portable lifecycle tests use fake owned processes and an
// explicitly isolated SQLite root, never live agent accounts or shared state.
package nativefleet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

type fakeProcess struct {
	mu          sync.Mutex
	pid         uint32
	input       bytes.Buffer
	reader      *io.PipeReader
	writer      *io.PipeWriter
	done        chan struct{}
	once        sync.Once
	closeCount  int
	cols, rows  int
	closeError  error
	waitError   error
	code        uint32
	closeGate   chan struct{}
	closeBegan  chan struct{}
	closeNotify sync.Once
}

func newProcess(pid uint32) *fakeProcess {
	r, w := io.Pipe()
	return &fakeProcess{pid: pid, reader: r, writer: w, done: make(chan struct{})}
}

func (p *fakeProcess) PID() uint32       { return p.pid }
func (p *fakeProcess) Input() io.Writer  { return fakeInput{p} }
func (p *fakeProcess) Output() io.Reader { return p.reader }
func (p *fakeProcess) Resize(cols, rows int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cols, p.rows = cols, rows
	return nil
}
func (p *fakeProcess) Wait() (uint32, error) { <-p.done; return p.code, p.waitError }
func (p *fakeProcess) Close() error {
	p.once.Do(func() {
		if p.closeBegan != nil {
			p.closeNotify.Do(func() { close(p.closeBegan) })
		}
		if p.closeGate != nil {
			<-p.closeGate
		}
		p.mu.Lock()
		p.closeCount++
		p.mu.Unlock()
		_ = p.writer.Close()
		close(p.done)
	})
	return p.closeError
}

type fakeInput struct{ process *fakeProcess }

func (w fakeInput) Write(data []byte) (int, error) {
	w.process.mu.Lock()
	defer w.process.mu.Unlock()
	return w.process.input.Write(data)
}

func (p *fakeProcess) received() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.String()
}

type launcher struct {
	mu        sync.Mutex
	processes []*fakeProcess
}

func (l *launcher) launch(terminalhost.LaunchSpec) (terminalhost.Process, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := newProcess(uint32(4200 + len(l.processes)))
	l.processes = append(l.processes, p)
	return p, nil
}
func (l *launcher) latest() *fakeProcess {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.processes[len(l.processes)-1]
}
func (l *launcher) count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.processes) }

func testRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" && os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 to allow isolated Windows registry files")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "fleet-registry")
}

func fixture(t *testing.T, launch func(terminalhost.LaunchSpec) (terminalhost.Process, error)) (*Server, *nativeregistry.Store, string) {
	t.Helper()
	root := testRoot(t)
	store, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(store, launch)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = store.Close() })
	return s, store, root
}

func launchSpec() terminalhost.LaunchSpec {
	return terminalhost.LaunchSpec{Executable: "cmd.exe", Args: []string{"/d", "/q"}, Dir: `C:\native-test-project`, Cols: 80, Rows: 24}
}

func requireOK(t *testing.T, r terminalhost.Response) terminalhost.Response {
	t.Helper()
	if !r.OK {
		t.Fatalf("unexpected manager error: %+v", r)
	}
	return r
}

func create(t *testing.T, s *Server, name string, start bool) terminalhost.Response {
	t.Helper()
	spec := launchSpec()
	return requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: name, Tool: "shell", Launch: &spec, AutoStart: start}))
}

func forRun(op string, r terminalhost.Response) terminalhost.Request {
	return terminalhost.Request{Op: op, SessionID: r.SessionID, RunID: r.RunID}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expected observation did not arrive")
}

func TestConcurrentStartOnceAndGenerationFenceAfterRestart(t *testing.T) {
	l := &launcher{}
	s, _, _ := fixture(t, l.launch)
	descriptor := create(t, s, "中文 并发", false)
	var wg sync.WaitGroup
	results := make(chan terminalhost.Response, 20)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.Dispatch(forRun("fleet-start", descriptor)) }()
	}
	wg.Wait()
	close(results)
	var first terminalhost.Response
	for r := range results {
		if r.OK {
			if first.RunID != "" && r.RunID != first.RunID {
				t.Fatal("concurrent start replaced an already live run")
			}
			first = r
		} else if !strings.Contains(r.Error, "run changed") {
			t.Fatalf("unexpected concurrent start rejection: %+v", r)
		}
	}
	if first.RunID == "" || l.count() != 1 {
		t.Fatalf("launch count=%d run=%q", l.count(), first.RunID)
	}
	old := l.latest()
	requireOK(t, s.Dispatch(forRun("fleet-stop", first)))
	second := requireOK(t, s.Dispatch(forRun("fleet-start", first)))
	if second.RunID == first.RunID || second.PID == first.PID || l.count() != 2 {
		t.Fatalf("restart did not create a fresh fenced run: %+v -> %+v", first, second)
	}
	for _, op := range []string{"send", "resize", "stop", "fleet-stop", "fleet-remove", "fleet-rename", "fleet-move", "fleet-start", "status", "capture"} {
		request := forRun(op, first)
		request.Data, request.Name, request.Group, request.Cols, request.Rows = []byte("stale\r"), "stale-name", "stale-group", 90, 30
		if r := s.Dispatch(request); r.OK || !strings.Contains(r.Error, "run changed") {
			t.Fatalf("%s accepted stale generation: %+v", op, r)
		}
	}
	for _, op := range []string{"send", "resize", "stop", "fleet-stop", "fleet-remove", "fleet-rename", "fleet-move", "fleet-start"} {
		if r := s.Dispatch(terminalhost.Request{Op: op, SessionID: second.SessionID}); r.OK {
			t.Fatalf("%s accepted missing expected run", op)
		}
	}
	request := forRun("send", second)
	request.Data = []byte("fresh 中文\r")
	requireOK(t, s.Dispatch(request))
	if old.received() != "" || l.latest().received() != "fresh 中文\r" {
		t.Fatal("input reached the wrong generation")
	}
	if r := s.Dispatch(terminalhost.Request{Op: "status", SessionID: second.SessionID}); !r.OK || r.RunID != second.RunID {
		t.Fatal("first snapshot could not bind current generation")
	}
}

func TestDurableDescriptionsReopenStoppedAndRemovePreservesProject(t *testing.T) {
	l := &launcher{}
	s, store, root := fixture(t, l.launch)
	project := filepath.Join(filepath.Dir(root), "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(project, "raw-history.txt")
	if err := os.WriteFile(marker, []byte("keep exact raw evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	spec := launchSpec()
	spec.Dir = project
	r := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "persistent", Tool: "shell", Launch: &spec, AutoStart: true}))
	rename := forRun("fleet-rename", r)
	rename.Name = "重开仍保留"
	requireOK(t, s.Dispatch(rename))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); !errors.Is(err, nativeregistry.ErrClosed) {
		t.Fatalf("manager did not release its owned store: %v", err)
	}
	reopened, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(reopened, l.launch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(); _ = reopened.Close() })
	listed := requireOK(t, second.Dispatch(terminalhost.Request{Op: "fleet-list"}))
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != r.SessionID || listed.Sessions[0].Name != rename.Name || listed.Sessions[0].State != "stopped" || listed.Sessions[0].RunID != "" || l.count() != 1 {
		t.Fatalf("reopening restarted or lost the descriptor: %+v", listed)
	}
	requireOK(t, second.Dispatch(terminalhost.Request{Op: "fleet-remove", SessionID: r.SessionID}))
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep exact raw evidence" {
		t.Fatalf("descriptor removal changed raw project data: %q %v", data, err)
	}
}

func TestFailedLaunchRetainsStoppedDescriptionIncludingTypedNil(t *testing.T) {
	for _, mode := range []string{"nil-error", "typed-nil-error", "typed-nil-success"} {
		t.Run(mode, func(t *testing.T) {
			s, store, _ := fixture(t, func(terminalhost.LaunchSpec) (terminalhost.Process, error) {
				var typedNil *fakeProcess
				switch mode {
				case "nil-error":
					return nil, errors.New("missing executable")
				case "typed-nil-error":
					return typedNil, errors.New("missing executable")
				default:
					return typedNil, nil
				}
			})
			spec := launchSpec()
			r := s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: mode, Tool: "custom", Launch: &spec, AutoStart: true})
			if r.OK || r.SessionID == "" || r.RunID != "" || len(r.Sessions) != 1 || r.Sessions[0].State != "stopped" || r.Sessions[0].Error == "" {
				t.Fatalf("failed start hid or ran its persistent description: %+v", r)
			}
			if _, err := store.Get(r.SessionID); err != nil {
				t.Fatal("failed launch rolled back the description", err)
			}
			requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-remove", SessionID: r.SessionID}))
		})
	}
}

func TestLiveOutputFailureRemainsRunningAndStoppable(t *testing.T) {
	l := &launcher{}
	s, _, _ := fixture(t, l.launch)
	r := create(t, s, "output observation", true)
	p := l.latest()
	_ = p.writer.CloseWithError(errors.New("native output observation failed"))
	eventually(t, func() bool { return strings.Contains(s.Dispatch(forRun("status", r)).Error, "observation failed") })
	listed := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-status"}))
	if !listed.Running || len(listed.Sessions) != 1 || listed.Sessions[0].State != "running" || !strings.Contains(listed.Sessions[0].Error, "observation failed") {
		t.Fatalf("a live process became unmanageable after an observation error: %+v", listed)
	}
	requireOK(t, s.Dispatch(forRun("fleet-stop", r)))
	select {
	case <-p.done:
	default:
		t.Fatal("live errored process survived stop")
	}
}

func TestPartialLaunchErrorRetainsUncertainOwnershipUntilSuccessfulCleanup(t *testing.T) {
	p := newProcess(91)
	p.closeError = errors.New("partial cleanup uncertain")
	s, store, _ := fixture(t, func(terminalhost.LaunchSpec) (terminalhost.Process, error) {
		return p, errors.New("native setup failed after spawning")
	})
	spec := launchSpec()
	r := s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "partial owned", Tool: "custom", Launch: &spec, AutoStart: true})
	if r.OK || r.Running || r.RunID == "" || len(r.Sessions) != 1 || r.Sessions[0].State != "error" || r.Sessions[0].Error == "" {
		t.Fatalf("partial cleanup error invented liveness or lost ownership: %+v", r)
	}
	for _, op := range []string{"fleet-stop", "fleet-remove", "fleet-start", "fleet-rename", "fleet-move"} {
		if response := s.Dispatch(terminalhost.Request{Op: op, SessionID: r.SessionID}); response.OK || !strings.Contains(response.Error, "run changed") {
			t.Fatalf("partial resource %s accepted missing ownership generation: %+v", op, response)
		}
	}
	if response := s.Dispatch(forRun("fleet-remove", r)); response.OK {
		t.Fatal("partial owned cleanup failure allowed descriptor removal")
	}
	if _, err := store.Get(r.SessionID); err != nil {
		t.Fatal("partial-owned descriptor disappeared", err)
	}
	// No terminalhost workers were started for a failed launcher; the held
	// process's subsequent explicit cleanup can now be confirmed.
	p.closeError = nil
	requireOK(t, s.Dispatch(forRun("fleet-stop", r)))
	requireOK(t, s.Dispatch(forRun("fleet-remove", r)))
}

func TestRemoveAndShutdownRefuseUnconfirmedOwnedCleanup(t *testing.T) {
	p := newProcess(77)
	p.closeError = errors.New("owned job cleanup unknown")
	s, store, root := fixture(t, func(terminalhost.LaunchSpec) (terminalhost.Process, error) { return p, nil })
	r := create(t, s, "must retain", true)
	if response := s.Dispatch(forRun("fleet-remove", r)); response.OK || !strings.Contains(response.Error, "cleanup unknown") {
		t.Fatalf("failed removal claimed cleanup: %+v", response)
	}
	if _, err := store.Get(r.SessionID); err != nil {
		t.Fatalf("unconfirmed removal erased descriptor: %v", err)
	}
	if response := s.Dispatch(terminalhost.Request{Op: "fleet-shutdown"}); response.OK || !strings.Contains(response.Error, "cleanup unknown") {
		t.Fatalf("shutdown acknowledged unconfirmed cleanup: %+v", response)
	}
	if err := s.Close(); err == nil {
		t.Fatal("Close released an unconfirmed manager")
	}
	if other, err := nativeregistry.Open(root); !errors.Is(err, nativeregistry.ErrOwned) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("uncertain cleanup released registry ownership: %v", err)
	}
	if response := s.Dispatch(terminalhost.Request{Op: "fleet-list"}); response.OK || !strings.Contains(response.Error, "shutting down") {
		t.Fatalf("fenced manager accepted more work: %+v", response)
	}
}

func TestSlowStopDoesNotBlockAnotherSessionsInput(t *testing.T) {
	l := &launcher{}
	s, _, _ := fixture(t, l.launch)
	first := create(t, s, "slow cleanup", true)
	slow := l.latest()
	slow.closeGate, slow.closeBegan = make(chan struct{}), make(chan struct{})
	second := create(t, s, "independent", true)
	stopDone := make(chan terminalhost.Response, 1)
	go func() { stopDone <- s.Dispatch(forRun("fleet-stop", first)) }()
	select {
	case <-slow.closeBegan:
	case <-time.After(time.Second):
		t.Fatal("stop did not reach native cleanup")
	}
	request := forRun("send", second)
	request.Data = []byte("unrelated alive\r")
	inputDone := make(chan terminalhost.Response, 1)
	go func() { inputDone <- s.Dispatch(request) }()
	select {
	case response := <-inputDone:
		requireOK(t, response)
	case <-time.After(time.Second):
		close(slow.closeGate)
		t.Fatal("slow native work held a global route lock")
	}
	close(slow.closeGate)
	requireOK(t, <-stopDone)
}

func TestSessionBoundsInvalidIDsAndClosedStoreNeverLaunch(t *testing.T) {
	l := &launcher{}
	s, store, _ := fixture(t, l.launch)
	first := create(t, s, "record0", false)
	for i := 1; i < MaxSessions; i++ {
		create(t, s, "bounded", false)
	}
	spec := launchSpec()
	if response := s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "overflow", Tool: "shell", Launch: &spec}); response.OK || !strings.Contains(response.Error, "32-session") {
		t.Fatalf("manager exceeded session bound: %+v", response)
	}
	for _, id := range []string{"", "..", "../project", strings.ToUpper(first.SessionID)} {
		if response := s.Dispatch(terminalhost.Request{Op: "fleet-start", SessionID: id}); response.OK {
			t.Fatalf("accepted invalid ID %q", id)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if response := s.Dispatch(forRun("fleet-start", first)); response.OK || l.count() != 0 {
		t.Fatalf("closed store launched cached metadata: %+v", response)
	}
}

func serve(t *testing.T, s *Server) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0") // Test transport only; production uses private Windows pipes.
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(listener) }()
	return listener.Addr().String(), done
}

func call(t *testing.T, address string, request terminalhost.Request) terminalhost.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := terminalhost.Call(ctx, conn, request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestDetachedClientsKeepIndependentOutputAndShutdownAckMeansCleanup(t *testing.T) {
	l := &launcher{}
	s, _, root := fixture(t, l.launch)
	address, served := serve(t, s)
	first := create(t, s, "one", true)
	one := l.latest()
	second := create(t, s, "two", true)
	two := l.latest()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	_, _ = one.writer.Write([]byte("first 中文\r\n"))
	_, _ = two.writer.Write([]byte("second independent\r\n"))
	eventually(t, func() bool {
		return s.Dispatch(forRun("status", first)).HeadOffset > 0 && s.Dispatch(forRun("status", second)).HeadOffset > 0
	})
	firstView := call(t, address, forRun("capture", first))
	secondView := call(t, address, forRun("capture", second))
	if !strings.Contains(firstView.Screen, "first 中文") || strings.Contains(firstView.Screen, "second") || !strings.Contains(secondView.Screen, "second independent") || !firstView.Running || !secondView.Running {
		t.Fatal("detached sessions lost or mixed their screen/process ownership")
	}
	call(t, address, terminalhost.Request{Op: "fleet-shutdown"})
	for _, p := range []*fakeProcess{one, two} {
		select {
		case <-p.done:
		default:
			t.Fatal("successful shutdown acknowledgement preceded owned cleanup")
		}
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not drain and close its listener")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	newOwner, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatalf("shutdown retained a cleanly released registry: %v", err)
	}
	_ = newOwner.Close()
}

func TestBoundedClientsAndShutdownWaitsForAdmittedResponses(t *testing.T) {
	l := &launcher{}
	s, _, _ := fixture(t, l.launch)
	address, served := serve(t, s)
	clients := make([]net.Conn, 0, MaxClients)
	for i := 0; i < MaxClients; i++ {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
	}
	defer func() {
		for _, conn := range clients {
			_ = conn.Close()
		}
	}()
	eventually(t, func() bool {
		s.transport.mu.Lock()
		defer s.transport.mu.Unlock()
		return len(s.transport.connections) == MaxClients
	})
	extra, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	_ = extra.SetReadDeadline(time.Now().Add(time.Second))
	_, err = extra.Read(make([]byte, 1))
	_ = extra.Close()
	if err == nil {
		t.Fatal("extra client was admitted beyond 64 slots")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("extra client was left open rather than rejected at the client bound")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closing })
	select {
	case err := <-closeDone:
		t.Fatalf("Close discarded admitted responses instead of draining: %v", err)
	default:
	}
	for _, conn := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		response, err := terminalhost.Call(ctx, conn, terminalhost.Request{Op: "fleet-list"})
		cancel()
		if err == nil || response.OK || !strings.Contains(response.Error, "shutting down") {
			t.Fatalf("admitted client did not receive explicit shutdown reply: %+v %v", response, err)
		}
		_ = conn.Close()
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
