package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/core/daemon"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: exercise the real Unix transport, demand accounting and
// collector ownership while replacing only slow external pane observations.
type fakeRegistry struct {
	mu              sync.Mutex
	version, loaded int
	account         string
	status          session.Status
	two             bool
	failure         string
	closed          atomic.Bool
}

func (f *fakeRegistry) Changed() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != "" {
		return false, errors.New(f.failure)
	}
	return f.version != f.loaded, nil
}
func (f *fakeRegistry) Load() ([]*session.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = f.version
	i := session.NewInstanceWithTool("one", "/test", "codex")
	i.ID = "one"
	i.Account = f.account
	i.Status = f.status
	instances := []*session.Instance{i}
	if f.two {
		healthy := session.NewInstanceWithTool("healthy", "/test", "codex")
		healthy.ID = "healthy"
		healthy.Status = session.StatusIdle
		instances = append(instances, healthy)
	}
	return instances, nil
}
func (f *fakeRegistry) Close() error { f.closed.Store(true); return nil }
func (f *fakeRegistry) edit(account string, status session.Status) {
	f.mu.Lock()
	f.account = account
	f.status = status
	f.version++
	f.mu.Unlock()
}
func (f *fakeRegistry) fail(message string) { f.mu.Lock(); f.failure = message; f.mu.Unlock() }

func fixtureProbe(i *session.Instance, _ *session.StatusUpdatePass, at time.Time) session.RuntimeSessionSnapshot {
	return session.RuntimeSessionSnapshot{ID: i.ID, Identity: i.RuntimeIdentity(), ObservedAt: at, Status: session.StatusWaiting, Substate: session.SubstateTurnInterrupted, Detail: "turn_aborted: interrupted", Hook: &session.HookStatus{Status: "waiting", DoneStatus: "ok", DoneSummary: "complete", Sequence: 9}, HookKnown: true, TmuxAlive: true, TmuxAliveKnown: true}
}
func canonicalTemp(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TMUX_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	short, err := os.MkdirTemp(base, "mon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	dir, err := filepath.EvalSymlinks(short)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func startTestServer(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	p := daemon.PathsIn(canonicalTemp(t))
	owner, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(opts)
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, owner.Listener()) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(6 * time.Second):
			t.Error("monitor shutdown blocked")
		}
		_ = owner.Close()
	})
	return s, p.Socket
}
func testOptions(reg *fakeRegistry) Options {
	return Options{Version: "test+matrix.39", Interval: time.Hour, DemandTTL: time.Minute, PassBudget: 200 * time.Millisecond, LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }, ValidateProfile: func(string) error { return nil }, Probe: fixtureProbe}
}
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestUnixConsumersShareOneProducerAndPrivateState(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	var probes atomic.Int64
	opts := testOptions(reg)
	opts.Probe = func(i *session.Instance, p *session.StatusUpdatePass, at time.Time) session.RuntimeSessionSnapshot {
		probes.Add(1)
		return fixtureProbe(i, p, at)
	}
	s, socket := startTestServer(t, opts)
	ctx := testContext(t)
	clients := make([]*Client, 16)
	snapshots := make([]*session.RuntimeProfileSnapshot, 16)
	var wg sync.WaitGroup
	for n := range clients {
		clients[n] = NewClient(socket, nil)
		t.Cleanup(func() { _ = clients[n].Close() })
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			snapshot, err := clients[n].Snapshot(ctx, "a")
			if err != nil {
				t.Errorf("snapshot: %v", err)
				return
			}
			snapshots[n] = snapshot
		}(n)
	}
	wg.Wait()
	if got := probes.Load(); got != 1 {
		t.Fatalf("%d clients started %d probes", len(clients), got)
	}
	for _, snapshot := range snapshots {
		if snapshot == nil || len(snapshot.Sessions) != 1 || snapshot.Sessions[0].Detail != "turn_aborted: interrupted" {
			t.Fatalf("lost detailed state: %#v", snapshot)
		}
	}
	snapshots[0].Sessions[0].Hook.DoneSummary = "consumer mutation"
	if snapshots[1].Sessions[0].Hook.DoneSummary != "complete" {
		t.Fatal("consumer snapshots alias")
	}
	if err := clients[0].Refresh(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if got := probes.Load(); got != 2 {
		t.Fatalf("refresh probes=%d", got)
	}
	if s.Status().SourceLoads != 2 {
		t.Fatalf("source loads=%d", s.Status().SourceLoads)
	}
	if err := clients[0].Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := clients[1].Status(ctx); err != nil {
		t.Fatalf("closing one consumer stopped owner: %v", err)
	}
}

