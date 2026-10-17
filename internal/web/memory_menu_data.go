package web

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// MenuSessionState is a lightweight status/tool update for one session.
type MenuSessionState struct {
	Status session.Status
	Tool   string
}

// MemoryMenuData stores one menu snapshot for web mode.
// SetSnapshot gives snapshot ownership to the in-process publisher.
// A fallback-owned snapshot uses a storage revision to find external changes.
type MemoryMenuData struct {
	// mu guards the snapshot and all fields that describe its storage revision.
	// A fallback load holds this mutex to give all callers one complete snapshot.
	mu                   sync.Mutex
	snapshot             *MenuSnapshot
	fallback             MenuDataLoader
	snapshotFromFallback bool
	fallbackRevision     int64
	lastRevisionCheck    time.Time
	lastFallbackError    error
}

const (
	memoryMenuRevisionCheckInterval  = time.Second
	memoryMenuConsistentLoadAttempts = 3
)

// menuDataRevisionLoader gives the revision of persisted menu data.
// Each persisted menu change must update this revision.
type menuDataRevisionLoader interface {
	MenuDataRevision() (int64, error)
}

// liveStateMenuDataLoader marks fallbacks whose snapshots include process state
// that can change without a persisted menu-data revision.
type liveStateMenuDataLoader interface {
	refreshesLiveState() bool
}

// NewMemoryMenuData creates an in-memory menu data store.
func NewMemoryMenuData(fallback MenuDataLoader) *MemoryMenuData {
	return &MemoryMenuData{
		fallback: fallback,
	}
}

// matrix/hardened: runtime source configuration follows the storage fallback;
// wrappers must not accidentally re-enable a second hook/status producer.
func (m *MemoryMenuData) SetRuntimeObservationSource(source session.RuntimeObservationSource) {
	if m == nil {
		return
	}
	if consumer, ok := m.fallback.(interface {
		SetRuntimeObservationSource(session.RuntimeObservationSource)
	}); ok {
		consumer.SetRuntimeObservationSource(source)
	}
	m.InvalidateCache()
}

func (m *MemoryMenuData) RuntimeObservationConfigured() bool {
	if m == nil {
		return false
	}
	if consumer, ok := m.fallback.(interface{ RuntimeObservationConfigured() bool }); ok {
		return consumer.RuntimeObservationConfigured()
	}
	return false
}

func (m *MemoryMenuData) RefreshRuntimeObservations(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if consumer, ok := m.fallback.(interface{ RefreshRuntimeObservations(context.Context) error }); ok {
		return consumer.RefreshRuntimeObservations(ctx)
	}
	return nil
}

