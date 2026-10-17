package tmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// matrix/hardened: batch only raw visible captures, preserving every original
// caller's classifier, ACK/history update and independent observation time.
const CapturePaneBatchLimit = 4

const matrixCaptureTTL = 500 * time.Millisecond
const matrixCaptureBatchOutputLimit = 8 << 20

type matrixCaptureTicket struct {
	session    *Session
	name       string
	socket     string
	generation uint64
}

func (s *Session) reserveMatrixIndependentCapture(useCache bool) (matrixCaptureTicket, string, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if useCache && s.cacheContent != "" && time.Since(s.cacheTime) < matrixCaptureTTL {
		return matrixCaptureTicket{}, s.cacheContent, true
	}
	s.cacheGeneration++
	s.cacheCaptures++
	return matrixCaptureTicket{s, s.Name, s.SocketName, s.cacheGeneration}, "", false
}

func (s *Session) reserveMatrixBatchCapture() (matrixCaptureTicket, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheCaptures != 0 || (s.cacheContent != "" && time.Since(s.cacheTime) < matrixCaptureTTL) {
		return matrixCaptureTicket{}, false
	}
	s.cacheGeneration++
	return matrixCaptureTicket{s, s.Name, s.SocketName, s.cacheGeneration}, true
}

// finish is paired only with independent acquisitions. Even a non-cacheable
// older call's completion fences a pending batch; overlapping ordinary/Fresh
// calls never allow that batch to supersede their independent observations.
func (ticket matrixCaptureTicket) finish() {
	s := ticket.session
	s.cacheMu.Lock()
	s.cacheCaptures--
	s.cacheGeneration++
	s.cacheMu.Unlock()
}

// publish never takes s.mu; the capture cache's existing lock order is retained.
func (ticket matrixCaptureTicket) publish(content string, at time.Time) bool {
	s := ticket.session
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheGeneration != ticket.generation || s.Name != ticket.name || s.SocketName != ticket.socket {
		return false
	}
	s.cacheContent, s.cacheTime = content, at
	s.cacheGeneration++
	return true
}

// PrefetchCapturePanes opportunistically fills the existing 500ms capture cache
// in small socket-local groups. Failures leave the original per-pane capture
// path authoritative; no partial batch result or marker is ever published.
// Call immediately before launching the corresponding bounded probe group.
func PrefetchCapturePanes(ctx context.Context, sessions []*Session) {
	prefetchMatrixCapturePanes(ctx, sessions, rand.Reader)
}

func prefetchMatrixCapturePanes(ctx context.Context, sessions []*Session, entropy io.Reader) {
	// The control pipe already avoids subprocesses. Preserve its capture path
	// rather than adding a batch subprocess ahead of zero-subprocess reads.
	if len(sessions) < 2 || GetPipeManager() != nil || ctx.Err() != nil {
		return
	}
	var groups [][]*Session
	seen := make(map[*Session]bool, len(sessions))
	for _, s := range sessions {
		if s == nil || s.Name == "" || seen[s] {
			continue
		}
		seen[s] = true
		s.cacheMu.RLock()
		fresh := s.cacheContent != "" && time.Since(s.cacheTime) < matrixCaptureTTL
		s.cacheMu.RUnlock()
		if fresh {
			continue
		}
		// tmuxArgs trims SocketName, but an empty selector remains distinct from
		// explicit "default" because tmux may resolve it through inherited TMUX.
		socket := strings.TrimSpace(s.SocketName)
		placed := false
		for i := range groups {
			if len(groups[i]) < CapturePaneBatchLimit && strings.TrimSpace(groups[i][0].SocketName) == socket {
				groups[i] = append(groups[i], s)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, []*Session{s})
		}
	}
	for _, group := range groups {
		if len(group) < 2 || ctx.Err() != nil {
			continue
		}
		prefetchMatrixCaptureGroup(ctx, group, entropy)
	}
}

func matrixCaptureMarker(token string, index int, boundary string) string {
	return fmt.Sprintf("__agentdeck_capture_%s_%d_%s__", token, index, boundary)
}

