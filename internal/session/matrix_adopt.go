package session

// matrix/hardened: adopt a Claude Code process that is already running in a
// tmux session agent-deck did not create (`agent-deck session adopt`, the
// TUI's alt+u list), without interrupting it.
//
// Claude Code keeps one status file per live process under
// <config>/sessions/<pid>.json naming the conversation it runs (sessionId)
// and its directory. A pane is matched to such a process by walking the
// process tree up from the Claude pid to the pane's pid. Adopting registers a
// claude session bound to that conversation and renames the tmux session to
// the name the new record expects, which leaves the process running. Only a
// tmux session holding exactly that one pane can be adopted: agent-deck
// manages whole tmux sessions.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// AdoptDefaultSocket names the user's default tmux server (`tmux -L default`
// is the same server as plain `tmux`).
const AdoptDefaultSocket = "default"

// adoptTmuxTimeout bounds every tmux/ps call made while looking for panes.
const adoptTmuxTimeout = 5 * time.Second

// AdoptCandidate is one tmux pane running an interactive Claude Code process.
type AdoptCandidate struct {
	Socket       string `json:"socket"`
	TmuxSession  string `json:"tmux_session"`
	PaneID       string `json:"pane_id"`
	ClaudePID    int    `json:"claude_pid"`
	SessionID    string `json:"claude_session_id"`
	Path         string `json:"path"`
	ClaudeStatus string `json:"claude_status,omitempty"`
	Attached     bool   `json:"attached"`
	Adoptable    bool   `json:"adoptable"`
	Reason       string `json:"reason,omitempty"`
	OtherPIDs    []int  `json:"other_pids,omitempty"`
	ManagedBy    string `json:"managed_by,omitempty"`
}

// claudeProcess is the part of a Claude Code <config>/sessions/<pid>.json
// status file that adoption reads.
type claudeProcess struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
}

type adoptPane struct {
	socket, session, paneID string
	panePID                 int
	windows, panes          int
	attached                bool
}

// AdoptSockets returns the tmux servers to scan: only the named one when
// socket is set, otherwise the default server plus agent-deck's configured
// server when it is a different one.
func AdoptSockets(cfg *UserConfig, socket string) []string {
	if s := strings.TrimSpace(socket); s != "" {
		return []string{s}
	}
	out := []string{AdoptDefaultSocket}
	if cfg != nil {
		if s := cfg.Tmux.GetSocketName(); s != "" && s != AdoptDefaultSocket {
			out = append(out, s)
		}
	}
	return out
}

// NormalizeAdoptSocket maps an instance's TmuxSocketName ("" = no -L, the
// default server) to the socket name used by adoption.
func NormalizeAdoptSocket(name string) string {
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	return AdoptDefaultSocket
}

// FindAdoptCandidates lists the Claude panes on the given tmux servers and
// says for each whether it can be adopted, and if not, why. instances are the
// profile's registered sessions.
func FindAdoptCandidates(cfg *UserConfig, instances []*Instance, sockets []string) ([]AdoptCandidate, error) {
	parents, err := adoptProcessParents()
	if err != nil {
		return nil, err
	}
	procs := liveClaudeProcesses(cfg, parents)

	var panes []adoptPane
	for _, socket := range sockets {
		found, err := listAdoptPanes(socket)
		if err != nil {
			return nil, err
		}
		panes = append(panes, found...)
	}

	byPanePID := map[int]*adoptPane{}
	for i := range panes {
		byPanePID[panes[i].panePID] = &panes[i]
	}
	inPane := map[*adoptPane][]claudeProcess{}
	for _, p := range procs {
		if pane := enclosingPane(p.PID, parents, byPanePID); pane != nil {
			inPane[pane] = append(inPane[pane], p)
		}
	}
	byConversation := map[string][]int{}
	for _, p := range procs {
		byConversation[p.SessionID] = append(byConversation[p.SessionID], p.PID)
	}

	var out []AdoptCandidate
	for i := range panes {
		pane := &panes[i]
		found := inPane[pane]
		if len(found) == 0 {
			continue
		}
		p := found[0]
		c := AdoptCandidate{
			Socket: pane.socket, TmuxSession: pane.session, PaneID: pane.paneID,
			ClaudePID: p.PID, SessionID: p.SessionID, Path: p.CWD,
			ClaudeStatus: p.Status, Attached: pane.attached,
		}
		if loc, err := LocateClaudeResumeDir(cfg, p.SessionID); err == nil {
			c.Path = loc.Dir
		}
		for _, pid := range byConversation[p.SessionID] {
			if pid != p.PID {
				c.OtherPIDs = append(c.OtherPIDs, pid)
			}
		}
		c.ManagedBy = adoptOwner(instances, pane.socket, pane.session, p.SessionID)
		c.Reason = adoptRefusal(c, pane, len(found))
		c.Adoptable = c.Reason == ""
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Adoptable != out[j].Adoptable {
			return out[i].Adoptable
		}
		if out[i].Socket != out[j].Socket {
			return out[i].Socket < out[j].Socket
		}
		return out[i].TmuxSession < out[j].TmuxSession
	})
	return out, nil
}

