package ui

// matrix/hardened: remote pin changes require authoritative readback and the
// aggregate pinned view must preserve host identity, filters, and saved folds.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

type matrixFakeRemotePinRunner struct {
	calls    [][]string
	config   session.RemoteConfig
	readback []byte
	setErr   error
	showErr  error
}

func (r *matrixFakeRemotePinRunner) RunCommand(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 1 && args[1] == "set" {
		return []byte(`{"success":true}`), r.setErr
	}
	return r.readback, r.showErr
}

func matrixRemotePinPointer(pin session.PinMode) *session.PinMode { return &pin }

func matrixRemotePinReadback(t *testing.T, id string, pin session.PinMode) []byte {
	t.Helper()
	data, err := json.Marshal(struct {
		ID  string          `json:"id"`
		Pin session.PinMode `json:"pin"`
	}{ID: id, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func matrixArmRemotePinRunners(t *testing.T, home *Home, runners map[string]*matrixFakeRemotePinRunner) {
	t.Helper()
	if home.remotePollConfig == nil {
		home.remotePollConfig = make(map[string]session.RemoteConfig)
	}
	for name := range runners {
		if _, exists := home.remotePollConfig[name]; !exists {
			home.remotePollConfig[name] = session.RemoteConfig{Host: name + ".invalid"}
		}
	}
	previous := newMatrixRemotePinRunner
	newMatrixRemotePinRunner = func(name string, rc session.RemoteConfig) (matrixRemotePinRunner, error) {
		if runner, ok := runners[name]; ok {
			runner.config = rc
			return runner, nil
		}
		return nil, errors.New("missing fake remote " + name)
	}
	t.Cleanup(func() { newMatrixRemotePinRunner = previous })
}

func matrixRemotePinInfo(t *testing.T, home *Home, name, id string) session.RemoteSessionInfo {
	t.Helper()
	info, ok := home.remoteSessionInfo(name, id)
	if !ok {
		t.Fatalf("remote %q session %q is missing", name, id)
	}
	return info
}

func matrixSelectRemotePin(t *testing.T, home *Home, name, id string) {
	t.Helper()
	for i, item := range home.flatItems {
		if item.Type == session.ItemTypeRemoteSession && item.RemoteName == name && item.RemoteSession != nil && item.RemoteSession.ID == id {
			home.cursor = i
			return
		}
	}
	t.Fatalf("remote %q session %q has no visible row", name, id)
}

func TestMatrixRemotePinKeyboardCycleRequiresReadback(t *testing.T) {
	home := matrixPinnedHome(t)
	const id = "remote-full-id-0123456789"
	runner := &matrixFakeRemotePinRunner{}
	matrixArmRemotePinRunners(t, home, map[string]*matrixFakeRemotePinRunner{"dev": runner})
	home.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {
		{ID: id, Title: "remote pin", Group: "work", Status: "idle", Pin: matrixRemotePinPointer(session.PinNone)},
	}}
	home.rebuildFlatItems()
	before := session.PinNone
	for _, want := range []session.PinMode{session.PinTop, session.PinBottom, session.PinNone} {
		matrixSelectRemotePin(t, home, "dev", id)
		runner.calls = nil
		runner.readback = matrixRemotePinReadback(t, id, want)
		_, cmd := home.Update(plainKeyMsg(','))
		if cmd == nil {
			t.Fatalf("comma did not dispatch the %q -> %q remote pin change", before, want)
		}
		if got := matrixRemotePinInfo(t, home, "dev", id).PinPosition(); got != before {
			t.Fatalf("remote pin changed optimistically: %q, want %q", got, before)
		}
		msg, ok := cmd().(matrixRemotePinResultMsg)
		if !ok || msg.err != nil {
			t.Fatalf("pin command result = %+v, result message = %v", msg, ok)
		}
		if got := matrixRemotePinInfo(t, home, "dev", id).PinPosition(); got != before {
			t.Fatalf("readback patched UI before message dispatch: %q", got)
		}
		wantCalls := [][]string{{"session", "set", "--json", id, "pin", string(want)}, {"session", "show", "--json", id}}
		if !reflect.DeepEqual(runner.calls, wantCalls) {
			t.Fatalf("remote calls = %#v, want %#v", runner.calls, wantCalls)
		}
		// The returned refresh Cmd is deliberately left unexecuted: this test
		// owns only fake set/show commands and must never contact a real host.
		home.Update(msg)
		if got := matrixRemotePinInfo(t, home, "dev", id).PinPosition(); got != want {
			t.Fatalf("confirmed remote pin = %q, want %q", got, want)
		}
		if len(home.matrixRemotePinPending) != 0 {
			t.Fatalf("completed pin left pending state: %v", home.matrixRemotePinPending)
		}
		if want == session.PinTop {
			home.groupViewMode = session.GroupViewPinnedOnly
			home.rebuildFlatItems()
		} else if want == session.PinBottom {
			item := home.flatItems[home.cursor]
			if item.RemoteName != "dev" || item.RemoteSession == nil || item.RemoteSession.ID != id {
				t.Fatal("confirmed remote pin change lost its selected host/session")
			}
		} else if len(home.flatItems) != 0 || home.cursor != 0 || home.groupViewMode != session.GroupViewPinnedOnly {
			t.Fatalf("unpinning the last remote changed the empty view: rows=%d cursor=%d mode=%v", len(home.flatItems), home.cursor, home.groupViewMode)
		}
		before = want
	}
}

func TestMatrixRemotePinUnconfirmedResultsNeverPatch(t *testing.T) {
	const id = "remote-full-id"
	for _, tc := range []struct {
		name   string
		runner matrixFakeRemotePinRunner
		calls  int
	}{
		{"set failure", matrixFakeRemotePinRunner{setErr: errors.New("SSH failed")}, 1},
		{"show failure", matrixFakeRemotePinRunner{showErr: errors.New("readback timeout")}, 2},
		{"malformed", matrixFakeRemotePinRunner{readback: []byte(`{"id":`)}, 2},
		{"missing pin", matrixFakeRemotePinRunner{readback: []byte(`{"id":"remote-full-id"}`)}, 2},
		{"null pin", matrixFakeRemotePinRunner{readback: []byte(`{"id":"remote-full-id","pin":null}`)}, 2},
		{"wrong ID", matrixFakeRemotePinRunner{readback: []byte(`{"id":"remote-full-id-other","pin":"top"}`)}, 2},
		{"wrong value", matrixFakeRemotePinRunner{readback: []byte(`{"id":"remote-full-id","pin":"bottom"}`)}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := matrixPinnedHome(t)
			runner := tc.runner
			matrixArmRemotePinRunners(t, home, map[string]*matrixFakeRemotePinRunner{"dev": &runner})
			home.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {
				{ID: id, Title: "unchanged", Status: "idle", Pin: matrixRemotePinPointer(session.PinNone)},
			}}
			home.rebuildFlatItems()
			cmd := home.startMatrixRemotePin("dev", id, session.PinTop)
			if cmd == nil {
				t.Fatal("known remote did not dispatch a pin command")
			}
			msg := cmd().(matrixRemotePinResultMsg)
			if msg.err == nil {
				t.Fatal("unconfirmed write was treated as successful")
			}
			if next := home.applyMatrixRemotePin(msg); next == nil {
				t.Fatal("failed remote pin did not schedule the existing refresh")
			}
			if home.err == nil || len(home.matrixRemotePinPending) != 0 {
				t.Fatalf("result lacks error or retains pending: err=%v pending=%v", home.err, home.matrixRemotePinPending)
			}
			if got := matrixRemotePinInfo(t, home, "dev", id).PinPosition(); got != session.PinNone {
				t.Fatalf("unconfirmed write patched the UI to %q", got)
			}
			if len(runner.calls) != tc.calls {
				t.Fatalf("command count = %d, want %d; unexpected retry or readback: %v", len(runner.calls), tc.calls, runner.calls)
			}
		})
	}
}

