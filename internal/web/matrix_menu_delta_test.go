package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: enough unchanged rows make a whole-row runtime delta useful
// without relying on an artificial pre-encoded full payload or a size threshold.
func matrixMenuDeltaSnapshot() *MenuSnapshot {
	snapshot := matrixPublisherNestedSnapshot()
	snapshot.GeneratedAt = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	snapshot.Items[0].Index = 0
	snapshot.Items[0].Group.Expanded = true
	snapshot.Items[1].Index = 1
	snapshot.Items[1].Level = 1
	snapshot.Items[1].Path = "work"
	snapshot.Items[1].Session.GroupPath = "work"
	snapshot.Items[1].Session.Status = session.StatusRunning
	for index := 2; index < 18; index++ {
		snapshot.Items = append(snapshot.Items, MenuItem{
			Index: index, Type: MenuItemTypeSession, Level: 1, Path: "work",
			Session: &MenuSession{ID: fmt.Sprintf("stable-%d", index), Title: "unchanged worker",
				Status: session.StatusRunning, GroupPath: "work", Notes: strings.Repeat("stable context ", 16)},
		})
	}
	snapshot.TotalSessions = len(snapshot.Items) - 1
	return snapshot
}

// matrix/hardened: encode actual owner fields and preserve the existing legacy
// envelope, so helper tests detect mutations rather than rebuilding expectations.
func matrixMenuDeltaPublication(t *testing.T, snapshot *MenuSnapshot, version string) menuPublication {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dashboard := append([]byte(`{"type":"menu","data":`), encoded...)
	dashboard = append(dashboard, '}')
	return menuPublication{Snapshot: snapshot, Fingerprint: version, encoded: encoded, dashboardMenu: dashboard, commandCenter: &commandCenterDerived{}}
}

// matrix/hardened: fixtures own their nested values; changes to one side must
// never mutate the baseline and accidentally hide a removed field.
func matrixMenuDeltaCopy(t *testing.T, snapshot *MenuSnapshot) *MenuSnapshot {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var copy MenuSnapshot
	if err := json.Unmarshal(encoded, &copy); err != nil {
		t.Fatal(err)
	}
	return &copy
}

// matrix/hardened: decode the external JSON contract rather than trusting the
// producer's internal delta structs to have the same field tags as the browser.
type matrixMenuDeltaEnvelope struct {
	Type        string `json:"type"`
	Version     string `json:"version"`
	BaseVersion string `json:"baseVersion"`
	Data        struct {
		Profile       string    `json:"profile"`
		GeneratedAt   time.Time `json:"generatedAt"`
		TotalGroups   int       `json:"totalGroups"`
		TotalSessions int       `json:"totalSessions"`
		Changes       []struct {
			Index int      `json:"index"`
			Item  MenuItem `json:"item"`
		} `json:"changes"`
	} `json:"data"`
}

