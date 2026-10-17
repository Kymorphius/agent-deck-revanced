package web

import (
	"context"
	"fmt"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: only a configured monitor can supply runtime verdicts. Its
// errors remain visible instead of launching a second Web fleet sampler.
func loadRuntimeObservations(ctx context.Context, source session.RuntimeObservationSource, profile string) (*session.RuntimeProfileSnapshot, error) {
	snapshot, err := source.Snapshot(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("load runtime observations for profile %q: %w", profile, err)
	}
	if snapshot == nil {
		return nil, fmt.Errorf("runtime observations for profile %q are unavailable", profile)
	}
	if snapshot.Protocol != session.RuntimeProtocolVersion || snapshot.Profile != profile || snapshot.Epoch == "" || snapshot.ObservedAt.IsZero() {
		return nil, fmt.Errorf("runtime observations protocol or profile mismatch for %q", profile)
	}
	if snapshot.Error != "" {
		return nil, fmt.Errorf("runtime observations for profile %q: %s", profile, snapshot.Error)
	}
	return snapshot, nil
}

func applyRuntimeObservations(snapshot *session.RuntimeProfileSnapshot, profile string, instances []*session.Instance, registryUpdatedAt time.Time) error {
	rows := make(map[string]session.RuntimeSessionSnapshot, len(snapshot.Sessions))
	for _, row := range snapshot.Sessions {
		if row.Profile != profile || row.Epoch != snapshot.Epoch || row.Revision != snapshot.Revision {
			return fmt.Errorf("runtime observation envelope mismatch for profile %q", profile)
		}
		if _, exists := rows[row.ID]; exists {
			return fmt.Errorf("duplicate runtime observation for profile %q", profile)
		}
		rows[row.ID] = row
	}
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		inst.ExpectRuntimeSnapshot()
		if row, ok := rows[inst.ID]; ok && !row.ObservedAt.Before(registryUpdatedAt) {
			inst.ApplyRuntimeSnapshot(profile, row)
		}
	}
	return nil
}

func (s *Server) runtimeObservationConfigured() bool {
	if s.cfg.RuntimeObservationSource != nil {
		return true
	}
	if consumer, ok := s.menuData.(interface{ RuntimeObservationConfigured() bool }); ok {
		return consumer.RuntimeObservationConfigured()
	}
	return false
}

func (s *Server) refreshRuntimeObservations(ctx context.Context) error {
	if source := s.cfg.RuntimeObservationSource; source != nil {
		profile := s.cfg.Profile
		if provider, ok := s.menuData.(interface{ Profile() string }); ok {
			profile = provider.Profile()
		}
		return source.Refresh(ctx, profile)
	}
	if consumer, ok := s.menuData.(interface{ RefreshRuntimeObservations(context.Context) error }); ok {
		return consumer.RefreshRuntimeObservations(ctx)
	}
	return nil
}

// RefreshRuntimeObservations requests a monitor pass after a persisted mutation.
func (s *SessionDataService) RefreshRuntimeObservations(ctx context.Context) error {
	if source := s.runtimeObservationSource(); source != nil {
		return source.Refresh(ctx, s.Profile())
	}
	return nil
}
