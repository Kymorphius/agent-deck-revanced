package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic native documents only: these tests never read real transcripts,
// execute Codex, alter notify/configuration, or contact a live tmux server.
func codexHandoffFixtureRecords(instanceID, sessionID, cwd string, launched time.Time) []map[string]any {
	at := launched.Add(time.Second).Format(time.RFC3339Nano)
	identity := "# agent-deck session context\n" +
		"You are running inside agent-deck.\n\n" +
		"## This session (snapshot from the agent-deck database at launch)\n" +
		"- session id: " + instanceID + "\n- tool: codex\n\n" +
		"## agent-deck CLI (flags go BEFORE positional arguments)\n"
	return []map[string]any{
		{"timestamp": at, "type": "session_meta", "payload": map[string]any{"id": sessionID, "session_id": sessionID, "timestamp": at, "cwd": cwd, "source": "cli", "thread_source": "user"}},
		{"timestamp": at, "type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "01a1347b-0261-70d1-8489-d9d9d7a53d87"}},
		{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "developer", "content": []map[string]any{{"type": "input_text", "text": identity}}}},
	}
}

func encodeCodexHandoffFixture(t *testing.T, records []map[string]any) []byte {
	t.Helper()
	var data []byte
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	return data
}

func TestCodexHandoffReadiness_RejectsUnboundStaleAndForeignRecords(t *testing.T) {
	launched := time.Date(2026, 10, 13, 8, 57, 18, 500000000, time.UTC)
	const instanceID, sessionID, cwd = "switch-fixture", "01a1347b-0082-7742-af0b-f24cf3d9cce7", "/fixture/project"
	for _, tc := range []struct {
		name   string
		mutate func([]map[string]any) []map[string]any
		ready  bool
	}{
		{"exact native binding", func(r []map[string]any) []map[string]any { return r }, true},
		{"repeat exact developer injection after compaction", func(r []map[string]any) []map[string]any { return append(r, r[2]) }, true},
		{"stale header even with fresh mtime", func(r []map[string]any) []map[string]any {
			r[0]["timestamp"] = launched.Add(-time.Second).Format(time.RFC3339Nano)
			return r
		}, false},
		{"stale metadata creation", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["timestamp"] = launched.Add(-time.Second).Format(time.RFC3339Nano)
			return r
		}, false},
		{"future header", func(r []map[string]any) []map[string]any {
			r[0]["timestamp"] = launched.Add(time.Hour).Format(time.RFC3339Nano)
			return r
		}, false},
		{"wrong header id", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["id"] = "01a1347b-0082-7742-af0b-f24cf3d9cce8"
			return r
		}, false},
		{"conflicting session id", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["session_id"] = "01a1347b-0082-7742-af0b-f24cf3d9cce8"
			return r
		}, false},
		{"foreign cwd", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["cwd"] = "/fixture/other"
			return r
		}, false},
		{"relative cwd", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["cwd"] = "fixture/project"
			return r
		}, false},
		{"subagent", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["thread_source"] = "subagent"
			return r
		}, false},
		{"parent thread", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["parent_thread_id"] = sessionID
			return r
		}, false},
		{"forked thread", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["forked_from_id"] = sessionID
			return r
		}, false},
		{"exec source", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["source"] = "exec"
			return r
		}, false},
		{"structured subagent source", func(r []map[string]any) []map[string]any {
			r[0]["payload"].(map[string]any)["source"] = map[string]any{"subagent": "thread"}
			return r
		}, false},
		{"user quote cannot bind", func(r []map[string]any) []map[string]any { r[2]["payload"].(map[string]any)["role"] = "user"; return r }, false},
		{"tool quote cannot bind", func(r []map[string]any) []map[string]any {
			r[2]["payload"].(map[string]any)["type"] = "custom_tool_call_output"
			return r
		}, false},
		{"missing native task start", func(r []map[string]any) []map[string]any {
			r[1]["payload"].(map[string]any)["type"] = "task_complete"
			return r
		}, false},
		{"response item cannot start lifecycle", func(r []map[string]any) []map[string]any { r[1]["type"] = "response_item"; return r }, false},
		{"missing turn id", func(r []map[string]any) []map[string]any {
			delete(r[1]["payload"].(map[string]any), "turn_id")
			return r
		}, false},
		{"stale task start", func(r []map[string]any) []map[string]any {
			r[1]["timestamp"] = launched.Add(-time.Second).Format(time.RFC3339Nano)
			return r
		}, false},
		{"duplicate header", func(r []map[string]any) []map[string]any { return append(r, r[0]) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := tc.mutate(codexHandoffFixtureRecords(instanceID, sessionID, cwd, launched))
			err := validateFreshCodexHandoffRecords(context.Background(), encodeCodexHandoffFixture(t, records), instanceID, sessionID, cwd, launched, launched.Add(5*time.Second))
			if (err == nil) != tc.ready {
				t.Fatalf("ready=%v, err=%v; want ready=%v", err == nil, err, tc.ready)
			}
		})
	}
}