// matrix/hardened: whole-row replacement removes omitted nested fields and
// carries current metadata while treating versions as opaque JSON strings.
func TestMatrixMenuDeltaWholeRowRemovalMetadataAndOwnerImmutability(t *testing.T) {
	previous := matrixMenuDeltaSnapshot()
	previous.Items[1].Session.Hints["deleted"] = "removed hint"
	previous.Items[1].Session.Model = "removed-model"
	previous.Items[1].Session.Substate = "old-substate"
	current := matrixMenuDeltaCopy(t, previous)
	current.GeneratedAt = current.GeneratedAt.Add(time.Second)
	changed := current.Items[1].Session
	changed.Status = session.StatusWaiting
	changed.Title = "finished now"
	changed.Substate = ""
	changed.Model = ""
	delete(changed.Hints, "deleted")
	changed.Tags = nil
	changed.ToolOptionsJSON = nil
	changed.Sandbox = nil
	changed.GeminiYoloMode = nil
	changed.GeminiAnalytics = nil
	beforePrevious, beforeCurrent := matrixMenuDeltaCopy(t, previous), matrixMenuDeltaCopy(t, current)
	const baseVersion = "18446744073709551617:baseline"
	const nextVersion = "18446744073709551618:quoted-\"版本"
	owner := matrixMenuDeltaPublication(t, current, nextVersion)
	beforeEncoded, beforeLegacy := append([]byte(nil), owner.encoded...), append([]byte(nil), owner.dashboardMenu...)
	publication := withVersionedMenu(previous, baseVersion, owner)
	if len(publication.menuDelta) == 0 || len(publication.menuDelta) >= len(publication.versionedMenu) || publication.deltaBaseVersion != baseVersion {
		t.Fatalf("useful runtime delta missing: delta=%d full=%d base=%q", len(publication.menuDelta), len(publication.versionedMenu), publication.deltaBaseVersion)
	}
	var delta matrixMenuDeltaEnvelope
	if err := json.Unmarshal(publication.menuDelta, &delta); err != nil {
		t.Fatal(err)
	}
	if delta.Type != "menu-delta" || delta.Version != nextVersion || delta.BaseVersion != baseVersion || delta.Data.Profile != current.Profile || !delta.Data.GeneratedAt.Equal(current.GeneratedAt) || delta.Data.TotalGroups != current.TotalGroups || delta.Data.TotalSessions != current.TotalSessions || len(delta.Data.Changes) != 1 || delta.Data.Changes[0].Index != 1 || !reflect.DeepEqual(delta.Data.Changes[0].Item, current.Items[1]) {
		t.Fatalf("delta lost a complete changed row, metadata or opaque version: %+v", delta)
	}
	// Apply only whole-row replacement, matching the browser's operation. The
	// removed sandbox/maps/slices must not survive via a recursive JSON merge.
	patched := matrixMenuDeltaCopy(t, previous)
	for _, change := range delta.Data.Changes {
		patched.Items[change.Index] = change.Item
	}
	patched.GeneratedAt = delta.Data.GeneratedAt
	if !reflect.DeepEqual(patched, current) {
		t.Fatal("whole-row delta retained removed nested state")
	}
	var full struct {
		Type    string       `json:"type"`
		Version string       `json:"version"`
		Data    MenuSnapshot `json:"data"`
	}
	if err := json.Unmarshal(publication.versionedMenu, &full); err != nil || full.Type != "menu" || full.Version != nextVersion || !reflect.DeepEqual(&full.Data, current) {
		t.Fatalf("versioned full envelope changed fields/version: %+v, %v", full, err)
	}
	if !reflect.DeepEqual(previous, beforePrevious) || !reflect.DeepEqual(current, beforeCurrent) || publication.Snapshot != owner.Snapshot || publication.commandCenter != owner.commandCenter || &publication.encoded[0] != &owner.encoded[0] || &publication.dashboardMenu[0] != &owner.dashboardMenu[0] || !bytes.Equal(publication.encoded, beforeEncoded) || !bytes.Equal(publication.dashboardMenu, beforeLegacy) {
		t.Fatal("delta construction mutated or copied immutable legacy/owner fields")
	}
}

