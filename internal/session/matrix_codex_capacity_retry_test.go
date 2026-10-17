package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// matrix/hardened: actual native task_complete error shape, not quoted chat.
func TestMatrixCodexCapacityRecord(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	makeRecord := func(code, message string) string {
		data, _ := json.Marshal(map[string]any{"timestamp": at, "type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": "turn-1", "error": map[string]any{"codex_error_info": code, "message": message}}})
		return string(data)
	}
	capacity := makeRecord("server_overloaded", "Selected model is at capacity. Please try a different model.")
	for _, tc := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{"native capacity", []string{cxMeta, capacity}, true},
		{"quoted assistant text", []string{cxMeta, `{"type":"response_item","payload":{"role":"assistant","content":[{"text":"Selected model is at capacity. Please try a different model."}]}}`, cxDone}, false},
		{"other overload", []string{cxMeta, makeRecord("server_overloaded", "server unavailable")}, false},
		{"other error", []string{cxMeta, makeRecord("usage_limit", "Selected model is at capacity.")}, false},
		{"user continued", []string{cxMeta, capacity, `{"type":"event_msg","payload":{"type":"user_message","message":"continue"}}`}, false},
		{"new turn started", []string{cxMeta, capacity, cxStarted}, false},
		{"successful turn", []string{cxMeta, capacity, cxDone}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, found := matrixLatestCodexTurnInterrupt(matrixWriteTranscript(t, tc.lines...))
			if found != tc.want {
				t.Fatalf("found=%v", found)
			}
			if found && (v.TurnID != "turn-1" || !v.At.Equal(at) || v.Kind != "server_overloaded") {
				t.Fatalf("verdict=%+v", v)
			}
		})
	}
	// A subagent's failed task must not become a root-session retry candidate.
	cache := newMatrixTurnInterruptFileCache(8)
	path := matrixWriteTranscript(t, cxSubMeta, capacity)
	result := cache.load("codex", path)
	if result.meta.ThreadSource != "subagent" && result.meta.ParentThreadID == "" {
		t.Fatal("subagent scope lost")
	}
}

func TestMatrixCapacityRetryBackoffAndBounds(t *testing.T) {
	now := time.Now().UTC()
	s := matrixCapacityRetryState{}
	for i, delay := range matrixCapacityRetryDelays {
		v := matrixTurnInterrupt{Kind: "server_overloaded", Message: "Selected model is at capacity.", TurnID: fmt.Sprint(i), At: now}
		_, due, err := s.plan(v, now)
		if due || err != nil {
			t.Fatal("no initial wait")
		}
		if s.Due.Sub(now) != delay {
			t.Fatalf("delay=%v", s.Due.Sub(now))
		}
		_, due, _ = s.plan(v, now.Add(delay-time.Second))
		if due {
			t.Fatal("early retry")
		}
		_, due, _ = s.plan(v, now.Add(delay))
		if !due {
			t.Fatal("due retry missing")
		}
		s.Attempts++
		s.Claimed = v.TurnID
		s.Pending = ""
		_, due, _ = s.plan(v, now.Add(delay))
		if due {
			t.Fatal("same failure replayed")
		}
		now = now.Add(delay + time.Second)
	}
	v := matrixTurnInterrupt{Kind: "server_overloaded", Message: "Selected model is at capacity.", TurnID: "fourth", At: now}
	detail, due, _ := s.plan(v, now)
	if due || !strings.Contains(detail, "exhausted") {
		t.Fatal("budget reset across repeated failures")
	}
	v.At = now.Add(-6 * time.Minute)
	detail, due, _ = s.plan(v, now)
	if due || detail != "" {
		t.Fatal("stale history retried")
	}
}

const matrixCapacityIdleFrame = "⚠ Selected model is at capacity. Please try a different model.\n\n› \n\n  ? for shortcuts            100% context left"

type matrixCapacityFakeTarget struct {
	raw    string
	err    error
	sends  atomic.Int32
	path   string
	turn   string
	t      *testing.T
	marker string
}

