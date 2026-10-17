package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// matrix/hardened: LocateClaudeResumeDir must return the directory a
// conversation was started in, not the last cwd it recorded, and must ignore
// the stub copies a mis-directed `--session-id` launch leaves behind.

const matrixResumeSID = "0b5e7a2c-4d1f-4e8a-9c3b-7f2d6e1a8b94"

// writeMatrixTranscript writes <home>/.claude/projects/<dirName>/<sid>.jsonl
// with one user record per cwd and returns its path.
func writeMatrixTranscript(t *testing.T, home, dirName string, cwds ...string) string {
	t.Helper()
	proj := filepath.Join(home, ".claude", "projects", dirName)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`{"type":"file-history-snapshot","messageId":"m0"}` + "\n")
	for n, cwd := range cwds {
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"step %d"},"uuid":"u%d","sessionId":%q,"cwd":%q}`+"\n", n, n, matrixResumeSID, cwd)
	}
	path := filepath.Join(proj, matrixResumeSID+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func matrixResumeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	t.Setenv("HOME", home)
	return home
}

func TestMatrixLocateClaudeResumeDir_UsesLaunchDirNotLastCWD(t *testing.T) {
	home := matrixResumeHome(t)
	launch := filepath.Join(home, "work")
	last := filepath.Join(launch, "worktrees", "panel", "static")
	if err := os.MkdirAll(last, 0o755); err != nil {
		t.Fatal(err)
	}
	main := writeMatrixTranscript(t, home, ConvertToClaudeDirName(launch), launch, launch, filepath.Join(launch, "worktrees"), last)
	// The stub a `--session-id` launch from the last cwd wrote: it encodes
	// its own dir correctly, but is smaller than the real conversation.
	writeMatrixTranscript(t, home, ConvertToClaudeDirName(last), last)
	// A copy whose first cwd does not encode its dir cannot be resumed from
	// anywhere and must never be chosen, however large.
	writeMatrixTranscript(t, home, "-elsewhere", launch, launch, launch, launch, launch, launch, launch, launch)

	loc, err := LocateClaudeResumeDir(nil, matrixResumeSID)
	if err != nil {
		t.Fatalf("LocateClaudeResumeDir: %v", err)
	}
	if loc.Dir != launch || loc.Transcript != main {
		t.Fatalf("got dir %q transcript %q, want %q %q", loc.Dir, loc.Transcript, launch, main)
	}
}

func TestMatrixLocateClaudeResumeDir_Refusals(t *testing.T) {
	home := matrixResumeHome(t)
	if _, err := LocateClaudeResumeDir(nil, "not-a-uuid"); err == nil {
		t.Fatal("non-uuid id accepted")
	}
	if _, err := LocateClaudeResumeDir(nil, matrixResumeSID); err == nil || !strings.Contains(err.Error(), "nothing to resume") {
		t.Fatalf("missing transcript: %v", err)
	}
	// Only a mis-filed copy: refused rather than resumed from its cwd.
	launch := filepath.Join(home, "app")
	if err := os.MkdirAll(launch, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMatrixTranscript(t, home, "-elsewhere", launch)
	if _, err := LocateClaudeResumeDir(nil, matrixResumeSID); err == nil {
		t.Fatal("copy under a foreign project dir accepted")
	}
	// The real one, but its launch dir is gone (a removed worktree).
	writeMatrixTranscript(t, home, ConvertToClaudeDirName(launch), launch)
	if err := os.RemoveAll(launch); err != nil {
		t.Fatal(err)
	}
	if _, err := LocateClaudeResumeDir(nil, matrixResumeSID); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("vanished launch dir: %v", err)
	}
}

func TestMatrixClaudeResumeLocation_LiveFor(t *testing.T) {
	now := time.Now()
	if _, live := (ClaudeResumeLocation{ModTime: now.Add(-30 * time.Second)}).LiveFor(now); !live {
		t.Fatal("a transcript written 30s ago is not reported live")
	}
	if _, live := (ClaudeResumeLocation{ModTime: now.Add(-time.Hour)}).LiveFor(now); live {
		t.Fatal("a transcript written an hour ago is reported live")
	}
}

// A conversation continued from another directory keeps its older records:
// the first cwd names the old directory, and the one that encodes the
// project dir the file sits in is where it must be resumed.
func TestMatrixLocateClaudeResumeDir_ContinuedFromAnotherDir(t *testing.T) {
	home := matrixResumeHome(t)
	oldDir := filepath.Join(home, "old", "theOne")
	newDir := filepath.Join(home, "work", "theOne")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMatrixTranscript(t, home, ConvertToClaudeDirName(newDir), oldDir, oldDir, newDir, filepath.Join(newDir, "backend"))
	loc, err := LocateClaudeResumeDir(nil, matrixResumeSID)
	if err != nil {
		t.Fatalf("LocateClaudeResumeDir: %v", err)
	}
	if loc.Dir != newDir {
		t.Fatalf("got %q, want %q", loc.Dir, newDir)
	}
}

func TestMatrixRecordCWD(t *testing.T) {
	for _, tc := range []struct {
		line, want string
		ok         bool
	}{
		{`{"parentUuid":null,"cwd":"/data/work/theone","sessionId":"x"}`, "/data/work/theone", true},
		{`{"cwd":"/a \"b\"/c","x":1}`, `/a "b"/c`, true},
		// a "cwd" quoted inside a message body is escaped and must not match
		{`{"message":{"content":"run {\"cwd\":\"/etc\"}"}}`, "", false},
		{`{"cwd":""}`, "", false},
		{`{"cwd":"/unterminated`, "", false},
	} {
		got, ok := recordCWD([]byte(tc.line))
		if got != tc.want || ok != tc.ok {
			t.Errorf("recordCWD(%s) = %q, %v; want %q, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}