// matrix/hardened: structural changes and malformed identities/layout never
// patch an existing browser list, even when a runtime change could be smaller.
func TestMatrixMenuDeltaStructuralAndInvalidRowsFallBackToFull(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*MenuSnapshot, *MenuSnapshot)
	}{
		{"add", func(_, next *MenuSnapshot) {
			item := next.Items[2]
			item.Index = len(next.Items)
			copy := *item.Session
			copy.ID = "new-worker"
			item.Session = &copy
			next.Items = append(next.Items, item)
			next.TotalSessions++
		}},
		{"delete", func(_, next *MenuSnapshot) { next.Items = next.Items[:len(next.Items)-1]; next.TotalSessions-- }},
		{"reorder", func(_, next *MenuSnapshot) {
			next.Items[1], next.Items[2] = next.Items[2], next.Items[1]
			next.Items[1].Index, next.Items[2].Index = 1, 2
		}},
		{"profile", func(_, next *MenuSnapshot) { next.Profile = "different-profile" }},
		{"group-count", func(_, next *MenuSnapshot) { next.TotalGroups++ }},
		{"session-count", func(_, next *MenuSnapshot) { next.TotalSessions++ }},
		{"both-wrong-group-count", func(old, next *MenuSnapshot) { old.TotalGroups++; next.TotalGroups++ }},
		{"both-wrong-session-count", func(old, next *MenuSnapshot) { old.TotalSessions--; next.TotalSessions-- }},
		{"negative-count", func(_, next *MenuSnapshot) { next.TotalSessions = -1 }},
		{"group-identity", func(_, next *MenuSnapshot) { next.Items[0].Group.Path = "different-group" }},
		{"session-identity", func(_, next *MenuSnapshot) { next.Items[1].Session.ID = "different-worker" }},
		{"duplicate-session", func(old, next *MenuSnapshot) {
			old.Items[2].Session.ID = old.Items[1].Session.ID
			next.Items[2].Session.ID = next.Items[1].Session.ID
		}},
		{"duplicate-group", func(old, next *MenuSnapshot) {
			old.Items[2] = MenuItem{Index: 2, Type: MenuItemTypeGroup, Group: &MenuGroup{Path: old.Items[0].Group.Path}}
			next.Items[2] = MenuItem{Index: 2, Type: MenuItemTypeGroup, Group: &MenuGroup{Path: next.Items[0].Group.Path}}
			old.TotalGroups++
			next.TotalGroups++
			old.TotalSessions--
			next.TotalSessions--
		}},
		{"empty-session-id", func(_, next *MenuSnapshot) { next.Items[1].Session.ID = " " }},
		{"empty-group-id", func(_, next *MenuSnapshot) { next.Items[0].Group.Path = "" }},
		{"nul-identity", func(_, next *MenuSnapshot) { next.Items[1].Session.ID = "bad\x00id" }},
		{"mixed-identity", func(_, next *MenuSnapshot) { next.Items[1].Group = &MenuGroup{Path: "mixed"} }},
		{"missing-session", func(_, next *MenuSnapshot) { next.Items[1].Session = nil }},
		{"missing-group", func(_, next *MenuSnapshot) { next.Items[0].Group = nil }},
		{"unknown-type", func(_, next *MenuSnapshot) { next.Items[1].Type = "unknown" }},
		{"wrong-current-index", func(_, next *MenuSnapshot) { next.Items[1].Index = 2 }},
		{"wrong-baseline-index", func(old, _ *MenuSnapshot) { old.Items[1].Index = 2 }},
		{"level", func(_, next *MenuSnapshot) { next.Items[1].Level++ }},
		{"path", func(_, next *MenuSnapshot) { next.Items[1].Path = "different-path" }},
		{"last-in-group", func(_, next *MenuSnapshot) { next.Items[1].IsLastInGroup = true }},
		{"subsession", func(_, next *MenuSnapshot) { next.Items[1].IsSubSession = true }},
		{"last-subsession", func(_, next *MenuSnapshot) { next.Items[1].IsLastSubSession = true }},
		{"parent-last", func(_, next *MenuSnapshot) { next.Items[1].ParentIsLastInGroup = true }},
		{"group-order", func(_, next *MenuSnapshot) { next.Items[0].Group.Order++ }},
		{"group-expanded", func(_, next *MenuSnapshot) { next.Items[0].Group.Expanded = false }},
		{"session-group", func(_, next *MenuSnapshot) { next.Items[1].Session.GroupPath = "other-group" }},
		{"parent-session", func(_, next *MenuSnapshot) { next.Items[1].Session.ParentSessionID = "different-parent" }},
		{"session-order", func(_, next *MenuSnapshot) { next.Items[1].Session.Order++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := matrixMenuDeltaSnapshot()
			current := matrixMenuDeltaCopy(t, previous)
			current.Items[1].Session.Status = session.StatusWaiting
			test.change(previous, current)
			publication := withVersionedMenu(previous, "base", matrixMenuDeltaPublication(t, current, "next"))
			if len(publication.versionedMenu) == 0 || publication.menuDelta != nil || publication.deltaBaseVersion != "" {
				t.Fatalf("unsafe structural delta was produced: delta=%s base=%q", publication.menuDelta, publication.deltaBaseVersion)
			}
		})
	}
}

