package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/costs"
	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: share account-aware, exact artifact resolution between
// historical sync and live collection. Unmanaged and SSH rollouts stay out.
func costSyncSessions(instances []*session.Instance) ([]costs.SyncSession, []error) {
	var out []costs.SyncSession
	var errs []error
	// matrix/hardened: enumerate each resolved home once for this cost pass;
	// the next invocation takes a new directory snapshot, including duplicates.
	rollouts := session.NewCodexCostRolloutResolver()
	for _, inst := range instances {
		if inst.Tool == "claude" && inst.ClaudeSessionID != "" {
			// matrix/hardened: manual sync follows the bound Claude account and
			// effective launch directory rather than a default-home guess.
			path, err := inst.ClaudeCostTranscriptPath()
			if err != nil {
				errs = append(errs, fmt.Errorf("Claude cost source %s: %w", inst.ID, err))
				continue
			}
			out = append(out, costs.SyncSession{InstanceID: inst.ID, ClaudeSessionID: inst.ClaudeSessionID, ClaudePath: path, ProjectPath: inst.ProjectPath, Tool: inst.Tool})
		} else if session.IsCodexCompatible(inst.Tool) && inst.CodexSessionID != "" && inst.ResolvedCodexHome() != "" {
			path, err := rollouts.Resolve(inst)
			if err != nil {
				errs = append(errs, fmt.Errorf("Codex cost source %s: %w", inst.ID, err))
				continue
			}
			out = append(out, costs.SyncSession{InstanceID: inst.ID, Tool: inst.Tool, CodexPath: path, CodexSessionID: inst.CodexSessionID})
		}
	}
	return out, errs
}

// matrix/hardened: all views share one lifetime cost collector per resolved
// profile. Full history is preserved; only its owner opens and advances tails.
func startCodexCostCollection(profile string, store *costs.Store) func() {
	probe, err := session.NewLiveReadOnlyStorageWithProfile(profile)
	if err != nil {
		slog.Warn("Codex costs unavailable", "error", err)
		return func() {}
	}
	resolvedProfile, profileDir := probe.Profile(), filepath.Dir(probe.Path())
	if err := probe.Close(); err != nil {
		slog.Warn("Codex costs storage close", "error", err)
		return func() {}
	}
	return startCostCollectorOwner(context.Background(), profileDir, codexCostCollectionInterval, func(ctx context.Context, lock *costCollectorLock) error {
		return runCodexCostCollection(ctx, resolvedProfile, profileDir, store, lock)
	})
}

// matrix/hardened: this body runs only after ownership. Its storage and native
// tail state close before the caller releases the process-lifetime lock.
func runCodexCostCollection(ctx context.Context, profile, profileDir string, store *costs.Store, lock *costCollectorLock) error {
	storage, err := session.NewLiveReadOnlyStorageWithProfile(profile)
	if err != nil {
		return err
	}
	defer storage.Close()
	if filepath.Dir(storage.Path()) != profileDir {
		return fmt.Errorf("Codex cost storage path changed after ownership")
	}
	ticker := time.NewTicker(codexCostCollectionInterval)
	defer ticker.Stop()
	tails := map[string]*reader.CodexUsageTail{}
	for ctx.Err() == nil {
		if err := lock.valid(); err != nil {
			return err
		}
		instances, err := storage.Load()
		if err == nil {
			sources, errs := costSyncSessions(instances)
			for _, err := range errs {
				slog.Warn("Codex cost source", "error", err)
			}
			importer := costs.NewUsageImporter(store, newPricerFromConfig())
			for _, src := range sources {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if src.CodexPath == "" {
					continue
				}
				tail := tails[src.CodexPath]
				if tail == nil {
					tail = &reader.CodexUsageTail{}
					tails[src.CodexPath] = tail
				}
				events, err := tail.Read(ctx, src.CodexPath, src.CodexSessionID)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					slog.Warn("Codex cost read", "error", err)
					delete(tails, src.CodexPath)
					continue
				}
				_, _, errs := importer.Import(src.InstanceID, events)
				if len(errs) > 0 {
					slog.Warn("Codex cost import", "errors", errs)
					delete(tails, src.CodexPath)
				}
			}
		} else {
			slog.Warn("Codex cost sessions", "error", err)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return ctx.Err()
}
