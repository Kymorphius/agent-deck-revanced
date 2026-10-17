package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: tests for matrix_turn_interrupt.go. Record shapes are
// copied from real Claude Code 2.1.x transcripts (2026-10-09).

const (
	tiUser      = `{"type":"user","isSidechain":false,"message":{"role":"user","content":"继续"}}`
	tiToolRes   = `{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`
	tiAssistant = `{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"Builds."}]}}`
	tiLost      = `{"type":"assistant","isSidechain":false,"isApiErrorMessage":true,"error":"server_error","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"API Error: Connection lost mid-response. The response above may be incomplete."}]}}`
	tiReset     = `{"type":"assistant","isSidechain":false,"isApiErrorMessage":true,"error":"server_error","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"API Error: Connection dropped (ECONNRESET)"}]}}`
	tiSideLost  = `{"type":"assistant","isSidechain":true,"isApiErrorMessage":true,"error":"server_error","message":{"role":"assistant","content":[{"type":"text","text":"API Error: Connection lost mid-response."}]}}`
	tiDuration  = `{"type":"system","subtype":"turn_duration","isSidechain":false}`
	tiSnapshot  = `{"type":"file-history-snapshot","snapshot":{}}`
)

func matrixWriteTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sid.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMatrixLatestTurnInterrupt(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		wantFound  bool
		wantDetail string
	}{
		{"connection lost then turn_duration", []string{tiUser, tiAssistant, tiLost, tiDuration}, true,
			"server_error: Connection lost mid-response."},
		{"econnreset is last", []string{tiUser, tiToolRes, tiReset}, true,
			"server_error: Connection dropped (ECONNRESET)"},
		{"user continued after the error", []string{tiUser, tiLost, tiDuration, tiUser, tiSnapshot}, false, ""},
		{"user continued, reply in flight", []string{tiLost, tiUser, tiAssistant}, false, ""},
		{"normal finished turn", []string{tiUser, tiAssistant, tiDuration}, false, ""},
		{"subagent error does not mark the parent", []string{tiUser, tiAssistant, tiSideLost}, false, ""},
		{"subagent traffic after the main error is skipped", []string{tiUser, tiLost, tiSideLost, tiDuration}, true,
			"server_error: Connection lost mid-response."},
		{"error is the first line", []string{tiLost}, true, "server_error: Connection lost mid-response."},
		{"no conversation records", []string{tiSnapshot, tiDuration}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := matrixLatestTurnInterrupt(matrixWriteTranscript(t, tt.lines...))
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if found && got.Detail() != tt.wantDetail {
				t.Errorf("Detail() = %q, want %q", got.Detail(), tt.wantDetail)
			}
		})
	}
}

func TestMatrixLatestTurnInterrupt_MissingOrEmpty(t *testing.T) {
	if _, found := matrixLatestTurnInterrupt(filepath.Join(t.TempDir(), "absent.jsonl")); found {
		t.Error("missing file: found = true")
	}
	if _, found := matrixLatestTurnInterrupt(matrixWriteTranscript(t)); found {
		t.Error("empty file: found = true")
	}
}

// An oversized line's type is unknown, so it must stop the walk rather than
// let an older error show through a possibly newer user record.
func TestMatrixLatestTurnInterrupt_OversizedLineStops(t *testing.T) {
	big := `{"type":"user","message":{"content":"` + strings.Repeat("x", matrixTurnInterruptMaxLineBytes) + `"}}`
	if _, found := matrixLatestTurnInterrupt(matrixWriteTranscript(t, tiLost, big)); found {
		t.Error("error behind an oversized line was reported")
	}
}

// The decisive record sits several read windows back behind small records.
func TestMatrixLatestTurnInterrupt_MultiChunkWalk(t *testing.T) {
	lines := []string{tiUser, tiLost}
	for n := 0; n < 3*matrixTurnInterruptChunkBytes/len(tiDuration); n++ {
		lines = append(lines, tiDuration)
	}
	if _, found := matrixLatestTurnInterrupt(matrixWriteTranscript(t, lines...)); !found {
		t.Error("error several chunks back was not found")
	}
	lines = append([]string{tiLost, tiUser}, lines[2:]...)
	if _, found := matrixLatestTurnInterrupt(matrixWriteTranscript(t, lines...)); found {
		t.Error("user record several chunks back did not clear the older error")
	}
}

