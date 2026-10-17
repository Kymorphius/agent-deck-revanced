package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// matrix/hardened: status is a read of committed metadata, even while the
// single writer belongs to a sweep that has not committed its next marker.
func TestMatrixInitialBackfillStatusContextReadsBesideWriter(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "recall.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetInitialBackfillState(InitialBackfillPending, 0); err != nil {
		t.Fatal(err)
	}
	tx, err := s.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE meta SET v=? WHERE k=?`, InitialBackfillRunning, metaInitialBackfillState); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status, err := s.InitialBackfillStatusContext(ctx)
	if err != nil {
		t.Fatalf("read beside writer: %v", err)
	}
	if status.State != InitialBackfillPending {
		t.Fatalf("state = %q; want last committed state %q", status.State, InitialBackfillPending)
	}
}

// matrix/hardened: pool contention must honor the caller's deadline rather
// than leave the status command waiting after its overlay was canceled.
func TestMatrixInitialBackfillStatusContextHonorsReaderDeadline(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "recall.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.R.SetMaxOpenConns(1)
	conn, err := s.R.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := s.InitialBackfillStatusContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("status error = %v; want reader deadline", err)
	}
}