// matrix/hardened: Command Center upgrades a queued publication to current
// state atomically; menu selection must use that same version, not queued bytes
// whose older delta happens to match the browser's acknowledged baseline.
func TestMatrixMenuDeltaQueuedPublicationUpgradeUsesCurrentGapFallback(t *testing.T) {
	firstSnapshot := matrixMenuDeltaSnapshot()
	source := &matrixPublisherIntegrationSource{snapshot: firstSnapshot}
	publisher := newMenuSnapshotPublisher(context.Background(), source.LoadMenuSnapshot, time.Hour)
	defer publisher.Close()
	updates, release := publisher.subscribeWire(time.Hour, nil)
	defer release()
	first := matrixPublisherPublication(t, updates)
	secondSnapshot := matrixMenuDeltaCopy(t, firstSnapshot)
	secondSnapshot.Items[1].Session.Title = "queued-B"
	secondSnapshot.Items[1].Session.Status = session.StatusWaiting
	source.replace(secondSnapshot, nil)
	publisher.Invalidate()
	queued := matrixPublisherPublication(t, updates)
	if queued.deltaBaseVersion != first.Fingerprint || len(queued.menuDelta) == 0 {
		t.Fatal("queued B fixture did not have a usable delta from acknowledged A")
	}
	thirdSnapshot := matrixMenuDeltaCopy(t, secondSnapshot)
	thirdSnapshot.Items[1].Session.Title = "current-C"
	thirdSnapshot.Items[1].Session.Status = session.StatusRunning
	source.replace(thirdSnapshot, nil)
	publisher.Invalidate()
	current := matrixPublisherPublication(t, updates)
	if current.deltaBaseVersion != queued.Fingerprint || len(current.menuDelta) == 0 {
		t.Fatal("current C fixture did not require a gap fallback from A")
	}
	srv := &Server{menuPublisher: publisher, commandCenterBuilder: func(menu *MenuSnapshot) *CommandCenterSnapshot {
		return buildCommandCenterSnapshot(menu, menu.Profile, "", nil)
	}}
	upgraded, cc, err := srv.commandCenterForPublication(queued, &ccPublicationTracker{})
	if err != nil || upgraded.Fingerprint != current.Fingerprint || upgraded.deltaBaseVersion != queued.Fingerprint || upgraded.Snapshot != nil || cc.snapshot == nil {
		t.Fatalf("CC did not upgrade queued B to current C: menu=%#v cc=%#v error=%v", upgraded, cc, err)
	}
	foundCurrent := false
	for _, conductor := range cc.snapshot.Conductors {
		for _, worker := range conductor.Sessions {
			if worker.ID == thirdSnapshot.Items[1].Session.ID && worker.Title == "current-C" {
				foundCurrent = true
			}
		}
	}
	if !foundCurrent {
		t.Fatal("CC snapshot retained queued B instead of current C")
	}
	last := first.Fingerprint
	var sent []byte
	if err := writeDashboardMenu(upgraded, true, &last, func(encoded []byte) error { sent = encoded; return nil }); err != nil || last != current.Fingerprint || !bytes.Equal(sent, current.versionedMenu) || bytes.Equal(sent, queued.menuDelta) {
		t.Fatalf("upgraded menu incorrectly used queued B's matching delta: last=%q error=%v", last, err)
	}
	var full struct {
		Type    string       `json:"type"`
		Version string       `json:"version"`
		Data    MenuSnapshot `json:"data"`
	}
	if err := json.Unmarshal(sent, &full); err != nil || full.Type != "menu" || full.Version != current.Fingerprint || !reflect.DeepEqual(&full.Data, thirdSnapshot) || source.calls.Load() != 3 {
		t.Fatalf("gap fallback did not send the current full menu: %+v, %v", full, err)
	}
}

// matrix/hardened: initial/missing/unchanged baselines and an unprofitable
// single-row patch choose full data rather than emitting misleading deltas.
func TestMatrixMenuDeltaMissingUnchangedAndLargerPatchFallBack(t *testing.T) {
	previous := matrixMenuDeltaSnapshot()
	current := matrixMenuDeltaCopy(t, previous)
	current.Items[1].Session.Status = session.StatusWaiting
	for _, test := range []struct {
		name, baseVersion, nextVersion string
		previous                       *MenuSnapshot
	}{
		{"first", "", "next", nil},
		{"missing-version", "", "next", previous},
		{"same-version", "same", "same", previous},
	} {
		t.Run(test.name, func(t *testing.T) {
			publication := withVersionedMenu(test.previous, test.baseVersion, matrixMenuDeltaPublication(t, current, test.nextVersion))
			if len(publication.versionedMenu) == 0 || publication.menuDelta != nil || publication.deltaBaseVersion != "" {
				t.Fatal("missing/unchanged baseline produced a delta")
			}
		})
	}
	publication := withVersionedMenu(previous, "base", matrixMenuDeltaPublication(t, matrixMenuDeltaCopy(t, previous), "next"))
	if publication.menuDelta != nil {
		t.Fatal("unchanged rows produced an empty delta")
	}
	smallPrevious := &MenuSnapshot{Profile: "small", TotalSessions: 1, Items: []MenuItem{{Index: 0, Type: MenuItemTypeSession, Session: &MenuSession{ID: "one", Status: session.StatusRunning}}}}
	smallCurrent := matrixMenuDeltaCopy(t, smallPrevious)
	smallCurrent.Items[0].Session.Status = session.StatusWaiting
	publication = withVersionedMenu(smallPrevious, "base", matrixMenuDeltaPublication(t, smallCurrent, "next"))
	if publication.menuDelta != nil || publication.deltaBaseVersion != "" || len(publication.versionedMenu) == 0 {
		t.Fatal("single changed row produced a patch larger than the full menu")
	}
}