func TestMatrixRemotePinPendingIsScopedByHostAndRejectsDuplicates(t *testing.T) {
	home := matrixPinnedHome(t)
	const id = "same-id"
	a := &matrixFakeRemotePinRunner{readback: matrixRemotePinReadback(t, id, session.PinTop)}
	b := &matrixFakeRemotePinRunner{readback: matrixRemotePinReadback(t, id, session.PinBottom)}
	matrixArmRemotePinRunners(t, home, map[string]*matrixFakeRemotePinRunner{"a": a, "b": b})
	home.remoteSessions = map[string][]session.RemoteSessionInfo{
		"a": {{ID: id, Title: "host a", Status: "idle", Pin: matrixRemotePinPointer(session.PinNone)}},
		"b": {{ID: id, Title: "host b", Status: "idle", Pin: matrixRemotePinPointer(session.PinNone)}},
	}
	first := home.startMatrixRemotePin("a", id, session.PinTop)
	if first == nil || home.startMatrixRemotePin("a", id, session.PinBottom) != nil {
		t.Fatal("same-host in-flight pin was missing or permitted a duplicate command")
	}
	second := home.startMatrixRemotePin("b", id, session.PinBottom)
	if second == nil || len(home.matrixRemotePinPending) != 2 {
		t.Fatal("equal IDs on two hosts shared pending state")
	}
	home.applyMatrixRemotePin(first().(matrixRemotePinResultMsg))
	if matrixRemotePinInfo(t, home, "a", id).PinPosition() != session.PinTop || matrixRemotePinInfo(t, home, "b", id).PinPosition() != session.PinNone {
		t.Fatal("host a confirmation patched host b")
	}
	if len(home.matrixRemotePinPending) != 1 {
		t.Fatal("host a completion cleared host b pending state")
	}
	home.applyMatrixRemotePin(second().(matrixRemotePinResultMsg))
	if matrixRemotePinInfo(t, home, "b", id).PinPosition() != session.PinBottom || len(home.matrixRemotePinPending) != 0 {
		t.Fatal("host b confirmation was lost")
	}
}