func (f *matrixCapacityFakeTarget) CapturePaneFresh() (string, error) { return f.raw, nil }
func (f *matrixCapacityFakeTarget) SendKeysAndEnter(message string) error {
	if message != matrixCapacityRetryPrompt {
		f.t.Error("unexpected input")
	}
	state, err := matrixReadCapacityRetryState(f.path)
	if err != nil || state.Claimed != f.turn || state.Attempts < 1 {
		f.t.Error("send preceded durable claim")
	}
	f.sends.Add(1)
	if f.marker != "" {
		file, err := os.OpenFile(f.marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = file.WriteString("sent\n")
		file.Close()
		if err != nil {
			return err
		}
	}
	return f.err
}
func matrixDueCapacityFixture(t *testing.T) (string, matrixTurnInterrupt, time.Time, *matrixCapacityFakeTarget) {
	t.Helper()
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "retry.json")
	v := matrixTurnInterrupt{Kind: "server_overloaded", Message: "Selected model is at capacity.", TurnID: "failure", At: now.Add(-time.Minute)}
	s := matrixCapacityRetryState{WindowStart: now.Add(-time.Minute), Pending: v.TurnID, Due: now.Add(-time.Second)}
	if err := matrixWriteCapacityRetryState(path, s); err != nil {
		t.Fatal(err)
	}
	return path, v, now, &matrixCapacityFakeTarget{raw: matrixCapacityIdleFrame, path: path, turn: v.TurnID, t: t}
}
func TestMatrixCapacityRetryClaimAndUncertainDelivery(t *testing.T) {
	path, v, now, target := matrixDueCapacityFixture(t)
	target.err = errors.New("uncertain send")
	detail, err := matrixRunCapacityRetry(path, v, now, target, func() bool { return true })
	if err == nil || !strings.Contains(detail, "uncertain") || target.sends.Load() != 1 {
		t.Fatal(detail, err)
	}
	// New process/Instance simulation: reload the same durable claim.
	matrixRunCapacityRetry(path, v, now.Add(time.Second), target, func() bool { return true })
	if target.sends.Load() != 1 {
		t.Fatal("uncertain delivery was repeated")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("state is not private")
	}
}
func TestMatrixCapacityRetryGuards(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"draft", "› operator draft\n\n  ? for shortcuts 100% context left", true},
		{"working", "• Working (0s • esc to interrupt)\n\n› \n\n  ? for shortcuts 100% context left", true},
		{"no composer", "blank", true},
		{"changed turn", matrixCapacityIdleFrame, false},
		{"approval", "Would you like to run this command?\n› 1. Yes\n  2. No", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, v, now, target := matrixDueCapacityFixture(t)
			target.raw = tc.raw
			matrixRunCapacityRetry(path, v, now, target, func() bool { return tc.valid })
			if target.sends.Load() != 0 {
				t.Fatal("guard sent input")
			}
		})
	}
	path, v, now, target := matrixDueCapacityFixture(t)
	calls := 0
	matrixRunCapacityRetry(path, v, now, target, func() bool { calls++; return calls < 3 })
	if target.sends.Load() != 0 {
		t.Fatal("post-claim turn change sent input")
	}
}
func TestMatrixCapacityRetryConcurrentOwners(t *testing.T) {
	path, v, now, target := matrixDueCapacityFixture(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); matrixRunCapacityRetry(path, v, now, target, func() bool { return true }) }()
	}
	wg.Wait()
	if target.sends.Load() != 1 {
		t.Fatalf("sends=%d", target.sends.Load())
	}
}
func TestMatrixCapacityRetryOptInAndCorruptState(t *testing.T) {
	cfg := &UserConfig{}
	if cfg.Codex.CapacityRetry {
		t.Fatal("enabled by default")
	}
	path, v, now, target := matrixDueCapacityFixture(t)
	os.WriteFile(path, []byte("bad state"), 0600)
	_, err := matrixRunCapacityRetry(path, v, now, target, func() bool { return true })
	if err == nil || target.sends.Load() != 0 {
		t.Fatal("corrupt state enabled retry")
	}
}

