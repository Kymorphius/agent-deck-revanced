// Package nativefleet owns independent native terminals and durable descriptions.
// matrix/hardened: this experimental manager never touches production tmux,
// account data, hooks, automatic startup, or a session's working-directory files.
package nativefleet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

const (
	MaxSessions     = 32
	MaxClients      = 64
	ProtocolVersion = 2
)

var (
	ErrClosed   = errors.New("native session manager is shutting down or closed")
	ErrStaleRun = errors.New("native session run changed; refresh the session before retrying")
	ErrCapacity = errors.New("native session manager has reached its 32-session limit")
)

// Registry is the native manager's persistence seam; the original StateDB and
// the v0.3 private registry can both supply immutable launch descriptions.
// matrix/hardened: lifecycle code does not own a second database schema.
type Registry interface {
	Directory() (string, error)
	List() ([]nativeregistry.Record, error)
	Get(string) (nativeregistry.Record, error)
	CreateInGroup(string, string, string, terminalhost.LaunchSpec) (nativeregistry.Record, error)
	Move(string, string) error
	Rename(string, string) error
	Delete(string) error
	Close() error
}

type entry struct {
	// matrix/hardened: route and lifecycle changes share one session lock;
	// a viewer for an old run cannot send to a replacement while it launches.
	mu           sync.Mutex
	record       nativeregistry.Record
	host         *terminalhost.Server
	orphan       terminalhost.Process
	runID        string
	stopped      bool
	startupError string
	deleted      bool
}

type Server struct {
	store    Registry
	launch   func(terminalhost.LaunchSpec) (terminalhost.Process, error)
	identity string
	metadata Identity
	mu       sync.Mutex
	sessions map[string]*entry
	closing  bool
	active   sync.WaitGroup
	createMu sync.Mutex

	quiesceOnce sync.Once
	quiesceErr  error
	closeOnce   sync.Once
	closeErr    error

	transport transport
}

// Identity describes the explicit manager scope clients must verify before
// reusing a background process. It does not change registry or launch paths.
// matrix/hardened: mismatched old managers must never be silently restarted.
type Identity struct {
	StateDir string
	Version  string
}

// New consumes store ownership only on success. Recovered descriptions are
// stopped; restarting a manager must never restart an agent or account flow.
func New(store Registry, launch func(terminalhost.LaunchSpec) (terminalhost.Process, error), identities ...Identity) (*Server, error) {
	if store == nil || reflect.ValueOf(store).Kind() == reflect.Pointer && reflect.ValueOf(store).IsNil() || launch == nil {
		return nil, errors.New("native registry and process launcher are required")
	}
	var metadata Identity
	if len(identities) > 1 {
		return nil, errors.New("native manager accepts at most one identity")
	}
	if len(identities) == 1 {
		metadata = identities[0]
		if !filepath.IsAbs(metadata.StateDir) || filepath.Clean(metadata.StateDir) != metadata.StateDir {
			return nil, errors.New("native manager identity requires a canonical absolute state directory")
		}
		if metadata.Version == "" || strings.TrimSpace(metadata.Version) != metadata.Version || strings.ContainsFunc(metadata.Version, unicode.IsControl) {
			return nil, errors.New("native manager identity requires a nonempty version without whitespace or controls")
		}
		root, err := store.Directory()
		if err != nil {
			return nil, err
		}
		matches := metadata.StateDir == root
		if runtime.GOOS == "windows" {
			matches = strings.EqualFold(metadata.StateDir, root)
		}
		if !matches {
			return nil, errors.New("native manager identity state directory differs from its owned registry")
		}
		// matrix/hardened: the store verifies held path identities before this
		// handshake; a caller label cannot advertise a different namespace.
		metadata.StateDir = root
	}
	records, err := store.List()
	if err != nil {
		return nil, err
	}
	if len(records) > MaxSessions {
		return nil, ErrCapacity
	}
	s := &Server{store: store, launch: launch, identity: uuid.NewString(), metadata: metadata, sessions: make(map[string]*entry)}
	for _, record := range records {
		if !supportedTool(record.Tool) {
			return nil, fmt.Errorf("unsupported native tool %q", record.Tool)
		}
		s.sessions[record.ID] = &entry{record: record, stopped: true}
	}
	s.transport.initialize()
	return s, nil
}

