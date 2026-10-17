package web

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: keyboard streams persist at most once per interval, even
// when a database is unavailable; empty messages are not user interactions.
func TestMatrixWebRecentAccessThrottle(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failed-write"}[fail], func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			recorded := make(chan time.Time, 10)
			r := webSessionAccessRecorder{
				profile: "owning", sessionID: "recent",
				now: func() time.Time { return now },
				record: func(profile, sessionID string, at time.Time) error {
					if profile != "owning" || sessionID != "recent" {
						t.Errorf("incorrect target: %q/%q", profile, sessionID)
					}
					recorded <- at
					if fail {
						return errors.New("database unavailable")
					}
					return nil
				},
			}
			r.acceptedInput("")
			if len(recorded) != 0 {
				t.Fatal("empty input recorded a visit")
			}
			r.acceptedInput("a")
			matrixWaitRecentRecord(t, recorded)
			matrixWaitRecentAccessIdle(t, &r)
			for range 100 {
				r.acceptedInput("b")
			}
			now = now.Add(webRecentAccessInterval - time.Nanosecond)
			r.acceptedInput("c")
			matrixWaitRecentAccessIdle(t, &r)
			if len(recorded) != 0 {
				t.Fatalf("extra writes before interval = %d, want 0", len(recorded))
			}
			now = now.Add(time.Nanosecond)
			r.acceptedInput("\r")
			if got := matrixWaitRecentRecord(t, recorded); !got.Equal(now) {
				t.Fatalf("write at interval boundary = %v, want %v", got, now)
			}
			matrixWaitRecentAccessIdle(t, &r)
		})
	}
}

func matrixWaitRecentRecord(t *testing.T, recorded <-chan time.Time) time.Time {
	t.Helper()
	select {
	case at := <-recorded:
		return at
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for access persistence")
		return time.Time{}
	}
}

func matrixWaitRecentAccessIdle(t *testing.T, r *webSessionAccessRecorder) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		busy := r.inFlight
		r.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("access writer did not exit after draining pending input")
}

// matrix/hardened: a blocked database write cannot hold the WebSocket read
// loop. More accepted input produces one coalesced timestamp, never another
// concurrent write or a goroutine that waits forever after disconnect.
func TestMatrixWebRecentAccessBlockedWriteCoalescesWithoutBlockingInput(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	var inFlight, maxInFlight atomic.Int32
	started := make(chan time.Time, 10)
	release := make(chan struct{}, 10)
	r := webSessionAccessRecorder{
		profile: "owning", sessionID: "recent",
		now: func() time.Time { return time.Unix(0, clock.Load()) },
		record: func(profile, sessionID string, at time.Time) error {
			if profile != "owning" || sessionID != "recent" {
				t.Errorf("incorrect target: %q/%q", profile, sessionID)
			}
			active := inFlight.Add(1)
			for previous := maxInFlight.Load(); active > previous; previous = maxInFlight.Load() {
				if maxInFlight.CompareAndSwap(previous, active) {
					break
				}
			}
			started <- at
			<-release
			inFlight.Add(-1)
			return nil
		},
	}
	t.Cleanup(func() {
		close(release)
		matrixWaitRecentAccessIdle(t, &r)
	})
	acceptResponsive := func(at time.Time) {
		t.Helper()
		clock.Store(at.UnixNano())
		done := make(chan struct{})
		go func() {
			r.acceptedInput("x")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("acceptedInput blocked on database persistence")
		}
	}
	acceptResponsive(base)
	if got := matrixWaitRecentRecord(t, started); !got.Equal(base) {
		t.Fatalf("first write = %v, want %v", got, base)
	}
	for _, delta := range []time.Duration{time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 16 * time.Second} {
		acceptResponsive(base.Add(delta))
	}
	if len(started) != 0 || inFlight.Load() != 1 {
		t.Fatalf("blocked write spawned another writer: queued=%d active=%d", len(started), inFlight.Load())
	}
	release <- struct{}{}
	wantLatest := base.Add(15 * time.Second)
	if got := matrixWaitRecentRecord(t, started); !got.Equal(wantLatest) {
		t.Fatalf("coalesced write = %v, want latest eligible input %v", got, wantLatest)
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf("maximum concurrent writes = %d, want 1", maxInFlight.Load())
	}
	// Simulate disconnect: no more input arrives. The writer must finish after
	// the last queued finite operation, without any timer or shutdown signal.
	release <- struct{}{}
	matrixWaitRecentAccessIdle(t, &r)
	if len(started) != 0 || inFlight.Load() != 0 {
		t.Fatalf("writer did not drain exactly one pending visit: queued=%d active=%d", len(started), inFlight.Load())
	}
	// An idle connection can start one new writer on later eligible input.
	acceptResponsive(base.Add(20 * time.Second))
	if got := matrixWaitRecentRecord(t, started); !got.Equal(base.Add(20 * time.Second)) {
		t.Fatalf("restarted writer timestamp = %v", got)
	}
	release <- struct{}{}
	matrixWaitRecentAccessIdle(t, &r)
	if maxInFlight.Load() != 1 {
		t.Fatalf("maximum concurrent writes after restart = %d, want 1", maxInFlight.Load())
	}
}