// matrix/hardened: legacy frames retain their exact bytes, while opt-in
// connections only use a delta after successfully receiving its exact baseline.
func TestMatrixMenuDeltaWriterSelectionAndFailedWriteVersion(t *testing.T) {
	previous := matrixMenuDeltaSnapshot()
	current := matrixMenuDeltaCopy(t, previous)
	current.Items[1].Session.Status = session.StatusWaiting
	publication := withVersionedMenu(previous, "opaque-base", matrixMenuDeltaPublication(t, current, "opaque-next"))
	if len(publication.menuDelta) == 0 {
		t.Fatal("writer fixture did not create a useful delta")
	}
	for _, test := range []struct {
		name, last, selected string
		optIn                bool
		fail                 bool
	}{
		{"legacy-first", "", "legacy", false, false},
		{"legacy-baseline", "opaque-base", "legacy", false, false},
		{"opt-in-first", "", "full", true, false},
		{"opt-in-gap", "missing-version", "full", true, false},
		{"opt-in-baseline", "opaque-base", "delta", true, false},
		{"already-sent", "opaque-next", "none", true, false},
		{"legacy-failed", "opaque-base", "legacy", false, true},
		{"delta-failed", "opaque-base", "delta", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			last, calls := test.last, 0
			wantErr := errors.New("synthetic network write failure")
			var sent []byte
			write := func(encoded []byte) error {
				calls++
				sent = encoded
				if test.fail {
					return wantErr
				}
				return nil
			}
			err := writeDashboardMenu(publication, test.optIn, &last, write)
			var expected []byte
			switch test.selected {
			case "legacy":
				expected = publication.dashboardMenu
			case "full":
				expected = publication.versionedMenu
			case "delta":
				expected = publication.menuDelta
			}
			if test.selected == "none" {
				if calls != 0 || err != nil || last != test.last {
					t.Fatal("already-sent version invoked writer or advanced")
				}
				return
			}
			if calls != 1 || !bytes.Equal(sent, expected) || &sent[0] != &expected[0] {
				t.Fatal("writer did not select the existing immutable frame")
			}
			if test.fail {
				if !errors.Is(err, wantErr) || last != test.last {
					t.Fatalf("failed write advanced baseline: last=%q error=%v", last, err)
				}
				if err := writeDashboardMenu(publication, test.optIn, &last, func(encoded []byte) error {
					if !bytes.Equal(encoded, expected) {
						t.Error("retry changed frame without an acknowledged baseline")
					}
					return nil
				}); err != nil || last != publication.Fingerprint {
					t.Fatalf("retry failed to advance after success: last=%q error=%v", last, err)
				}
			} else if err != nil || last != publication.Fingerprint {
				t.Fatalf("successful write did not advance baseline: last=%q error=%v", last, err)
			}
		})
	}
	noDelta := publication
	noDelta.menuDelta = nil
	last := "opaque-base"
	if err := writeDashboardMenu(noDelta, true, &last, func(encoded []byte) error {
		if !bytes.Equal(encoded, noDelta.versionedMenu) {
			t.Error("missing delta did not fall back to versioned full")
		}
		return nil
	}); err != nil || last != "opaque-next" {
		t.Fatalf("full fallback write=%v last=%q", err, last)
	}
}