func supportedTool(tool string) bool {
	return tool == "shell" || tool == "codex" || tool == "powershell" || tool == "custom"
}

func (s *Server) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return ErrClosed
	}
	s.active.Add(1)
	return nil
}

func failed(err error) terminalhost.Response {
	return terminalhost.Response{OK: false, Error: err.Error(), HostPID: os.Getpid()}
}

// Dispatch performs the same generation-checked operation as the wire handler.
// matrix/hardened: clients bind RunID from their first capture/status snapshot;
// every later provided generation, including reads, must still match.
func (s *Server) Dispatch(request terminalhost.Request) terminalhost.Response {
	if request.Op == "fleet-shutdown" {
		err := s.quiesce()
		response := terminalhost.Response{OK: err == nil, HostPID: os.Getpid(), RunID: s.identity}
		if err != nil {
			response.Error = err.Error()
		}
		return response
	}
	if err := s.begin(); err != nil {
		return failed(err)
	}
	defer s.active.Done()
	switch request.Op {
	case "fleet-list", "fleet-status":
		return s.list(request.Op == "fleet-status")
	case "fleet-create":
		return s.create(request)
	case "fleet-start", "fleet-resume", "fleet-stop", "fleet-rename", "fleet-move", "fleet-remove", "status", "capture", "send", "resize", "stop":
		return s.route(request)
	default:
		return failed(fmt.Errorf("unknown native manager operation %q", request.Op))
	}
}

func (s *Server) lookup(id string) (*entry, error) {
	if err := nativeregistry.ValidateID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	e := s.sessions[id]
	s.mu.Unlock()
	if e == nil {
		return nil, nativeregistry.ErrNotFound
	}
	return e, nil
}

func (e *entry) hasRuntime() bool { return e.host != nil || e.orphan != nil }

func (e *entry) checkRun(runID string, mutation bool) error {
	if runID != "" && runID != e.runID || mutation && e.hasRuntime() && runID == "" {
		return ErrStaleRun
	}
	return nil
}

func (e *entry) response() terminalhost.Response {
	r := terminalhost.Response{OK: true, HostPID: os.Getpid(), Cols: e.record.Launch.Cols, Rows: e.record.Launch.Rows}
	if e.host != nil {
		r = e.host.Dispatch(terminalhost.Request{Op: "status"})
	} else if e.orphan != nil {
		r.PID = e.orphan.PID()
		// matrix/hardened: cleanup failure proves owned resources, not process
		// liveness. Preserve the generation/error without inventing running.
	}
	if e.startupError != "" {
		r.Error = e.startupError
	}
	r.SessionID, r.RunID = e.record.ID, e.runID
	return r
}

func (e *entry) info() terminalhost.SessionInfo {
	r := e.response()
	state := "stopped"
	if e.hasRuntime() {
		switch {
		case r.Running:
			state = "running"
		case r.Error != "":
			state = "error"
		case !e.stopped:
			state = "exited"
		}
	}
	return terminalhost.SessionInfo{ID: e.record.ID, Name: e.record.Name, Tool: e.record.Tool, Group: e.record.Group, Dir: e.record.Launch.Dir,
		State: state, PID: r.PID, HostPID: r.HostPID, RunID: e.runID, ExitCode: r.ExitCode, Error: r.Error,
		CreatedAt: e.record.CreatedAt, UpdatedAt: e.record.UpdatedAt}
}

func (s *Server) entries() []*entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]*entry, 0, len(s.sessions))
	for _, e := range s.sessions {
		entries = append(entries, e)
	}
	return entries
}

