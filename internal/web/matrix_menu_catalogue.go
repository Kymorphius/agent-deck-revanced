package web

// matrix/hardened: registry-only catalogue ownership is separate from runtime
// observations. A source failure never turns the previous overlay into success.
import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// MenuCatalogueStats exposes bounded counters, never registry data or paths.
type MenuCatalogueStats struct {
	Loads         uint64 `json:"loads"`
	Opens         uint64 `json:"opens"`
	Reloads       uint64 `json:"reloads"`
	RegistryReads uint64 `json:"registryReads"`
	Hits          uint64 `json:"hits"`
	Errors        uint64 `json:"errors"`
	Replacements  uint64 `json:"replacements"`
	RuntimeRows   uint64 `json:"runtimeRows"`
	Closed        bool   `json:"closed"`
}

type menuCatalogue struct {
	storage  *session.Storage
	observer *statedb.RegistryObserver
	file     os.FileInfo
	config   os.FileInfo
	version  int64
	epoch    uint64
	touch    time.Time
	// The serialized base is immutable, and decodes into private nested values.
	base       []byte
	registry   []byte
	identities map[string]string
}

func (s *SessionDataService) runtimeMenuCatalogueEnabled() bool {
	return s != nil && s.catalogueEnabled && !s.catalogueUnsupported.Load() && s.RuntimeObservationConfigured()
}

var errCatalogueFactoryUnsupported = errors.New("injected catalogue factory has no persistent observer")

func (s *SessionDataService) CatalogueDiagnostics() MenuCatalogueStats {
	if s == nil {
		return MenuCatalogueStats{}
	}
	s.catalogueMu.Lock()
	defer s.catalogueMu.Unlock()
	d := s.catalogueStats
	d.Closed = s.catalogueClosed.Load()
	return d
}

// Close is permanent and only closes this service's owned read-only resources.
// Cancellation precedes the mutex; an uncancelable source runs outside it and
// must encounter the closed gate before it can reopen or publish anything.
func (s *SessionDataService) Close() error {
	if s == nil {
		return nil
	}
	s.catalogueClosed.Store(true)
	if s.catalogueCancel != nil {
		s.catalogueCancel()
	}
	s.catalogueMu.Lock()
	defer s.catalogueMu.Unlock()
	return s.closeCatalogueLocked()
}

func (s *SessionDataService) closeCatalogueLocked() error {
	c := s.catalogue
	s.catalogue = nil
	if c == nil {
		return nil
	}
	a := c.observer.Close()
	b := c.storage.Close()
	return errors.Join(a, b)
}

func catalogueConfigInfo() (os.FileInfo, error) {
	path, err := session.GetUserConfigPath()
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return info, err
}

func catalogueSameConfig(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}

func (s *SessionDataService) catalogueReaderLocked() (*menuCatalogue, string, error) {
	if s.catalogueClosed.Load() {
		return nil, "", fmt.Errorf("session catalogue is closed")
	}
	if !s.RuntimeObservationConfigured() {
		return nil, "", fmt.Errorf("runtime observation source changed")
	}
	if c := s.catalogue; c != nil {
		info, err := os.Stat(c.storage.Path())
		if err != nil {
			_ = s.closeCatalogueLocked()
			return nil, "", fmt.Errorf("stat session catalogue: %w", err)
		}
		if os.SameFile(c.file, info) {
			return c, c.storage.Profile(), nil
		}
		s.catalogueStats.Replacements++
		_ = s.closeCatalogueLocked()
	}
	// Capture path identity BEFORE either SQLite connection opens. Otherwise
	// storage could read an old inode while its observer probes the replacement.
	requested := s.Profile()
	expectedProfile, resolveErr := session.ResolveProfileForStorage(requested)
	expectedPath, pathErr := session.GetDBPathForProfile(expectedProfile)
	beforeFile, statErr := os.Stat(expectedPath)
	opened, profile, err := s.resolveAndOpenStorage()
	if err != nil {
		return nil, "", err
	}
	storage, ok := opened.(*session.Storage)
	if !ok {
		_ = opened.Close()
		s.catalogueUnsupported.Store(true)
		return nil, "", errCatalogueFactoryUnsupported
	}
	if resolveErr != nil || pathErr != nil || statErr != nil || storage.Path() != expectedPath || storage.Profile() != expectedProfile {
		_ = storage.Close()
		return nil, "", fmt.Errorf("session catalogue identity is unavailable or changed before opening")
	}
	info, err := os.Stat(storage.Path())
	if err != nil || !os.SameFile(beforeFile, info) {
		_ = storage.Close()
		return nil, "", fmt.Errorf("session catalogue changed while opening storage")
	}
	observer, err := storage.GetDB().NewRegistryObserver()
	if err != nil {
		_ = storage.Close()
		return nil, "", err
	}
	after, err := os.Stat(storage.Path())
	if err != nil || !os.SameFile(info, after) {
		_ = observer.Close()
		_ = storage.Close()
		return nil, "", fmt.Errorf("session catalogue changed while opening")
	}
	c := &menuCatalogue{storage: storage, observer: observer, file: info}
	s.catalogue = c
	s.catalogueStats.Opens++
	return c, profile, nil
}

