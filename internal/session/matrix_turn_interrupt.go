package session

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// matrix/hardened: a turn that stopped before it finished (the "abnormal
// interruption" class), for Claude and Codex sessions.
//
// Claude: when the API fails mid-turn ("API Error: Connection lost
// mid-response", "Connection dropped (ECONNRESET)", "Can't reach the API
// server") Claude Code ends the turn without firing Stop and redraws an empty
// composer. Every signal agent-deck reads then says the turn finished normally:
// the light goes to waiting, the substate to idle-at-empty-prompt. Nothing tells
// the user the work stopped halfway and needs a "continue". The pane banner is
// no use: it renders as an ordinary "⏺" reply line without the " · " separator
// the #1400 banner guard requires, and its wording is shared with prose. The
// transcript records the failure as structured data, the same source usage-limit
// (#1802) reads:
//
//	{"type":"assistant","isApiErrorMessage":true,"error":"server_error",
//	 "message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: ..."}]}}
//
// The Claude verdict: the latest main-conversation user or assistant record is
// such an API-error reply. Any later user record (a typed prompt, "继续", the
// automatic "usage limit has reset" continuation) means the session moved past
// it. Checked against every transcript on the maintainer's machine (2026-10-09):
// 83 API-error records, each followed by a user record before any other
// assistant record.
//
// Codex: the pane shows "■ Conversation interrupted - use /feedback if
// something went wrong" and the rollout records
//
//	{"type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted",...}}
//
// The Codex verdict: the latest turn event (task_started/turn_started,
// task_complete/turn_complete, turn_aborted, user_message) is turn_aborted.
// A steer that interrupts a turn starts the next one at once, and "继续" writes
// user_message and task_started, so both clear it. matrix/hardened: current
// Codex also records native task_complete.error for server-overloaded capacity
// failures; those have their own bounded, explicitly enabled continuation. The
// rollout does not say who interrupted (Esc, another client of the shared
// thread, the app server), so the detail names the reason Codex gives and no
// more. Subagent threads are skipped: their parent interrupts them as routine.
// Checked against every rollout on the maintainer's machine (2026-10-11): 384
// aborts on main threads, 21 of them never followed by another turn.
//
// Known limitation (Claude): a local slash command typed after the failure
// (/cost, /model) also writes a user record, so it clears the verdict although
// the task itself was not resumed.
//
// The coarse status is never changed. The verdict is cached on the Instance by
// the status poll (updateStatus) so CachedSubstate, which the TUI calls on its
// render path, stays filesystem-free.

// SubstateTurnInterrupted marks a session whose last turn stopped before it
// finished: a Claude API error or a Codex aborted turn. Pairs with status
// "waiting"/"idle". The cause is in SubstateDetail ("server_error: Connection
// lost mid-response.", "turn_aborted: interrupted").
const SubstateTurnInterrupted Substate = "turn-interrupted"

const (
	// matrixTurnInterruptScanInterval throttles the transcript read per session,
	// like usageLimitScanInterval.
	matrixTurnInterruptScanInterval = 5 * time.Second
	// matrixTurnInterruptChunkBytes is the backward read window.
	matrixTurnInterruptChunkBytes = 256 << 10
	// matrixTurnInterruptMaxScanBytes bounds how far back the scan walks. The
	// decisive record is normally within the last few KiB; if none is found
	// within this many bytes there is no verdict.
	matrixTurnInterruptMaxScanBytes = 8 << 20
	// matrixTurnInterruptMaxLineBytes: a longer line is not read. Its type is
	// then unknown, so the scan stops without a verdict rather than look past
	// what may be a newer decisive record (the records it looks for are a few
	// KiB).
	matrixTurnInterruptMaxLineBytes = 1 << 20
	// matrixTurnInterruptMessageMax caps the message kept for display.
	matrixTurnInterruptMessageMax = 160
)

// matrixTurnInterrupt is one interrupted turn end.
type matrixTurnInterrupt struct {
	Kind    string // Claude's "error" (server_error, rate_limit, ...) or "turn_aborted"
	Message string // Claude: the rendered text minus "API Error: "; Codex: the abort reason
	// matrix/hardened: native capacity failure identity; never inferred from prose.
	TurnID string
	At     time.Time
}

