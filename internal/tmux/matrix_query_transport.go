package tmux

// matrix/hardened: the monitor's opt-in, read-only query channel is independent
// of viewer/control pipes. Errors always leave the existing native CLI as the
// authority. No command, pane contents, socket or target is exposed in metrics.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const matrixQueryLimit = 8 << 20
const matrixQuerySocketLimit = 32
const matrixQueryProfileLimit = 16
const matrixQueryDeadline = 350 * time.Millisecond
const matrixQueryBackoff = 5 * time.Second

var errMatrixQueryProtocol = errors.New("ambiguous tmux query framing")
var errMatrixQueryCommand = errors.New("tmux query returned error")

// QueryTransportStats contains bounded local diagnostics only.
type QueryTransportStats struct {
	Connections     int    `json:"connections"`
	ClientPIDs      []int  `json:"client_pids,omitempty"`
	Queries         uint64 `json:"queries"`
	Successes       uint64 `json:"successes"`
	Fallbacks       uint64 `json:"fallbacks"`
	FramingFailures uint64 `json:"framing_failures"`
	Timeouts        uint64 `json:"timeouts"`
	Starts          uint64 `json:"starts"`
}

type matrixQueryCounters struct{ queries, successes, fallbacks, framing, timeouts, starts atomic.Uint64 }
type matrixQueryTarget struct{ socket, name string }
type matrixQuerySocket struct {
	gate     chan struct{}
	queue    chan struct{}
	anchor   matrixQueryTarget
	conn     *matrixQueryConnection // protected by gate
	retry    time.Time
	retired  atomic.Bool
	observed atomic.Pointer[matrixQueryConnection]
}

// QueryTransportManager owns only clients it created. Registration is lazy;
// it never attaches until an eligible read actually arrives.
type QueryTransportManager struct {
	registrationMu sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	profiles       map[string][]matrixQueryTarget
	sockets        map[string]*matrixQuerySocket
	closed         bool
	counters       matrixQueryCounters
	closeOnce      sync.Once
}

var matrixQueryGlobal struct {
	sync.RWMutex
	manager *QueryTransportManager
}

// EnableQueryTransport is called only by the shared monitor owner. Ordinary
// TUI/Web/CLI processes retain their native query paths by default.
func EnableQueryTransport(parent context.Context) *QueryTransportManager {
	ctx, cancel := context.WithCancel(parent)
	m := &QueryTransportManager{ctx: ctx, cancel: cancel, profiles: make(map[string][]matrixQueryTarget), sockets: make(map[string]*matrixQuerySocket)}
	matrixQueryGlobal.Lock()
	previous := matrixQueryGlobal.manager
	matrixQueryGlobal.manager = m
	matrixQueryGlobal.Unlock()
	if previous != nil {
		previous.Close()
	}
	context.AfterFunc(ctx, m.Close)
	return m
}

func matrixQuerySocketKey(socket string) string {
	socket = strings.TrimSpace(socket)
	// Match tmux socket selection: an inherited TMUX path wins only without -L.
	// Explicit default and implicit default share one client when they resolve to
	// the same path; inherited other servers never alias a named default.
	path := ""
	if socket == "" {
		inherited := strings.Split(os.Getenv("TMUX"), ",")
		if len(inherited) == 3 && filepath.IsAbs(inherited[0]) {
			path = inherited[0]
		} else {
			socket = "default"
		}
	}
	if path == "" {
		root := os.Getenv("TMUX_TMPDIR")
		if root == "" {
			root = "/tmp"
		}
		path = filepath.Join(root, "tmux-"+strconv.Itoa(os.Getuid()), socket)
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		return canonical
	}
	return filepath.Clean(path)
}