func (s *Server) list(identity bool) terminalhost.Response {
	// Check the still-owned registry before presenting its cached descriptions.
	if _, err := s.store.List(); err != nil {
		return failed(err)
	}
	r := terminalhost.Response{OK: true, HostPID: os.Getpid(), Sessions: make([]terminalhost.SessionInfo, 0)}
	if identity {
		r.RunID = s.identity
		// matrix/hardened: only the startup handshake needs manager scope and
		// protocol details; frequent session-list refreshes omit them.
		r.ManagerStateDir, r.ManagerVersion, r.ProtocolVersion = s.metadata.StateDir, s.metadata.Version, ProtocolVersion
	}
	for _, e := range s.entries() {
		e.mu.Lock()
		if !e.deleted {
			info := e.info()
			r.Sessions = append(r.Sessions, info)
			r.Running = r.Running || info.State == "running"
		}
		e.mu.Unlock()
	}
	sort.Slice(r.Sessions, func(i, j int) bool {
		if r.Sessions[i].CreatedAt.Equal(r.Sessions[j].CreatedAt) {
			return r.Sessions[i].ID < r.Sessions[j].ID
		}
		return r.Sessions[i].CreatedAt.Before(r.Sessions[j].CreatedAt)
	})
	return r
}

func (s *Server) create(request terminalhost.Request) terminalhost.Response {
	if request.Launch == nil {
		return failed(errors.New("native session launch description is required"))
	}
	if !supportedTool(request.Tool) {
		return failed(errors.New("native tool must be shell, codex, powershell, or custom"))
	}
	if request.SessionID != "" || request.RunID != "" {
		return failed(errors.New("new native session must not specify an existing session or run ID"))
	}
	// Serialize only descriptor creation, not unrelated native launches/routes.
	s.createMu.Lock()
	s.mu.Lock()
	full := len(s.sessions) >= MaxSessions
	s.mu.Unlock()
	if full {
		s.createMu.Unlock()
		return failed(ErrCapacity)
	}
	record, err := s.store.CreateInGroup(request.Name, request.Tool, request.Group, *request.Launch)
	if err != nil {
		s.createMu.Unlock()
		return failed(err)
	}
	e := &entry{record: record, stopped: true}
	e.mu.Lock()
	s.mu.Lock()
	s.sessions[record.ID] = e
	s.mu.Unlock()
	s.createMu.Unlock()
	defer e.mu.Unlock()
	if request.AutoStart {
		err = s.start(e)
	}
	r := e.response()
	r.Sessions = []terminalhost.SessionInfo{e.info()}
	if err != nil {
		r.OK, r.Error = false, err.Error()
	}
	return r
}

