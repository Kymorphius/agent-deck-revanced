package query

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/store"
)

// matrix/hardened: the whole status path, including initial-backfill
// metadata, uses the reader pool while Refresh owns the writer.
func TestMatrixStatusDoesNotWaitForSweepWriter(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "recall.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tx, err := st.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		status Status
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := New(st, "").Status(ctx)
		done <- result{status: status, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("status beside writer: %v", got.err)
		}
		if got.status.InitialBackfill.State != store.InitialBackfillPending {
			t.Fatalf("initial backfill status: %+v", got.status.InitialBackfill)
		}
	case <-ctx.Done():
		// Release the old implementation's uncancelable writer query before
		// failing, so this regression cannot leave a goroutine in DB.Close.
		_ = tx.Rollback()
		<-done
		t.Fatal("status waited for the sweep's writer connection")
	}
}
