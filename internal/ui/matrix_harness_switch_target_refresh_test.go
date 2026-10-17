package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: model a watcher that loaded a hidden pending target before
// the executor's atomic source/target commit. No harness or worker is started.
func matrixHarnessTargetRefreshHome(t *testing.T, finalize bool) (*Home, *session.Instance, *session.Instance, accountSwitchedMsg) {
	t.Helper()
	withTempAgentDeckHome(t, "")
	storage, err := session.NewStorageWithProfile(session.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	source := session.NewInstanceWithTool("Claude source", t.TempDir(), "claude")
	source.GroupPath, source.Account = "team/feature", "claude-source"
	source.ClaudeSessionID = "11111111-2222-3333-4444-555555555555"
	source.Notes = "original source notes"
	target := session.NewInstanceWithTool("Codex continuation", source.ProjectPath, "codex")
	target.GroupPath, target.Supersedes = source.GroupPath, source.ID
	target.ArchivedAt = time.Now().UTC()
	store := session.StorageCrossHarnessTargetStore{Storage: storage}
	for _, inst := range []*session.Instance{source, target} {
		if err := store.CreateTarget(inst); err != nil {
			t.Fatal(err)
		}
	}
	// Independent hydration is the key race: the row and receipt must not
	// accidentally share the executor's mutable target pointer.
	listed, err := storage.Load()
	if err != nil {
		t.Fatal(err)
	}
	h := NewGateHome(GateHomeOptions{Width: 120, Height: 40, Instances: listed, Profile: session.DefaultProfile}).home
	h.storage = storage
	h.switchGenerations = map[string]uint64{source.ID: 1}
	h.resumingSessions[source.ID] = time.Now()
	h.moveCursorToSession(source.ID)
	source, pending := h.instanceByID[source.ID], h.instanceByID[target.ID]
	target, err = store.LoadTarget(target.ID)
	if err != nil || target == nil {
		t.Fatalf("hydrate executor target: %v", err)
	}
	msg := accountSwitchedMsg{
		sessionID: source.ID, generation: 1,
		sourceTool: source.Tool, sourceAccount: source.Account,
		sourceProject: source.ProjectPath, sourceTitle: source.Title, sourceGroup: source.GroupPath,
		sourceCommand: source.Command, sourceStatus: source.Status,
		sourceClaudeID: source.ClaudeSessionID, sourceCodexID: source.CodexSessionID, sourceCWD: source.EffectiveWorkingDir(),
		targetHarness: "codex", account: "", target: target, targetReady: finalize,
		summary: "status=success; readiness=ready",
	}
	if finalize {
		target.CodexSessionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		if err := store.SaveTarget(target); err != nil {
			t.Fatal(err)
		}
		if err := store.FinalizeTargetHandoff(source, target); err != nil {
			t.Fatal(err)
		}
		// The production executor also clears its own target's archive flag
		// after the durable transaction; the watcher pointer remains hidden.
		target.ArchivedAt = time.Time{}
	}
	return h, source, pending, msg
}

// matrix/hardened: a ready result must replace the already-listed pending
// pointer everywhere the main list, search and preview read it.
func TestMatrixHarnessTargetRefreshExistingPendingRow(t *testing.T) {
	h, source, pending, msg := matrixHarnessTargetRefreshHome(t, true)
	h.previewCache[source.ID], h.previewCache[pending.ID] = "old source", "old target"
	h.previewCacheTime[source.ID], h.previewCacheTime[pending.ID] = time.Now(), time.Now()
	h.groupTree.CollapseGroup(source.GroupPath)
	_, _ = h.Update(msg)
	ready := h.getInstanceByID(pending.ID)
	if ready == pending || ready == msg.target || ready.IsArchived() || ready.CodexSessionID != msg.target.CodexSessionID {
		t.Fatalf("pending pointer was not replaced by the persisted ready target: same pending=%t, same receipt=%t, archived=%t, native=%q", ready == pending, ready == msg.target, ready.IsArchived(), ready.CodexSessionID)
	}
	if selected := h.getSelectedSession(); selected != ready {
		t.Fatalf("cursor did not select refreshed Codex row: selected=%v", selected)
	}
	activeSources, activeTargets := 0, 0
	for _, item := range h.flatItems {
		if item.Session == nil {
			continue
		}
		if item.Session.ID == source.ID {
			activeSources++
		}
		if item.Session.ID == ready.ID {
			activeTargets++
			if item.Session != ready {
				t.Fatal("flat row retained the pending target")
			}
		}
	}
	if activeSources != 0 || activeTargets != 1 {
		t.Fatalf("active rows: source=%d target=%d", activeSources, activeTargets)
	}
	for _, inst := range h.search.allItems {
		if inst.ID == ready.ID && inst != ready {
			t.Fatal("search retained the pending target")
		}
	}
	if h.previewCache[source.ID] != "" || h.previewCache[ready.ID] != "" {
		t.Fatal("handoff previews retained stale content")
	}
	archived := h.getInstanceByID(source.ID)
	if !archived.IsArchived() || archived.SupersededBy != ready.ID || archived.Tool != source.Tool || archived.Account != source.Account || archived.ClaudeSessionID != source.ClaudeSessionID || archived.Notes != source.Notes || ready.Account != "" {
		t.Fatal("source identity or target default account was not preserved")
	}
	if h.confirmDialog.noticeTitle != "Harness transfer verified" || h.resumingSessions[source.ID] != (time.Time{}) {
		t.Fatalf("ready completion presentation: %q", h.confirmDialog.noticeTitle)
	}
}

// matrix/hardened: a ready receipt must reveal a newly created target even
// when no pending-row watcher event arrived; it must also retire an older load.
func TestMatrixHarnessTargetRefreshAbsentRowAndOlderLoad(t *testing.T) {
	h, source, pending, msg := matrixHarnessTargetRefreshHome(t, true)
	h.instances = []*session.Instance{source}
	delete(h.instanceByID, pending.ID)
	h.groupTree.SyncWithInstances(h.instances)
	h.rebuildFlatItems()
	h.sessionLoadSequence = 7
	h.isReloading = true
	_, _ = h.Update(msg)
	ready := h.getInstanceByID(pending.ID)
	if ready == nil || ready.IsArchived() || h.getSelectedSession() != ready || h.isReloading {
		t.Fatal("ready target was not installed and selected after no pending-row event")
	}
	_, _ = h.Update(loadSessionsMsg{loadSequence: 7, instances: []*session.Instance{source, pending}})
	if h.getInstanceByID(pending.ID) != ready || h.getSelectedSession() != ready {
		t.Fatal("older registry load restored the hidden pending target")
	}
}

// matrix/hardened: the commit's archive/lineage change is expected source
// evolution; a watcher seeing it before the completion must not discard ready.
func TestMatrixHarnessTargetRefreshAcceptsCommittedSourceLineage(t *testing.T) {
	h, source, pending, msg := matrixHarnessTargetRefreshHome(t, true)
	store := session.StorageCrossHarnessTargetStore{Storage: h.storage}
	committed, err := store.LoadTarget(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, inst := range h.instances {
		if inst.ID == source.ID {
			h.instances[i] = committed
		}
	}
	h.instanceByID[source.ID] = committed
	_, _ = h.Update(msg)
	if ready := h.getInstanceByID(pending.ID); ready == pending || ready.IsArchived() || h.getSelectedSession() != ready {
		t.Fatal("committed source lineage caused a ready target to be discarded")
	}
}

// matrix/hardened: an explicitly verified continuation is selectable even
// when the source view was scoped to Claude, archived rows, or older activity.
func TestMatrixHarnessTargetRefreshRevealsFilteredTarget(t *testing.T) {
	for _, filter := range []string{"archived", "claude", "status", "group scope", "time"} {
		t.Run(filter, func(t *testing.T) {
			h, _, pending, msg := matrixHarnessTargetRefreshHome(t, true)
			h.keepEmptyFilter = true
			switch filter {
			case "archived":
				h.statusFilter = FilterModeArchived
			case "claude":
				h.statusFilter = "claude"
			case "status":
				h.statusFilter = "running"
			case "group scope":
				h.groupScope = "another/group"
			case "time":
				store := session.StorageCrossHarnessTargetStore{Storage: h.storage}
				latest, err := store.LoadTarget(pending.ID)
				if err != nil {
					t.Fatal(err)
				}
				latest.CreatedAt, latest.LastAccessedAt = time.Now().Add(-60*24*time.Hour), time.Time{}
				if err := store.SaveTarget(latest); err != nil {
					t.Fatal(err)
				}
				h.timeFilter = session.TimeFilterToday
			}
			h.rebuildFlatItems()
			_, _ = h.Update(msg)
			if ready := h.getInstanceByID(pending.ID); ready.IsArchived() || h.getSelectedSession() != ready {
				t.Fatalf("%s filter hid the verified Codex continuation", filter)
			}
		})
	}
}

// matrix/hardened: authoritative readback preserves newer target edits, a
// user's later rearchive, and unrelated unsaved session/group state.
func TestMatrixHarnessTargetRefreshPreservesLaterEdits(t *testing.T) {
	for _, rearchive := range []bool{false, true} {
		name := "rename and notes"
		if rearchive {
			name = "user rearchive"
		}
		t.Run(name, func(t *testing.T) {
			h, _, pending, msg := matrixHarnessTargetRefreshHome(t, true)
			store := session.StorageCrossHarnessTargetStore{Storage: h.storage}
			latest, err := store.LoadTarget(pending.ID)
			if err != nil {
				t.Fatal(err)
			}
			latest.Title, latest.Notes, latest.GroupPath = "Newer title", "Newer notes", "team/newer"
			if rearchive {
				latest.ArchivedAt = time.Now().UTC()
			}
			if err := store.SaveTarget(latest); err != nil {
				t.Fatal(err)
			}
			unrelated := session.NewInstanceWithTool("Unrelated unsaved title", t.TempDir(), "pi")
			unrelated.GroupPath = "unrelated"
			h.instances = append(h.instances, unrelated)
			h.instanceByID[unrelated.ID] = unrelated
			h.groupTree.AddSession(unrelated)
			h.groupTree.CollapseGroup(unrelated.GroupPath)
			_, _ = h.Update(msg)
			got := h.getInstanceByID(pending.ID)
			if got.Title != latest.Title || got.Notes != latest.Notes || got.GroupPath != latest.GroupPath || got.IsArchived() != rearchive {
				t.Fatal("completion overwrote the target's later edits")
			}
			if h.getInstanceByID(unrelated.ID) != unrelated || h.groupTree.Groups[unrelated.GroupPath].Expanded {
				t.Fatal("selective refresh discarded unrelated in-memory state")
			}
			if !rearchive && h.getSelectedSession() != got {
				t.Fatal("moved/renamed ready target was not selected")
			}
		})
	}
}

// matrix/hardened: genuine stale generations and source identity changes keep
// their existing protection, including the newer operation's spinner.
func TestMatrixHarnessTargetRefreshRejectsStaleCompletion(t *testing.T) {
	for _, change := range []string{"generation", "source title", "source account", "source cwd", "source native ID", "source removed"} {
		t.Run(change, func(t *testing.T) {
			h, source, pending, msg := matrixHarnessTargetRefreshHome(t, true)
			switch change {
			case "generation":
				h.switchGenerations[source.ID]++
			case "source title":
				source.Title = "Concurrent rename"
			case "source account":
				source.Account = "Concurrent account"
			case "source cwd":
				source.ProjectPath = t.TempDir()
			case "source native ID":
				source.ClaudeSessionID = "ffffffff-2222-3333-4444-555555555555"
			case "source removed":
				delete(h.instanceByID, source.ID)
			}
			_, _ = h.Update(msg)
			if h.getInstanceByID(pending.ID) != pending || !pending.IsArchived() || h.resumingSessions[source.ID].IsZero() || h.confirmDialog.IsVisible() {
				t.Fatal("stale completion changed rows, cleared the spinner or reported success")
			}
		})
	}
}

// matrix/hardened: pending/error outcomes leave both source and hidden target
// untouched; positive readiness on an error cannot reveal a target.
func TestMatrixHarnessTargetRefreshPendingAndErrorPreserveRows(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "error"}[failed], func(t *testing.T) {
			h, source, pending, msg := matrixHarnessTargetRefreshHome(t, false)
			if failed {
				msg.targetReady, msg.err = true, errors.New("post-observation persistence failure")
			} else {
				msg.pending, msg.err, msg.summary = true, session.ErrCrossHarnessPending, "status=pending; readiness=pending"
			}
			_, _ = h.Update(msg)
			if h.getInstanceByID(source.ID) != source || source.IsArchived() || source.SupersededBy != "" || h.getInstanceByID(pending.ID) != pending || !pending.IsArchived() || pending.CodexSessionID != "" {
				t.Fatal("pending/error completion altered the source or revealed the target")
			}
		})
	}
}

// matrix/hardened: a missing row or a changed native identity is a visible
// reconciliation failure; a stale receipt must never resurrect a deleted row.
func TestMatrixHarnessTargetRefreshRefusesMissingOrChangedStoredTarget(t *testing.T) {
	for _, change := range []string{"removed", "native identity"} {
		t.Run(change, func(t *testing.T) {
			h, _, pending, msg := matrixHarnessTargetRefreshHome(t, true)
			if change == "removed" {
				if err := h.storage.DeleteInstance(pending.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				store := session.StorageCrossHarnessTargetStore{Storage: h.storage}
				latest, err := store.LoadTarget(pending.ID)
				if err != nil {
					t.Fatal(err)
				}
				latest.CodexSessionID = "ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee"
				if err := store.SaveTarget(latest); err != nil {
					t.Fatal(err)
				}
			}
			_, _ = h.Update(msg)
			if h.getInstanceByID(pending.ID) != pending || !pending.IsArchived() || h.confirmDialog.noticeTitle != "Account switch failed" || !strings.Contains(h.confirmDialog.noticeBody, "refresh verified harness transfer") {
				t.Fatal("inconsistent persisted target was overwritten or hidden as success")
			}
		})
	}
}
