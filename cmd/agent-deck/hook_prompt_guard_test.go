package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/testutil"
)

// matrix/hardened: cmd test binaries resolve only their isolated HOME; this
// initializer is never compiled into the installed CLI.
func init() { hookPromptGuardHome = os.UserHomeDir }

const matrixPromptGuardUUID = "11111111-2222-4333-8444-555555555555"

// matrix/hardened: fake helper records only synthetic UUID arguments and
// whether stdin/account/project environment leaked across the boundary.
func matrixHookPromptFixture(t *testing.T, body string) (home, calls string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "AGENT_DECK_HOME", "AGENT_DECK_PROFILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "account-config"))
	t.Setenv("AGENTDECK_INSTANCE_ID", "prompt-guard-parent")
	t.Setenv("AGENTDECK_NO_CHILDREN_CONTEXT", "1")
	t.Setenv("AGENTDECK_TOOL", "claude")
	t.Cleanup(session.ClearUserConfigCache)
	session.ClearUserConfigCache()
	dir := filepath.Join(home, ".local", "lib", "claude-launch-guard")
	bin := filepath.Join(home, ".local", "bin")
	for _, p := range []string{dir, bin} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "messages-enabled.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls = filepath.Join(home, "prompt-guard-calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellescape.Quote(calls) + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude-launch-guard"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return home, calls
}

func matrixPromptResponse(allowed bool, reason string, code int) string {
	return "printf '%s\\n' " + shellescape.Quote(fmt.Sprintf(`{"allowed":%t,"reason":%q,"period_seconds":60,"retry_after":3600}`, allowed, reason)) + fmt.Sprintf("\nexit %d", code)
}

func matrixPromptPayload(event, cwd string) string {
	p, _ := json.Marshal(map[string]string{"hook_event_name": event, "session_id": matrixPromptGuardUUID, "cwd": cwd, "prompt": "synthetic private prompt $(not-run)"})
	return string(p)
}