func (s *SessionDataService) loadCatalogueMenu() (menu *MenuSnapshot, err error) {
	s.catalogueMu.Lock()
	s.catalogueStats.Loads++
	_, profile, err := s.catalogueReaderLocked()
	s.catalogueMu.Unlock()
	defer func() {
		if err != nil {
			s.catalogueMu.Lock()
			s.catalogueStats.Errors++
			if s.catalogue != nil {
				s.catalogue.base = nil
			}
			s.catalogueMu.Unlock()
		}
	}()
	if err != nil {
		return nil, err
	}
	s.runtimeSourceMu.RLock()
	source, sourceGeneration := s.runtimeSource, s.runtimeSourceGeneration
	s.runtimeSourceMu.RUnlock()
	if source == nil {
		return nil, fmt.Errorf("runtime observation source changed")
	}
	// Source first: it may have committed a new native binding; registry/touch
	// sampled afterwards fence operator edits made while this request waited.
	snapshot, err := loadRuntimeObservations(s.catalogueCtx, source, profile)
	if err != nil {
		return nil, err
	}
	rows, err := catalogueRuntimeRows(snapshot, profile)
	if err != nil {
		return nil, err
	}
	s.catalogueMu.Lock()
	defer s.catalogueMu.Unlock()
	for attempt := 0; attempt < memoryMenuConsistentLoadAttempts; attempt++ {
		c, currentProfile, e := s.catalogueReaderLocked()
		if e != nil {
			return nil, e
		}
		if currentProfile != profile {
			return nil, fmt.Errorf("session catalogue profile changed")
		}
		before, epoch, e := c.observer.Probe()
		if e != nil {
			return nil, e
		}
		touch, e := c.storage.GetUpdatedAt()
		if e != nil {
			return nil, e
		}
		config, e := catalogueConfigInfo()
		if e != nil {
			return nil, e
		}
		if c.base == nil || c.version != before || c.epoch != epoch || !c.touch.Equal(touch) || !catalogueSameConfig(c.config, config) {
			// matrix/hardened: heartbeat/cost/annotation commits also change the
			// database version. Compare only persisted registry tables before
			// paying for Instance hydration and menu reconstruction again.
			registry, registryBefore, registryAfter, e := c.observer.Snapshot()
			if e != nil {
				return nil, e
			}
			s.catalogueStats.RegistryReads++
			if registryBefore != registryAfter || registryBefore != before {
				c.base = nil
				continue
			}
			registryBytes, e := json.Marshal(registry)
			if e != nil {
				return nil, e
			}
			if c.base != nil && bytes.Equal(registryBytes, c.registry) && catalogueSameConfig(c.config, config) {
				s.catalogueStats.Hits++
			} else {
				instances, groups, e := c.storage.LoadWithGroups()
				if e != nil {
					return nil, e
				}
				active := session.FilterInstancesByArchive(instances, false)
				identities := make(map[string]string, len(active))
				for _, inst := range active {
					if inst == nil {
						continue
					}
					inst.ExpectRuntimeSnapshot()
					identities[inst.ID] = inst.RuntimeIdentity()
				}
				base := BuildMenuSnapshot(profile, active, groups, s.now())
				encoded, e := json.Marshal(base)
				if e != nil {
					return nil, e
				}
				c.base, c.identities = encoded, identities
				c.registry = registryBytes
				s.catalogueStats.Reloads++
			}
		} else {
			s.catalogueStats.Hits++
		}
		after, afterEpoch, e := c.observer.Probe()
		if e != nil {
			return nil, e
		}
		afterTouch, e := c.storage.GetUpdatedAt()
		if e != nil {
			return nil, e
		}
		afterConfig, e := catalogueConfigInfo()
		if e != nil {
			return nil, e
		}
		info, e := os.Stat(c.storage.Path())
		if e != nil {
			return nil, e
		}
		if before != after || epoch != afterEpoch || !touch.Equal(afterTouch) || !catalogueSameConfig(config, afterConfig) || !os.SameFile(c.file, info) {
			c.base = nil
			continue
		}
		c.version, c.epoch, c.touch, c.config = after, afterEpoch, afterTouch, afterConfig
		var result MenuSnapshot
		if e := json.Unmarshal(c.base, &result); e != nil {
			return nil, e
		}
		result.GeneratedAt = s.now().UTC()
		for index := range result.Items {
			item := &result.Items[index]
			if item.Session == nil {
				continue
			}
			row, ok := rows[item.Session.ID]
			if !ok || row.ObservedAt.IsZero() || row.ObservedAt.Before(touch) || row.Error != "" || row.Identity != c.identities[item.Session.ID] {
				continue
			}
			catalogueOverlayRuntime(item.Session, row)
			s.catalogueStats.RuntimeRows++
		}
		// Overlay/encoding time is also fenced; unknown/racing state is retried.
		endVersion, endEpoch, e := c.observer.Probe()
		if e != nil {
			return nil, e
		}
		endTouch, e := c.storage.GetUpdatedAt()
		if e != nil {
			return nil, e
		}
		endFile, e := os.Stat(c.storage.Path())
		if e != nil {
			return nil, e
		}
		endConfig, e := catalogueConfigInfo()
		if e != nil {
			return nil, e
		}
		if endVersion != after || endEpoch != afterEpoch || !endTouch.Equal(touch) || !os.SameFile(c.file, endFile) || !catalogueSameConfig(afterConfig, endConfig) {
			c.base = nil
			continue
		}
		s.runtimeSourceMu.RLock()
		unchanged := sourceGeneration == s.runtimeSourceGeneration
		s.runtimeSourceMu.RUnlock()
		if !unchanged || s.catalogueClosed.Load() || s.catalogueCtx.Err() != nil {
			return nil, fmt.Errorf("session catalogue source is closed or changed")
		}
		return &result, nil
	}
	return nil, fmt.Errorf("session catalogue changed during consecutive snapshot loads")
}

