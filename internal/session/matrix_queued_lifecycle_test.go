package session

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
	"github.com/stretchr/testify/require"
)

// matrix/hardened: queuing deliberately creates no pane. Status polling must
// retain that lifecycle state both before and after storage hydration.
func TestMatrixQueuedStatusPreservesAbsentPane(t *testing.T) {
	skipIfNoTmuxBinary(t)
	for _, recent := range []bool{true, false} {
		for _, reloaded := range []bool{false, true} {
			for _, withoutTmux := range []bool{false, true} {
				name := "old"
				if recent {
					name = "new"
				}
				if reloaded {
					name += "/reloaded"
				} else {
					name += "/in-process"
				}
				if withoutTmux {
					name += "/nil-tmux"
				}
				t.Run(name, func(t *testing.T) {
					inst := NewInstanceWithGroupAndTool("queued", t.TempDir(), "serial", "codex")
					inst.Status = StatusQueued
					if !recent {
						inst.CreatedAt = time.Now().Add(-time.Minute)
					}
					if withoutTmux {
						inst.tmuxSession = nil
					}
					if reloaded {
						tmuxName := ""
						if inst.tmuxSession != nil {
							tmuxName = inst.tmuxSession.Name
						}
						instances, _, err := (&Storage{}).convertToInstances(&StorageData{Instances: []*InstanceData{{
							ID: inst.ID, Title: inst.Title, ProjectPath: inst.ProjectPath,
							GroupPath: inst.GroupPath, Tool: inst.Tool, Status: inst.Status,
							CreatedAt: inst.CreatedAt, TmuxSession: tmuxName,
						}}})
						require.NoError(t, err)
						inst = instances[0]
						require.False(t, inst.addedThisProcess)
					}

					for poll := 0; poll < 2; poll++ {
						require.NoError(t, inst.UpdateStatus())
						require.Equal(t, StatusQueued, inst.GetStatusThreadSafe())
						require.Equal(t, SubstateNone, inst.CachedSubstate())
						require.Same(t, inst, FindNextQueued([]*Instance{inst}, "serial"))
					}
				})
			}
		}
	}
}

// matrix/hardened: a stale queued registry value must not mask a real pane,
// and launching a queued session must restore normal death classification.
func TestMatrixQueuedStatusReconcilesStartedPane(t *testing.T) {
	skipIfNoTmuxBinary(t)
	inst := NewInstanceWithGroupAndTool("queued-start", t.TempDir(), "serial", "codex")
	inst.Command = "exec sleep 60"
	inst.Status = StatusQueued
	inst.CreatedAt = time.Now().Add(-time.Minute)
	require.NoError(t, inst.UpdateStatus())
	require.Equal(t, StatusQueued, inst.GetStatusThreadSafe())

	// The queue drain calls Start, which must replace the queued state.
	require.NoError(t, inst.Start())
	t.Cleanup(func() { _ = inst.Kill() })
	require.NoError(t, inst.VerifySpawned(time.Second))
	require.NotEqual(t, StatusQueued, inst.GetStatusThreadSafe())

	// A legacy/concurrent queued write may describe an already-live pane.
	inst.Status = StatusQueued
	require.NoError(t, inst.UpdateStatus())
	require.NotEqual(t, StatusQueued, inst.GetStatusThreadSafe())
	require.NotEqual(t, StatusError, inst.GetStatusThreadSafe())

	// Once actually started, losing the pane is still an unknown exit.
	require.NoError(t, inst.tmuxSession.Kill())
	inst.lastStartTime = time.Now().Add(-time.Minute)
	tmux.RefreshExistingSessions()
	require.NoError(t, inst.UpdateStatus())
	require.Equal(t, StatusError, inst.GetStatusThreadSafe())
	require.Equal(t, SubstateUnknownExit, inst.CachedSubstate())
}