// matrix/hardened: separate processes exercise the actual sibling-file flock.
func TestMatrixCapacityRetryProcessHelper(t *testing.T) {
	if os.Getenv("AD_CAPACITY_HELPER") != "1" {
		return
	}
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("AD_CAPACITY_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("AD_CAPACITY_PATH")
	v := matrixTurnInterrupt{Kind: "server_overloaded", Message: "Selected model is at capacity.", TurnID: "failure", At: now.Add(-time.Minute)}
	target := &matrixCapacityFakeTarget{raw: matrixCapacityIdleFrame, path: path, turn: v.TurnID, t: t, marker: path + ".sent"}
	if _, err := matrixRunCapacityRetry(path, v, now, target, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
}
func TestMatrixCapacityRetryAcrossProcesses(t *testing.T) {
	path, _, now, _ := matrixDueCapacityFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var children []*exec.Cmd
	for range 3 {
		cmd := exec.Command(exe, "-test.run=^TestMatrixCapacityRetryProcessHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "AD_CAPACITY_HELPER=1", "AD_CAPACITY_PATH="+path, "AD_CAPACITY_NOW="+now.Format(time.RFC3339Nano))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, cmd)
	}
	for _, cmd := range children {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path + ".sent")
	if err != nil || string(data) != "sent\n" {
		t.Fatalf("cross-process sends=%q err=%v", data, err)
	}
}
func TestMatrixCapacityConfigRoundTrip(t *testing.T) {
	cfg := &UserConfig{}
	key, ok := LookupConfigKey("codex.capacity_retry")
	if !ok {
		t.Fatal("no operator control")
	}
	key.Set(cfg, true)
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	var read UserConfig
	if _, err := toml.Decode(encoded.String(), &read); err != nil || !read.Codex.CapacityRetry {
		t.Fatal("opt-in lost", err)
	}
	key.Set(&read, false)
	if read.Codex.CapacityRetry {
		t.Fatal("cannot disable")
	}
}

func TestMatrixCapacityRetryStatusGate(t *testing.T) {
	for _, status := range []Status{StatusRunning, StatusStopped, StatusStarting, StatusQueued, ""} {
		if matrixCapacityRetryStatusAllowed(status) {
			t.Fatalf("allowed %s", status)
		}
	}
	for _, status := range []Status{StatusWaiting, StatusIdle, StatusError} {
		if !matrixCapacityRetryStatusAllowed(status) {
			t.Fatalf("refused %s", status)
		}
	}
}
func TestMatrixCapacityRetryClaimHistory(t *testing.T) {
	path, v, now, target := matrixDueCapacityFixture(t)
	matrixRunCapacityRetry(path, v, now, target, func() bool { return true })
	v.TurnID = "second"
	v.At = now
	matrixRunCapacityRetry(path, v, now, target, func() bool { return true })
	// Emulate the next due native failure, then return to the older turn.
	state, err := matrixReadCapacityRetryState(path)
	if err != nil {
		t.Fatal(err)
	}
	state.Due = now.Add(-time.Second)
	matrixWriteCapacityRetryState(path, state)
	target.turn = v.TurnID
	matrixRunCapacityRetry(path, v, now, target, func() bool { return true })
	v.TurnID = "failure"
	matrixRunCapacityRetry(path, v, now, target, func() bool { return true })
	if target.sends.Load() != 2 {
		t.Fatalf("older claimed turn replayed: %d", target.sends.Load())
	}
}

func TestMatrixCapacityRetryConsumerCannotAct(t *testing.T) {
	i := &Instance{Tool: "codex", Status: StatusWaiting}
	i.matrixInterrupt = matrixTurnInterruptMemo{found: true, verdict: matrixTurnInterrupt{Kind: "server_overloaded", At: time.Now(), TurnID: "failure", Message: "Selected model is at capacity."}}
	i.SetRuntimeProducerFence(1)
	i.SetRuntimeConsumer(true)
	i.matrixMaybeRetryCodexCapacity()
	if !i.matrixInterrupt.retryCheckedAt.IsZero() {
		t.Fatal("consumer entered retry work")
	}
}
func TestMatrixCapacityRetryInvalidStateCannotResetBudget(t *testing.T) {
	for _, data := range []string{"null", "{}", `{"version":1,"attempts":3,"window_start":"2026-10-13T00:00:00Z"}`} {
		path, v, now, target := matrixDueCapacityFixture(t)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := matrixRunCapacityRetry(path, v, now, target, func() bool { return true })
		if err == nil || target.sends.Load() != 0 {
			t.Fatal("invalid state reset retry budget")
		}
	}
}

func TestMatrixCapacityRetryComposerPlaceholder(t *testing.T) {
	for _, style := range []string{"\x1b[2m", "\x1b[90m", "\x1b[38;5;8m"} {
		frame := "⚠ Selected model is at capacity.\n\n› " + style + "Ask Codex to do anything\x1b[0m\n\n  ? for shortcuts 100% context left"
		if !matrixCapacityComposerReady(frame) {
			t.Fatalf("idle placeholder blocked for style %q", style)
		}
	}
	if matrixCapacityComposerReady("› Ask Codex to do anything\n\n  ? for shortcuts 100% context left") {
		t.Fatal("plain operator text was treated as placeholder")
	}
}

// matrix/hardened: real tmux input delivery to an inert Python stand-in. No
// Codex executable, credentials, network request or real conversation is used.
func TestMatrixCapacityRetryPTYDelivery(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	if _, _, err := SetConfigValue("codex.capacity_retry", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetConfigValue("codex.capacity_retry", "false") })
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			path, v, now, _ := matrixDueCapacityFixture(t)
			marker := path + ".received"
			frame := matrixCapacityIdleFrame
			if draft {
				frame = strings.Replace(frame, "› ", "› operator draft", 1)
			}
			controller := filepath.Join(t.TempDir(), "controller.py")
			frameJSON, _ := json.Marshal(frame)
			markerJSON, _ := json.Marshal(marker)
			script := fmt.Sprintf("import json,sys,tty\ntty.setcbreak(sys.stdin.fileno())\nframe=json.loads(%q)\nmarker=json.loads(%q)\ntext=''\ndef render():\n sys.stdout.write('\\x1b[2J\\x1b[H'+frame.replace('› ','› '+text,1)); sys.stdout.flush()\nrender()\nwhile True:\n ch=sys.stdin.read(1)\n if not ch: break\n if ch in '\\r\\n':\n  with open(marker,'a') as f: f.write(text+'\\n')\n  text=''\n else: text+=ch\n render()\n", string(frameJSON), string(markerJSON))
			if err := os.WriteFile(controller, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			target := tmux.NewSession("matrix-capacity-"+fmt.Sprint(time.Now().UnixNano()), t.TempDir())
			target.SocketName = "capacity-" + fmt.Sprint(time.Now().UnixNano())
			cmd := exec.Command("tmux", "-L", target.SocketName, "new-session", "-d", "-s", target.Name, "-x", "120", "-y", "40", python, controller)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("create private pane: %v %s", err, out)
			}
			t.Cleanup(func() { exec.Command("tmux", "-L", target.SocketName, "kill-session", "-t", target.Name).Run() })
			ready := false
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				raw, _ := target.CapturePaneFresh()
				if strings.Contains(raw, "Selected model is at capacity.") {
					ready = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !ready {
				t.Fatal("mock pane never appeared")
			}
			// An auxiliary pane deliberately looks ready too; delivery must stay on 0.0.
			auxMarker := path + ".aux"
			auxJSON, _ := json.Marshal(auxMarker)
			auxScript := filepath.Join(t.TempDir(), "aux.py")
			auxBody := fmt.Sprintf("import json,sys\nsys.stdout.write(json.loads(%q))\nsys.stdout.flush()\nfor line in sys.stdin:\n with open(json.loads(%q),'a') as f: f.write(line)\n", string(frameJSON), string(auxJSON))
			if err := os.WriteFile(auxScript, []byte(auxBody), 0600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("tmux", "-L", target.SocketName, "new-window", "-t", target.Name+":1", python, auxScript).CombinedOutput(); err != nil {
				t.Fatalf("aux: %v %s", err, out)
			}
			pinned := &tmux.Session{Name: target.Name + ":0.0", SocketName: target.SocketName}
			owner := newTestStorage(t)
			inst := &Instance{ID: "capacity-pty", Tool: "codex", Command: "codex", Status: StatusWaiting, ProjectPath: t.TempDir(), CodexSessionID: "cxid", tmuxSession: target}
			seedInstanceRow(t, owner.db, inst, `{}`)
			inst.restartDB.Store(owner.db)
			fence, err := owner.db.LastModified()
			if err != nil {
				t.Fatal(err)
			}
			inst.SetRuntimeProducerFence(fence)
			record, _ := json.Marshal(map[string]any{"timestamp": v.At, "type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": v.TurnID, "error": map[string]any{"codex_error_info": v.Kind, "message": v.Message}}})
			rollout := matrixWriteTranscript(t, cxMeta, string(record))
			adapter := matrixCapacityInstanceTarget{instance: inst, target: target, pane: pinned, key: "codex:cxid", rollout: rollout, turn: v.TurnID}
			if _, err := matrixRunCapacityRetry(path, v, now, adapter, func() bool { return true }); err != nil {
				t.Fatal(err)
			}
			if draft {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("draft pane received keys")
				}
				raw, _ := pinned.CapturePaneFresh()
				if !strings.Contains(raw, "operator draft") || strings.Contains(raw, matrixCapacityRetryPrompt) {
					t.Fatal("draft was cleared")
				}
				return
			}
			var data []byte
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				data, _ = os.ReadFile(marker)
				if len(data) > 0 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if _, err := os.Stat(auxMarker); !os.IsNotExist(err) {
				t.Fatal("retry redirected into auxiliary window")
			}
			if string(data) != matrixCapacityRetryPrompt+"\n" {
				t.Fatalf("received=%q", data)
			}
			matrixRunCapacityRetry(path, v, now.Add(time.Second), adapter, func() bool { return true })
			after, _ := os.ReadFile(marker)
			if string(after) != string(data) {
				t.Fatal("same native failure delivered twice")
			}
		})
	}
}

