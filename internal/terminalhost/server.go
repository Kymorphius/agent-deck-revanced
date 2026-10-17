// matrix/hardened: bounded screen/output ownership survives client disconnects.
package terminalhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

type Server struct {
	process      Process
	mu           sync.Mutex
	inputMu      sync.Mutex
	resizeMu     sync.Mutex
	emulator     *vt.Emulator
	cols, rows   int
	history      []byte
	head         uint64
	running      bool
	exitCode     *uint32
	processError string
	connMu       sync.Mutex
	listener     net.Listener
	connections  map[net.Conn]struct{}
	closed       bool
	closeOnce    sync.Once
	stopOnce     sync.Once
	closeError   error
	done         chan struct{}
	outputDone   chan struct{}
	workers      sync.WaitGroup

	bracketedPaste bool
}

func validSize(cols, rows int) bool {
	return cols >= 1 && rows >= 1 && cols <= 500 && rows <= 300
}

func New(process Process, cols, rows int) (*Server, error) {
	if process == nil || !validSize(cols, rows) {
		return nil, errors.New("process and terminal dimensions (1..500 by 1..300) required")
	}
	s := &Server{process: process, cols: cols, rows: rows, running: true, emulator: vt.NewEmulator(cols, rows), connections: make(map[net.Conn]struct{}), done: make(chan struct{}), outputDone: make(chan struct{})}
	// matrix/hardened: expose only the input mode a viewer needs, never replay
	// arbitrary child control sequences. VT callbacks run while s.mu owns Write
	// or Resize; RIS also resets this mode through DisableMode.
	s.emulator.SetCallbacks(vt.Callbacks{
		EnableMode: func(mode ansi.Mode) {
			if mode == ansi.ModeBracketedPaste {
				s.bracketedPaste = true
			}
		},
		DisableMode: func(mode ansi.Mode) {
			if mode == ansi.ModeBracketedPaste {
				s.bracketedPaste = false
			}
		},
	})
	// A hosted program cannot request clipboard access through the snapshot engine.
	s.emulator.RegisterOscHandler(52, func([]byte) bool { return true })
	// matrix/hardened: plain DSR requires a plain reply; the pinned VT library
	// emits a private DEC status reply for CSI 5 n.
	s.emulator.RegisterCsiHandler(ansi.Command(0, 0, 'n'), func(params ansi.Params) bool {
		n, _, ok := params.Param(0, 1)
		if ok && n == 5 {
			_, _ = io.WriteString(s.emulator.InputPipe(), "\x1b[0n")
			return true
		}
		return false
	})
	s.workers.Add(3)
	go s.readOutput()
	go func() {
		defer s.workers.Done()
		defer func() {
			if writer, ok := s.emulator.InputPipe().(io.Closer); ok {
				_ = writer.Close()
			}
		}()
		// VT device queries must be answered even while no viewer is connected.
		_, _ = io.Copy(serialInput{s}, s.emulator)
	}()
	go func() {
		defer s.workers.Done()
		code, err := process.Wait()
		closeErr := process.Close()
		<-s.outputDone
		s.mu.Lock()
		s.running = false
		if err == nil {
			s.exitCode = &code
		}
		if joined := errors.Join(err, closeErr); joined != nil {
			s.processError = joined.Error()
		}
		s.mu.Unlock()
	}()
	return s, nil
}

type serialInput struct{ server *Server }

func (w serialInput) Write(p []byte) (int, error) {
	w.server.inputMu.Lock()
	defer w.server.inputMu.Unlock()
	return w.server.process.Input().Write(p)
}

func (s *Server) readOutput() {
	defer s.workers.Done()
	defer close(s.outputDone)
	buffer := make([]byte, 32*1024)
	for {
		n, err := s.process.Output().Read(buffer)
		if n > 0 {
			s.mu.Lock()
			s.head += uint64(n)
			s.history = append(s.history, buffer[:n]...)
			if len(s.history) > HistoryBytes {
				s.history = append([]byte(nil), s.history[len(s.history)-HistoryBytes:]...)
			}
			_, _ = s.emulator.Write(buffer[:n])
			s.mu.Unlock()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.mu.Lock()
				s.processError = err.Error()
				s.mu.Unlock()
			}
			return
		}
	}
}

