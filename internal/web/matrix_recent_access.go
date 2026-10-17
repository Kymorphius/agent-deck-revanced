package web

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: bound terminal input persistence to one targeted write per
// five seconds per connection, without sharing mutable menu/Instance pointers.
const webRecentAccessInterval = 5 * time.Second

type webSessionAccessRecorder struct {
	profile      string
	sessionID    string
	mu           sync.Mutex
	lastRecorded time.Time
	inFlight     bool
	pending      time.Time
	now          func() time.Time
	record       func(profile, sessionID string, at time.Time) error
}

func (r *webSessionAccessRecorder) acceptedInput(data string) {
	if data == "" {
		return
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	at := now()
	r.mu.Lock()
	if !r.lastRecorded.IsZero() && at.Sub(r.lastRecorded) < webRecentAccessInterval {
		r.mu.Unlock()
		return
	}
	// matrix/hardened: failed persistence is also throttled, so unavailable
	// storage cannot turn a stream of keypresses into repeated blocking I/O.
	r.lastRecorded = at
	// matrix/hardened: SQLite contention must never block terminal input or
	// keepalive reads. Coalesce eligible input into one latest pending visit
	// while the single writer is occupied, with no timer or idle worker.
	if r.inFlight {
		r.pending = at
		r.mu.Unlock()
		return
	}
	r.inFlight = true
	r.mu.Unlock()
	record := writeWebSessionAccess
	if r.record != nil {
		record = r.record
	}
	// matrix/hardened: copy immutable target metadata into the worker instead
	// of touching any shared menu or Instance. After disconnect there can only
	// be the current finite DB write and one pending write before it exits.
	go r.persist(r.profile, r.sessionID, at, record)
}

func (r *webSessionAccessRecorder) persist(profile, sessionID string, at time.Time, record func(string, string, time.Time) error) {
	for {
		if err := record(profile, sessionID, at); err != nil {
			logging.ForComponent(logging.CompWeb).Debug("last_accessed_persist_failed",
				slog.String("session_id", logging.SanitizeValue(sessionID)),
				slog.String("error", err.Error()))
		}
		r.mu.Lock()
		if r.pending.IsZero() {
			r.inFlight = false
			r.mu.Unlock()
			return
		}
		at, r.pending = r.pending, time.Time{}
		r.mu.Unlock()
	}
}

// matrix/hardened: update only the existing owning-profile row; a stale web
// terminal must never initialize/migrate a profile or perform a full save.
func writeWebSessionAccess(profile, sessionID string, at time.Time) error {
	path, err := session.GetDBPathForProfile(profile)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	db, err := statedb.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.WriteLastAccessed(sessionID, at)
}