func TestCodexHandoffReadiness_IdentityMustComeFromExactSnapshotSection(t *testing.T) {
	identity := codexHandoffFixtureRecords("switch-fixture", "", "", time.Now())[2]["payload"].(map[string]any)["content"].([]map[string]any)[0]["text"].(string)
	for _, tc := range []struct {
		name, text string
		ready      bool
	}{
		{"exact section", identity, true},
		{"wrong instance", strings.Replace(identity, "switch-fixture", "switch-other", 1), false},
		{"wrong tool", strings.Replace(identity, "- tool: codex", "- tool: claude", 1), false},
		{"duplicate instance field", strings.Replace(identity, "- tool:", "- session id: switch-fixture\n- tool:", 1), false},
		{"duplicate tool field", strings.Replace(identity, "- tool: codex", "- tool: claude\n- tool: codex", 1), false},
		{"fields belong to different section", strings.Replace(identity, "- session id:", "## quoted context\n- session id:", 1), false},
		{"snapshot outside heading", "## This session (snapshot from the agent-deck database at launch)\n- session id: switch-fixture\n- tool: codex\n# agent-deck session context\n", false},
		{"duplicate heading", identity + identity, false},
		{"arbitrary prose", "example '# agent-deck session context' with - session id: switch-fixture and - tool: codex", false},
		{"quoted heading", "> " + identity, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ready := codexHandoffDeveloperBinding(tc.text, "switch-fixture")
			if ready != tc.ready {
				t.Fatalf("ready=%v; want %v", ready, tc.ready)
			}
		})
	}
}

