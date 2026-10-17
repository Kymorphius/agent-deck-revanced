package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Options configures one user-level collector. Zero durations use production
// defaults. matrix/hardened: consumers activate profiles; there is no fleet scan.
type Options struct {
	Version      string
	OwnerUID     int
	Interval     time.Duration
	DemandTTL    time.Duration
	HistoryLimit int
	PassBudget   time.Duration
	MaxWorkers   int
	// matrix/hardened: ownership transfers to Server; nil keeps native queries.
	QueryTransport *tmux.QueryTransportManager
	// Test seams also allow embedding without owning the application's registry.
	LoadProfile     func(string) (ProfileRegistry, error)
	Probe           func(*session.Instance, *session.StatusUpdatePass, time.Time) session.RuntimeSessionSnapshot
	ValidateProfile func(string) error
}

type Server struct {
	opts         Options
	epoch        string
	startedAt    time.Time
	mu           sync.Mutex
	profiles     map[string]*profileState
	wake         chan struct{}
	stop         chan struct{}
	stopOnce     sync.Once
	closeOnce    sync.Once
	workers      chan struct{}
	requests     atomic.Uint64
	connections  atomic.Int64
	loads        atomic.Uint64
	publications atomic.Uint64
	errors       atomic.Uint64
	hookMu       sync.Mutex
	hooks        *session.StatusFileWatcher
	hookDone     chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	serving      atomic.Bool
}

func New(opts Options) *Server {
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.DemandTTL <= 0 {
		opts.DemandTTL = 30 * time.Second
	}
	if opts.HistoryLimit <= 0 || opts.HistoryLimit > 64 {
		opts.HistoryLimit = 64
	}
	if opts.PassBudget <= 0 {
		opts.PassBudget = 4 * time.Second
	}
	if opts.MaxWorkers <= 0 || opts.MaxWorkers > 10 {
		opts.MaxWorkers = 10
	}
	if opts.OwnerUID == 0 && os.Getuid() != 0 {
		opts.OwnerUID = os.Getuid()
	}
	if opts.LoadProfile == nil {
		opts.LoadProfile = openRegistry
	}
	if opts.ValidateProfile == nil {
		opts.ValidateProfile = validateProfile
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("monitor epoch entropy: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{opts: opts, epoch: hex.EncodeToString(b), startedAt: time.Now().UTC(), profiles: make(map[string]*profileState), wake: make(chan struct{}, 1), stop: make(chan struct{}), workers: make(chan struct{}, opts.MaxWorkers), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func validateProfile(profile string) error {
	if profile == "" || len(profile) > 128 || profile != filepath.Base(profile) || profile == "." || profile == ".." || strings.ContainsAny(profile, "/\\\x00") {
		return fmt.Errorf("invalid monitor profile")
	}
	exists, err := session.ProfileExists(profile)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("monitor profile does not exist")
	}
	return nil
}

type peerListener struct {
	net.Listener
	uid    int
	server *Server
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if checkPeer(conn, l.uid) != nil {
			_ = conn.Close()
			continue
		}
		if l.server.connections.Load() >= 64 {
			_ = conn.Close()
			continue
		}
		l.server.connections.Add(1)
		return &countedConn{Conn: conn, count: &l.server.connections}, nil
	}
}

type countedConn struct {
	net.Conn
	once  sync.Once
	count *atomic.Int64
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.count.Add(-1) })
	return err
}

