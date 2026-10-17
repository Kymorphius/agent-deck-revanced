package ui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/store"
)

// matrix/hardened: manual retries do not accumulate behind an old sweep.
func TestMatrixRecallIndexRefreshDoesNotWaitForPreviousSweep(t *testing.T) {
	idx := newRealRecallIndex(t)
	idx.mu.Lock()
	locked := true
	defer func() {
		if locked {
			idx.mu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := idx.Refresh(context.Background(), false)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrLocked) {
			t.Fatalf("refresh error = %v; want sweep already running", err)
		}
	case <-time.After(time.Second):
		locked = false
		idx.mu.Unlock()
		<-done
		t.Fatal("refresh queued behind the previous sweep")
	}
}

// matrix/hardened: a canceled refresh is a no-op before either lock is
// acquired, including when an earlier sweep still holds the local mutex.
func TestMatrixRecallIndexRefreshRejectsCanceledContext(t *testing.T) {
	idx := newRealRecallIndex(t)
	idx.mu.Lock()
	locked := true
	defer func() {
		if locked {
			idx.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := idx.Refresh(ctx, false)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("refresh error = %v; want canceled", err)
		}
	case <-time.After(time.Second):
		locked = false
		idx.mu.Unlock()
		<-done
		t.Fatal("canceled refresh waited for the previous sweep")
	}
}
