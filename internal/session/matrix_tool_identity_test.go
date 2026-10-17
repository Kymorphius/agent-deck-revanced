package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: exercise the real status/save path with tmux reads served by
// a fixture executable. No test here opens or mutates a real tmux socket.
func matrixToolIdentityFixture(t *testing.T) {
	t.Helper()
	home := identityTestEnv(t)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = -u ]; then shift; fi
if [ "$1" = -L ] || [ "$1" = -S ]; then shift 2; fi
case "$1" in
has-session) exit 0;;
list-sessions) exit 0;;
list-panes) printf '0\n';;
capture-pane) printf 'ordinary shell output\n';;
display-message) printf '1\n';;
show-environment) exit 1;;
*) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmux.ResetSocketSessionCacheForTest()
	t.Cleanup(tmux.ResetSocketSessionCacheForTest)
}

func matrixIdentityInstance(t *testing.T, tool, observedCommand string) *Instance {
	t.Helper()
	inst := &Instance{
		ID:                  "matrix-identity-test",
		Title:               "tool identity",
		ProjectPath:         t.TempDir(),
		Command:             tool,
		Tool:                tool,
		Status:              StatusRunning,
		CreatedAt:           time.Now().Add(-time.Hour),
		lastSessionMetaSync: time.Now(),
	}
	inst.tmuxSession = tmux.ReconnectSessionLazy("agentdeck_matrix_tool_identity", inst.Title, inst.ProjectPath, observedCommand, "active")
	inst.tmuxSession.SocketName = "matrix-tool-identity-fixture"
	return inst
}

func TestMatrixUpdateStatusPreservesConfiguredTool(t *testing.T) {
	for _, tc := range []struct {
		name, configured, observed string
	}{
		{"codex with Claude foreground", "codex", "claude"},
		{"codex with shell foreground", "codex", "sh"},
		{"Claude with Codex foreground", "claude", "codex"},
		{"Claude with shell foreground", "claude", "sh"},
		{"Gemini with Claude foreground", "gemini", "claude"},
		{"OpenCode with Codex foreground", "opencode", "codex"},
		{"other builtin with Claude foreground", "copilot", "claude"},
		{"custom identity with Codex foreground", "my-agent", "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matrixToolIdentityFixture(t)
			inst := matrixIdentityInstance(t, tc.configured, tc.observed)
			storage := newTestStorage(t)
			if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
				t.Fatal(err)
			}
			wantObserved := tc.observed
			if wantObserved == "sh" {
				wantObserved = "shell"
			}
			if got := inst.tmuxSession.DetectTool(); got != wantObserved {
				t.Fatalf("fixture detected %q from %q", got, tc.observed)
			}
			if err := inst.UpdateStatus(); err != nil {
				t.Fatal(err)
			}
			if inst.Tool != tc.configured {
				t.Errorf("status observation changed configured tool from %q to %q", tc.configured, inst.Tool)
			}
			if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
				t.Fatal(err)
			}
			loaded, _, err := storage.LoadLite()
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != 1 {
				t.Fatalf("loaded %d instances, want 1", len(loaded))
			}
			if loaded[0].Tool != tc.configured {
				t.Fatalf("saved tool = %q, want configured %q", loaded[0].Tool, tc.configured)
			}
		})
	}
}

func TestMatrixUpdateStatusPromotesUnconfiguredTool(t *testing.T) {
	for _, configured := range []string{"", "shell"} {
		for _, observed := range []string{"claude", "codex", "gemini", "opencode", "sh"} {
			t.Run(configured+" to "+observed, func(t *testing.T) {
				matrixToolIdentityFixture(t)
				inst := matrixIdentityInstance(t, configured, observed)
				if err := inst.UpdateStatus(); err != nil {
					t.Fatal(err)
				}
				want := observed
				if observed == "sh" {
					want = "shell"
				}
				if inst.Tool != want {
					t.Fatalf("unconfigured %q tool was not promoted: got %q, want %q", configured, inst.Tool, want)
				}
			})
		}
	}
}

func TestMatrixGeneratedCodexLaunchKeepsIdentityAfterStatusSave(t *testing.T) {
	matrixToolIdentityFixture(t)
	inst := matrixIdentityInstance(t, "codex", "")
	inst.Title = "Codex reviewing Claude"
	command := inst.buildCodexCommand("codex")
	prepared, _, err := inst.prepareCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prepared, "developer_instructions") || !strings.Contains(strings.ToLower(prepared), "claude") {
		t.Fatalf("fixture must contain generated identity instructions naming Claude: %s", prepared)
	}
	inst.tmuxSession.Command = prepared
	inst.CodexSessionID = matrixCodexThread
	storage := newTestStorage(t)
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatal(err)
	}
	if err := inst.UpdateStatus(); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := storage.LoadLite()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d instances, want 1", len(loaded))
	}
	if got := loaded[0]; got.Tool != "codex" || got.Command != "codex" || got.CodexSessionID != matrixCodexThread {
		t.Fatalf("generated launch/status/save: tool=%q command=%q native=%q; want original Codex configuration", got.Tool, got.Command, got.CodexSessionID)
	}
}

func TestMatrixExplicitToolSwitchPersistsAfterStatusObservation(t *testing.T) {
	matrixToolIdentityFixture(t)
	inst := matrixIdentityInstance(t, "codex", "codex")
	storage := newTestStorage(t)
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetField(inst, FieldTool, "claude", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetField(inst, FieldCommand, "claude --model sonnet", nil); err != nil {
		t.Fatal(err)
	}
	// The pane is still Codex until the explicitly requested restart. A status
	// refresh must not undo the operator's newly selected launch configuration.
	if err := inst.UpdateStatus(); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := storage.LoadLite()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d instances, want 1", len(loaded))
	}
	if got := loaded[0]; got.Tool != "claude" || got.Command != "claude --model sonnet" {
		t.Fatalf("explicit switch saved tool=%q command=%q, want Claude and its selected command", got.Tool, got.Command)
	}
}