// Serve runs HTTP exclusively over authenticated local Unix peers. Unknown
// routes, methods and protocol versions fail before creating collection demand.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	if !s.serving.CompareAndSwap(false, true) {
		return fmt.Errorf("monitor server already served")
	}
	go s.run()
	server := &http.Server{Handler: s, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 4096}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(&peerListener{Listener: ln, uid: s.opts.OwnerUID, server: s}) }()
	var err error
	select {
	case <-ctx.Done():
	case <-s.stop:
	case err = <-serveDone:
	}
	s.cancel()
	_ = server.Close()
	s.Close()
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Close never stops AI panes. Hung legacy probes retain their registry owner
// until they finish; shutting a DB underneath those targeted writes is unsafe.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		// matrix/hardened: close owned clients even before Serve, on HTTP stop,
		// or after listener errors; never rely on CLI defers surviving os.Exit.
		s.opts.QueryTransport.Close()
		s.hookMu.Lock()
		hookDone := s.hookDone
		if s.hooks != nil {
			s.hooks.Stop()
		}
		s.hookMu.Unlock()
		if hookDone != nil {
			select {
			case <-hookDone:
			case <-time.After(time.Second):
			}
		}
		if !s.serving.Load() {
			return
		}
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	w.Header().Set(protocolHeader, strconv.Itoa(Protocol))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get(protocolHeader) != strconv.Itoa(Protocol) {
		s.fail(w, http.StatusPreconditionFailed, "monitor protocol version mismatch", nil)
		return
	}
	wantMethod := http.MethodGet
	if r.URL.Path == "/refresh" || r.URL.Path == "/stop" {
		wantMethod = http.MethodPost
	}
	switch r.URL.Path {
	case "/snapshot", "/observations", "/refresh", "/status", "/stop":
	default:
		s.fail(w, http.StatusNotFound, "unknown monitor endpoint", nil)
		return
	}
	if r.Method != wantMethod {
		w.Header().Set("Allow", wantMethod)
		s.fail(w, http.StatusMethodNotAllowed, "unsupported monitor method", nil)
		return
	}
	if r.ContentLength != 0 || len(r.URL.RawQuery) > 2048 {
		s.fail(w, http.StatusBadRequest, "monitor request must be empty and bounded", nil)
		return
	}
	if r.URL.Path == "/status" {
		_ = json.NewEncoder(w).Encode(s.Status())
		return
	}
	if r.URL.Path == "/stop" {
		body := []byte("{\"stopping\":true}\n")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
		_ = http.NewResponseController(w).Flush()
		s.stopOnce.Do(func() { close(s.stop) })
		return
	}
	profile := r.URL.Query().Get("profile")
	if err := s.opts.ValidateProfile(profile); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	var cursor uint64
	if r.URL.Path == "/observations" {
		var err error
		cursor, err = strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		if err != nil || (cursor > 0 && r.URL.Query().Get("epoch") == "") {
			s.fail(w, http.StatusBadRequest, "invalid revision cursor", nil)
			return
		}
	}
	p, err := s.demand(profile)
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err.Error(), nil)
		return
	}
	if r.URL.Path == "/refresh" {
		requestedAt := time.Now()
		p.mu.Lock()
		p.refresh = true
		p.mu.Unlock()
		s.signal()
		if err := p.waitFresh(r.Context(), requestedAt); err != nil {
			s.fail(w, http.StatusServiceUnavailable, err.Error(), nil)
			return
		}
	}
	if err := p.wait(r.Context(), 0); err != nil {
		s.fail(w, http.StatusServiceUnavailable, err.Error(), nil)
		return
	}
	if r.URL.Path == "/observations" {
		body, gap, err := p.observations(r.URL.Query().Get("epoch"), cursor)
		if gap != nil {
			s.fail(w, http.StatusConflict, gap.Error(), gap)
			return
		}
		if err != nil {
			s.fail(w, http.StatusServiceUnavailable, err.Error(), nil)
			return
		}
		_, _ = w.Write(body)
		return
	}
	p.mu.Lock()
	body := p.latest.encoded
	p.mu.Unlock()
	_, _ = w.Write(body)
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string, gap *GapError) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(wireError{Error: msg, Gap: gap})
}
func (s *Server) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Server) forceActive() {
	s.mu.Lock()
	for _, p := range s.profiles {
		p.mu.Lock()
		if time.Now().Before(p.demandUntil) {
			p.eventRefresh = true
		}
		p.mu.Unlock()
	}
	s.mu.Unlock()
	s.signal()
}
func (s *Server) demand(profile string) (*profileState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, ErrClosed
	}
	p := s.profiles[profile]
	if p == nil {
		if len(s.profiles) >= 16 {
			return nil, fmt.Errorf("monitor profile capacity reached")
		}
		p = newProfile(profile, s.epoch)
		s.profiles[profile] = p
	}
	p.mu.Lock()
	now := time.Now()
	wasIdle := now.After(p.demandUntil)
	p.demandUntil = now.Add(s.opts.DemandTTL)
	if wasIdle {
		p.refresh = true
		p.wakeFence = now
	}
	p.mu.Unlock()
	s.signal()
	return p, nil
}

func (s *Server) Status() Status {
	st := Status{Protocol: Protocol, PID: os.Getpid(), Version: s.opts.Version, Epoch: s.epoch, StartedAt: s.startedAt, Connections: s.connections.Load(), Requests: s.requests.Load(), SourceLoads: s.loads.Load(), Publications: s.publications.Load(), Errors: s.errors.Load(), Inflight: len(s.workers)}
	// matrix/hardened: status reuses the private monitor route and adds no probes.
	st.TmuxCommands = tmux.SubprocessMetrics()
	st.BatchScheduling = CaptureBatchSchedulingSnapshot()
	st.QueryTransport = s.opts.QueryTransport.Diagnostics()
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Profiles = len(s.profiles)
	for _, p := range s.profiles {
		p.mu.Lock()
		if time.Now().Before(p.demandUntil) {
			st.ActiveProfiles++
		}
		p.mu.Unlock()
	}
	return st
}

func (s *Server) run() {
	defer close(s.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			s.retireProfiles()
			return
		case <-s.wake:
		case <-timer.C:
		}
		s.mu.Lock()
		profiles := sortedProfiles(s.profiles)
		s.mu.Unlock()
		for _, p := range profiles {
			if s.ctx.Err() != nil {
				break
			}
			p.mu.Lock()
			now := time.Now()
			active := now.Before(p.demandUntil)
			// matrix/hardened: a busy hooks directory cannot trigger a complete
			// fleet probe every 100ms. Explicit mutation refreshes stay immediate.
			eventDue := p.eventRefresh && now.Sub(p.lastPass) >= 500*time.Millisecond
			due := active && (p.refresh || eventDue || p.latest == nil || now.Sub(p.lastPass) >= s.opts.Interval)
			p.refresh = false
			if due {
				p.eventRefresh = false
			}
			p.mu.Unlock()
			if due {
				s.collect(p)
			} else if !active {
				// matrix/hardened: idle profiles own no persistent query client.
				if s.opts.QueryTransport != nil {
					s.opts.QueryTransport.ReleaseTargets(p.name)
				}
				if p.sse != nil {
					p.sse.Sync(nil)
				}
			}
		}
		// No consumer keeps a hooks filesystem watcher or OpenCode stream alive.
		if s.Status().ActiveProfiles == 0 {
			s.hookMu.Lock()
			if s.hooks != nil {
				s.hooks.Stop()
				s.hooks = nil
			}
			s.hookMu.Unlock()
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		// Only a pending evidence event needs the shorter coalescing deadline.
		next := s.opts.Interval
		for _, p := range profiles {
			p.mu.Lock()
			if p.eventRefresh && time.Now().Before(p.demandUntil) {
				remaining := 500*time.Millisecond - time.Since(p.lastPass)
				if remaining < time.Millisecond {
					remaining = time.Millisecond
				}
				if remaining < next {
					next = remaining
				}
			}
			p.mu.Unlock()
		}
		timer.Reset(next)
	}
}