// Detail is the SubstateDetail text.
func (t matrixTurnInterrupt) Detail() string {
	kind := t.Kind
	if kind == "" {
		kind = "unknown"
	}
	if t.Message == "" {
		return kind
	}
	return kind + ": " + t.Message
}

// matrixTurnInterruptMemo is the per-Instance cache, guarded by Instance.mu.
// key names the conversation the verdict was formed for ("claude:<id>",
// "codex:<id>"); a rebind discards it. gen versions in-flight scans so an
// older one cannot publish over a newer result.
type matrixTurnInterruptMemo struct {
	key       string
	scannedAt time.Time
	gen       uint64
	found     bool
	verdict   matrixTurnInterrupt
	// matrix/hardened: retry work is throttled separately and detail stays in memory.
	retryCheckedAt time.Time
	retryDetail    string
}

// matrixTurnInterruptKey is the memo key for the conversation the instance is
// bound to now, or "" when it has no local transcript to read. Caller holds
// i.mu.
func (i *Instance) matrixTurnInterruptKeyLocked() string {
	if i.IsSSH() { // the conversation lives on the remote host
		return ""
	}
	switch {
	case IsClaudeCompatible(i.Tool):
		if id := strings.TrimSpace(i.ClaudeSessionID); id != "" {
			return "claude:" + id
		}
	case IsCodexCompatible(i.Tool):
		if id := strings.TrimSpace(i.CodexSessionID); id != "" {
			return "codex:" + id
		}
	}
	return ""
}

// matrixWalkLinesBackward calls decide on each line of the file at path, last
// line first, until decide reports stop. A line longer than
// matrixTurnInterruptMaxLineBytes, a read error, or walking past
// matrixTurnInterruptMaxScanBytes ends the walk with no verdict.
func matrixWalkLinesBackward(path string, decide func(line []byte) (matrixTurnInterrupt, bool, bool)) (matrixTurnInterrupt, bool) {
	verdict, found, _ := matrixWalkLinesBackwardComplete(path, decide)
	return verdict, found
}

// matrix/hardened: a shared file cache must distinguish a completed scan from
// an I/O failure or an unreadable line, even though both have no UI verdict.
func matrixWalkLinesBackwardComplete(path string, decide func(line []byte) (matrixTurnInterrupt, bool, bool)) (matrixTurnInterrupt, bool, bool) {
	f, err := os.Open(path)
	if err != nil {
		return matrixTurnInterrupt{}, false, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return matrixTurnInterrupt{}, false, false
	}

	complete := true
	consider := func(start, end int64) (matrixTurnInterrupt, bool, bool) {
		if end <= start {
			return matrixTurnInterrupt{}, false, false
		}
		if end-start > matrixTurnInterruptMaxLineBytes {
			complete = false
			return matrixTurnInterrupt{}, false, true
		}
		line := make([]byte, end-start)
		if n, err := f.ReadAt(line, start); n != len(line) || (err != nil && err != io.EOF) {
			complete = false
			return matrixTurnInterrupt{}, false, true
		}
		return decide(line)
	}

	buf := make([]byte, min(int64(matrixTurnInterruptChunkBytes), info.Size()))
	lineEnd := info.Size()
	floor := max(info.Size()-matrixTurnInterruptMaxScanBytes, 0)
	for end := info.Size(); end > floor; {
		start := max(end-int64(len(buf)), floor)
		n := end - start
		if read, err := f.ReadAt(buf[:n], start); int64(read) != n || (err != nil && err != io.EOF) {
			return matrixTurnInterrupt{}, false, false
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				continue
			}
			if v, found, stop := consider(start+i+1, lineEnd); stop {
				return v, found, complete
			}
			lineEnd = start + i
		}
		end = start
	}
	if floor == 0 {
		// The first line has no newline before it.
		if v, found, stop := consider(0, lineEnd); stop {
			return v, found, complete
		}
	}
	return matrixTurnInterrupt{}, false, complete
}