func (s *Server) route(request terminalhost.Request) terminalhost.Response {
	e, err := s.lookup(request.SessionID)
	if err != nil {
		return failed(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.deleted {
		return failed(nativeregistry.ErrNotFound)
	}
	mutation := request.Op != "status" && request.Op != "capture"
	if err := e.checkRun(request.RunID, mutation); err != nil {
		r := e.response()
		r.OK, r.Error = false, err.Error()
		return r
	}
	// matrix/hardened: selecting history is bound to exactly the displayed old
	// generation, including an empty generation after a manager restart.
	if request.Op == "fleet-resume" && request.RunID != e.runID {
		return failed(ErrStaleRun)
	}
	switch request.Op {
	case "fleet-start":
		err = s.start(e)
	case "fleet-resume":
		err = s.startWithHistory(e, true)
	case "fleet-stop", "stop":
		err = e.stop()
	case "fleet-rename":
		if err = s.store.Rename(e.record.ID, request.Name); err == nil {
			e.record, err = s.store.Get(e.record.ID)
		}
	case "fleet-move":
		// matrix/hardened: moving updates only durable metadata under the same
		// run fence as rename; it must never restart or reconfigure the process.
		if err = s.store.Move(e.record.ID, request.Group); err == nil {
			e.record, err = s.store.Get(e.record.ID)
		}
	case "fleet-remove":
		if err = e.stop(); err == nil {
			err = s.store.Delete(e.record.ID)
		}
		if err == nil {
			e.deleted = true
			s.mu.Lock()
			delete(s.sessions, e.record.ID)
			s.mu.Unlock()
		}
	default:
		if e.host != nil {
			r := e.host.Dispatch(request)
			r.SessionID, r.RunID = e.record.ID, e.runID
			return r
		}
		if request.Op == "send" || request.Op == "resize" {
			err = errors.New("native session is stopped; explicitly start it first")
		}
	}
	r := e.response()
	if !e.deleted {
		r.Sessions = []terminalhost.SessionInfo{e.info()}
	}
	if err != nil {
		r.OK, r.Error = false, err.Error()
	}
	return r
}

func (s *Server) start(e *entry) error {
	return s.startWithHistory(e, false)
}

// matrix/hardened: Codex owns its cwd-filtered history picker. Resume adds only
// a temporary subcommand, never model, account, approval or saved-launch flags.
func (s *Server) startWithHistory(e *entry, resume bool) error {
	// matrix/hardened: validate registry ownership immediately before launch;
	// caller-closing or replacing the owned store cannot launch stale metadata.
	record, err := s.store.Get(e.record.ID)
	if err != nil {
		return err
	}
	e.record = record
	launch := record.Launch
	if resume {
		state := e.info().State
		if state != "stopped" && state != "exited" {
			return errors.New("Codex history can only be selected after the current run stops or exits")
		}
		executable := filepath.Base(record.Launch.Executable)
		nativeCodex := executable == "codex"
		if runtime.GOOS == "windows" {
			nativeCodex = strings.EqualFold(executable, "codex.exe")
		}
		if record.Tool != "codex" || !nativeCodex || len(record.Launch.Args) != 0 {
			return errors.New("Codex history requires a native Codex executable with no saved custom arguments")
		}
		launch.Args = []string{"resume"}
	}
	if e.host != nil && e.host.Dispatch(terminalhost.Request{Op: "status"}).Running {
		return nil // Concurrent explicit starts observe the same owned run.
	}
	if err := e.stop(); err != nil {
		return err
	}
	e.host, e.orphan, e.runID, e.startupError = nil, nil, "", ""
	runID := uuid.NewString()
	process, err := s.launch(launch)
	// matrix/hardened: Go adapters may return a typed nil pointer through the
	// Process interface on startup failure; never invoke its native methods.
	if nilProcess(process) {
		process = nil
	}
	if err == nil && process == nil {
		err = errors.New("native launcher returned no process")
	}
	if err != nil {
		if process != nil {
			if cleanupErr := process.Close(); cleanupErr != nil {
				e.orphan, e.runID = process, runID
				err = errors.Join(err, fmt.Errorf("partial launch cleanup: %w", cleanupErr))
			}
		}
		e.startupError = err.Error()
		return err
	}
	host, err := terminalhost.New(process, record.Launch.Cols, record.Launch.Rows)
	if err != nil {
		if cleanupErr := process.Close(); cleanupErr != nil {
			e.orphan, e.runID = process, runID
			err = errors.Join(err, fmt.Errorf("terminal setup cleanup: %w", cleanupErr))
		}
		e.startupError = err.Error()
		return err
	}
	e.host, e.runID, e.stopped = host, runID, false
	return nil
}

func nilProcess(process terminalhost.Process) bool {
	if process == nil {
		return true
	}
	value := reflect.ValueOf(process)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (e *entry) stop() error {
	if e.orphan != nil {
		if err := e.orphan.Close(); err != nil {
			e.startupError = fmt.Sprintf("owned partial process cleanup: %v", err)
			return err
		}
		e.orphan = nil
	}
	if e.host != nil {
		if err := e.host.Close(); err != nil {
			return fmt.Errorf("owned native process cleanup: %w", err)
		}
		if e.host.Dispatch(terminalhost.Request{Op: "status"}).Running {
			return errors.New("owned native process cleanup has no confirmed exit")
		}
	}
	e.stopped = true
	return nil
}

func (s *Server) quiesce() error {
	s.quiesceOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.transport.stopAdmission()
		// No global lock is held while accepted operations or native I/O drain.
		s.active.Wait()
		for _, e := range s.entries() {
			e.mu.Lock()
			if !e.deleted {
				if err := e.stop(); err != nil {
					s.quiesceErr = errors.Join(s.quiesceErr, fmt.Errorf("session %s: %w", e.record.ID, err))
				}
			}
			e.mu.Unlock()
		}
	})
	return s.quiesceErr
}

// Close waits for exact owned native cleanup and in-flight replies before
// releasing the exclusive registry. A failed cleanup keeps ownership fenced.
func (s *Server) Close() error {
	if err := s.quiesce(); err != nil {
		return err
	}
	s.closeOnce.Do(func() {
		s.transport.drainAndClose()
		s.closeErr = s.store.Close()
	})
	return s.closeErr
}