func TestCodexHandoffReadiness_AppendsAreBoundedAndCancellationRejects(t *testing.T) {
	launched := time.Date(2026, 10, 13, 8, 57, 18, 0, time.UTC)
	const sid = "01a1347b-0082-7742-af0b-f24cf3d9cce7"
	valid := encodeCodexHandoffFixture(t, codexHandoffFixtureRecords("switch-fixture", sid, "/fixture/project", launched))
	for _, tc := range []struct {
		name  string
		data  []byte
		ready bool
	}{
		{"incomplete later append tolerated", append(append([]byte{}, valid...), []byte(`{"type":"event_msg"`)...), true},
		{"complete malformed record refused", append(append([]byte{}, valid...), []byte("{broken}\n")...), false},
		{"binding without durable newline refused", valid[:len(valid)-1], false},
		{"size bound", append(append([]byte{}, valid...), make([]byte, codexHandoffArtifactMaxBytes)...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFreshCodexHandoffRecords(context.Background(), tc.data, "switch-fixture", sid, "/fixture/project", launched, launched.Add(5*time.Second))
			if (err == nil) != tc.ready {
				t.Fatalf("ready=%v, err=%v; want %v", err == nil, err, tc.ready)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateFreshCodexHandoffRecords(ctx, valid, "switch-fixture", sid, "/fixture/project", launched, launched.Add(5*time.Second)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled parser: %v", err)
	}
}

func writeCodexHandoffArtifactFixture(t *testing.T, home, id string, target *Instance, launched time.Time) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "2026", "10", "13", "rollout-2026-10-13T01-57-18-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := encodeCodexHandoffFixture(t, codexHandoffFixtureRecords(target.ID, id, target.EffectiveWorkingDir(), launched))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, launched.Add(time.Second), launched.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexHandoffReadiness_ExactLiveProcessAccountAndArtifactRequired(t *testing.T) {
	launched := time.Date(2026, 10, 13, 8, 57, 18, 0, time.UTC)
	for _, tc := range []string{"exact live rollout", "no process", "incomplete process probe", "incomplete file probe", "ambiguous live thread", "foreign account rollout", "foreign account process with copied uuid", "duplicate exact artifact", "symlink file", "symlink date directory", "oversized artifact", "stale file", "thread changes during read"} {
		t.Run(tc, func(t *testing.T) {
			target, home := newCodexGateInstance(t)
			id := uniqueSID(t)
			path := writeCodexHandoffArtifactFixture(t, home, id, target, launched)
			lock := filepath.Join(home, "thread-writer-locks", id+".lock")
			stubCodexPaneProcessPIDs(t, []int{4242}, nil)
			stubCodexPaneOpenPaths(t, []string{lock, path}, nil)
			switch tc {
			case "no process":
				stubCodexPaneProcessPIDs(t, nil, nil)
			case "incomplete process probe":
				stubCodexPaneProcessPIDs(t, []int{4242}, errors.New("partial"))
			case "incomplete file probe":
				stubCodexPaneOpenPaths(t, []string{lock, path}, errors.New("partial"))
			case "ambiguous live thread":
				stubCodexPaneOpenPaths(t, []string{lock, filepath.Join(home, "thread-writer-locks", uniqueSID(t)+".lock")}, nil)
			case "foreign account rollout":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				foreign := writeCodexHandoffArtifactFixture(t, t.TempDir(), id, target, launched)
				stubCodexPaneOpenPaths(t, []string{lock, foreign}, nil)
			case "foreign account process with copied uuid":
				foreign := writeCodexHandoffArtifactFixture(t, t.TempDir(), id, target, launched)
				stubCodexPaneOpenPaths(t, []string{foreign}, nil)
			case "duplicate exact artifact":
				duplicate := filepath.Join(filepath.Dir(path), "rollout-duplicate-"+id+".jsonl")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(duplicate, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink file":
				source := filepath.Join(t.TempDir(), "original.jsonl")
				if err := os.Rename(path, source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(source, path); err != nil {
					t.Fatal(err)
				}
			case "symlink date directory":
				dir := filepath.Dir(path)
				moved := filepath.Join(t.TempDir(), "date")
				if err := os.Rename(dir, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, dir); err != nil {
					t.Fatal(err)
				}
			case "oversized artifact":
				if err := os.Truncate(path, codexHandoffArtifactMaxBytes+1); err != nil {
					t.Fatal(err)
				}
			case "stale file":
				if err := os.Chtimes(path, launched.Add(-time.Second), launched.Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
			case "thread changes during read":
				calls := 0
				other := uniqueSID(t)
				codexPaneOpenPaths = func(*Instance) ([]string, error) {
					calls++
					if calls <= 2 {
						return []string{lock, path}, nil
					}
					return []string{filepath.Join(home, "thread-writer-locks", other+".lock")}, nil
				}
			}
			expected := FreshTargetIdentity{InstanceID: target.ID, Tool: "codex", ProjectPath: target.EffectiveWorkingDir()}
			artifact, err := readFreshCodexHandoffArtifact(context.Background(), target, expected, launched, launched.Add(5*time.Second))
			if tc == "exact live rollout" {
				if err != nil || artifact.SessionID != id || artifact.Path != path || !artifact.Ready {
					t.Fatalf("artifact=%#v, err=%v", artifact, err)
				}
			} else if err == nil || artifact.Ready {
				t.Fatalf("unsafe artifact=%#v, err=%v", artifact, err)
			}
		})
	}
}

func TestNativeCrossHarnessObserver_CodexCustomNotifyFallbackRestoresLaunchBaseline(t *testing.T) {
	target, home := newCodexGateInstance(t)
	launched := time.Date(2026, 10, 13, 8, 57, 18, 500000000, time.UTC)
	id := uniqueSID(t)
	path := writeCodexHandoffArtifactFixture(t, home, id, target, launched)
	stubCodexPaneProcessPIDs(t, []int{4242}, nil)
	stubCodexPaneOpenPaths(t, []string{path, filepath.Join(home, "thread-writer-locks", id+".lock")}, nil)
	configPath := filepath.Join(home, "config.toml")
	customNotify := []byte("notify = [\"/fixture/user-custom-notify\"]\n")
	if err := os.WriteFile(configPath, customNotify, 0o600); err != nil {
		t.Fatal(err)
	}
	// Recovery uses the durable pre-launch lower bound, even if mutable
	// LastStartedAt was updated later than the native creation event.
	target.LastStartedAt = launched.Add(time.Minute)
	observer := &NativeCrossHarnessTargetObserver{
		Reader: CrossHarnessTargetEvidenceReader{
			ProcessAlive:   func(*Instance) (bool, error) { return true, nil },
			ReadHookStatus: func(string) (*HookStatus, time.Time, error) { return nil, time.Time{}, errors.New("custom notify") },
		},
		Now:          func() time.Time { return launched.Add(5 * time.Second) },
		PollInterval: time.Millisecond,
	}
	observer.RestoreTargetObservation(target, crossHarnessObservationBaseline{StartedAt: launched})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := observer.ObserveTarget(ctx, target, FreshTargetIdentity{InstanceID: target.ID, Tool: "codex", ProjectPath: target.EffectiveWorkingDir()})
	if err != nil || !got.Ready || got.SessionID != id || got.ArtifactPath != path || got.Evidence != "codex-native-session" {
		t.Fatalf("custom-notify recovered readiness=%#v, err=%v", got, err)
	}
	if data, err := os.ReadFile(configPath); err != nil || string(data) != string(customNotify) {
		t.Fatalf("custom notify changed: data=%q, err=%v", data, err)
	}
}
