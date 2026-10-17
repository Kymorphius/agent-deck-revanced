package ui

import (
	"os"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: createSessionFromGlobalSearch must not bake a resume
// command into inst.Command — Start's buildClaudeResumeCommand wraps any
// non-"claude" Command again and the pane dies on "exec exec claude ...".
// See internal/session/matrix_global_search_resume_test.go for the
// command-shape half; this guards the call site.
func TestMatrixGlobalSearchImport_DoesNotBakeResumeCommand(t *testing.T) {
	src, err := os.ReadFile("home.go")
	if err != nil {
		t.Fatalf("read home.go: %v", err)
	}
	for _, bad := range []string{`"exec claude --resume `, `"exec claude --session-id `} {
		if strings.Contains(string(src), bad) {
			t.Errorf("home.go builds a %s... command again; let Start build it from ClaudeSessionID", bad)
		}
	}
}

// matrix/hardened: the G import resumes from the transcript's launch dir via
// session.LocateClaudeResumeDir, never from result.CWD (the conversation's
// last cwd), so a picked conversation is not restarted empty elsewhere.
func TestMatrixGlobalSearchImport_ResumesFromLaunchDir(t *testing.T) {
	src, err := os.ReadFile("home.go")
	if err != nil {
		t.Fatalf("read home.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (h *Home) createSessionFromGlobalSearch(")
	if start < 0 {
		t.Fatal("createSessionFromGlobalSearch not found")
	}
	fn := body[start:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, "session.LocateClaudeResumeDir(") || !strings.Contains(fn, "LiveFor(") {
		t.Error("createSessionFromGlobalSearch no longer locates the launch dir / checks for a live conversation")
	}
	if strings.Contains(fn, "result.CWD") {
		t.Error("createSessionFromGlobalSearch uses result.CWD (the last cwd) again")
	}
}

// matrix/hardened: a Codex hit jumps to the session already on its thread,
// and otherwise is resumed (createCodexSessionFromGlobalSearch), which
// refuses a thread with no rollout in the launch's Codex home instead of
// starting a fresh one.
func TestMatrixGlobalSearch_CodexHit(t *testing.T) {
	const thread = "01a12172-c424-7502-b2c3-5e250fde3050"
	t.Setenv("CODEX_HOME", t.TempDir())
	home := NewHome()
	home.width, home.height = 120, 40
	inst := session.NewInstanceWithTool("codex-on-thread", t.TempDir(), "codex")
	inst.CodexSessionID = thread
	home.instancesMu.Lock()
	home.instances = []*session.Instance{inst}
	home.instanceByID[inst.ID] = inst
	home.instancesMu.Unlock()
	home.groupTree = session.NewGroupTree(home.instances)
	home.rebuildFlatItems()

	if cmd := home.handleGlobalSearchSelection(&GlobalSearchResult{Harness: "codex", SessionID: thread}); cmd != nil {
		t.Fatal("hit on a thread a session already owns must jump, not create")
	}
	if item := home.flatItems[home.cursor]; item.Session == nil || item.Session.ID != inst.ID {
		t.Fatalf("cursor not on the owning session: %+v", item)
	}

	cmd := home.handleGlobalSearchSelection(&GlobalSearchResult{Harness: "codex", SessionID: "01a12172-0000-7000-8000-000000000000"})
	if cmd == nil {
		t.Fatal("unowned Codex hit produced no command")
	}
	msg, ok := cmd().(sessionCreatedMsg)
	if !ok || msg.err == nil || !strings.Contains(msg.err.Error(), "nothing to resume") {
		t.Fatalf("thread without rollout: %#v", msg)
	}
}