// RegisterTargets replaces this profile's bounded socket anchors. One target
// per socket is sufficient; the channel can query other explicit targets.
func (m *QueryTransportManager) RegisterTargets(profile string, sessions []*Session) {
	if m == nil || profile == "" || len(profile) > 256 {
		return
	}
	m.registrationMu.Lock()
	defer m.registrationMu.Unlock()
	targets := make([]matrixQueryTarget, 0, matrixQuerySocketLimit)
	seen := make(map[string]bool)
	for _, s := range sessions {
		if s == nil || s.Name == "" || !matrixQuerySafeArgument(s.Name) {
			continue
		}
		socket := strings.TrimSpace(s.SocketName)
		if !matrixQuerySafeArgument(socket) {
			continue
		}
		key := matrixQuerySocketKey(socket)
		if seen[key] {
			continue
		}
		seen[key] = true
		targets = append(targets, matrixQueryTarget{socket, s.Name})
		if len(targets) == matrixQuerySocketLimit {
			break
		}
	}
	m.mu.Lock()
	if m.closed || (m.profiles[profile] == nil && len(m.profiles) >= matrixQueryProfileLimit) {
		m.mu.Unlock()
		return
	}
	if len(targets) == 0 {
		delete(m.profiles, profile)
	} else {
		m.profiles[profile] = targets
	}
	retired, wanted := m.reconcileLocked()
	m.mu.Unlock()
	for _, s := range retired {
		m.closeSocket(s)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		for key, t := range wanted {
			if m.sockets[key] == nil {
				m.sockets[key] = &matrixQuerySocket{gate: make(chan struct{}, 1), queue: make(chan struct{}, 32), anchor: t}
			}
		}
	}
}
func (m *QueryTransportManager) ReleaseTargets(profile string) { m.RegisterTargets(profile, nil) }

func (m *QueryTransportManager) reconcileLocked() ([]*matrixQuerySocket, map[string]matrixQueryTarget) {
	wanted := make(map[string]matrixQueryTarget)
	profiles := make([]string, 0, len(m.profiles))
	for p := range m.profiles {
		profiles = append(profiles, p)
	}
	sort.Strings(profiles)
	for _, p := range profiles {
		for _, t := range m.profiles[p] {
			key := matrixQuerySocketKey(t.socket)
			if _, ok := wanted[key]; !ok && len(wanted) < matrixQuerySocketLimit {
				wanted[key] = t
			}
		}
	}
	var retired []*matrixQuerySocket
	for key, s := range m.sockets {
		t, ok := wanted[key]
		if !ok || s.anchor != t {
			s.retired.Store(true)
			retired = append(retired, s)
			delete(m.sockets, key)
		}
	}
	return retired, wanted
}
func (m *QueryTransportManager) closeSocket(s *matrixQuerySocket) {
	// Acquiring this gate is bounded by every query's deadline, including startup
	// and writes. Retirement prevents queued callers opening another client.
	s.gate <- struct{}{}
	if s.conn != nil {
		s.conn.close()
		s.conn = nil
		s.observed.Store(nil)
	}
	<-s.gate
}
func (m *QueryTransportManager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		m.cancel()
		m.registrationMu.Lock()
		defer m.registrationMu.Unlock()
		m.mu.Lock()
		m.closed = true
		var all []*matrixQuerySocket
		for _, s := range m.sockets {
			s.retired.Store(true)
			all = append(all, s)
		}
		m.sockets = make(map[string]*matrixQuerySocket)
		m.profiles = make(map[string][]matrixQueryTarget)
		m.mu.Unlock()
		for _, s := range all {
			m.closeSocket(s)
		}
		matrixQueryGlobal.Lock()
		if matrixQueryGlobal.manager == m {
			matrixQueryGlobal.manager = nil
		}
		matrixQueryGlobal.Unlock()
	})
}
func (m *QueryTransportManager) Diagnostics() QueryTransportStats {
	if m == nil {
		return QueryTransportStats{}
	}
	d := QueryTransportStats{Queries: m.counters.queries.Load(), Successes: m.counters.successes.Load(), Fallbacks: m.counters.fallbacks.Load(), FramingFailures: m.counters.framing.Load(), Timeouts: m.counters.timeouts.Load(), Starts: m.counters.starts.Load()}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sockets {
		c := s.observed.Load()
		if c != nil && !c.dead() {
			d.Connections++
			d.ClientPIDs = append(d.ClientPIDs, c.cmd.Process.Pid)
		}
	}

	sort.Ints(d.ClientPIDs)
	return d
}
func QueryTransportDiagnostics() QueryTransportStats {
	matrixQueryGlobal.RLock()
	m := matrixQueryGlobal.manager
	matrixQueryGlobal.RUnlock()
	return m.Diagnostics()
}
func matrixQueryOutput(ctx context.Context, socket string, args ...string) ([]byte, bool) {
	matrixQueryGlobal.RLock()
	m := matrixQueryGlobal.manager
	matrixQueryGlobal.RUnlock()
	if m == nil {
		return nil, false
	}
	return m.output(ctx, socket, args)
}