func catalogueRuntimeRows(snapshot *session.RuntimeProfileSnapshot, profile string) (map[string]session.RuntimeSessionSnapshot, error) {
	rows := make(map[string]session.RuntimeSessionSnapshot, len(snapshot.Sessions))
	for _, row := range snapshot.Sessions {
		if row.Profile != profile || row.Epoch != snapshot.Epoch || row.Revision != snapshot.Revision {
			return nil, fmt.Errorf("runtime observation envelope mismatch for profile %q", profile)
		}
		if _, exists := rows[row.ID]; exists {
			return nil, fmt.Errorf("duplicate runtime observation for profile %q", profile)
		}
		rows[row.ID] = row
	}
	return rows, nil
}

// This mirrors toMenuSession's volatile getters on a newly loaded consumer.
// Raw native IDs, title, group and launch configuration remain registry-owned.
func catalogueOverlayRuntime(item *MenuSession, row session.RuntimeSessionSnapshot) {
	row = row.Clone()
	item.Status = row.Status
	item.Substate, item.SubstateDetail = string(row.Substate), row.Detail
	item.CanFork = row.CanForkKnown && row.CanFork
	if row.BackgroundWork.InFlight() {
		work := row.BackgroundWork
		item.BackgroundWork = &work
	} else {
		item.BackgroundWork = nil
	}
	item.ModelID, item.Model, item.ModelVersion = row.LaunchModel.ModelID, row.LaunchModel.Model, row.LaunchModel.Version
	item.GeminiModel, item.GeminiAnalytics = row.GeminiModel, row.GeminiAnalytics
	item.LatestPrompt, item.ToolOptionsJSON = row.LatestPrompt, row.ToolOptions
}

// Only known catalogue owners transfer resource ownership to Web shutdown.
func (m *MemoryMenuData) Close() error {
	if m == nil {
		return nil
	}
	if owned, ok := m.fallback.(*SessionDataService); ok {
		return owned.Close()
	}
	return nil
}

func (s *Server) closeMenuCatalogue() {
	switch owned := s.menuData.(type) {
	case *MemoryMenuData:
		_ = owned.Close()
	case *SessionDataService:
		_ = owned.Close()
	}
}

func (s *Server) handleMenuCatalogueDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if !s.authorizeRequest(r) {
		writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return
	}
	var owned *SessionDataService
	switch data := s.menuData.(type) {
	case *SessionDataService:
		owned = data
	case *MemoryMenuData:
		owned, _ = data.fallback.(*SessionDataService)
	}
	writeJSON(w, http.StatusOK, owned.CatalogueDiagnostics())
}
