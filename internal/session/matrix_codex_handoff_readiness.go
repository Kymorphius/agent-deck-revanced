package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const codexHandoffArtifactMaxBytes = int64(8 << 20)

// readFreshCodexHandoffArtifact is a read-only fallback for a user-owned
// custom notify. matrix/hardened: only the single thread held by this pane's
// live Codex process is eligible. Never discover a thread by cwd, recency,
// stored mutable IDs, or chat text, and never change the user's notify.
func readFreshCodexHandoffArtifact(ctx context.Context, target *Instance, expected FreshTargetIdentity, startedAt, now time.Time) (CrossHarnessNativeArtifact, error) {
	if err := ctx.Err(); err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	if target == nil || target.ID == "" || target.ID != expected.InstanceID || canonicalSwitchHarness(target.Tool) != "codex" || canonicalSwitchHarness(expected.Tool) != "codex" || !target.CodexRolloutIsResolvableLocally() || target.Account != expected.Account || startedAt.IsZero() {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex fresh target binding is unavailable")
	}
	cwd := target.EffectiveWorkingDir()
	if !filepath.IsAbs(cwd) || (expected.ProjectPath != "" && filepath.Clean(cwd) != filepath.Clean(expected.ProjectPath)) {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex fresh target cwd mismatch")
	}
	id := codexHandoffLiveThreadID(target)
	if !uuidBareRegex.MatchString(id) {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex fresh thread has no unique live process owner")
	}
	if expected.SessionID != "" && expected.SessionID != id {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex fresh target native identity mismatch")
	}
	if err := ctx.Err(); err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	home := filepath.Clean(ExpandPath(target.getCodexHomeDir()))
	if !filepath.IsAbs(home) {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex target account home must be absolute")
	}
	paths, err := exactCodexRolloutMatches(id, home)
	if err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	path, err := uniqueRegularArtifact(paths, "fresh Codex rollout for "+id)
	if err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	if expected.NativeSessionPath != "" && filepath.Clean(expected.NativeSessionPath) != path {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex fresh target native path mismatch")
	}
	info, err := validateCodexHandoffArtifactPath(home, path)
	if err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	if !codexHandoffProcessOwnsArtifact(target, id, home, path) {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex live process has no exact account-home artifact ownership")
	}
	artifact := CrossHarnessNativeArtifact{Path: path, SessionID: id, ModifiedAt: info.ModTime(), Ready: true}
	if !freshCrossHarnessArtifact(artifact, startedAt, now) {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex target artifact is not fresh")
	}
	f, err := os.Open(path)
	if err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > codexHandoffArtifactMaxBytes {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex target artifact changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, codexHandoffArtifactMaxBytes+1))
	if err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	if int64(len(data)) > codexHandoffArtifactMaxBytes {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex target artifact exceeds readiness read bound")
	}
	if err := validateFreshCodexHandoffRecords(ctx, data, target.ID, id, cwd, startedAt, now); err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	// The writer can append while we read, but replacement or a thread switch
	// must not turn the prior process/artifact observation into readiness.
	current, err := validateCodexHandoffArtifactPath(home, path)
	if err != nil || !os.SameFile(info, current) {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex target artifact changed while reading")
	}
	if codexHandoffLiveThreadID(target) != id || !codexHandoffProcessOwnsArtifact(target, id, home, path) {
		return CrossHarnessNativeArtifact{}, fmt.Errorf("Codex target live thread changed while reading")
	}
	if err := ctx.Err(); err != nil {
		return CrossHarnessNativeArtifact{}, err
	}
	return artifact, nil
}

func codexHandoffLiveThreadID(target *Instance) string {
	// The usual bootstrap helper can write a lifecycle rejection log. Keep
	// this observer entirely read-only; pedigree is validated in the document.
	pids, err := codexPaneProcessPIDs(target)
	if err != nil || len(pids) == 0 {
		return ""
	}
	return target.LiveCodexThreadID()
}

func codexHandoffProcessOwnsArtifact(target *Instance, id, home, path string) bool {
	paths, err := codexPaneOpenPaths(target)
	if err != nil {
		return false
	}
	// LiveCodexThreadID's rollout filename extraction alone is not an
	// account-home gate. Require an exact open artifact or writer lock in the
	// configured home too, including the OS-canonical spelling of that home.
	canonicalHome := canonicalDir(home)
	lock := filepath.Join(home, "thread-writer-locks", id+".lock")
	canonicalLock := filepath.Join(canonicalHome, "thread-writer-locks", id+".lock")
	canonicalPath := filepath.Join(canonicalHome, strings.TrimPrefix(path, home+string(filepath.Separator)))
	for _, open := range paths {
		if open == path || open == canonicalPath || open == lock || open == canonicalLock {
			return true
		}
	}
	return false
}

// matrix/hardened: an exact filename match still cannot cross account-home
// boundaries via symlinked directories, or read a nonregular/oversized file.
func validateCodexHandoffArtifactPath(home, path string) (os.FileInfo, error) {
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 5 || parts[0] != "sessions" || !strings.HasPrefix(parts[4], "rollout-") || !strings.HasSuffix(parts[4], ".jsonl") {
		return nil, fmt.Errorf("Codex target artifact is outside its account sessions directory")
	}
	if _, err := time.Parse("2006/01/02", strings.Join(parts[1:4], "/")); err != nil {
		return nil, fmt.Errorf("Codex target artifact has a malformed date directory")
	}
	current := home
	for n := -1; n < len(parts); n++ {
		if n >= 0 {
			current = filepath.Join(current, parts[n])
		}
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing symlink in Codex target artifact path")
		}
		if n == len(parts)-1 {
			if !info.Mode().IsRegular() || info.Size() > codexHandoffArtifactMaxBytes {
				return nil, fmt.Errorf("Codex target artifact is not a bounded regular file")
			}
			return info, nil
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("Codex target artifact parent is not a directory")
		}
	}
	return nil, fmt.Errorf("Codex target artifact is unavailable")
}

type codexHandoffRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// validateFreshCodexHandoffRecords requires a fresh native user-thread header,
// the trusted developer identity injection, and a native task_started event.
// A copied handoff/user/tool message cannot supply the instance binding.
func validateFreshCodexHandoffRecords(ctx context.Context, data []byte, instanceID, sessionID, cwd string, startedAt, now time.Time) error {
	if int64(len(data)) > codexHandoffArtifactMaxBytes || startedAt.IsZero() || !uuidBareRegex.MatchString(sessionID) {
		return fmt.Errorf("Codex readiness document has invalid bounds or identity")
	}
	// Codex appends JSONL. A final record without its newline is not durable
	// evidence yet, whether it happens to be valid JSON or remains unfinished.
	last := bytes.LastIndexByte(data, '\n')
	if last < 0 {
		return fmt.Errorf("Codex readiness document has no complete record")
	}
	lines := bytes.Split(data[:last], []byte{'\n'})
	header, bound, started := false, false, false
	for n, line := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		var record codexHandoffRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return fmt.Errorf("Codex readiness document contains a malformed complete record")
		}
		if n == 0 && record.Type != "session_meta" {
			return fmt.Errorf("Codex readiness document has no native header")
		}
		switch record.Type {
		case "session_meta":
			var meta struct {
				ID             string `json:"id"`
				SessionID      string `json:"session_id"`
				Timestamp      string `json:"timestamp"`
				Cwd            string `json:"cwd"`
				Source         string `json:"source"`
				ThreadSource   string `json:"thread_source"`
				ParentThreadID string `json:"parent_thread_id"`
				ForkedFromID   string `json:"forked_from_id"`
			}
			if header || json.Unmarshal(record.Payload, &meta) != nil || meta.ID != sessionID || (meta.SessionID != "" && meta.SessionID != sessionID) || !filepath.IsAbs(meta.Cwd) || filepath.Clean(meta.Cwd) != filepath.Clean(cwd) || meta.Source != "cli" || meta.ThreadSource != "user" || meta.ParentThreadID != "" || meta.ForkedFromID != "" || !freshCodexHandoffTimestamp(record.Timestamp, startedAt, now) || (meta.Timestamp != "" && !freshCodexHandoffTimestamp(meta.Timestamp, startedAt, now)) {
				return fmt.Errorf("Codex readiness native header does not bind the fresh target")
			}
			header = true
		case "response_item":
			var item struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(record.Payload, &item) != nil {
				return fmt.Errorf("Codex readiness response item is malformed")
			}
			if item.Type != "message" || item.Role != "developer" {
				continue
			}
			for _, content := range item.Content {
				if content.Type != "input_text" {
					continue
				}
				present, matches := codexHandoffDeveloperBinding(content.Text, instanceID)
				if present {
					// Codex can repeat the same trusted injection after compaction.
					// Every occurrence must still identify this exact instance.
					if !matches || !freshCodexHandoffTimestamp(record.Timestamp, startedAt, now) {
						return fmt.Errorf("Codex readiness developer identity is ambiguous or foreign")
					}
					bound = true
				}
			}
		case "event_msg":
			var event struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			}
			if json.Unmarshal(record.Payload, &event) != nil {
				return fmt.Errorf("Codex readiness lifecycle event is malformed")
			}
			if event.Type == "task_started" {
				if !uuidBareRegex.MatchString(event.TurnID) || !freshCodexHandoffTimestamp(record.Timestamp, startedAt, now) {
					return fmt.Errorf("Codex readiness lifecycle event has no fresh native turn")
				}
				started = true
			}
		}
	}
	if !header || !bound || !started {
		return fmt.Errorf("Codex fresh native header, developer identity, and task start are required")
	}
	return ctx.Err()
}

func freshCodexHandoffTimestamp(value string, startedAt, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !startedAt.IsZero() && !at.Before(startedAt) && (now.IsZero() || !at.After(now.Add(2*time.Second)))
}

// The session fields belong to the named snapshot section under the exact
// generated heading. IDs quoted in prose, other sections, or other roles are
// not bindings. Duplicate headings or identity fields are ambiguous.
func codexHandoffDeveloperBinding(text, instanceID string) (present, matches bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	heading, section := -1, -1
	for n, line := range lines {
		if line == "# agent-deck session context" {
			if heading >= 0 {
				return true, false
			}
			heading = n
		}
	}
	if heading < 0 {
		return false, false
	}
	for n := heading + 1; n < len(lines); n++ {
		if strings.HasPrefix(lines[n], "#") {
			if lines[n] != "## This session (snapshot from the agent-deck database at launch)" {
				return true, false
			}
			section = n
			break
		}
	}
	if section < 0 {
		return true, false
	}
	var id, tool string
	idCount, toolCount := 0, 0
	for _, line := range lines[section+1:] {
		if strings.HasPrefix(line, "#") {
			break
		}
		if strings.HasPrefix(line, "- session id:") {
			idCount++
			id = strings.TrimPrefix(line, "- session id: ")
		}
		if strings.HasPrefix(line, "- tool:") {
			toolCount++
			tool = strings.TrimPrefix(line, "- tool: ")
		}
	}
	return true, idCount == 1 && toolCount == 1 && id == instanceID && tool == "codex"
}