func adoptRefusal(c AdoptCandidate, pane *adoptPane, claudes int) string {
	switch {
	case c.ManagedBy != "":
		return "already managed by agent-deck session " + c.ManagedBy
	case claudes > 1:
		return fmt.Sprintf("%d Claude processes run in this pane", claudes)
	case pane.windows != 1 || pane.panes != 1:
		return fmt.Sprintf("tmux session has %d window(s) and %d pane(s) in this window; agent-deck manages a whole tmux session, so give Claude a tmux session of its own first", pane.windows, pane.panes)
	case !IsBareClaudeSessionUUID(c.SessionID):
		return fmt.Sprintf("conversation id %q is not a Claude session uuid", c.SessionID)
	case len(c.OtherPIDs) > 0:
		return fmt.Sprintf("the conversation is also open in Claude process %s; close the other one first, two processes writing one conversation corrupt it", joinPIDs(c.OtherPIDs))
	}
	if info, err := os.Stat(c.Path); c.Path == "" || err != nil || !info.IsDir() {
		return fmt.Sprintf("directory %q no longer exists", c.Path)
	}
	return ""
}

// adoptOwner names the registered session that already owns the tmux session
// or the conversation, or "".
func adoptOwner(instances []*Instance, socket, tmuxName, sessionID string) string {
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		if ts := inst.GetTmuxSession(); ts != nil && ts.Name == tmuxName && NormalizeAdoptSocket(inst.TmuxSocketName) == socket {
			return fmt.Sprintf("%q (%s)", inst.Title, inst.ID)
		}
		if sessionID != "" && inst.ClaudeSessionID == sessionID {
			return fmt.Sprintf("%q (%s)", inst.Title, inst.ID)
		}
	}
	return ""
}

func joinPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	return strings.Join(parts, ", ")
}

// enclosingPane walks up from pid to the first ancestor that is a pane's
// process.
func enclosingPane(pid int, parents map[int]int, panes map[int]*adoptPane) *adoptPane {
	for hops := 0; pid > 1 && hops < 64; hops++ {
		if pane := panes[pid]; pane != nil {
			return pane
		}
		next, ok := parents[pid]
		if !ok || next == pid {
			return nil
		}
		pid = next
	}
	return nil
}

// liveClaudeProcesses reads every known config dir's sessions/*.json and
// keeps the interactive processes that are still running.
func liveClaudeProcesses(cfg *UserConfig, parents map[int]int) []claudeProcess {
	seen := map[int]bool{}
	var out []claudeProcess
	for _, dir := range conversationConfigDirCandidates(cfg, GetClaudeConfigDir()) {
		files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			var p claudeProcess
			if json.Unmarshal(data, &p) != nil || p.PID <= 1 || p.SessionID == "" || seen[p.PID] {
				continue
			}
			if p.Kind != "" && p.Kind != "interactive" {
				continue
			}
			if _, alive := parents[p.PID]; !alive {
				continue // a status file left behind by an exited process
			}
			seen[p.PID] = true
			out = append(out, p)
		}
	}
	return out
}