func matrixRunPromptHook(t *testing.T, payload string) (code int, stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	in, err := os.Create(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = in.WriteString(payload)
	_, _ = in.Seek(0, 0)
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errout, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = in, out, errout
	defer func() {
		os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
		_ = in.Close()
		_ = out.Close()
		_ = errout.Close()
	}()
	code = handleHookHandlerExitCode()
	outData, _ := os.ReadFile(out.Name())
	errData, _ := os.ReadFile(errout.Name())
	return code, string(outData), string(errData)
}

func matrixSeedPromptInbox(t *testing.T, id string) []byte {
	t.Helper()
	if err := session.WriteInboxEvent(id, session.TransitionNotificationEvent{
		ChildSessionID: "synthetic-child", ChildTitle: "synthetic child",
		FromStatus: "running", ToStatus: "waiting", Timestamp: time.Now(),
		Kind: "finished", DoneStatus: "ok", DoneSummary: "synthetic preserved inbox",
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(session.InboxPathFor(id))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMatrixHookPromptGuardDenialPreservesInboxAndStatus(t *testing.T) {
	for _, reason := range []string{"periodic_messages", "periodic_message_cooldown", "cooldown"} {
		t.Run(reason, func(t *testing.T) {
			_, calls := matrixHookPromptFixture(t, matrixPromptResponse(false, reason, 75))
			t.Cleanup(session.SetCommsLedgerForTest(true))
			const id = "prompt-guard-parent"
			before := matrixSeedPromptInbox(t, id)
			code, stdout, stderr := matrixRunPromptHook(t, matrixPromptPayload("UserPromptSubmit", ""))
			if code != 2 || stdout != "" || !strings.Contains(stderr, "已阻止本条消息") || !strings.Contains(stderr, "3600") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if reason == "periodic_message_cooldown" && !strings.Contains(stderr, "仍处于周期消息保护冷却期") {
				t.Fatal("helper's actual cooldown reason did not produce the cooldown explanation")
			}
			after, err := os.ReadFile(session.InboxPathFor(id))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("blocked prompt consumed durable inbox")
			}
			if _, err := os.Stat(filepath.Join(getHooksDir(), id+".json")); !os.IsNotExist(err) {
				t.Fatal("blocked prompt wrote running status")
			}
			if entries, err := session.ReadCommsSpool(id); err != nil || len(entries) != 0 {
				t.Fatalf("blocked prompt spooled content: %v %v", entries, err)
			}
			args, err := os.ReadFile(calls)
			want := "prompt-check\n--session-id\n" + matrixPromptGuardUUID + "\n--json\n"
			if err != nil || string(args) != want {
				t.Fatalf("helper received unexpected payload: %q %v", args, err)
			}
		})
	}
}

func TestMatrixHookPromptGuardAllowedPreservesOriginalContext(t *testing.T) {
	_, _ = matrixHookPromptFixture(t, matrixPromptResponse(true, "", 0))
	t.Cleanup(session.SetCommsLedgerForTest(false))
	matrixSeedPromptInbox(t, "prompt-guard-parent")
	code, stdout, stderr := matrixRunPromptHook(t, matrixPromptPayload("UserPromptSubmit", ""))
	if code != 0 || stderr != "" || !strings.Contains(stdout, "synthetic preserved inbox") {
		t.Fatalf("allowed prompt lost context: %d %q %q", code, stdout, stderr)
	}
	var out struct {
		HookSpecificOutput struct {
			Event string `json:"hookEventName"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(stdout), &out) != nil || out.HookSpecificOutput.Event != "UserPromptSubmit" || session.InboxHasPending("prompt-guard-parent") {
		t.Fatal("allowed prompt did not retain normal context/inbox handling")
	}
}

func TestMatrixHookPromptGuardCannotBypassThroughEarlyReturns(t *testing.T) {
	for _, entry := range []string{"missing-instance", "invalid-instance", "missing-cwd"} {
		t.Run(entry, func(t *testing.T) {
			home, calls := matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
			cwd := ""
			switch entry {
			case "missing-instance":
				t.Setenv("AGENTDECK_INSTANCE_ID", "")
			case "invalid-instance":
				t.Setenv("AGENTDECK_INSTANCE_ID", "../../escape")
			case "missing-cwd":
				cwd = filepath.Join(home, "missing-project")
			}
			code, stdout, _ := matrixRunPromptHook(t, matrixPromptPayload("UserPromptSubmit", cwd))
			if code != 2 || stdout != "" {
				t.Fatal("entry fast return bypassed enabled prompt guard")
			}
			if _, err := os.Stat(calls); err != nil {
				t.Fatal("guard did not run before entry fast return")
			}
		})
	}
}

func TestMatrixHookPromptGuardExactClaudeEventOnly(t *testing.T) {
	for _, payload := range []string{
		`{"hook_event_name":"Stop","session_id":"s"}`,
		`{"hook_event_name":"BeforeAgent","session_id":"s"}`,
		`{"hook_event_name":"beforeSubmitPrompt","conversation_id":"c"}`,
		`{"hook_event_name":"userpromptsubmit","session_id":"s"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"s","turn_id":"codex-turn"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"s","turn_id":null}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"s","turn_id":""}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"s","conversation_id":"cursor-conversation"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			_, calls := matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
			t.Setenv("AGENTDECK_INSTANCE_ID", "")
			code, stdout, stderr := matrixRunPromptHook(t, payload)
			if code != 0 || stdout != "" || stderr != "" {
				t.Fatalf("non-Claude prompt blocked: %d %q %q", code, stdout, stderr)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatal("non-Claude event counted by prompt guard")
			}
		})
	}
}

func TestMatrixHookPromptGuardDoesNotTrustInheritedToolOrSource(t *testing.T) {
	_, _ = matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
	t.Setenv("AGENTDECK_TOOL", "codex")
	code, _, _ := matrixRunPromptHook(t, `{"hook_event_name":"UserPromptSubmit","session_id":"`+matrixPromptGuardUUID+`","source":"task"}`)
	if code != 2 {
		t.Fatal("inherited tool or lifecycle source bypassed Claude prompt guard")
	}
}

func TestMatrixHookPromptGuardOnlyUUIDReachesHelper(t *testing.T) {
	body := "if IFS= read -r input; then exit 78; fi\nif [ -n \"$CLAUDE_CONFIG_DIR$AGENTDECK_INSTANCE_ID$AGENTDECK_TOOL\" ]; then exit 78; fi\n" + matrixPromptResponse(true, "", 0)
	_, calls := matrixHookPromptFixture(t, body)
	t.Setenv("AGENTDECK_INSTANCE_ID", "")
	code, stdout, stderr := matrixRunPromptHook(t, matrixPromptPayload("UserPromptSubmit", "/synthetic/private/path"))
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("helper inherited submitted/environment data: %d %q %q", code, stdout, stderr)
	}
	args, _ := os.ReadFile(calls)
	if string(args) != "prompt-check\n--session-id\n"+matrixPromptGuardUUID+"\n--json\n" {
		t.Fatalf("unexpected helper arguments: %q", args)
	}
}

