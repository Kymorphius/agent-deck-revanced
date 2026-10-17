package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// matrix/hardened: all rollout fixtures live in synthetic Codex homes. These
// tests exercise lookup invalidation without scanning the operator's history.
func matrixSeedCachedCodexRollout(t testing.TB, home, day, sid string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", filepath.FromSlash(day))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-fixture-"+sid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMatrixCodexRolloutPathCacheSeparatesHomes(t *testing.T) {
	sid := "019f0000-0000-7000-8000-999999999901"
	firstHome, secondHome := t.TempDir(), t.TempDir()
	firstPath := matrixSeedCachedCodexRollout(t, firstHome, "2026/10/01", sid)
	secondPath := matrixSeedCachedCodexRollout(t, secondHome, "2026/10/02", sid)
	for n := 0; n < 3; n++ {
		if got := codexRolloutPathInHome(sid, firstHome); got != firstPath {
			t.Fatalf("first home path = %q, want %q", got, firstPath)
		}
		if got := codexRolloutPathInHome(sid, secondHome); got != secondPath {
			t.Fatalf("second home path = %q, want %q", got, secondPath)
		}
	}
}

func TestMatrixCodexRolloutPathCacheRetriesAfterFirstFlush(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999902"
	if got := codexRolloutPathInHome(sid, home); got != "" {
		t.Fatalf("unflushed path = %q, want empty", got)
	}
	path := matrixSeedCachedCodexRollout(t, home, "2026/10/11", sid)
	if got := codexRolloutPathInHome("  "+sid+"  ", home); got != path {
		t.Fatalf("first flushed path = %q, want %q", got, path)
	}
}

func TestMatrixCodexRolloutPathCacheRediscoversRemovedOrMovedRollout(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999903"
	path := matrixSeedCachedCodexRollout(t, home, "2026/10/01", sid)
	if got := codexRolloutPathInHome(sid, home); got != path {
		t.Fatalf("initial path = %q, want %q", got, path)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := codexRolloutPathInHome(sid, home); got != "" {
		t.Fatalf("removed path = %q, want empty", got)
	}
	restored := matrixSeedCachedCodexRollout(t, home, "2026/10/02", sid)
	if got := codexRolloutPathInHome(sid, home); got != restored {
		t.Fatalf("restored path = %q, want %q", got, restored)
	}
	movedDir := filepath.Join(home, "sessions", "2026", "10", "03")
	if err := os.MkdirAll(movedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(movedDir, filepath.Base(restored))
	if err := os.Rename(restored, moved); err != nil {
		t.Fatal(err)
	}
	if got := codexRolloutPathInHome(sid, home); got != moved {
		t.Fatalf("moved path = %q, want %q", got, moved)
	}
}

func TestMatrixCodexRolloutPathCacheAvoidsStableTreeScans(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999904"
	path := matrixSeedCachedCodexRollout(t, home, "2026/10/11", sid)
	var resolver codexRolloutPathResolver
	scans := 0
	scan := func(pattern string) ([]string, error) {
		scans++
		return filepath.Glob(pattern)
	}
	for n := 0; n < 100; n++ {
		if got := resolver.find(sid, home, scan); got != path {
			t.Fatalf("lookup %d = %q, want %q", n, got, path)
		}
	}
	if scans != 1 {
		t.Fatalf("stable rollout scanned its history tree %d times, want 1", scans)
	}
	if got := resolver.find("  ", home, scan); got != "" || scans != 1 {
		t.Fatalf("blank session scanned history: path=%q, scans=%d", got, scans)
	}
	missing := "019f0000-0000-7000-8000-999999999905"
	for n := 0; n < 2; n++ {
		if got := resolver.find(missing, home, scan); got != "" {
			t.Fatalf("missing rollout path = %q, want empty", got)
		}
	}
	if scans != 3 {
		t.Fatalf("misses must retry scans: got %d total scans, want 3", scans)
	}
}

func TestMatrixCodexRolloutPathCacheConcurrentLookups(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999906"
	path := matrixSeedCachedCodexRollout(t, home, "2026/10/11", sid)
	const workers = 32
	start := make(chan struct{})
	results := make(chan string, workers)
	var group sync.WaitGroup
	for n := 0; n < workers; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for lookup := 0; lookup < 50; lookup++ {
				if got := codexRolloutPathInHome(sid, home); got != path {
					results <- got
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for got := range results {
		t.Errorf("concurrent path = %q, want %q", got, path)
	}
}

// matrix/hardened: compare the old whole-tree Glob with a warm positive cache
// across 120 synthetic daily directories containing 12,001 rollout files.
func BenchmarkMatrixCodexRolloutPathLookup(b *testing.B) {
	home := b.TempDir()
	sid := "019f0000-0000-7000-8000-999999999907"
	for month := 1; month <= 12; month++ {
		for day := 1; day <= 10; day++ {
			date := fmt.Sprintf("2026/%02d/%02d", month, day)
			for file := 0; file < 100; file++ {
				matrixSeedCachedCodexRollout(b, home, date, fmt.Sprintf("fixture-%04d", file))
			}
		}
	}
	path := matrixSeedCachedCodexRollout(b, home, "2026/12/10", sid)
	pattern := filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+sid+".jsonl")
	b.Run("OriginalGlob", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			matches, err := filepath.Glob(pattern)
			if err != nil || len(matches) != 1 || matches[0] != path {
				b.Fatalf("glob matches=%v, err=%v, want %q", matches, err, path)
			}
		}
	})
	b.Run("CachedStat", func(b *testing.B) {
		var resolver codexRolloutPathResolver
		if got := resolver.find(sid, home, filepath.Glob); got != path {
			b.Fatalf("initial path = %q, want %q", got, path)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if got := resolver.find(sid, home, filepath.Glob); got != path {
				b.Fatalf("cached path = %q, want %q", got, path)
			}
		}
	})
}