// matrix/hardened: equal-content polls preserve the last different content as
// a useful baseline, while metadata uses the newest successful observation.
func TestMatrixMenuDeltaPublisherUnchangedPollKeepsUsefulBaseline(t *testing.T) {
	firstSnapshot := matrixMenuDeltaSnapshot()
	source := &matrixPublisherIntegrationSource{snapshot: firstSnapshot}
	publisher := newMenuSnapshotPublisher(context.Background(), source.LoadMenuSnapshot, time.Hour)
	defer publisher.Close()
	updates, release := publisher.subscribeWire(time.Hour, nil)
	defer release()
	first := matrixPublisherPublication(t, updates)
	changed := matrixMenuDeltaCopy(t, firstSnapshot)
	changed.Items[1].Session.Status = session.StatusWaiting
	changed.GeneratedAt = changed.GeneratedAt.Add(time.Second)
	source.replace(changed, nil)
	publisher.Invalidate()
	second := matrixPublisherPublication(t, updates)
	if second.Fingerprint == first.Fingerprint || second.deltaBaseVersion != first.Fingerprint || len(second.menuDelta) == 0 {
		t.Fatal("changed publication did not establish the previous content baseline")
	}
	unchanged := matrixMenuDeltaCopy(t, changed)
	unchanged.GeneratedAt = unchanged.GeneratedAt.Add(time.Second)
	source.replace(unchanged, nil)
	publisher.Invalidate()
	third := matrixPublisherPublication(t, updates)
	var delta matrixMenuDeltaEnvelope
	if err := json.Unmarshal(third.menuDelta, &delta); err != nil || third.Fingerprint != second.Fingerprint || third.deltaBaseVersion != first.Fingerprint || len(delta.Data.Changes) != 1 || !delta.Data.GeneratedAt.Equal(unchanged.GeneratedAt) {
		t.Fatalf("unchanged poll lost useful delta/current metadata: %+v, %v", delta, err)
	}
	publisher.mu.Lock()
	content, previousContent := publisher.content, publisher.previousContent
	publisher.mu.Unlock()
	if content.version != second.Fingerprint || previousContent.version != first.Fingerprint || previousContent.snapshot.Items[1].Session.Status != session.StatusRunning || source.calls.Load() != 3 {
		t.Fatal("unchanged poll replaced the last different baseline or collected twice")
	}
}

// matrix/hardened: a rejected in-flight generation cannot change either the
// accepted content owner or the baseline chosen for the next successful delta.
func TestMatrixMenuDeltaPublisherInflightInvalidationPreservesAcceptedBaseline(t *testing.T) {
	started, unblock := make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	defer unblockOnce.Do(func() { close(unblock) })
	var calls, obsoleteObservations atomic.Int32
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		snapshot := matrixMenuDeltaSnapshot()
		switch calls.Add(1) {
		case 1:
		case 2:
			close(started)
			<-unblock
			snapshot.Items[1].Session.Title = "obsolete-generation"
		default:
			snapshot.Items[1].Session.Title = "fresh-generation"
			snapshot.Items[1].Session.Status = session.StatusWaiting
		}
		return snapshot, nil
	}, time.Hour)
	defer publisher.Close()
	updates, release := publisher.subscribeWire(time.Hour, func(snapshot *MenuSnapshot) {
		if snapshot.Items[1].Session.Title == "obsolete-generation" {
			obsoleteObservations.Add(1)
		}
	})
	defer release()
	first := matrixPublisherPublication(t, updates)
	publisher.Invalidate()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("second load did not start")
	}
	publisher.Invalidate()
	unblockOnce.Do(func() { close(unblock) })
	fresh := matrixPublisherPublication(t, updates)
	var delta matrixMenuDeltaEnvelope
	if err := json.Unmarshal(fresh.menuDelta, &delta); err != nil || fresh.deltaBaseVersion != first.Fingerprint || delta.BaseVersion != first.Fingerprint || len(delta.Data.Changes) != 1 || delta.Data.Changes[0].Item.Session.Title != "fresh-generation" || obsoleteObservations.Load() != 0 {
		t.Fatalf("obsolete generation became a delta baseline/publication: %+v, %v", delta, err)
	}
	publisher.mu.Lock()
	content, previousContent := publisher.content, publisher.previousContent
	publisher.mu.Unlock()
	if content.version != fresh.Fingerprint || previousContent.version != first.Fingerprint || previousContent.snapshot.Items[1].Session.Title == "obsolete-generation" || calls.Load() != 3 {
		t.Fatal("rejected generation modified accepted content retention")
	}
}