func TestMatrixHookPromptGuardHelperFailuresBlock(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid-json", "printf '%s' '{'"},
		{"allowed-exit-mismatch", matrixPromptResponse(true, "", 78)},
		{"denied-exit-mismatch", matrixPromptResponse(false, "periodic_messages", 0)},
		{"missing-allowed", "printf '%s' '{\"reason\":\"cooldown\"}'; exit 75"},
		{"unknown-reason", matrixPromptResponse(false, "secret-reason", 75)},
		{"oversize", "dd if=/dev/zero bs=20000 count=1 2>/dev/null"},
		{"timeout", "exec sleep 30"},
		{"missing-helper", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := matrixHookPromptFixture(t, tc.body)
			if tc.name == "missing-helper" {
				_ = os.Remove(filepath.Join(home, ".local", "bin", "claude-launch-guard"))
			}
			started := time.Now()
			code, stdout, stderr := matrixRunPromptHook(t, matrixPromptPayload("UserPromptSubmit", ""))
			if code != 2 || stdout != "" || !strings.Contains(stderr, "保护检查失败") || strings.Contains(stderr, "secret-reason") {
				t.Fatalf("helper error did not fail safely: %d %q %q", code, stdout, stderr)
			}
			if elapsed := time.Since(started); elapsed > 8*time.Second {
				t.Fatalf("helper error exceeded timeout bound: %v", elapsed)
			}
		})
	}
}

func TestMatrixHookPromptGuardMarkerAndHomeBoundaries(t *testing.T) {
	for _, mode := range []string{"absent", "symlink", "writable", "different-HOME", "invalid-session"} {
		t.Run(mode, func(t *testing.T) {
			home, calls := matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
			marker := filepath.Join(home, ".local", "lib", "claude-launch-guard", "messages-enabled.json")
			payload := matrixPromptPayload("UserPromptSubmit", "")
			switch mode {
			case "absent":
				_ = os.Remove(marker)
				t.Setenv("AGENTDECK_INSTANCE_ID", "")
			case "symlink":
				_ = os.Remove(marker)
				if err := os.Symlink(filepath.Join(home, "not-present"), marker); err != nil {
					t.Fatal(err)
				}
			case "writable":
				_ = os.Chmod(marker, 0o666)
			case "different-HOME":
				old := hookPromptGuardHome
				hookPromptGuardHome = func() (string, error) { return home, nil }
				t.Cleanup(func() { hookPromptGuardHome = old })
				t.Setenv("HOME", t.TempDir())
			case "invalid-session":
				payload = `{"hook_event_name":"UserPromptSubmit","session_id":"synthetic prompt pretending to be ID"}`
			}
			code, stdout, _ := matrixRunPromptHook(t, payload)
			if mode == "absent" {
				if code != 0 {
					t.Fatal("absent marker changed portable behavior")
				}
			} else if code != 2 || stdout != "" {
				t.Fatal("unsafe guard metadata bypassed blocking")
			}
			if mode != "different-HOME" {
				if _, err := os.Stat(calls); !os.IsNotExist(err) {
					t.Fatal("absent/unsafe metadata reached helper")
				}
			}
			if mode == "symlink" || mode == "writable" {
				t.Setenv("AGENTDECK_INSTANCE_ID", "")
				if code, _, stderr := matrixRunPromptHook(t, matrixPromptPayload("Stop", "")); code != 0 || stderr != "" {
					t.Fatal("unsafe marker blocked a non-prompt hook")
				}
			}
		})
	}
}

