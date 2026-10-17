package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: a pane title equal to the session name is not repeated
// as the dim suffix.
func TestMatrixSubtitleDropsTitleEcho(t *testing.T) {
	state := sessionRenderState{title: "agent-deck UI 美化", paneTitle: " agent-deck UI 美化 "}
	if _, subtitle := sessionDisplayLabelsFromState(state); subtitle != "" {
		t.Fatalf("subtitle = %q, want it dropped as an echo of the title", subtitle)
	}
	state.paneTitle = "Running tests"
	if _, subtitle := sessionDisplayLabelsFromState(state); subtitle != "Running tests" {
		t.Fatalf("subtitle = %q, want the distinct pane title kept", subtitle)
	}
}

// matrix/hardened: long CJK worktree branches truncate on cell boundaries,
// never mid-rune.
func TestMatrixWorktreeBadgeCJKTruncation(t *testing.T) {
	forceTrueColorProfile()
	h := newSeamATestHome()
	h.width, h.height = 100, 30
	inst := &session.Instance{
		ID: "wt", Title: "财务盘点", Tool: "codex", Status: session.StatusIdle,
		WorktreePath: "/tmp/wt", WorktreeRepoRoot: "/tmp/repo", WorktreeBranch: "fork/财务盘点与报表整理",
	}
	h.flatItems = []session.Item{{Type: session.ItemTypeSession, Session: inst}}
	h.refreshSessionRenderSnapshot([]*session.Instance{inst})
	out := stripAnsi(h.renderSessionList(100, 4))
	if !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) {
		t.Fatalf("row has a broken rune: %q", out)
	}
	if !strings.Contains(out, "[fork/财务") || !strings.Contains(out, "…]") {
		t.Fatalf("row lacks the truncated branch badge: %q", out)
	}
}