func TestDemandExpiresAndSnapshotWakesOneProfile(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	opts := testOptions(reg)
	opts.Interval = 20 * time.Millisecond
	opts.DemandTTL = 50 * time.Millisecond
	s, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	if _, err := client.Snapshot(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(130 * time.Millisecond)
	before := s.Status().SourceLoads
	if s.Status().ActiveProfiles != 0 {
		t.Fatal("expired demand is still active")
	}
	time.Sleep(80 * time.Millisecond)
	if after := s.Status().SourceLoads; after != before {
		t.Fatalf("idle collector sampled %d more times", after-before)
	}
	if _, err := client.Snapshot(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for s.Status().SourceLoads == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Status().SourceLoads == before {
		t.Fatal("demand did not wake collector")
	}
}

func TestEvidenceBurstCoalescesFleetPassButExplicitRefreshIsImmediate(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	s, socket := startTestServer(t, testOptions(reg))
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	if _, err := client.Snapshot(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 20; n++ {
		s.forceActive()
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.Status().SourceLoads; got != 1 {
		t.Fatalf("hook burst accelerated fleet scans: %d", got)
	}
	if err := client.Refresh(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if got := s.Status().SourceLoads; got != 2 {
		t.Fatalf("explicit refresh did not bypass evidence coalescing: %d", got)
	}
	// The explicit pass consumed the pending evidence; it must not immediately
	// schedule an additional stale force request after the burst has ended.
	time.Sleep(550 * time.Millisecond)
	if got := s.Status().SourceLoads; got != 2 {
		t.Fatalf("retired evidence caused duplicate pass: %d", got)
	}
}

func TestOrderedHistoryReportsGapAndPreservesCompletionDetails(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	opts := testOptions(reg)
	opts.HistoryLimit = 3
	_, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	first, err := client.Snapshot(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err := client.Refresh(ctx, "a"); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := client.Observations(ctx, "a", first.Epoch, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Revision+1 != rows[1].Revision || rows[1].Sessions[0].Hook.DoneStatus != "ok" {
		t.Fatalf("ordered detail lost: %#v", rows)
	}
	for n := 0; n < 2; n++ {
		if err := client.Refresh(ctx, "a"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = client.Observations(ctx, "a", first.Epoch, first.Revision)
	var gap *GapError
	if !errors.As(err, &gap) || gap.Oldest != 3 || gap.Latest != 5 {
		t.Fatalf("missing explicit gap: %v", err)
	}
	_, err = client.Observations(ctx, "a", "retired-owner", 4)
	if !errors.As(err, &gap) {
		t.Fatalf("owner epoch did not force resync: %v", err)
	}
	baseline, err := client.Observations(ctx, "a", "", 0)
	if err != nil || len(baseline) != 1 || baseline[0].Revision != 5 {
		t.Fatalf("baseline: %#v %v", baseline, err)
	}
}

func TestOrderedRecoveryKeepsErrorPublicationAndFollowingGoodVerdict(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	_, socket := startTestServer(t, testOptions(reg))
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	first, err := client.Snapshot(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	reg.fail("registry temporarily unavailable")
	if err := client.Refresh(ctx, "a"); err == nil {
		t.Fatal("source failure reported successful refresh")
	}
	reg.fail("")
	if err := client.Refresh(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	publications, err := client.Observations(ctx, "a", first.Epoch, first.Revision)
	if err != nil || len(publications) != 2 || publications[0].Error == "" || publications[1].Error != "" || publications[1].Sessions[0].Detail == "" {
		t.Fatalf("error publication wedged ordered recovery: %#v %v", publications, err)
	}
}

func TestLargestObservationFitsArrayFramingAndPagination(t *testing.T) {
	p := newProfile("a", "epoch")
	base := []byte(`{"payload":""}`)
	encoded := []byte(`{"payload":"` + strings.Repeat("x", maxResponseBytes-2-len(base)) + `"}`)
	if len(encoded) != maxResponseBytes-2 {
		t.Fatal("invalid boundary fixture")
	}
	p.revision = 2
	p.history = []*publication{{snapshot: session.RuntimeProfileSnapshot{Revision: 1}, encoded: encoded}, {snapshot: session.RuntimeProfileSnapshot{Revision: 2}, encoded: []byte(`{"revision":2}`)}}
	body, gap, err := p.observations("epoch", 0)
	if err != nil || gap != nil || len(body) != maxResponseBytes || !json.Valid(body) {
		t.Fatalf("large first publication silently lost or oversized: len=%d %v %v", len(body), gap, err)
	}
	body, gap, err = p.observations("epoch", 1)
	if err != nil || gap != nil || string(body) != `[{"revision":2}]` {
		t.Fatalf("cursor pagination failed: %q %v %v", body, gap, err)
	}
}

func TestHungProbeCannotOverlapOrReanimateReboundGeneration(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var probes atomic.Int64
	opts := testOptions(reg)
	opts.Interval = 20 * time.Millisecond
	opts.PassBudget = 35 * time.Millisecond
	opts.Probe = func(i *session.Instance, p *session.StatusUpdatePass, at time.Time) session.RuntimeSessionSnapshot {
		if probes.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		return fixtureProbe(i, p, at)
	}
	s, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	first, err := client.Snapshot(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if first.Sessions[0].Error == "" {
		t.Fatal("hung observation was presented as current")
	}
	reg.edit("new-account", session.StatusStopped)
	time.Sleep(100 * time.Millisecond)
	if got := probes.Load(); got != 1 {
		t.Fatalf("hung ID launched overlapping calls: %d", got)
	}
	close(release)
	if err := client.Refresh(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	latest, err := client.Snapshot(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Sessions[0].Identity == first.Sessions[0].Identity {
		t.Fatal("rebound generation retained old identity")
	}
	if s.Status().Inflight > 1 {
		t.Fatal("collector grew overlapping probe backlog")
	}
}

func TestNewHungPaneLeavesHealthyRowsUsable(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle, two: true}
	release := make(chan struct{})
	opts := testOptions(reg)
	opts.PassBudget = 25 * time.Millisecond
	opts.Probe = func(i *session.Instance, p *session.StatusUpdatePass, at time.Time) session.RuntimeSessionSnapshot {
		if i.ID == "one" {
			<-release
		}
		return fixtureProbe(i, p, at)
	}
	_, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	snapshot, err := client.Snapshot(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Error != "" || len(snapshot.Sessions) != 2 {
		t.Fatalf("one pane invalidated profile: %#v", snapshot)
	}
	for _, row := range snapshot.Sessions {
		if row.ObservedAt.IsZero() || row.Revision != snapshot.Revision || row.Epoch != snapshot.Epoch || row.Profile != snapshot.Profile {
			t.Fatalf("invalid runtime row envelope: %#v", row)
		}
		if row.ID == "healthy" && (row.Error != "" || !row.TmuxAliveKnown || row.Detail == "") {
			t.Fatalf("healthy row lost usable verdict: %#v", row)
		}
		if row.ID == "one" && row.Error == "" {
			t.Fatal("hung new pane appeared observed")
		}
	}
	close(release)
}

func TestStopWhileProbeRunsClosesRegistryAfterProbeOnly(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	release := make(chan struct{})
	opts := testOptions(reg)
	opts.PassBudget = 20 * time.Millisecond
	opts.Probe = func(i *session.Instance, p *session.StatusUpdatePass, at time.Time) session.RuntimeSessionSnapshot {
		<-release
		return fixtureProbe(i, p, at)
	}
	_, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	if _, err := client.Snapshot(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := client.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if reg.closed.Load() {
		t.Fatal("registry closed underneath an active probe")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for !reg.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !reg.closed.Load() {
		t.Fatal("finished probe retained registry owner")
	}
}

func TestClosingClientCancelsItsPendingReadAndKeepsOwnerRunning(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	opts := testOptions(reg)
	opts.PassBudget = time.Second
	opts.Probe = func(i *session.Instance, p *session.StatusUpdatePass, at time.Time) session.RuntimeSessionSnapshot {
		entered <- struct{}{}
		<-release
		return fixtureProbe(i, p, at)
	}
	_, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	ctx := testContext(t)
	done := make(chan error, 1)
	go func() { _, err := client.Snapshot(ctx, "a"); done <- err }()
	<-entered
	_ = client.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed client retained pending read")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("client close did not cancel read")
	}
	peer := NewClient(socket, nil)
	defer peer.Close()
	if _, err := peer.Status(ctx); err != nil {
		t.Fatalf("closing client stopped shared owner: %v", err)
	}
	close(release)
}

func TestInvalidProtocolAndMethodNeverCreateDemand(t *testing.T) {
	reg := &fakeRegistry{version: 1, status: session.StatusIdle}
	s, socket := startTestServer(t, testOptions(reg))
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	for _, request := range []struct{ method, path, protocol string }{{"GET", "/snapshot?profile=a", "999"}, {"POST", "/snapshot?profile=a", "1"}, {"GET", "/commands?profile=a", "1"}, {"GET", "/observations?profile=a&after=bad", "1"}} {
		req, err := http.NewRequestWithContext(ctx, request.method, "http://monitor"+request.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(protocolHeader, request.protocol)
		resp, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("bad request accepted: %+v", request)
		}
	}
	if status := s.Status(); status.Profiles != 0 || status.SourceLoads != 0 {
		t.Fatalf("invalid requests collected: %+v", status)
	}
}

func TestOwnershipRejectsSymlinkAndForeignPeer(t *testing.T) {
	dir := canonicalTemp(t)
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(daemon.PathsIn(link)); err == nil {
		t.Fatal("symlink runtime owner accepted")
	}
	p := daemon.PathsIn(target)
	if err := os.Symlink(filepath.Join(dir, "secret"), p.Lock); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(p); err == nil {
		t.Fatal("symlink owner lock accepted")
	}
	opts := testOptions(&fakeRegistry{version: 1})
	opts.OwnerUID = os.Getuid() + 1
	s, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	if _, err := client.Snapshot(ctx, "a"); err == nil {
		t.Fatal("foreign peer accepted")
	}
	if s.Status().Profiles != 0 {
		t.Fatal("foreign peer created demand")
	}
}

func TestProfileOwnersKeepStatusAndNativeWritesInTheirOwnDB(t *testing.T) {
	// Both profiles deliberately share a management ID: wrong global-DB routing
	// would silently corrupt the peer, rather than merely fail a missing-row write.
	stores := make(map[string]*session.Storage)
	for _, profile := range []string{"a", "b"} {
		store, err := session.NewStorageWithProfile(profile)
		if err != nil {
			t.Fatal(err)
		}
		stores[profile] = store
		t.Cleanup(func() { _ = store.Close() })
		inst := session.NewInstanceWithTool(profile, "/profile/"+profile, "claude")
		inst.ID = "same-id"
		inst.Status = session.StatusIdle
		if err := store.SaveWithGroups([]*session.Instance{inst}, nil); err != nil {
			t.Fatal(err)
		}
	}
	prior := statedb.GetGlobal()
	statedb.SetGlobal(stores["b"].GetDB())
	t.Cleanup(func() { statedb.SetGlobal(prior) })
	ids := map[string]string{"/profile/a": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "/profile/b": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}
	opts := Options{Interval: time.Hour, DemandTTL: time.Minute, PassBudget: time.Second, Probe: func(inst *session.Instance, pass *session.StatusUpdatePass, at time.Time) session.RuntimeSessionSnapshot {
		inst.UpdateHookStatus(&session.HookStatus{Status: "waiting", Event: "Stop", SessionID: ids[inst.ProjectPath], UpdatedAt: at})
		return fixtureProbe(inst, pass, at)
	}}
	_, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	defer client.Close()
	ctx := testContext(t)
	for _, profile := range []string{"a", "b"} {
		if _, err := client.Snapshot(ctx, profile); err != nil {
			t.Fatal(err)
		}
	}
	for profile, store := range stores {
		instances, _, err := store.LoadWithGroups()
		if err != nil {
			t.Fatal(err)
		}
		if len(instances) != 1 || instances[0].ClaudeSessionID != ids["/profile/"+profile] || instances[0].GetStatusThreadSafe() != session.StatusWaiting {
			t.Fatalf("profile %s received peer writes: %#v", profile, instances)
		}
	}
}

func TestDetectedToolPersistsWhenStatusDoesNotChange(t *testing.T) {
	store, err := session.NewStorageWithProfile("detected-tool")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inst := session.NewInstanceWithTool("detected-tool", "/private-tool-fixture", "shell")
	inst.Status = session.StatusWaiting
	if err := store.SaveWithGroups([]*session.Instance{inst}, nil); err != nil {
		t.Fatal(err)
	}
	opts := Options{Interval: time.Hour, PassBudget: time.Second, Probe: func(inst *session.Instance, pass *session.StatusUpdatePass, at time.Time) session.RuntimeSessionSnapshot {
		inst.SetToolThreadSafe("codex")
		return fixtureProbe(inst, pass, at)
	}}
	_, socket := startTestServer(t, opts)
	client := NewClient(socket, nil)
	defer client.Close()
	snapshot, err := client.Snapshot(testContext(t), "detected-tool")
	if err != nil {
		t.Fatal(err)
	}
	loaded, _, err := store.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].GetToolThreadSafe() != "codex" {
		t.Fatal("unchanged verdict lost newly detected tool")
	}
	if len(snapshot.Sessions) != 1 || !loaded[0].ApplyRuntimeSnapshot("detected-tool", snapshot.Sessions[0]) {
		t.Fatal("detected tool identity was not consumable")
	}
}
