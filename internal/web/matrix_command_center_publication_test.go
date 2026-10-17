package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// matrix/hardened: live synthesis reads the owned immutable menu directly,
// while REST callers can edit every nested result without changing live data.
func TestMatrixCommandCenterWireOwnedInputAndPrivateRESTOutput(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: ccTestMenu()}
	srv, _, _ := matrixPublisherIntegrationServer(t, source, "")
	tracker := &ccPublicationTracker{}
	updates, release := srv.menuPublisher.subscribeWire(time.Hour, tracker.observe)
	defer release()
	publication := matrixPublisherPublication(t, updates)
	if publication.Snapshot != nil {
		t.Fatal("wire subscriber received a mutable menu DTO")
	}
	srv.menuPublisher.mu.Lock()
	owned := srv.menuPublisher.current.Snapshot
	srv.menuPublisher.mu.Unlock()
	srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
		if menu != owned {
			t.Error("live derived builder decoded another per-reader menu DTO")
		}
		return &CommandCenterSnapshot{
			Profile:           "owned",
			Conductors:        []CommandCenterConductor{{Name: "original", Sessions: []CommandCenterSession{{Title: "child"}}}},
			DecisionsWaiting:  []CommandCenterDecision{{Question: "decision"}},
			RecentlyCompleted: []CommandCenterCompletion{},
			AskTargets:        []string{"maestro"},
			FleetSummaries:    []CommandCenterSummary{{Markdown: "summary"}},
		}
	}
	liveMenu, base, err := srv.commandCenterForPublication(publication, tracker)
	if err != nil {
		t.Fatal(err)
	}
	if liveMenu.Snapshot != nil || liveMenu.commandCenter != publication.commandCenter {
		t.Fatal("live helper exposed the owned DTO or replaced the shared derived owner")
	}
	first, err := srv.loadCommandCenterSnapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	first.Conductors[0].Name = "changed"
	first.Conductors[0].Sessions[0].Title = "changed"
	first.DecisionsWaiting[0].Question = "changed"
	first.AskTargets[0] = "changed"
	first.FleetSummaries[0].Markdown = "changed"
	first.RecentlyCompleted = append(first.RecentlyCompleted, CommandCenterCompletion{ID: "changed"})
	second, err := srv.loadCommandCenterSnapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	for label, snapshot := range map[string]*CommandCenterSnapshot{"owned base": base.snapshot, "second REST": second} {
		encoded, err := json.Marshal(snapshot)
		if err != nil || !bytes.Equal(encoded, base.encoded) {
			t.Fatalf("REST caller mutated %s: %s, %v", label, encoded, err)
		}
	}
	if srv.commandCenterBuilds.Load() != 1 || source.calls.Load() != 1 {
		t.Fatalf("wire/REST repeated work: builds=%d source=%d", srv.commandCenterBuilds.Load(), source.calls.Load())
	}
}

