package session

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: the caller pane is explicit; no probe reads or writes terminal streams.
func TestPermanentDeleteTerminalProbeTargetsPane(t *testing.T) {
	target := &tmux.Session{Name: "delete-me", SocketName: "managed"}
	var calls int
	err := checkPermanentDeleteTerminal(target, "/tmp/caller.sock,42,0", "%13", func(socket string, args ...string) ([]byte, error) {
		calls++
		want := []string{"-S", "/tmp/caller.sock", "display-message", "-p", "-t", "%13", "#{socket_path}|#{session_name}"}
		if socket != "" || !reflect.DeepEqual(args, want) {
			t.Fatalf("caller probe (%q, %q), want empty named socket and %q", socket, args, want)
		}
		return []byte("/tmp/caller.sock|main-menu\n"), nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("outside target: calls=%d err=%v", calls, err)
	}
}

// matrix/hardened: equal names alone do not identify a terminal across servers.
func TestPermanentDeleteTerminalSameNameServers(t *testing.T) {
	root := t.TempDir()
	realSocket := filepath.Join(root, "real.sock")
	if err := os.WriteFile(realSocket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.sock")
	if err := os.Symlink(realSocket, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, callerSocket, targetSocket string
		blocked                          bool
	}{
		{"same", realSocket, realSocket, true},
		{"cleaned", filepath.Join(root, "missing") + "/../real.sock", realSocket, true},
		{"symlink", alias, realSocket, true},
		{"different", realSocket, filepath.Join(root, "other.sock"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			target := &tmux.Session{Name: "same-name", SocketName: "managed"}
			err := checkPermanentDeleteTerminal(target, tc.callerSocket+",42,0", "%13", func(socket string, args ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					return []byte(tc.callerSocket + "|same-name\n"), nil
				}
				want := []string{"display-message", "-p", "-t", "=same-name", "#{socket_path}|#{session_name}"}
				if calls != 2 || socket != "managed" || !reflect.DeepEqual(args, want) {
					t.Fatalf("target probe (%q, %q), want managed and %q", socket, args, want)
				}
				return []byte(tc.targetSocket + "|same-name\n"), nil
			})
			if (err != nil) != tc.blocked || calls != 2 {
				t.Fatalf("blocked=%v calls=%d err=%v", tc.blocked, calls, err)
			}
			if tc.blocked && !strings.Contains(err.Error(), "outside the target session") {
				t.Fatalf("wrong rejection: %v", err)
			}
		})
	}
}

// matrix/hardened: unknown caller/target identity fails closed before teardown.
func TestPermanentDeleteTerminalRefusesUnknownIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, env, pane, caller, target string
		failProbe                       int
		wantCalls                       int
	}{
		{"relative socket", "relative.sock,42,0", "%1", "", "", 0, 0},
		{"missing delimiter", "/tmp/caller.sock", "%1", "", "", 0, 0},
		{"empty socket", ",42,0", "%1", "", "", 0, 0},
		{"missing pane", "/tmp/caller.sock,42,0", "", "", "", 0, 0},
		{"invalid pane", "/tmp/caller.sock,42,0", "%1;2", "", "", 0, 0},
		{"caller probe failure", "/tmp/caller.sock,42,0", "%1", "", "", 1, 1},
		{"empty caller identity", "/tmp/caller.sock,42,0", "%1", "", "", 0, 1},
		{"multiple caller rows", "/tmp/caller.sock,42,0", "%1", "/tmp/caller.sock|same\n/tmp/caller.sock|other\n", "", 0, 1},
		{"unknown caller name", "/tmp/caller.sock,42,0", "%1", "/tmp/caller.sock|\n", "", 0, 1},
		{"changed caller socket", "/tmp/caller.sock,42,0", "%1", "/tmp/other.sock|different\n", "", 0, 1},
		{"target probe failure", "/tmp/caller.sock,42,0", "%1", "/tmp/caller.sock|same\n", "", 2, 2},
		{"malformed target", "/tmp/caller.sock,42,0", "%1", "/tmp/caller.sock|same\n", "relative|same\n", 0, 2},
		{"changed target name", "/tmp/caller.sock,42,0", "%1", "/tmp/caller.sock|same\n", "/tmp/other.sock|other\n", 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := checkPermanentDeleteTerminal(&tmux.Session{Name: "same", SocketName: "managed"}, tc.env, tc.pane, func(_ string, _ ...string) ([]byte, error) {
				calls++
				if tc.failProbe == calls {
					return nil, errors.New("probe unavailable")
				}
				if calls == 1 {
					return []byte(tc.caller), nil
				}
				return []byte(tc.target), nil
			})
			if err == nil || calls != tc.wantCalls {
				t.Fatalf("calls=%d, want %d; err=%v", calls, tc.wantCalls, err)
			}
		})
	}
}

func TestPermanentDeleteTerminalOutsideTmux(t *testing.T) {
	// matrix/hardened: a plain terminal must not require an unrelated tmux probe.
	err := checkPermanentDeleteTerminal(&tmux.Session{Name: "same"}, "", "", func(string, ...string) ([]byte, error) {
		t.Fatal("unexpected tmux probe")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPermanentDeleteTerminalBackendStopsBeforeMutation(t *testing.T) {
	// matrix/hardened: direct backend execution cannot bypass the caller guard.
	st, inst, _, transcript := permanentFixture(t, "codex")
	plan, err := st.PlanPermanentDelete(inst)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "/tmp/caller.sock,42,0")
	t.Setenv("TMUX_PANE", "")
	if err := st.PermanentlyDelete(inst, plan); err == nil || !strings.Contains(err.Error(), "TMUX_PANE") {
		t.Fatalf("missing caller pane was accepted: %v", err)
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("guard failure mutated transcript: %v", err)
	}
	instances, err := st.Load()
	if err != nil || len(instances) != 1 || instances[0].ID != inst.ID {
		t.Fatalf("guard failure mutated registry: instances=%v err=%v", instances, err)
	}
}