// matrixInterruptRecord is the subset of a Claude transcript line the scan reads.
type matrixInterruptRecord struct {
	Type              string `json:"type"`
	IsSidechain       bool   `json:"isSidechain"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Error             string `json:"error"`
	Message           struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// matrixInterruptMessage extracts the first text block, trimmed for display.
func matrixInterruptMessage(content json.RawMessage) string {
	var text string
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = b.Text
				break
			}
		}
	} else {
		_ = json.Unmarshal(content, &text)
	}
	text = strings.Join(strings.Fields(text), " ")
	text = strings.TrimPrefix(text, "API Error: ")
	// "Connection lost mid-response. The response above may be incomplete."
	// keeps only its first sentence; the rest is the same for every failure.
	text = strings.TrimSuffix(text, " The response above may be incomplete.")
	if r := []rune(text); len(r) > matrixTurnInterruptMessageMax {
		text = string(r[:matrixTurnInterruptMessageMax-1]) + "…"
	}
	return text
}

// matrixLatestTurnInterrupt reports whether the latest main-conversation
// user/assistant record in the Claude transcript at path is an API-error
// reply. found is false for "no", and also for "cannot tell" (unreadable file,
// no such record within the scan bound, an oversized line in the way): the
// caller then shows the session as before, never as interrupted.
func matrixLatestTurnInterrupt(path string) (matrixTurnInterrupt, bool) {
	verdict, found, _ := matrixScanLatestTurnInterrupt(path)
	return verdict, found
}

// matrix/hardened: partial JSON can precede a later flush at the same path;
// retain the current verdict, but never memoize that incomplete observation.
func matrixScanLatestTurnInterrupt(path string) (matrixTurnInterrupt, bool, bool) {
	parsed := true
	verdict, found, complete := matrixWalkLinesBackwardComplete(path, func(line []byte) (matrixTurnInterrupt, bool, bool) {
		var rec matrixInterruptRecord
		if json.Unmarshal(line, &rec) != nil {
			parsed = false
			return matrixTurnInterrupt{}, false, false // blank or partial line
		}
		if rec.IsSidechain {
			return matrixTurnInterrupt{}, false, false
		}
		switch rec.Type {
		case "user":
			return matrixTurnInterrupt{}, false, true
		case "assistant":
			if !rec.IsAPIErrorMessage {
				return matrixTurnInterrupt{}, false, true
			}
			return matrixTurnInterrupt{
				Kind:    strings.TrimSpace(rec.Error),
				Message: matrixInterruptMessage(rec.Message.Content),
			}, true, true
		}
		return matrixTurnInterrupt{}, false, false // system, attachment, snapshot, ...
	})
	return verdict, found, complete && parsed
}

// matrixCodexEvent is the subset of a Codex rollout line the scan reads.
type matrixCodexEvent struct {
	// matrix/hardened: actual failed completion, not a warning copied into chat.
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
		TurnID string `json:"turn_id"`
		Error  *struct {
			Message string `json:"message"`
			Code    string `json:"codex_error_info"`
		} `json:"error"`
	} `json:"payload"`
}

// matrixLatestCodexTurnInterrupt reports whether the latest turn event in the
// Codex rollout at path is turn_aborted. Same "cannot tell means no" rule as
// the Claude scan.
func matrixLatestCodexTurnInterrupt(path string) (matrixTurnInterrupt, bool) {
	verdict, found, _ := matrixScanLatestCodexTurnInterrupt(path)
	return verdict, found
}

// matrix/hardened: keep incomplete-tail and I/O observations out of the
// process-wide cache while preserving the existing latest-turn event rules.
func matrixScanLatestCodexTurnInterrupt(path string) (matrixTurnInterrupt, bool, bool) {
	parsed := true
	verdict, found, complete := matrixWalkLinesBackwardComplete(path, func(line []byte) (matrixTurnInterrupt, bool, bool) {
		var ev matrixCodexEvent
		if json.Unmarshal(line, &ev) != nil {
			parsed = false
			return matrixTurnInterrupt{}, false, false
		}
		if ev.Type != "event_msg" {
			return matrixTurnInterrupt{}, false, false
		}
		switch ev.Payload.Type {
		case "turn_aborted":
			reason := strings.TrimSpace(ev.Payload.Reason)
			if reason == "" {
				reason = "unknown"
			}
			return matrixTurnInterrupt{Kind: "turn_aborted", Message: reason}, true, true
		// matrix/hardened: custom/current Codex records overload in task_complete.error.
		case "task_complete", "turn_complete":
			if e := ev.Payload.Error; e != nil && e.Code == "server_overloaded" && matrixIsModelCapacityMessage(e.Message) {
				at, _ := time.Parse(time.RFC3339Nano, ev.Timestamp)
				return matrixTurnInterrupt{Kind: "server_overloaded", Message: e.Message, TurnID: ev.Payload.TurnID, At: at}, true, true
			}
			return matrixTurnInterrupt{}, false, true
		case "task_started", "turn_started", "user_message":
			return matrixTurnInterrupt{}, false, true
		}
		return matrixTurnInterrupt{}, false, false
	})
	return verdict, found, complete && parsed
}

// matrixRefreshTurnInterrupt rescans the transcript, throttled, and caches the
// verdict. Called by updateStatus after it releases i.mu; takes i.mu itself.
func (i *Instance) matrixRefreshTurnInterrupt() {
	i.matrixRefreshTurnInterruptWithCache(matrixInterruptFiles)
	// matrix/hardened: only an explicitly enabled, native capacity failure can send.
	i.matrixMaybeRetryCodexCapacity()
}

// matrix/hardened: fresh Instances loaded for Web snapshots share stable
// file parses, while each Instance keeps its own 5-second refresh and gen guard.
func (i *Instance) matrixRefreshTurnInterruptWithCache(cache *matrixTurnInterruptFileCache) {
	i.mu.Lock()
	key := i.matrixTurnInterruptKeyLocked()
	m := &i.matrixInterrupt
	if key == "" {
		*m = matrixTurnInterruptMemo{}
		i.mu.Unlock()
		return
	}
	if m.key != key {
		*m = matrixTurnInterruptMemo{key: key}
	}
	if !m.scannedAt.IsZero() && time.Since(m.scannedAt) < matrixTurnInterruptScanInterval {
		i.mu.Unlock()
		return
	}
	m.scannedAt = time.Now()
	m.gen++
	gen := m.gen
	i.mu.Unlock()

	tool, id, _ := strings.Cut(key, ":")
	var verdict matrixTurnInterrupt
	var found bool
	// matrix/hardened: failed discovery and a newly subagent-sourced file must
	// publish a cleared verdict through the same key/gen guard as a good read.
	switch tool {
	case "claude":
		path := locateHandoffTranscript(i)
		if !transcriptBelongsToSession(path, id) {
			break
		}
		result := cache.load(tool, path)
		verdict, found = result.verdict, result.found
	case "codex":
		path := CodexRolloutPathForInstance(i)
		if path == "" || !strings.HasSuffix(filepath.Base(path), "-"+id+".jsonl") {
			break
		}
		result := cache.load(tool, path)
		if result.meta.ThreadSource == "subagent" || result.meta.ParentThreadID != "" {
			break
		}
		verdict, found = result.verdict, result.found
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.matrixTurnInterruptKeyLocked() == key && m.key == key && m.gen == gen {
		m.found, m.verdict = found, verdict
	}
}

// matrixTurnInterruptCached returns the cached verdict for the conversation the
// instance is bound to now. Never touches the filesystem.
func (i *Instance) matrixTurnInterruptCached() (matrixTurnInterrupt, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	m := i.matrixInterrupt
	if !m.found || m.key == "" || m.key != i.matrixTurnInterruptKeyLocked() {
		return matrixTurnInterrupt{}, false
	}
	return m.verdict, true
}

// matrixOverlayTurnInterrupt replaces a pane verdict that says nothing more
// specific (none, idle at prompt, hook lag) with turn-interrupted when the
// cached transcript verdict says the last turn stopped before it finished. A
// pane that shows live work, an open menu, background work or its own error
// banner keeps its verdict.
func (i *Instance) matrixOverlayTurnInterrupt(sub Substate) Substate {
	switch sub {
	case SubstateNone, SubstateIdleAtEmptyPrompt, SubstateHookLag:
	default:
		return sub
	}
	if _, ok := i.matrixTurnInterruptCached(); ok {
		return SubstateTurnInterrupted
	}
	return sub
}