func TestMatrixRemotePinOldHostAndRepointedResultAreRejected(t *testing.T) {
	home := matrixPinnedHome(t)
	const id = "remote-id"
	runner := &matrixFakeRemotePinRunner{readback: matrixRemotePinReadback(t, id, session.PinTop)}
	matrixArmRemotePinRunners(t, home, map[string]*matrixFakeRemotePinRunner{"dev": runner})
	home.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {{ID: id, Title: "legacy", Status: "idle"}}}
	if cmd := home.startMatrixRemotePin("dev", id, session.PinTop); cmd != nil {
		t.Fatal("old payload without a pin field sent a remote mutation")
	}
	if home.err == nil || !strings.Contains(home.err.Error(), "update") || len(runner.calls) != 0 || len(home.matrixRemotePinPending) != 0 {
		t.Fatalf("legacy refusal = err:%v calls:%v pending:%v", home.err, runner.calls, home.matrixRemotePinPending)
	}
	home.remoteSessions["dev"][0].Pin = matrixRemotePinPointer(session.PinNone)
	home.remotePollConfig = map[string]session.RemoteConfig{"dev": {Host: "old.example"}}
	cmd := home.startMatrixRemotePin("dev", id, session.PinTop)
	if cmd == nil {
		t.Fatal("known pin was refused")
	}
	home.remotePollConfig["dev"] = session.RemoteConfig{Host: "replacement.example"}
	msg := cmd().(matrixRemotePinResultMsg)
	if runner.config.Host != "old.example" {
		t.Fatalf("delayed command was bound to replacement host: %+v", runner.config)
	}
	home.applyMatrixRemotePin(msg)
	if got := matrixRemotePinInfo(t, home, "dev", id).PinPosition(); got != session.PinNone {
		t.Fatalf("repointed host adopted an old confirmation: %q", got)
	}
}