// matrix/hardened: the subprocess invokes the actual main dispatcher with the
// test-only HOME resolver, proving only the CLI converts rejection to exit 2.
func TestMatrixHookPromptGuardCLIHelper(t *testing.T) {
	if os.Getenv("MATRIX_PROMPT_GUARD_CLI_HELPER") != "1" {
		return
	}
	// The intentional CLI os.Exit bypasses TestMain defers. This hook-only
	// subprocess needs no tmux; remove its own private fixture before exit.
	if dir := os.Getenv("TMUX_TMPDIR"); strings.HasPrefix(filepath.Base(dir), "ad-tmux-") {
		testutil.KillTmuxServersUnder(dir)
		_ = os.RemoveAll(dir)
		_ = os.Setenv("TMUX_TMPDIR", "/dev/null/agent-deck-prompt-hook-test")
	}
	os.Args = []string{"agent-deck", "hook-handler"}
	main()
}

func TestMatrixHookPromptGuardHomeLookupFailureDoesNotOptIn(t *testing.T) {
	_, calls := matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
	old := hookPromptGuardHome
	hookPromptGuardHome = func() (string, error) { return "", fmt.Errorf("synthetic unavailable passwd home") }
	t.Cleanup(func() { hookPromptGuardHome = old })
	t.Setenv("AGENTDECK_INSTANCE_ID", "")
	code, stdout, stderr := matrixRunPromptHook(t, matrixPromptPayload("UserPromptSubmit", ""))
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatal("passwd-home failure activated protection without established host opt-in")
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("home lookup failure reached the guard helper")
	}
}