// matrix/hardened: use real conductor files and count the full base builder,
// proving both disk-artifact reads and synthesis are shared per publication.
func matrixWriteCCArtifacts(t *testing.T, dir, headline string) {
	t.Helper()
	dir = filepath.Join(dir, "agent-deck")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]string{"headline": headline})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "OPEN-ITEMS.md"), []byte("## D. DECISIONS\n- #120 "+headline+"\n## E. DONE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// matrix/hardened: sixteen live views and REST reuse one artifact build; an
// unchanged menu in the next generation must still pick up artifact changes.
func TestMatrixCommandCenterPublicationFanoutAndArtifactRefresh(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: ccTestMenu()}
	srv, _, _ := matrixPublisherIntegrationServer(t, source, "")
	artifactDir := t.TempDir()
	matrixWriteCCArtifacts(t, artifactDir, "before")
	srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
		return buildCommandCenterSnapshot(menu, srv.cfg.Profile, artifactDir, nil)
	}
	trackers := make([]*ccPublicationTracker, 16)
	streams := make([]<-chan menuPublication, len(trackers))
	for n := range trackers {
		trackers[n] = &ccPublicationTracker{}
		var release func()
		streams[n], release = srv.menuPublisher.Subscribe(time.Hour, trackers[n].observe)
		t.Cleanup(release)
	}
	publications := make([]menuPublication, len(streams))
	for n := range streams {
		publications[n] = matrixPublisherPublication(t, streams[n])
	}
	type result struct {
		cc  commandCenterPublication
		err error
	}
	results := make(chan result, len(streams))
	var workers sync.WaitGroup
	for n := range streams {
		workers.Add(1)
		go func(n int) {
			defer workers.Done()
			_, cc, err := srv.commandCenterForPublication(publications[n], trackers[n])
			results <- result{cc, err}
		}(n)
	}
	workers.Wait()
	close(results)
	var first commandCenterPublication
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first.snapshot == nil {
			first = got.cc
		}
		if got.cc.snapshot != first.snapshot || got.cc.fingerprint != first.fingerprint {
			t.Fatal("a connection independently synthesized an equivalent base")
		}
	}
	if _, err := srv.loadCommandCenterSnapshot(nil); err != nil {
		t.Fatal(err)
	}
	if srv.commandCenterBuilds.Load() != 1 || source.calls.Load() != 1 || first.snapshot.Conductors[0].CurrentlyWorkingOn != "before" {
		t.Fatalf("first fanout builds=%d source=%d snapshot=%+v", srv.commandCenterBuilds.Load(), source.calls.Load(), first.snapshot)
	}
	matrixWriteCCArtifacts(t, artifactDir, "after")
	srv.notifyMenuChanged()
	publication := matrixPublisherPublication(t, streams[0])
	if publication.Fingerprint != publications[0].Fingerprint {
		t.Fatal("fixture changed menu contents instead of artifact-only state")
	}
	_, next, err := srv.commandCenterForPublication(publication, trackers[0])
	if err != nil {
		t.Fatal(err)
	}
	if next.snapshot.Conductors[0].CurrentlyWorkingOn != "after" || len(next.snapshot.DecisionsWaiting) != 1 || next.snapshot.DecisionsWaiting[0].Question != "#120 after" {
		t.Fatalf("unchanged menu hid new artifact state: %+v", next.snapshot)
	}
	if next.fingerprint == first.fingerprint || srv.commandCenterBuilds.Load() != 2 || source.calls.Load() != 2 || first.snapshot.Conductors[0].CurrentlyWorkingOn != "before" {
		t.Fatalf("derived generation was reused or mutated: builds=%d source=%d", srv.commandCenterBuilds.Load(), source.calls.Load())
	}
	if _, err := srv.loadCommandCenterSnapshot(nil); err != nil {
		t.Fatal(err)
	}
	if srv.commandCenterBuilds.Load() != 2 {
		t.Fatal("REST repeated a live view's artifact build")
	}
}

// matrix/hardened: an old generation's slow artifact read cannot hold the
// publisher mutex or delay a new authoritative menu collection.
func TestMatrixCommandCenterPublicationSlowBuildDoesNotBlockCollector(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: ccTestMenu()}
	srv, _, _ := matrixPublisherIntegrationServer(t, source, "")
	entered, gate := make(chan struct{}), make(chan struct{})
	var first sync.Once
	srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
		first.Do(func() { close(entered); <-gate })
		return buildCommandCenterSnapshot(menu, srv.cfg.Profile, "", nil)
	}
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	oldDone := make(chan error, 1)
	go func() { _, err := srv.loadCommandCenterSnapshot(nil); oldDone <- err }()
	<-entered
	srv.notifyMenuChanged()
	newDone := make(chan error, 1)
	go func() { _, err := srv.menuPublisher.LoadMenuSnapshot(); newDone <- err }()
	select {
	case err := <-newDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("artifact build held up the menu collector")
	}
	close(gate)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if source.calls.Load() != 2 {
		t.Fatalf("source generations=%d, want 2", source.calls.Load())
	}
}

// matrix/hardened: identical menu IDs/content on separate server instances do
// not share profile/artifact-derived data or counters across publisher owners.
func TestMatrixCommandCenterPublicationServerIsolation(t *testing.T) {
	for n := 0; n < 2; n++ {
		source := &matrixPublisherIntegrationSource{snapshot: ccTestMenu()}
		srv, _, _ := matrixPublisherIntegrationServer(t, source, "")
		artifactDir := t.TempDir()
		headline := fmt.Sprintf("server-%d", n)
		matrixWriteCCArtifacts(t, artifactDir, headline)
		srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
			return buildCommandCenterSnapshot(menu, headline, artifactDir, nil)
		}
		for read := 0; read < 2; read++ {
			cc, err := srv.loadCommandCenterSnapshot(nil)
			if err != nil || cc.Profile != headline || cc.Conductors[0].CurrentlyWorkingOn != headline {
				t.Fatalf("server %d inherited another owner: %+v, %v", n, cc, err)
			}
		}
		if srv.commandCenterBuilds.Load() != 1 {
			t.Fatalf("server %d builds=%d, want 1", n, srv.commandCenterBuilds.Load())
		}
	}
}