func TestMatrixRemotePinConfirmedStateSurvivesOlderPollAndQueuedPush(t *testing.T) {
	home := matrixPinnedHome(t)
	const id = "confirmed-pin"
	runner := &matrixFakeRemotePinRunner{readback: matrixRemotePinReadback(t, id, session.PinTop)}
	matrixArmRemotePinRunners(t, home, map[string]*matrixFakeRemotePinRunner{"dev": runner})
	old := session.RemoteSessionInfo{ID: id, Title: "before pin", Status: "idle", Pin: matrixRemotePinPointer(session.PinNone)}
	home.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {old}}
	home.remoteFetchSeq = 7
	home.rebuildFlatItems()
	cmd := home.startMatrixRemotePin("dev", id, session.PinTop)
	if cmd == nil {
		t.Fatal("known remote pin did not dispatch")
	}
	home.Update(cmd())
	// A poll begun before the mutation can finish after authoritative show.
	// Its result must not send the newly pinned row back out of the view.
	home.Update(remoteSessionsFetchedMsg{
		gen: 7, pollName: "dev", pollConfig: home.remotePollConfig["dev"],
		sessions: map[string][]session.RemoteSessionInfo{"dev": {old}},
	})
	if got := matrixRemotePinInfo(t, home, "dev", id).PinPosition(); got != session.PinTop {
		t.Fatalf("older poll reverted the confirmed pin: %q", got)
	}
	queued := session.RemoteChange{Remote: "dev", HasData: true, Sessions: []session.RemoteSessionInfo{old}}
	if !home.stalePushedChange(queued, time.Now()) {
		t.Fatal("queued push contradicting a just-confirmed pin was accepted")
	}
	home.Update(remoteChangedMsg{remoteName: "dev", change: queued})
	if got := matrixRemotePinInfo(t, home, "dev", id).PinPosition(); got != session.PinTop {
		t.Fatalf("queued push reverted the confirmed pin: %q", got)
	}
	fresh := old
	fresh.Title, fresh.Pin = "fresh pushed title", matrixRemotePinPointer(session.PinTop)
	matching := session.RemoteChange{Remote: "dev", HasData: true, Sessions: []session.RemoteSessionInfo{fresh}}
	if home.stalePushedChange(matching, time.Now()) {
		t.Fatal("matching push was suppressed along with stale data")
	}
	home.Update(remoteChangedMsg{remoteName: "dev", change: matching})
	if got := matrixRemotePinInfo(t, home, "dev", id); got.Title != fresh.Title || got.PinPosition() != session.PinTop {
		t.Fatalf("fresh matching push did not reconcile the remote row: %+v", got)
	}
}

func TestMatrixRemotePinnedViewCombinesCollapsedHostsAndFlatChildren(t *testing.T) {
	home := matrixPinnedHome(t)
	matrixPinnedInstance(t, home, "a1").Pin = session.PinTop
	home.remoteSessions = map[string][]session.RemoteSessionInfo{
		"dev": {
			{ID: "top", Title: "remote top", Group: "work/deep", Status: "idle", Pin: matrixRemotePinPointer(session.PinTop)},
			{ID: "child", Title: "remote child", Group: "work/deep", ParentSessionID: "unpinned", Status: "waiting", Pin: matrixRemotePinPointer(session.PinBottom)},
			{ID: "unpinned", Title: "remote parent", Group: "work/deep", Status: "idle", Pin: matrixRemotePinPointer(session.PinNone)},
			{ID: "legacy", Title: "legacy remote", Group: "work", Status: "idle"},
		},
		"other": {{ID: "top", Title: "second host", Group: "misc", Status: "idle", Pin: matrixRemotePinPointer(session.PinTop)}},
	}
	home.remoteGroupsCollapsed = map[string]bool{"remotes/dev": true, "remotes/dev/work": true, "remotes/other": true}
	folds := map[string]bool{"remotes/dev": true, "remotes/dev/work": true, "remotes/other": true}
	home.groupTree.Groups["alpha"].Expanded = false
	home.groupTree.Expanded["alpha"] = false
	home.groupViewMode = session.GroupViewPinnedOnly
	home.rebuildFlatItems()
	var rows []string
	for _, item := range home.flatItems {
		if item.Level != 0 || item.IsSubSession || item.ParentIsLastInGroup {
			t.Fatalf("pinned row retains an invisible tree parent: %+v", item)
		}
		switch item.Type {
		case session.ItemTypeSession:
			rows = append(rows, "local/"+item.Session.Title)
		case session.ItemTypeRemoteSession:
			rows = append(rows, item.RemoteName+"/"+item.RemoteSession.ID)
		default:
			t.Fatalf("pinned aggregate retained header or placeholder: %+v", item)
		}
	}
	sort.Strings(rows)
	want := []string{"dev/child", "dev/top", "local/a1", "other/top"}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("aggregate rows = %v, want %v", rows, want)
	}
	if !reflect.DeepEqual(home.remoteGroupsCollapsed, folds) || home.groupTree.Groups["alpha"].Expanded || home.groupTree.Expanded["alpha"] {
		t.Fatal("pinned view mutated saved local or remote folds")
	}
	if child := matrixRemotePinInfo(t, home, "dev", "child"); child.ParentSessionID != "unpinned" || child.Group != "work/deep" {
		t.Fatal("flat pinned display mutated persisted remote parent or group membership")
	}
	for _, embedded := range []bool{false, true} {
		home.embeddedLayout = embedded
		for _, width := range []int{26, 80, 120} {
			view := stripAnsi(home.renderSessionList(width, 24))
			if strings.Contains(view, "├─") || strings.Contains(view, "└─") {
				t.Fatalf("standalone remote pin drew an orphan connector at %d columns:\n%s", width, view)
			}
			if width == 120 && (!strings.Contains(view, "[dev]") || !strings.Contains(view, "[other]") || !strings.Contains(view, "📌")) {
				t.Fatalf("rendered aggregate lost host identity or pin glyph:\n%s", view)
			}
			for _, line := range strings.Split(view, "\n") {
				if cellWidth(line) > width {
					t.Fatalf("remote pin overflows %d columns: %q", width, line)
				}
			}
		}
	}
}