func (s *Server) snapshot(after uint64, capture bool) Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Response{OK: true, PID: s.process.PID(), HostPID: os.Getpid(), Running: s.running, Cols: s.cols, Rows: s.rows, HeadOffset: s.head, EndOffset: s.head}
	if s.exitCode != nil {
		code := *s.exitCode
		r.ExitCode = &code
	}
	if s.processError != "" {
		r.Error = s.processError
	}
	if capture {
		// matrix/hardened: an omitted false value keeps older snapshot clients compatible.
		r.BracketedPaste = s.bracketedPaste
		base := s.head - uint64(len(s.history))
		start := after
		if start < base {
			start = base
			r.Truncated = true
		}
		if start > s.head {
			r.OK = false
			r.Error = "output cursor is ahead of this host"
			return r
		}
		end := min(s.head, start+MaxOutputBytes)
		r.StartOffset = start
		r.EndOffset = end
		r.Output = append([]byte(nil), s.history[start-base:end-base]...)
		r.Render = s.emulator.Render()
		r.Screen = ansi.Strip(r.Render)
		position := s.emulator.CursorPosition()
		r.CursorX, r.CursorY = position.X, position.Y
	}
	return r
}

func (s *Server) handle(request Request) (Response, bool) {
	var err error
	switch request.Op {
	case "status":
		return s.snapshot(0, false), false
	case "capture":
		return s.snapshot(request.After, true), false
	case "send":
		if len(request.Data) > MaxInputBytes {
			err = fmt.Errorf("input exceeds %d bytes", MaxInputBytes)
			break
		}
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			err = errors.New("hosted process has exited")
			break
		}
		var n int
		n, err = serialInput{s}.Write(request.Data)
		if err == nil && n != len(request.Data) {
			err = io.ErrShortWrite
		}
	case "resize":
		if !validSize(request.Cols, request.Rows) {
			err = errors.New("terminal dimensions must be 1..500 by 1..300")
			break
		}
		s.resizeMu.Lock()
		err = s.process.Resize(request.Cols, request.Rows)
		if err == nil {
			s.mu.Lock()
			s.emulator.Resize(request.Cols, request.Rows)
			s.cols, s.rows = request.Cols, request.Rows
			s.mu.Unlock()
		}
		s.resizeMu.Unlock()
	case "stop":
		err = s.stopProcess()
		r := s.snapshot(0, false)
		if err != nil {
			r.OK = false
			r.Error = err.Error()
		}
		return r, true
	default:
		err = fmt.Errorf("unknown operation %q", request.Op)
	}
	r := s.snapshot(0, false)
	if err != nil {
		r.OK = false
		r.Error = err.Error()
	}
	return r, false
}

// Dispatch routes one operation inside an owning multi-session manager.
// matrix/hardened: session stop closes only this terminal, never its manager.
func (s *Server) Dispatch(request Request) Response {
	response, _ := s.handle(request)
	return response
}

// Serve accepts bounded independent clients; client EOF is never a stop action.
func (s *Server) Serve(listener net.Listener) error {
	s.connMu.Lock()
	if s.closed || s.listener != nil {
		s.connMu.Unlock()
		_ = listener.Close()
		return errors.New("host already served or closed")
	}
	s.listener = listener
	s.connMu.Unlock()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				return err
			}
		}
		s.connMu.Lock()
		if s.closed || len(s.connections) >= 16 {
			s.connMu.Unlock()
			_ = conn.Close()
			continue
		}
		s.connections[conn] = struct{}{}
		s.connMu.Unlock()
		go s.serveClient(conn)
	}
}

func (s *Server) serveClient(conn net.Conn) {
	defer func() { _ = conn.Close(); s.connMu.Lock(); delete(s.connections, conn); s.connMu.Unlock() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var request Request
	if err := json.NewDecoder(io.LimitReader(conn, maxRequestBytes)).Decode(&request); err != nil {
		return
	}
	response, stop := s.handle(request)
	_ = json.NewEncoder(conn).Encode(response)
	if stop {
		go func() { _ = s.Close() }()
	}
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.connMu.Lock()
		s.closed = true
		close(s.done)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		for conn := range s.connections {
			_ = conn.Close()
		}
		s.connMu.Unlock()
		_ = s.stopProcess()
	})
	return s.closeError
}

// stopProcess completes owned-tree and output cleanup before stop can succeed.
func (s *Server) stopProcess() error {
	s.stopOnce.Do(func() {
		if writer, ok := s.emulator.InputPipe().(io.Closer); ok {
			_ = writer.Close()
		}
		s.closeError = s.process.Close()
		s.workers.Wait()
		s.mu.Lock()
		_ = s.emulator.Close()
		s.mu.Unlock()
	})
	return s.closeError
}
