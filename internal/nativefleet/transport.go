// matrix/hardened: a bounded one-request transport cannot own terminal lifetime.
package nativefleet

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

const maxRequestBytes = 128 * 1024

type transport struct {
	mu          sync.Mutex
	listener    net.Listener
	connections map[net.Conn]struct{}
	workers     sync.WaitGroup
	draining    bool
	closed      chan struct{}
}

func (t *transport) initialize() {
	t.connections = make(map[net.Conn]struct{})
	t.closed = make(chan struct{})
}

func (t *transport) stopAdmission() {
	t.mu.Lock()
	t.draining = true
	t.mu.Unlock()
}

func (t *transport) drainAndClose() {
	t.workers.Wait()
	t.mu.Lock()
	defer t.mu.Unlock()
	close(t.closed)
	if t.listener != nil {
		_ = t.listener.Close()
	}
}

// Serve accepts at most 64 independent clients. EOF and malformed requests
// close only that client. Shutdown replies follow native cleanup, then all
// admitted replies drain before the listener and registry owner are closed.
func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("native manager listener is required")
	}
	t := &s.transport
	t.mu.Lock()
	if t.draining || t.listener != nil {
		t.mu.Unlock()
		_ = listener.Close()
		return errors.New("native manager is already served or closed")
	}
	t.listener = listener
	t.mu.Unlock()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-t.closed:
				return nil
			default:
				return err
			}
		}
		t.mu.Lock()
		if t.draining || len(t.connections) >= MaxClients {
			t.mu.Unlock()
			_ = conn.Close()
			continue
		}
		t.connections[conn] = struct{}{}
		t.workers.Add(1)
		t.mu.Unlock()
		go s.serveClient(conn)
	}
}

func (s *Server) serveClient(conn net.Conn) {
	shutdown := false
	defer func() {
		_ = conn.Close()
		s.transport.mu.Lock()
		delete(s.transport.connections, conn)
		s.transport.mu.Unlock()
		s.transport.workers.Done()
		if shutdown {
			go func() { _ = s.Close() }()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var request terminalhost.Request
	decoder := json.NewDecoder(io.LimitReader(conn, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return
	}
	response := s.Dispatch(request)
	shutdown = request.Op == "fleet-shutdown" && response.OK
	_ = json.NewEncoder(conn).Encode(response)
}