func TestMatrixRemotePinnedViewFiltersCompose(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status session.Status
		recent session.TimeFilterMode
		want   []string
	}{
		{"all active pins", "", session.TimeFilterAll, []string{"old", "running", "waiting"}},
		{"running recent", session.StatusRunning, session.TimeFilter7Days, []string{"running"}},
		{"waiting", session.StatusWaiting, session.TimeFilterAll, []string{"waiting"}},
		{"archived", FilterModeArchived, session.TimeFilter7Days, []string{"archived"}},
		{"no matching status", session.StatusStopped, session.TimeFilterAll, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := matrixPinnedHome(t)
			recent := now.Format(time.RFC3339Nano)
			home.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {
				{ID: "running", Status: "running", LastActivityAt: recent, Pin: matrixRemotePinPointer(session.PinTop)},
				{ID: "waiting", Status: "waiting", LastActivityAt: recent, Pin: matrixRemotePinPointer(session.PinBottom)},
				{ID: "old", Status: "running", LastActivityAt: now.AddDate(0, 0, -30).Format(time.RFC3339Nano), Pin: matrixRemotePinPointer(session.PinTop)},
				{ID: "archived", Status: "running", Archived: true, LastActivityAt: recent, Pin: matrixRemotePinPointer(session.PinTop)},
				{ID: "unknown", Status: "running", LastActivityAt: recent},
				{ID: "normal", Status: "running", LastActivityAt: recent, Pin: matrixRemotePinPointer(session.PinNone)},
			}}
			home.remoteGroupsCollapsed = map[string]bool{"remotes/dev": true}
			home.groupViewMode = session.GroupViewPinnedOnly
			home.statusFilter, home.timeFilter = tc.status, tc.recent
			home.keepEmptyFilter = false
			home.rebuildFlatItemsAt(now)
			got := visibleRemoteSessionIDs(home, "dev")
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("remote filtered pins = %v, want %v", got, tc.want)
			}
			if home.groupViewMode != session.GroupViewPinnedOnly || home.statusFilter != tc.status || home.timeFilter != tc.recent {
				t.Fatal("empty remote pins changed the current filters or view")
			}
		})
	}
}

func TestMatrixRemotePinBandsOverrideManualOrderWithoutMutation(t *testing.T) {
	sessions := []session.RemoteSessionInfo{
		{ID: "bottom", Group: "work", Pin: matrixRemotePinPointer(session.PinBottom)},
		{ID: "normal", Group: "work", Pin: matrixRemotePinPointer(session.PinNone)},
		{ID: "top-a", Group: "work", Pin: matrixRemotePinPointer(session.PinTop)},
		{ID: "top-b", Group: "work", Pin: matrixRemotePinPointer(session.PinTop)},
	}
	indices := []int{0, 1, 3, 2}
	if got := matrixOrderRemotePins(sessions, indices); !reflect.DeepEqual(got, []int{3, 2, 1, 0}) {
		t.Fatalf("pin bands = %v, want [3 2 1 0]", got)
	}
	if !reflect.DeepEqual(indices, []int{0, 1, 3, 2}) || sessions[0].ID != "bottom" {
		t.Fatal("pin ordering mutated its input")
	}
	home := matrixPinnedHome(t)
	home.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": sessions}
	home.remoteSessionOrder = remoteOrder{"dev": {"work": {"bottom", "normal", "top-b", "top-a"}}}
	home.rebuildFlatItems()
	if got := visibleRemoteSessionIDs(home, "dev"); !reflect.DeepEqual(got, []string{"top-b", "top-a", "normal", "bottom"}) {
		t.Fatalf("normal-view pins did not override manual order: %v", got)
	}
}