func matrixRecentAccessDB(t *testing.T, profile, id string, at time.Time) *statedb.StateDB {
	t.Helper()
	path, err := session.GetDBPathForProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveInstance(&statedb.InstanceRow{
		ID: id, Title: "recent", Tool: "shell", Status: "idle",
		CreatedAt: at, LastAccessed: at,
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func matrixReadRecentAccess(t *testing.T, db *statedb.StateDB, id string) time.Time {
	t.Helper()
	rows, err := db.LoadInstances()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row.LastAccessed
		}
	}
	t.Fatalf("session %q not found", id)
	return time.Time{}
}

// matrix/hardened: same IDs in another profile must not receive the Web visit.
func TestMatrixWebRecentAccessPersistsOnlyOwningProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := time.Unix(1_800_000_000, 0)
	own := matrixRecentAccessDB(t, "recent-own", "same-id", old)
	other := matrixRecentAccessDB(t, "recent-other", "same-id", old)
	newer := old.Add(time.Minute)
	if err := writeWebSessionAccess("recent-own", "same-id", newer); err != nil {
		t.Fatal(err)
	}
	if got := matrixReadRecentAccess(t, own, "same-id"); !got.Equal(newer) {
		t.Fatalf("own profile access = %v, want %v", got, newer)
	}
	if got := matrixReadRecentAccess(t, other, "same-id"); !got.Equal(old) {
		t.Fatalf("another profile access changed to %v", got)
	}
}

// matrix/hardened: two tabs/connections have independent asynchronous writers.
// A delayed older visit completing after a newer visit must not rewind SQLite.
func TestMatrixWebRecentAccessOutOfOrderConnectionsKeepNewest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := time.Unix(1_800_000_000, 0)
	const profile, sessionID = "recent-concurrent", "same-session"
	db := matrixRecentAccessDB(t, profile, sessionID, base)
	older, newer := base.Add(time.Minute), base.Add(2*time.Minute)
	startedA := make(chan time.Time, 1)
	releaseA := make(chan struct{}, 1)
	resultA, resultB := make(chan error, 1), make(chan error, 1)
	a := webSessionAccessRecorder{
		profile: profile, sessionID: sessionID,
		now: func() time.Time { return older },
		record: func(profile, sessionID string, at time.Time) error {
			startedA <- at
			<-releaseA
			err := writeWebSessionAccess(profile, sessionID, at)
			resultA <- err
			return err
		},
	}
	b := webSessionAccessRecorder{
		profile: profile, sessionID: sessionID,
		now: func() time.Time { return newer },
		record: func(profile, sessionID string, at time.Time) error {
			err := writeWebSessionAccess(profile, sessionID, at)
			resultB <- err
			return err
		},
	}
	t.Cleanup(func() {
		close(releaseA)
		matrixWaitRecentAccessIdle(t, &a)
		matrixWaitRecentAccessIdle(t, &b)
	})
	waitResult := func(result <-chan error) {
		t.Helper()
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("persist visit: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for database write")
		}
	}
	a.acceptedInput("old connection input")
	if got := matrixWaitRecentRecord(t, startedA); !got.Equal(older) {
		t.Fatalf("blocked visit = %v, want %v", got, older)
	}
	b.acceptedInput("new connection input")
	waitResult(resultB)
	matrixWaitRecentAccessIdle(t, &b)
	if got := matrixReadRecentAccess(t, db, sessionID); !got.Equal(newer) {
		t.Fatalf("newer connection visit = %v, want %v", got, newer)
	}
	releaseA <- struct{}{}
	waitResult(resultA)
	matrixWaitRecentAccessIdle(t, &a)
	if got := matrixReadRecentAccess(t, db, sessionID); !got.Equal(newer) {
		t.Fatalf("older connection rewound access to %v, want %v", got, newer)
	}
}

func TestMatrixWebRecentAccessMissingProfileDoesNotInitialize(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := session.GetDBPathForProfile("recent-missing")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeWebSessionAccess("recent-missing", "gone", time.Now()); err == nil {
		t.Fatal("expected unavailable database error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing profile database was initialized: %v", err)
	}
}
