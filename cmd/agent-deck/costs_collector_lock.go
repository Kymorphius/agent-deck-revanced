package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const codexCostCollectionInterval = 30 * time.Second

// matrix/hardened: every view may read costs, but only one process per actual
// profile directory owns the expensive native usage tail. Losing the owner
// releases its OS lock; waiting views retry without scanning any transcripts.
func startCostCollectorOwner(parent context.Context, profileDir string, retry time.Duration, collect func(context.Context, *costCollectorLock) error) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	if retry <= 0 {
		retry = codexCostCollectionInterval
	}
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			lock, owned, err := tryCostCollectorLock(profileDir)
			if err != nil {
				slog.Warn("Codex cost collector ownership unavailable", "error", err)
			} else if owned {
				// collect closes its storage and stops all reads before returning.
				// Never unlink the stable lock file: that would admit two owners.
				err = collect(ctx, lock)
				if closeErr := lock.Close(); err == nil {
					err = closeErr
				}
				if err != nil && ctx.Err() == nil {
					slog.Warn("Codex cost collector stopped", "error", err)
				}
			}
			timer := time.NewTimer(retry)
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { cancel(); <-done })
	}
}