// LoadMenuSnapshot returns the latest complete snapshot. It rate-limits
// storage checks and live-state reloads for fallback-owned snapshots.
func (m *MemoryMenuData) LoadMenuSnapshot() (snapshot *MenuSnapshot, err error) {
	if m == nil {
		return nil, fmt.Errorf("menu snapshot is unavailable")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// matrix/hardened: a mutation controller's local publication must never
	// mask subsequent shared observations. The outer publisher coalesces
	// requests; complete runtime state remains owned by its configured source.
	if m.RuntimeObservationConfigured() {
		// matrix/hardened: this owner fences registry/runtime itself and returns
		// a private DTO, avoiding the outer three-open revision/load/revision cycle.
		if owned, ok := m.fallback.(*SessionDataService); ok && owned.runtimeMenuCatalogueEnabled() {
			return owned.LoadMenuSnapshot()
		}
		return m.loadFallbackSnapshotLocked()
	}

	// Coalesce failed reads as well as successful revision checks. Keep the
	// error visible during the retry interval instead of serving stale data
	// as a successful refresh. Explicit invalidation or publishing resets it.
	if m.lastFallbackError != nil && time.Since(m.lastRevisionCheck) < memoryMenuRevisionCheckInterval {
		return nil, m.lastFallbackError
	}
	defer func() {
		m.lastFallbackError = err
		if err != nil {
			m.lastRevisionCheck = time.Now()
		}
	}()

	if m.snapshot == nil {
		return m.loadFallbackSnapshotLocked()
	}
	if !m.snapshotFromFallback {
		return cloneMenuSnapshot(m.snapshot), nil
	}

	revisionLoader, ok := m.fallback.(menuDataRevisionLoader)
	if !ok || time.Since(m.lastRevisionCheck) < memoryMenuRevisionCheckInterval {
		return cloneMenuSnapshot(m.snapshot), nil
	}
	if liveState, ok := m.fallback.(liveStateMenuDataLoader); ok && liveState.refreshesLiveState() {
		return m.loadFallbackSnapshotLocked()
	}

	revision, err := revisionLoader.MenuDataRevision()
	if err != nil {
		return nil, fmt.Errorf("load menu data revision: %w", err)
	}
	m.lastRevisionCheck = time.Now()
	if revision == m.fallbackRevision {
		return cloneMenuSnapshot(m.snapshot), nil
	}

	return m.loadFallbackSnapshotLocked()
}

// loadFallbackSnapshotLocked loads one snapshot with a stable storage revision.
// The caller must hold m.mu.
func (m *MemoryMenuData) loadFallbackSnapshotLocked() (*MenuSnapshot, error) {
	if m.fallback == nil {
		return nil, fmt.Errorf("menu snapshot is unavailable")
	}

	revisionLoader, versioned := m.fallback.(menuDataRevisionLoader)
	if !versioned {
		snapshot, err := m.fallback.LoadMenuSnapshot()
		if err != nil {
			return nil, err
		}
		m.setFallbackSnapshotLocked(snapshot, 0)
		return cloneMenuSnapshot(m.snapshot), nil
	}

	revision, err := revisionLoader.MenuDataRevision()
	if err != nil {
		return nil, fmt.Errorf("load menu data revision: %w", err)
	}
	for attempt := 0; attempt < memoryMenuConsistentLoadAttempts; attempt++ {
		snapshot, err := m.fallback.LoadMenuSnapshot()
		if err != nil {
			return nil, err
		}
		after, err := revisionLoader.MenuDataRevision()
		if err != nil {
			return nil, fmt.Errorf("load menu data revision: %w", err)
		}
		if revision == after {
			m.setFallbackSnapshotLocked(snapshot, after)
			return cloneMenuSnapshot(m.snapshot), nil
		}
		revision = after
	}

	return nil, fmt.Errorf("menu data changed during %d consecutive snapshot loads", memoryMenuConsistentLoadAttempts)
}

func (m *MemoryMenuData) setFallbackSnapshotLocked(snapshot *MenuSnapshot, revision int64) {
	m.snapshot = cloneMenuSnapshot(snapshot)
	m.snapshotFromFallback = true
	m.fallbackRevision = revision
	m.lastRevisionCheck = time.Now()
}

// LoadArchivedMenuSnapshot returns archived sessions from the storage fallback.
func (m *MemoryMenuData) LoadArchivedMenuSnapshot() (*MenuSnapshot, error) {
	if m == nil || m.fallback == nil {
		return nil, fmt.Errorf("menu snapshot is unavailable")
	}
	if loader, ok := m.fallback.(interface {
		LoadArchivedMenuSnapshot() (*MenuSnapshot, error)
	}); ok {
		return loader.LoadArchivedMenuSnapshot()
	}
	return nil, fmt.Errorf("archived session list is not available")
}

// InvalidateCache clears the cached snapshot so the next call to
// LoadMenuSnapshot reloads from the fallback storage-backed loader.
// Used after mutations in headless (--no-tui) mode to ensure the menu
// reflects the current persisted state on the next fetch.
func (m *MemoryMenuData) InvalidateCache() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.snapshot = nil
	m.snapshotFromFallback = false
	m.fallbackRevision = 0
	m.lastRevisionCheck = time.Time{}
	m.lastFallbackError = nil
	m.mu.Unlock()
}

// SetSnapshot replaces the stored menu snapshot.
func (m *MemoryMenuData) SetSnapshot(snapshot *MenuSnapshot) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.snapshot = cloneMenuSnapshot(snapshot)
	m.snapshotFromFallback = false
	m.fallbackRevision = 0
	m.lastRevisionCheck = time.Time{}
	m.lastFallbackError = nil
	m.mu.Unlock()
}

// UpdateSessionStates updates status/tool fields in-place for existing sessions.
func (m *MemoryMenuData) UpdateSessionStates(states map[string]MenuSessionState, generatedAt time.Time) {
	if m == nil || len(states) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.snapshot == nil {
		return
	}

	for i := range m.snapshot.Items {
		item := &m.snapshot.Items[i]
		if item.Type != MenuItemTypeSession || item.Session == nil {
			continue
		}
		state, ok := states[item.Session.ID]
		if !ok {
			continue
		}

		item.Session.Status = state.Status
		if state.Tool != "" {
			item.Session.Tool = state.Tool
		}
	}

	if generatedAt.IsZero() {
		generatedAt = time.Now()
	}
	m.snapshot.GeneratedAt = generatedAt.UTC()
}

func cloneMenuSnapshot(snapshot *MenuSnapshot) *MenuSnapshot {
	if snapshot == nil {
		return nil
	}

	cloned := *snapshot
	cloned.Items = make([]MenuItem, len(snapshot.Items))

	for i, item := range snapshot.Items {
		cloned.Items[i] = item
		if item.Group != nil {
			groupCopy := *item.Group
			cloned.Items[i].Group = &groupCopy
		}
		if item.Session != nil {
			sessionCopy := *item.Session
			cloned.Items[i].Session = &sessionCopy
		}
	}

	return &cloned
}
