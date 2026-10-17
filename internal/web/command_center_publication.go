package web

import (
	"encoding/json"
	"sync"
)

// matrix/hardened: artifact reads, grouping, encoding and fingerprinting happen
// lazily once per successful menu generation, outside the publisher mutex.
// Even unchanged menu contents receive a new derived owner on the next poll.
type commandCenterDerived struct {
	once        sync.Once
	snapshot    *CommandCenterSnapshot
	fingerprint string
	encoded     []byte
	err         error
}

func (d *commandCenterDerived) load(menu *MenuSnapshot, build func(*MenuSnapshot) *CommandCenterSnapshot) {
	d.once.Do(func() {
		d.snapshot = build(menu)
		d.fingerprint = commandCenterFingerprint(d.snapshot)
		d.encoded, d.err = json.Marshal(d.snapshot)
	})
}

// matrix/hardened: only completion overlays are private to a connection. The
// base DTO and bytes remain immutable; no per-connection artifact reads occur.
type commandCenterPublication struct {
	snapshot    *CommandCenterSnapshot
	fingerprint string
	encoded     []byte
}

func (s *Server) commandCenterBase(v menuPublication) (commandCenterPublication, error) {
	if v.Err != nil {
		return commandCenterPublication{}, v.Err
	}
	if v.commandCenter == nil {
		v.commandCenter = &commandCenterDerived{}
	}
	v.commandCenter.load(v.Snapshot, func(menu *MenuSnapshot) *CommandCenterSnapshot {
		// matrix/hardened: count the actual once-build, not per-reader cache hits.
		s.commandCenterBuilds.Add(1)
		return s.commandCenterBuilder(menu)
	})
	return commandCenterPublication{v.commandCenter.snapshot, v.commandCenter.fingerprint, v.commandCenter.encoded}, v.commandCenter.err
}

// matrix/hardened: capture the latest menu and completed transitions in one
// publication boundary, then release publisher.mu before lazy artifact I/O.
func (s *Server) commandCenterForPublication(v menuPublication, tracker *ccPublicationTracker) (menuPublication, commandCenterPublication, error) {
	if v.Err != nil {
		return menuPublication{}, commandCenterPublication{}, v.Err
	}
	s.menuPublisher.mu.Lock()
	// matrix/hardened: this owned generation is immutable. Capture it directly
	// for read-only synthesis instead of decoding a private menu for each client.
	v = s.menuPublisher.current
	if v.Err != nil {
		s.menuPublisher.mu.Unlock()
		return menuPublication{}, commandCenterPublication{}, v.Err
	}
	completed, err := tracker.drain()
	s.menuPublisher.mu.Unlock()
	if err != nil {
		return menuPublication{}, commandCenterPublication{}, err
	}
	base, err := s.commandCenterBase(v)
	if err != nil {
		return menuPublication{}, commandCenterPublication{}, err
	}
	if len(completed) == 0 {
		return v.wire(), base, nil
	}
	snapshot := *base.snapshot
	snapshot.RecentlyCompleted = completed
	encoded, err := json.Marshal(&snapshot)
	return v.wire(), commandCenterPublication{&snapshot, base.fingerprint, encoded}, err
}