// adoptProcessParents maps every running pid to its parent pid.
func adoptProcessParents() (map[int]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), adoptTmuxTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	parents := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 == nil && err2 == nil {
			parents[pid] = ppid
		}
	}
	return parents, nil
}

// adoptTmux runs tmux against one server.
//
// matrix/hardened: through tmux.ExecContext, the sanctioned factory, not a raw
// exec of "tmux": it adds -u, so non-ASCII session names are not printed as
// "_" when the locale is not UTF-8 (SSH, launchd), sets WaitDelay, and under
// `go test` refuses to touch the user's real default server. The socket is
// still explicit (-L <socket>, "default" for the user's own server).
func adoptTmux(socket string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), adoptTmuxTimeout)
	defer cancel()
	cmd := tmux.ExecContext(ctx, NormalizeAdoptSocket(socket), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("tmux -L %s %s: %w: %s", NormalizeAdoptSocket(socket), args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func isNoTmuxServer(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting to") || strings.Contains(msg, "No such file or directory")
}

func listAdoptPanes(socket string) ([]adoptPane, error) {
	// "|", not a control byte: tmux run with no attached client (every SSH
	// command) rewrites control bytes in format output to "_" (see
	// internal/tmux tmuxFieldSep). The free-text session name goes last.
	format := "#{pane_id}|#{pane_pid}|#{session_windows}|#{window_panes}|#{session_attached}|#{session_name}"
	out, err := adoptTmux(socket, "list-panes", "-a", "-F", format)
	if err != nil {
		if isNoTmuxServer(err) {
			return nil, nil
		}
		return nil, err
	}
	var panes []adoptPane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, "|", 6)
		if len(f) != 6 {
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		windows, _ := strconv.Atoi(f[2])
		count, _ := strconv.Atoi(f[3])
		attached, _ := strconv.Atoi(f[4])
		panes = append(panes, adoptPane{socket: NormalizeAdoptSocket(socket), session: f[5], paneID: f[0], panePID: pid, windows: windows, panes: count, attached: attached > 0})
	}
	return panes, nil
}

// SelectAdoptCandidate picks the candidate for a tmux session name, on socket
// when given. It fails when the name is unknown or exists on several servers.
func SelectAdoptCandidate(cands []AdoptCandidate, tmuxSession, socket string) (AdoptCandidate, error) {
	var hits []AdoptCandidate
	for _, c := range cands {
		if c.TmuxSession == tmuxSession && (socket == "" || c.Socket == NormalizeAdoptSocket(socket)) {
			hits = append(hits, c)
		}
	}
	switch len(hits) {
	case 0:
		return AdoptCandidate{}, fmt.Errorf("no Claude process found in tmux session %q; list the candidates with 'agent-deck session adopt --list'", tmuxSession)
	case 1:
		return hits[0], nil
	}
	sockets := make([]string, len(hits))
	for i, h := range hits {
		sockets[i] = h.Socket
	}
	return AdoptCandidate{}, fmt.Errorf("tmux session %q exists on several tmux servers (%s); choose one with --socket", tmuxSession, strings.Join(sockets, ", "))
}

// ErrAdoptNameTaken reports that the tmux name the new record expects is
// already in use on that server.
var ErrAdoptNameTaken = errors.New("tmux session name already in use")

// RenameAdoptedTmuxSession renames tmux session from to to on socket. It
// refuses when to already exists, so an adoption never merges into or
// shadows another session.
func RenameAdoptedTmuxSession(socket, from, to string) error {
	if _, err := adoptTmux(socket, "has-session", "-t", "="+to); err == nil {
		return fmt.Errorf("%w: %s", ErrAdoptNameTaken, to)
	}
	_, err := adoptTmux(socket, "rename-session", "-t", "="+from, to)
	return err
}