func (m *QueryTransportManager) output(parent context.Context, socket string, args []string) (output []byte, ok bool) {
	commands, valid := matrixQueryCommands(args)
	if !valid {
		return nil, false
	}
	m.counters.queries.Add(1)
	defer func() {
		if ok {
			m.counters.successes.Add(1)
		} else {
			m.counters.fallbacks.Add(1)
		}
	}()
	ctx, cancel := context.WithTimeout(parent, matrixQueryDeadline)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	m.mu.Lock()
	s := m.sockets[matrixQuerySocketKey(socket)]
	closed := m.closed
	m.mu.Unlock()
	if s == nil || closed {
		return nil, false
	}
	select {
	case s.queue <- struct{}{}:
		defer func() { <-s.queue }()
	default:
		return nil, false
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return nil, false
	}
	if s.retired.Load() || ctx.Err() != nil || time.Now().Before(s.retry) {
		return nil, false
	}
	var err error
	queried := false
	if s.conn != nil && s.conn.dead() {
		s.conn.close()
		s.conn = nil
		s.observed.Store(nil)
	}
	if s.conn == nil {
		s.conn, err = matrixStartQueryConnection(ctx, s.anchor)
		if err == nil {
			m.counters.starts.Add(1)
			s.observed.Store(s.conn)
		}
	}
	if err == nil {
		queried = true
		output, err = s.conn.query(ctx, commands)
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		return output, true
	}
	if queried && errors.Is(err, errMatrixQueryCommand) {
		return nil, false
	} // fully framed error keeps a healthy channel
	if errors.Is(err, errMatrixQueryProtocol) || errors.Is(err, io.ErrUnexpectedEOF) {
		m.counters.framing.Add(1)
	}
	if ctx.Err() != nil {
		m.counters.timeouts.Add(1)
	}
	if s.conn != nil {
		s.conn.close()
		s.conn = nil
		s.observed.Store(nil)
	}
	s.retry = time.Now().Add(matrixQueryBackoff)
	return nil, false
}