func TestMatrixHookPromptGuardCLIBlocksExitTwo(t *testing.T) {
	_, _ = matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestMatrixHookPromptGuardCLIHelper$")
	cmd.Env = append(os.Environ(), "MATRIX_PROMPT_GUARD_CLI_HELPER=1", "AGENT_DECK_TASK6_HELPER_PROCESS=1")
	cmd.Stdin = strings.NewReader(matrixPromptPayload("UserPromptSubmit", ""))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "已阻止本条消息") {
		t.Fatalf("actual CLI did not block: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

// matrix/hardened: big valid prompts cannot bypass admission by overflowing
// the original status handler's payload cap; no body is sent to the helper.
func TestMatrixHookPromptGuardLargePayloadStillChecked(t *testing.T) {
	big, _ := json.Marshal(strings.Repeat("汉字 \\\"", maxHookPayloadSize/4))
	metadata := `"hook_event_name":"UserPromptSubmit","session_id":"` + matrixPromptGuardUUID + `"`
	for _, tc := range []struct{ name, payload string }{
		{"prompt-before-metadata", `{"prompt":` + string(big) + `,` + metadata + `}`},
		{"prompt-after-metadata", `{` + metadata + `,"prompt":` + string(big) + `}`},
		{"unknown-nested-before-metadata", `{"nested":[{"hook_event_name":"Stop","session_id":"wrong","value":` + string(big) + `},true,false,null,0,-12.34e+5],` + metadata + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, calls := matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
			code, stdout, stderr := matrixRunPromptHook(t, tc.payload)
			if code != 2 || stdout != "" || !strings.Contains(stderr, "约每 60 秒") {
				t.Fatalf("large prompt bypassed guard: %d %q %q", code, stdout, stderr)
			}
			args, err := os.ReadFile(calls)
			if err != nil || string(args) != "prompt-check\n--session-id\n"+matrixPromptGuardUUID+"\n--json\n" {
				t.Fatal("large prompt did not use UUID-only admission")
			}
		})
	}
	t.Run("allowed-retains-legacy-softskip", func(t *testing.T) {
		_, calls := matrixHookPromptFixture(t, matrixPromptResponse(true, "", 0))
		before := matrixSeedPromptInbox(t, "prompt-guard-parent")
		code, stdout, stderr := matrixRunPromptHook(t, `{`+metadata+`,"prompt":`+string(big)+`}`)
		after, _ := os.ReadFile(session.InboxPathFor("prompt-guard-parent"))
		if code != 0 || stdout != "" || stderr != "" || !bytes.Equal(before, after) {
			t.Fatal("allowed large payload changed original oversized soft-skip behavior")
		}
		if _, err := os.Stat(calls); err != nil {
			t.Fatal("allowed large prompt skipped admission")
		}
	})
	t.Run("codex-turn-id-after-large-prompt", func(t *testing.T) {
		_, calls := matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
		code, stdout, stderr := matrixRunPromptHook(t, `{`+metadata+`,"prompt":`+string(big)+`,"turn_id":null}`)
		if code != 0 || stdout != "" || stderr != "" {
			t.Fatal("large Codex payload was counted as Claude")
		}
		if _, err := os.Stat(calls); !os.IsNotExist(err) {
			t.Fatal("Codex null turn_id reached message admission")
		}
	})
}

func TestMatrixHookPromptGuardStreamingMetadataSemantics(t *testing.T) {
	for _, input := range []string{
		`{"hook_event_name":"Stop","hook_event_name":"UserPromptSubmit","session_id":"wrong","session_id":"` + matrixPromptGuardUUID + `"}`,
		`{"HOOK_EVENT_NAME":"UserPromptSubmit","SESSION_ID":"` + matrixPromptGuardUUID + `","hook_event_name":null,"session_id":null}`,
		`{"hook\u005fevent_name":"UserPromptSubmit","session_id":"\u00311111111-2222-4333-8444-555555555555","prompt":"转义\"反斜杠\\和换行\n"}`,
		`{"nested":{"hook_event_name":"Stop","turn_id":null},"hook_event_name":"UserPromptSubmit","session_id":"` + matrixPromptGuardUUID + `"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"` + matrixPromptGuardUUID + `","turn_id":{}}`,
		`{"hook_event_name":"UserPromptSubmit","conversation_id":"cursor","conversation_id":null}`,
		`{"hook_event_name":"UserPromptSubmit","hook_event_name":"Stop"}`,
		`{"unknown":[[],{},-0,0.1,12e-2,1E+3,true,false,null,"\uabcd"],"hook_event_name":"Stop"}`,
	} {
		metadata, prefix, err := readHookPromptMetadata(strings.NewReader(input))
		var full hookPayload
		if err != nil || json.Unmarshal([]byte(input), &full) != nil || string(prefix) != input {
			t.Fatalf("valid metadata scan failed: %s: %v", input, err)
		}
		if metadata.HookEventName != full.HookEventName || metadata.SessionID != full.SessionID || metadata.ConversationID != full.ConversationID || (metadata.TurnID == nil) != (full.TurnID == nil) {
			t.Fatalf("metadata scan differs from original decoder: %+v vs %+v", metadata, full)
		}
	}
}

func TestMatrixHookPromptGuardMalformedKnownPromptBlocks(t *testing.T) {
	metadata := `"hook_event_name":"UserPromptSubmit","session_id":"` + matrixPromptGuardUUID + `"`
	for _, tail := range []string{
		`,"prompt":"unterminated`,
		`,"prompt":"bad\q"}`,
		`,"prompt":"bad\u1z23"}`,
		`,"unknown":01}`,
		`,"unknown":1.}`,
		`,"unknown":1e+}`,
		`,"unknown":[true,]}`,
		`,"unknown":{"x":false,}}`,
		`,"unknown":nul}`,
		`} {"second":true}`,
		`,"session_id":123}`,
		`,"session_id":"` + strings.Repeat("a", 2048) + `"}`,
		`,"unknown":` + strings.Repeat("[", 514) + `0` + strings.Repeat("]", 514) + `}`,
	} {
		t.Run(tail[:min(len(tail), 40)], func(t *testing.T) {
			_, calls := matrixHookPromptFixture(t, matrixPromptResponse(true, "", 0))
			code, stdout, stderr := matrixRunPromptHook(t, `{`+metadata+tail)
			if code != 2 || stdout != "" || !strings.Contains(stderr, "钩子数据无效") {
				t.Fatalf("malformed known prompt bypassed guard: %d %q %q", code, stdout, stderr)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatal("malformed payload counted as a message")
			}
		})
	}
	t.Run("other-event-softskip", func(t *testing.T) {
		_, calls := matrixHookPromptFixture(t, matrixPromptResponse(false, "periodic_messages", 75))
		if code, _, stderr := matrixRunPromptHook(t, `{"hook_event_name":"Stop","unknown":[}`); code != 0 || stderr != "" {
			t.Fatal("malformed non-prompt event changed original soft-failure behavior")
		}
		if _, err := os.Stat(calls); !os.IsNotExist(err) {
			t.Fatal("malformed non-prompt was counted")
		}
	})
}