func TestMatrixCapacityConfigPersistedControl(t *testing.T) {
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	if _, value, err := SetConfigValue("codex.capacity_retry", "true"); err != nil || value != true {
		t.Fatal("enable failed", value, err)
	}
	ClearUserConfigCache()
	cfg, err := LoadUserConfig()
	if err != nil || !cfg.Codex.CapacityRetry {
		t.Fatal("persisted opt-in lost", err)
	}
	if _, value, err := SetConfigValue("codex.capacity_retry", "false"); err != nil || value != false {
		t.Fatal("disable failed", value, err)
	}
}

func TestMatrixCapacityRetrySubmitAttribution(t *testing.T) {
	frame := strings.Replace(matrixCapacityIdleFrame, "› ", "› "+matrixCapacityRetryPrompt, 1)
	if !matrixCapacitySubmitReady(frame, matrixCapacityRetryPrompt) {
		t.Fatal("own payload rejected")
	}
	if matrixCapacitySubmitReady(strings.Replace(frame, "› ", "› operator draft ", 1), matrixCapacityRetryPrompt) {
		t.Fatal("foreign text would be submitted")
	}
	if matrixCapacitySubmitReady(matrixCapacityIdleFrame, matrixCapacityRetryPrompt) {
		t.Fatal("missing payload would be submitted")
	}
}
