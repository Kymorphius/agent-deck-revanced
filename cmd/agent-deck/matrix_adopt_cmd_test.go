package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// matrix/hardened: `session adopt` takes over a process running in a tmux
// session agent-deck did not create, without restarting it. A real tmux
// server on a private socket stands in for the user's, a `sleep` for the
// Claude process, and a hand-written <config>/sessions/<pid>.json for the
// status file Claude Code keeps per process.

func adoptTestTmux(t *testing.T, socket string, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", socket}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// runAdopt runs the CLI against the tmux servers this test started: TestMain
// isolates tmux under TMUX_TMPDIR, which runAgentDeck strips.
func runAdopt(t *testing.T, home string, args ...string) (string, string, int) {
	t.Helper()
	return runAgentDeckEnv(t, home, "", []string{"TMUX_TMPDIR=" + os.Getenv("TMUX_TMPDIR")}, args...)
}

func writeAdoptStatusFile(t *testing.T, home string, pid int, sid, cwd string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"kind":"interactive","status":"busy"}`, pid, sid, cwd)
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

type adoptListJSON struct {
	Candidates []struct {
		TmuxSession string `json:"tmux_session"`
		Socket      string `json:"socket"`
		ClaudePID   int    `json:"claude_pid"`
		Path        string `json:"path"`
		Adoptable   bool   `json:"adoptable"`
		Reason      string `json:"reason"`
	} `json:"candidates"`
}

func TestMatrixSessionAdopt_TakesOverWithoutRestart(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	proj := filepath.Join(home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	socket := fmt.Sprintf("adopt-test-%d", os.Getpid())
	adoptTestTmux(t, socket, "new-session", "-d", "-s", "work", "-c", proj, "sleep 600")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	pid, err := strconv.Atoi(adoptTestTmux(t, socket, "display-message", "-p", "-t", "=work:", "#{pane_pid}"))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "6a1f0c2e-9b7d-4e3a-8c5f-2d4b6e8a0c1f"
	writeAdoptStatusFile(t, home, pid, sid, proj)

	// A second live process on the same conversation blocks the adoption.
	writeAdoptStatusFile(t, home, os.Getpid(), sid, proj)
	stdout, _, code := runAdopt(t, home, "session", "adopt", "--list", "--socket", socket, "--json")
	var list adoptListJSON
	if code != 0 || json.Unmarshal([]byte(stdout), &list) != nil || len(list.Candidates) != 1 {
		t.Fatalf("list: %d %s", code, stdout)
	}
	if c := list.Candidates[0]; c.Adoptable || !strings.Contains(c.Reason, strconv.Itoa(os.Getpid())) {
		t.Fatalf("duplicate conversation not refused: %+v", c)
	}
	if _, _, code := runAdopt(t, home, "session", "adopt", "work", "--socket", socket); code == 0 {
		t.Fatal("adopt succeeded while the conversation is open twice")
	}
	if err := os.Remove(filepath.Join(home, ".claude", "sessions", strconv.Itoa(os.Getpid())+".json")); err != nil {
		t.Fatal(err)
	}

	stdout, _, code = runAdopt(t, home, "session", "adopt", "--list", "--socket", socket, "--json")
	if code != 0 || json.Unmarshal([]byte(stdout), &list) != nil || len(list.Candidates) != 1 {
		t.Fatalf("list: %d %s", code, stdout)
	}
	if c := list.Candidates[0]; !c.Adoptable || c.TmuxSession != "work" || c.ClaudePID != pid || c.Path != proj || c.Socket != socket {
		t.Fatalf("candidate: %+v", c)
	}

	stdout, stderr, code := runAdopt(t, home, "session", "adopt", "work", "--socket", socket, "-t", "taken", "--json")
	if code != 0 {
		t.Fatalf("adopt: %d %s %s", code, stdout, stderr)
	}
	var adopted struct {
		ID          string `json:"id"`
		TmuxSession string `json:"tmux_session"`
		Socket      string `json:"socket"`
	}
	if err := json.Unmarshal([]byte(stdout), &adopted); err != nil || adopted.ID == "" || adopted.Socket != socket {
		t.Fatalf("adopt output: %s", stdout)
	}
	// Same process, now under the managed name; the old name is gone.
	if got := adoptTestTmux(t, socket, "display-message", "-p", "-t", "="+adopted.TmuxSession+":", "#{pane_pid}"); got != strconv.Itoa(pid) {
		t.Fatalf("pane pid after adopt = %s, want %d", got, pid)
	}
	if exec.Command("tmux", "-L", socket, "has-session", "-t", "=work").Run() == nil {
		t.Fatal("old tmux session name still exists")
	}

	stdout, _, _ = runAgentDeck(t, home, "session", "show", "taken", "--json")
	var shown map[string]any
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
		t.Fatalf("show: %s", stdout)
	}
	if shown["tool"] != "claude" || shown["claude_session_id"] != sid || shown["tmux_session"] != adopted.TmuxSession || shown["path"] != proj {
		t.Fatalf("registered session: %v", shown)
	}

	// Now it is managed: listed, but not adoptable again.
	stdout, _, _ = runAdopt(t, home, "session", "adopt", "--list", "--socket", socket, "--json")
	if json.Unmarshal([]byte(stdout), &list) != nil || len(list.Candidates) != 1 || list.Candidates[0].Adoptable {
		t.Fatalf("after adopt: %s", stdout)
	}
}

func TestMatrixSessionAdopt_RefusesSharedTmuxSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	socket := fmt.Sprintf("adopt-test-split-%d", os.Getpid())
	adoptTestTmux(t, socket, "new-session", "-d", "-s", "shared", "-c", home, "sleep 600")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	adoptTestTmux(t, socket, "split-window", "-t", "=shared:", "sleep 600")
	pid, err := strconv.Atoi(adoptTestTmux(t, socket, "display-message", "-p", "-t", "=shared:0.0", "#{pane_pid}"))
	if err != nil {
		t.Fatal(err)
	}
	writeAdoptStatusFile(t, home, pid, "7b2e1d3f-0a8c-4f4b-9d6e-3e5c7f9b1d2a", home)
	if stdout, _, code := runAdopt(t, home, "session", "adopt", "shared", "--socket", socket, "--json"); code == 0 || !strings.Contains(stdout, "pane") {
		t.Fatalf("multi-pane adopt: %d %s", code, stdout)
	}
	if exec.Command("tmux", "-L", socket, "has-session", "-t", "=shared").Run() != nil {
		t.Fatal("refused adoption renamed the tmux session")
	}
}