func TestMatrixRemotePinnedViewScopePreservesHostPath(t *testing.T) {
	for _, tc := range []struct {
		scope string
		want  []string
	}{
		{"", []string{"dev/deep", "dev/default", "dev/other", "local/a1", "other/deep"}},
		{"alpha", []string{"local/a1"}},
		{"remotes/dev/work", []string{"dev/deep"}},
		{"remotes/dev/my-sessions", []string{"dev/default"}},
		{"remotes/other", []string{"other/deep"}},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			home := matrixPinnedHome(t)
			matrixPinnedInstance(t, home, "a1").Pin = session.PinTop
			home.remoteSessions = map[string][]session.RemoteSessionInfo{
				"dev": {
					{ID: "deep", Group: "work/api", Status: "idle", Pin: matrixRemotePinPointer(session.PinTop)},
					{ID: "default", Group: "", Status: "idle", Pin: matrixRemotePinPointer(session.PinTop)},
					{ID: "other", Group: "misc", Status: "idle", Pin: matrixRemotePinPointer(session.PinBottom)},
				},
				"other": {{ID: "deep", Group: "work/api", Status: "idle", Pin: matrixRemotePinPointer(session.PinTop)}},
			}
			home.groupViewMode, home.groupScope = session.GroupViewPinnedOnly, tc.scope
			home.rebuildFlatItems()
			var got []string
			for _, item := range home.flatItems {
				switch item.Type {
				case session.ItemTypeSession:
					got = append(got, "local/"+item.Session.Title)
				case session.ItemTypeRemoteSession:
					got = append(got, item.RemoteName+"/"+item.RemoteSession.ID)
				default:
					t.Fatalf("scope retained a non-session row: %+v", item)
				}
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) || home.groupScope != tc.scope {
				t.Fatalf("pinned scope %q = %v, want %v", tc.scope, got, tc.want)
			}
		})
	}
}

func TestMatrixRemotePinEditDialogUsesKnownBaseline(t *testing.T) {
	for _, pin := range []session.PinMode{session.PinNone, session.PinTop, session.PinBottom} {
		home := matrixPinnedHome(t)
		info := session.RemoteSessionInfo{ID: "edit", Title: "remote edit", Tool: "claude", Pin: matrixRemotePinPointer(pin)}
		dialog := home.editSessionDialog
		dialog.ShowRemote(info, "dev")
		if got := dialog.selectedPill(session.FieldPin); got != string(pin) {
			t.Fatalf("remote edit baseline = %q, want %q", got, pin)
		}
		if changes := dialog.GetChanges(remoteEditInstance(info)); len(changes) != 0 {
			t.Fatalf("unchanged remote pin generated edits: %+v", changes)
		}
		want := matrixNextPin(pin)
		selectEditPill(t, dialog, session.FieldPin, string(want))
		changes := dialog.GetChanges(remoteEditInstance(info))
		if len(changes) != 1 || changes[0].Field != session.FieldPin || changes[0].Value != string(want) || !changes[0].IsLive {
			t.Fatalf("changed remote pin = %+v, want live pin %q", changes, want)
		}
	}
	home := matrixPinnedHome(t)
	home.editSessionDialog.ShowRemote(session.RemoteSessionInfo{ID: "legacy", Title: "old host", Tool: "claude"}, "old")
	for _, field := range home.editSessionDialog.fields {
		if field.key == session.FieldPin {
			t.Fatal("old payload exposed a writable remote pin pill")
		}
	}
}