func TestMatrixInterruptMessage_Truncates(t *testing.T) {
	raw := []byte(`[{"type":"text","text":"API Error: ` + strings.Repeat("é", 400) + `"}]`)
	got := matrixInterruptMessage(raw)
	if n := len([]rune(got)); n != matrixTurnInterruptMessageMax {
		t.Errorf("len = %d runes, want %d", n, matrixTurnInterruptMessageMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated message has no ellipsis: %q", got)
	}
}

func TestMatrixOverlayTurnInterrupt(t *testing.T) {
	inst := &Instance{Tool: "claude", ClaudeSessionID: "sid"}
	inst.matrixInterrupt = matrixTurnInterruptMemo{key: "claude:sid", found: true,
		verdict: matrixTurnInterrupt{Kind: "server_error", Message: "Connection lost mid-response."}}

	for _, sub := range []Substate{SubstateNone, SubstateIdleAtEmptyPrompt, SubstateHookLag} {
		if got := inst.matrixOverlayTurnInterrupt(sub); got != SubstateTurnInterrupted {
			t.Errorf("overlay(%q) = %q, want turn-interrupted", sub, got)
		}
	}
	// A pane that says something more specific keeps it.
	for _, sub := range []Substate{SubstateRunning, SubstateInteractiveMenu, SubstateAuth401,
		SubstateModelUnavailable, SubstateBackgroundWork} {
		if got := inst.matrixOverlayTurnInterrupt(sub); got != sub {
			t.Errorf("overlay(%q) = %q, want unchanged", sub, got)
		}
	}

	// Rebound to another conversation: the verdict belongs to the old one.
	inst.ClaudeSessionID = "other"
	if got := inst.matrixOverlayTurnInterrupt(SubstateIdleAtEmptyPrompt); got != SubstateIdleAtEmptyPrompt {
		t.Errorf("after rebind overlay = %q, want idle-at-empty-prompt", got)
	}
}

// Sessions with no local transcript (other tools, SSH, unbound) clear any memo
// instead of reading files.
func TestMatrixRefreshTurnInterrupt_SkipsNonLocal(t *testing.T) {
	for _, inst := range []*Instance{
		{Tool: "gemini", ClaudeSessionID: "sid", CodexSessionID: "sid"},
		{Tool: "claude", ClaudeSessionID: "sid", SSHHost: "srv"},
		{Tool: "codex", CodexSessionID: "sid", SSHHost: "srv"},
		{Tool: "claude"},
		{Tool: "codex", ClaudeSessionID: "sid"},
	} {
		inst.matrixInterrupt = matrixTurnInterruptMemo{key: "claude:sid", found: true}
		inst.matrixRefreshTurnInterrupt()
		if _, ok := inst.matrixTurnInterruptCached(); ok {
			t.Errorf("tool=%q ssh=%q: verdict kept", inst.Tool, inst.SSHHost)
		}
	}
}

// Codex rollout shapes copied from codex 0.161/0.162 rollouts (2026-10-10).
const (
	cxMeta     = `{"timestamp":"2026-10-10T16:45:48.000Z","type":"session_meta","payload":{"id":"cxid","source":"cli","thread_source":"user"}}`
	cxSubMeta  = `{"timestamp":"2026-10-10T16:45:48.000Z","type":"session_meta","payload":{"id":"cxid","thread_source":"subagent","parent_thread_id":"parent"}}`
	cxStarted  = `{"timestamp":"2026-10-10T17:42:02.242Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`
	cxUserMsg  = `{"timestamp":"2026-10-10T17:42:02.300Z","type":"event_msg","payload":{"type":"user_message","message":"继续"}}`
	cxDevAbort = `{"timestamp":"2026-10-10T17:42:04.620Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<turn_aborted>The previous turn was interrupted on purpose.</turn_aborted>"}]}}`
	cxAborted  = `{"timestamp":"2026-10-10T17:42:04.627Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t1","reason":"interrupted","duration_ms":2387}}`
	cxDone     = `{"timestamp":"2026-10-10T16:47:44.101Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t0","last_agent_message":"ok"}}`
	cxTokens   = `{"timestamp":"2026-10-10T17:42:04.700Z","type":"event_msg","payload":{"type":"token_count","info":null}}`
)

func TestMatrixLatestCodexTurnInterrupt(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		wantFound  bool
		wantDetail string
	}{
		{"aborted and left", []string{cxMeta, cxDone, cxUserMsg, cxStarted, cxDevAbort, cxAborted, cxTokens}, true, "turn_aborted: interrupted"},
		{"continued after the abort", []string{cxMeta, cxStarted, cxAborted, cxUserMsg, cxStarted}, false, ""},
		{"steer: next turn starts at once", []string{cxMeta, cxStarted, cxAborted, cxStarted}, false, ""},
		{"finished turn", []string{cxMeta, cxStarted, cxDone, cxTokens}, false, ""},
		{"turn in progress", []string{cxMeta, cxDone, cxStarted, cxTokens}, false, ""},
		{"no turns yet", []string{cxMeta}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := matrixLatestCodexTurnInterrupt(matrixWriteTranscript(t, tt.lines...))
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if found && got.Detail() != tt.wantDetail {
				t.Errorf("Detail() = %q, want %q", got.Detail(), tt.wantDetail)
			}
		})
	}
}

// End to end through the refresh: the rollout is found under CODEX_HOME by
// the session's CodexSessionID; a subagent thread is never marked.
func TestMatrixRefreshTurnInterrupt_Codex(t *testing.T) {
	const id = "01a126b4-d738-7dc3-a840-6bce2b002dae"
	for _, tc := range []struct {
		name string
		meta string
		want bool
	}{{"user thread", cxMeta, true}, {"subagent thread", cxSubMeta, false}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			dir := filepath.Join(home, "sessions", "2026", "10", "10")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			body := strings.Join([]string{tc.meta, cxStarted, cxAborted}, "\n") + "\n"
			if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-10T09-45-48-"+id+".jsonl"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			inst := &Instance{Tool: "codex", CodexSessionID: id}
			inst.matrixRefreshTurnInterrupt()
			v, ok := inst.matrixTurnInterruptCached()
			if ok != tc.want {
				t.Fatalf("interrupted = %v, want %v", ok, tc.want)
			}
			if ok && v.Detail() != "turn_aborted: interrupted" {
				t.Errorf("Detail() = %q", v.Detail())
			}
		})
	}
}