func matrixQuerySafeArgument(s string) bool {
	if len(s) > 16384 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Single quoted tmux parser words suppress variable/format/parser expansion.
// Apostrophes are represented by ending the quote, escaping, and reopening it.
func matrixQuoteQuery(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// Exact command/option grammar rejects mutations, aliases, command injection,
// client-relative queries and format job expansion. Unknown forms stay native.
func matrixQueryCommands(args []string) ([][]string, bool) {
	if len(args) == 0 || len(args) > 160 {
		return nil, false
	}
	total := 0
	for _, arg := range args {
		total += len(arg)
		if total > 65536 {
			return nil, false
		}
	}
	var commands [][]string
	start := 0
	for i := 0; i <= len(args); i++ {
		if i != len(args) && args[i] != ";" {
			continue
		}
		c := args[start:i]
		if !matrixQueryAllowed(c) {
			return nil, false
		}
		commands = append(commands, c)
		start = i + 1
	}
	return commands, true
}
func matrixQueryAllowed(c []string) bool {
	if len(c) == 0 {
		return false
	}
	for _, a := range c {
		if !matrixQuerySafeArgument(a) || strings.Contains(a, "#(") {
			return false
		}
	}
	// matrix/hardened: only the existing explicit-session all-window PID probe
	// gains -s. Only ordinary session names, never client-relative selectors.
	if c[0] == "list-panes" && len(c) > 1 && c[1] == "-s" {
		return len(c) == 6 && c[2] == "-t" && c[3] != "" &&
			!strings.ContainsAny(c[3], ":.*?[]{}") && !strings.ContainsAny(c[3][:1], "$%@!=") &&
			c[4] == "-F" && matrixQuerySafeFormat(c[5])
	}
	flags, values := "", ""
	target, print, all := false, false, false
	switch c[0] {
	case "capture-pane":
		flags = "peJ"
		values = "tSE"
	case "display-message":
		flags = "p"
		values = "t"
	case "list-panes", "list-windows", "list-sessions":
		flags = "a"
		values = "tF"
	case "show-environment":
		flags = "g"
		values = "t"
	default:
		return false
	}
	positional := 0
	for i := 1; i < len(c); i++ {
		a := c[i]
		if len(a) == 2 && a[0] == '-' {
			f := a[1]
			if strings.ContainsRune(values, rune(f)) {
				i++
				if i >= len(c) {
					return false
				}
				if f == 't' {
					target = c[i] != ""
				}
				if f == 'F' && !matrixQuerySafeFormat(c[i]) {
					return false
				}
				continue
			}
			if strings.ContainsRune(flags, rune(f)) {
				if f == 'p' {
					print = true
				}
				if f == 'a' || f == 'g' {
					all = true
				}
				continue
			}
		}
		if strings.HasPrefix(a, "-") {
			return false
		}
		positional++
	}

	switch c[0] {
	case "capture-pane":
		return target && print && positional == 0
	case "display-message":
		return print && positional == 1 && (target || strings.HasPrefix(c[len(c)-1], "__agentdeck_capture_")) && matrixQuerySafeFormat(c[len(c)-1])
	case "list-sessions":
		return positional == 0
	case "list-panes", "list-windows":
		return (target || all) && positional == 0
	case "show-environment":
		return (target || all) && positional <= 1
	}
	return false
}

// Only simple built-in pane/window/session fields can expand. In particular
// E:/T: recursive expansion and user options can contain shell jobs.
func matrixQuerySafeFormat(s string) bool {
	for strings.Contains(s, "#{") {
		i := strings.Index(s, "#{")
		s = s[i+2:]
		j := strings.IndexByte(s, '}')
		if j < 0 {
			return false
		}
		field := s[:j]
		if !(strings.HasPrefix(field, "pane_") || strings.HasPrefix(field, "window_") || strings.HasPrefix(field, "session_")) {
			return false
		}
		for _, r := range field {
			if (r < 'a' || r > 'z') && r != '_' {
				return false
			}
		}
		s = s[j+1:]
	}
	return !strings.Contains(s, "#(")
}

type matrixQueryConnection struct {
	cmd        *exec.Cmd
	in         io.WriteCloser
	out        io.ReadCloser
	lines      chan []byte
	readerDone chan struct{}
	waitDone   chan struct{}
	stop       chan struct{}
	once       sync.Once
	lastNumber uint64
	buffered   atomic.Int64
}

func (c *matrixQueryConnection) dead() bool {
	select {
	case <-c.waitDone:
		return true
	default:
		return false
	}
}
func (c *matrixQueryConnection) close() {
	c.once.Do(func() {
		close(c.stop)
		_ = c.in.Close()
		_ = c.cmd.Process.Kill()
		_ = c.out.Close()
		<-c.readerDone
		<-c.waitDone
	})
}
func matrixStartQueryConnection(ctx context.Context, t matrixQueryTarget) (*matrixQueryConnection, error) {
	// -E preserves the session environment. ignore-size prevents geometry changes;
	// no-output prevents terminal stream events; read-only blocks client input.
	cmd := tmuxExec(t.socket, "-C", "attach-session", "-E", "-f", "ignore-size,no-output,read-only", "-t", "="+t.name)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err = commandStart(cmd); err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	c := &matrixQueryConnection{cmd: cmd, in: in, out: out, lines: make(chan []byte, 32), readerDone: make(chan struct{}), waitDone: make(chan struct{}), stop: make(chan struct{})}
	go func() { defer close(c.waitDone); _ = cmd.Wait() }()
	go c.readLines()
	stop := context.AfterFunc(ctx, func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = out.Close() })
	defer stop()
	_, err = c.frame(ctx)
	if err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}
func (c *matrixQueryConnection) readLines() {
	defer close(c.readerDone)
	defer close(c.lines)
	r := bufio.NewReaderSize(c.out, 32768)
	for {
		var line []byte
		for {
			part, err := r.ReadSlice('\n')
			if len(line)+len(part) > matrixQueryLimit {
				return
			}
			line = append(line, part...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				return
			}
			break
		}
		if c.buffered.Add(int64(len(line))) > matrixQueryLimit {
			return
		}
		select {
		case c.lines <- line:
		case <-c.stop:
			return
		}
	}
}
func (c *matrixQueryConnection) next(ctx context.Context) ([]byte, error) {
	select {
	case line, ok := <-c.lines:
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		c.buffered.Add(-int64(len(line)))
		return line, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func matrixProtocolIdentity(line []byte, kind string) (uint64, string, bool) {
	f := strings.Fields(string(line))
	if len(f) != 4 || f[0] != kind {
		return 0, "", false
	}
	n, e := strconv.ParseUint(f[2], 10, 64)
	if e != nil {
		return 0, "", false
	}
	if _, e = strconv.ParseUint(f[1], 10, 64); e != nil {
		return 0, "", false
	}
	if f[3] != "0" && f[3] != "1" {
		return 0, "", false
	}
	return n, strings.Join(f[1:], " "), true
}
func matrixQueryNotification(line []byte) bool {
	// Notifications may occur between complete response blocks only. Unexpected
	// events, including %output despite no-output, poison instead of dropping data.
	for _, p := range []string{"%session-changed ", "%sessions-changed\n", "%window-add ", "%window-close ", "%window-renamed ", "%layout-change ", "%window-pane-changed ", "%session-window-changed ", "%pane-mode-changed ", "%client-session-changed ", "%client-detached ", "%unlinked-window-add ", "%unlinked-window-close ", "%unlinked-window-renamed ", "%subscription-changed "} {
		if bytes.HasPrefix(line, []byte(p)) {
			return true
		}
	}
	return false
}
func (c *matrixQueryConnection) frame(ctx context.Context) ([]byte, error) {
	var identity string
	for notifications := 0; ; notifications++ {
		if notifications > 1024 {
			return nil, errMatrixQueryProtocol
		}
		line, err := c.next(ctx)
		if err != nil {
			return nil, err
		}
		n, id, ok := matrixProtocolIdentity(line, "%begin")
		if ok {
			if n <= c.lastNumber {
				return nil, errMatrixQueryProtocol
			}
			c.lastNumber = n
			identity = id
			break
		}
		if !matrixQueryNotification(line) {
			return nil, errMatrixQueryProtocol
		}
	}
	var output bytes.Buffer
	for {
		line, err := c.next(ctx)
		if err != nil {
			return nil, err
		}
		for _, kind := range []string{"%end", "%error"} {
			_, id, ok := matrixProtocolIdentity(line, kind)
			if ok {
				if id != identity {
					return nil, errMatrixQueryProtocol
				}
				if kind == "%error" {
					return output.Bytes(), errMatrixQueryCommand
				}
				return output.Bytes(), nil
			}
		}
		if bytes.HasPrefix(line, []byte("%begin")) || bytes.HasPrefix(line, []byte("%end")) || bytes.HasPrefix(line, []byte("%error")) {
			return nil, errMatrixQueryProtocol
		}
		if output.Len()+len(line) > matrixQueryLimit {
			return nil, errMatrixQueryProtocol
		}
		output.Write(line)
	}
}
func (c *matrixQueryConnection) query(ctx context.Context, commands [][]string) ([]byte, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	ack := "__agentdeck_query_" + hex.EncodeToString(random[:]) + "__"
	var command strings.Builder
	for index, args := range commands {
		if index > 0 {
			command.WriteString(" ; ")
		}
		for i, a := range args {
			if i > 0 {
				command.WriteByte(' ')
			}
			command.WriteString(matrixQuoteQuery(a))
		}
	}
	// Separate line ensures the ack executes even if the preceding command fails.
	command.WriteByte('\n')
	command.WriteString("display-message -p " + matrixQuoteQuery(ack) + "\n")
	stop := context.AfterFunc(ctx, func() { _ = c.in.Close(); _ = c.cmd.Process.Kill(); _ = c.out.Close() })
	defer stop()
	if _, err := io.WriteString(c.in, command.String()); err != nil {
		return nil, err
	}
	var result bytes.Buffer
	failed := false
	for range commands {
		out, err := c.frame(ctx)
		if errors.Is(err, errMatrixQueryCommand) {
			failed = true
			break
		}
		if err != nil {
			return nil, err
		}
		if result.Len()+len(out) > matrixQueryLimit {
			return nil, errMatrixQueryProtocol
		}
		result.Write(out)
	}
	out, err := c.frame(ctx)
	if err != nil {
		return nil, err
	}
	if string(out) != ack+"\n" {
		return nil, errMatrixQueryProtocol
	}
	if failed {
		return nil, errMatrixQueryCommand
	}
	return result.Bytes(), nil
}
