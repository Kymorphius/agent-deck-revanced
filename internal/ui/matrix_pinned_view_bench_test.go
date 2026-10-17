package ui

import (
	"fmt"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: measure the pinned view on inert, deterministic local
// sessions. No instance has a tmux handle, and fixture/setup work is outside
// the timed loops; these results cover synchronous list rebuilding, not terminal
// painting, process polling, storage, network, or background worker costs.
func matrixPinnedBenchmarkInstances(n int) []*session.Instance {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	instances := make([]*session.Instance, n)
	for i := range instances {
		groupIndex := i / 20
		inst := &session.Instance{
			ID:             fmt.Sprintf("matrix-pinned-bench-%05d", i),
			Title:          fmt.Sprintf("session-%05d", i),
			ProjectPath:    "/synthetic/matrix-pinned-bench",
			GroupPath:      fmt.Sprintf("root-%02d/team-%02d/project-%04d", groupIndex%8, groupIndex/8%4, groupIndex),
			Order:          i,
			Tool:           "shell",
			Status:         session.StatusIdle,
			CreatedAt:      now,
			LastAccessedAt: now,
		}
		if i%3 == 0 {
			inst.Status = session.StatusRunning
		}
		if i%20 == 0 { // 5% pinned, with both fixed pin positions represented.
			inst.Pin = session.PinTop
			if groupIndex%2 != 0 {
				inst.Pin = session.PinBottom
			}
		}
		if i%4 == 3 { // Include ordinary child rows without crossing group boundaries.
			inst.ParentSessionID = fmt.Sprintf("matrix-pinned-bench-%05d", i-1)
		}
		instances[i] = inst
	}
	return instances
}

// matrix/hardened: NewHome keeps production model defaults, while UI TestMain
// disables eager workers and isolates HOME/tmux. Remove storage and watcher
// references before measuring; no web menu is registered for this fixture.
func matrixPinnedBenchmarkHome(b *testing.B, n int, expanded bool, mode session.GroupViewMode) *Home {
	b.Helper()
	h := NewHome()
	b.Cleanup(h.cancel)
	if h.storageWatcher != nil {
		h.storageWatcher.Close()
		h.storageWatcher = nil
	}
	if h.storage != nil {
		if err := h.storage.Close(); err != nil {
			b.Fatal(err)
		}
		h.storage = nil
	}
	h.initialLoading = false
	h.debugMode = false
	h.width, h.height = 120, 40
	h.instances = matrixPinnedBenchmarkInstances(n)
	h.groupTree = session.NewGroupTree(h.instances)
	for path, group := range h.groupTree.Groups {
		group.Expanded = expanded
		h.groupTree.Expanded[path] = expanded
	}
	h.groupViewMode = mode
	return h
}

func BenchmarkMatrixPinnedRebuild(b *testing.B) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, expansion := range []struct {
		name     string
		expanded bool
	}{{"expanded", true}, {"collapsed", false}} {
		b.Run(expansion.name, func(b *testing.B) {
			for _, n := range []int{80, 1000, 10000} {
				b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
					for _, view := range []struct {
						name string
						mode session.GroupViewMode
					}{{"Normal", session.GroupViewNormal}, {"ActiveTop", session.GroupViewActiveTop}, {"Pinned", session.GroupViewPinnedOnly}} {
						b.Run(view.name, func(b *testing.B) {
							h := matrixPinnedBenchmarkHome(b, n, expansion.expanded, view.mode)
							b.ReportAllocs()
							for b.Loop() {
								h.rebuildFlatItemsAt(now)
							}
							if view.mode == session.GroupViewPinnedOnly && len(h.flatItems) != (n+19)/20 {
								b.Fatalf("pinned rows = %d, want %d", len(h.flatItems), (n+19)/20)
							}
							b.ReportMetric(float64(len(h.flatItems)), "rows/op")
						})
					}
				})
			}
		})
	}
}

// matrix/hardened: isolate pin collection to make allocation regressions
// visible even when the rest of Home's rebuild path dominates total work.
func BenchmarkMatrixPinnedItemsFromTree(b *testing.B) {
	for _, n := range []int{80, 1000, 10000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			tree := session.NewGroupTree(matrixPinnedBenchmarkInstances(n))
			var items []session.Item
			b.ReportAllocs()
			for b.Loop() {
				items = matrixPinnedItemsFromTree(tree)
			}
			if len(items) != (n+19)/20 {
				b.Fatalf("pinned rows = %d, want %d", len(items), (n+19)/20)
			}
			b.ReportMetric(float64(len(items)), "rows/op")
		})
	}
}