func matrixCaptureBatchArgs(token string, tickets []matrixCaptureTicket) []string {
	args := make([]string, 0, len(tickets)*17)
	for index, ticket := range tickets {
		if index != 0 {
			args = append(args, ";")
		}
		args = append(args, "display-message", "-p", matrixCaptureMarker(token, index, "begin"), ";",
			"capture-pane", "-t", ticket.name, "-p", "-e", ";",
			"display-message", "-p", matrixCaptureMarker(token, index, "end"))
	}
	return args
}

func parseMatrixCaptureBatch(output []byte, token string, count int) ([]string, bool) {
	if count < 2 || count > CapturePaneBatchLimit || len(output) > matrixCaptureBatchOutputLimit || len(token) != 32 {
		return nil, false
	}
	// Reject any marker-token occurrence in captured text, including duplicate
	// or embedded boundaries. The unpredictable 128-bit token is per command.
	if bytes.Count(output, []byte(token)) != count*2 {
		return nil, false
	}
	frames := make([]string, 0, count)
	for index := 0; index < count; index++ {
		begin := []byte(matrixCaptureMarker(token, index, "begin") + "\n")
		if !bytes.HasPrefix(output, begin) {
			return nil, false
		}
		output = output[len(begin):]
		end := []byte(matrixCaptureMarker(token, index, "end") + "\n")
		boundary := bytes.Index(output, end)
		if boundary < 0 || (boundary > 0 && output[boundary-1] != '\n') {
			return nil, false
		}
		// Keep ANSI and all leading/trailing blank lines byte for byte.
		frames = append(frames, string(output[:boundary]))
		output = output[boundary+len(end):]
	}
	return frames, len(output) == 0
}

type matrixCaptureBatchBuffer struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (b *matrixCaptureBatchBuffer) Write(p []byte) (int, error) {
	if len(p) > matrixCaptureBatchOutputLimit-b.Len() {
		b.cancel()
		return 0, errors.New("tmux capture batch exceeds output limit")
	}
	return b.Buffer.Write(p)
}

func prefetchMatrixCaptureGroup(parent context.Context, group []*Session, entropy io.Reader) {
	var tickets []matrixCaptureTicket
	for _, s := range group {
		ticket, reserved := s.reserveMatrixBatchCapture()
		if reserved {
			tickets = append(tickets, ticket)
		}
	}
	if len(tickets) < 2 {
		return
	}
	for _, ticket := range tickets[1:] {
		if strings.TrimSpace(ticket.socket) != strings.TrimSpace(tickets[0].socket) {
			return
		}
	}
	var nonce [16]byte
	if _, err := io.ReadFull(entropy, nonce[:]); err != nil {
		return
	}
	token := hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	output := &matrixCaptureBatchBuffer{cancel: cancel}
	cmd := tmuxExecContext(ctx, tickets[0].socket, matrixCaptureBatchArgs(token, tickets)...)
	cmd.Stdout = output
	// commandRun uses the same bounded exec lifecycle and subprocess accounting
	// as individual captures; capped stdout cancels before unbounded buffering.
	started := time.Now()
	// matrix/hardened: healthy monitor channels execute the same framed command list.
	if raw, ok := matrixQueryOutput(ctx, tickets[0].socket, matrixCaptureBatchArgs(token, tickets)...); ok {
		_, _ = output.Write(raw)
	} else if err := commandRun(cmd); err != nil || ctx.Err() != nil {
		return
	}
	frames, ok := parseMatrixCaptureBatch(output.Bytes(), token, len(tickets))
	if !ok || time.Since(started) >= matrixCaptureTTL {
		return
	}
	for index, ticket := range tickets {
		// The original TTL runs conservatively from command start, never from
		// parsing/publication. A slow batch therefore cannot extend freshness.
		if time.Since(started) >= matrixCaptureTTL {
			return
		}
		ticket.publish(frames[index], started)
	}
}
