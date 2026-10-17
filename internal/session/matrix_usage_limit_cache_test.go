package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// matrix/hardened: private caches count real bounded scans without replacing the
// shared production cache or touching live transcripts/configuration.
func matrixCountedUsageCache(limit int) (*matrixUsageLimitFileCache, *atomic.Int64) {
	c := newMatrixUsageLimitFileCache(limit)
	count := new(atomic.Int64)
	c.scan = func(path string) matrixUsageLimitScanResult {
		count.Add(1)
		return scanLatestAssistantUsageLimitFact(path)
	}
	return c, count
}

func matrixWriteUsageBody(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func matrixRequireUsageFact(t *testing.T, c *matrixUsageLimitFileCache, path string, now time.Time, limited, formed bool) {
	t.Helper()
	got := c.load(path)
	if got.formed != formed || got.fact.limited(now) != limited {
		t.Fatalf("usage fact = limited:%v formed:%v, want %v/%v", got.fact.limited(now), got.formed, limited, formed)
	}
}

func TestMatrixUsageLimitCacheStablePositiveAndNegative(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limited    bool
	}{
		{"rate limit", recRateLimitAt(stampAt(-time.Minute)) + "\n", true},
		{"normal", recAssistantAt(stampAt(-time.Minute), "ready") + "\n", false},
		{"different API error", recAPIErrorAt(stampAt(-time.Minute), 401, "authentication_failed") + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thread.jsonl")
			matrixWriteUsageBody(t, path, tc.body)
			cache, count := matrixCountedUsageCache(16)
			for n := 0; n < 100; n++ {
				matrixRequireUsageFact(t, cache, path, refNow, tc.limited, true)
			}
			if scans := count.Load(); scans != 1 {
				t.Fatalf("stable transcript scanned %d times", scans)
			}
		})
	}
}

func TestMatrixUsageLimitCacheRecomputesExpiryFutureAndInvalidTime(t *testing.T) {
	for _, tc := range []struct {
		name, stamp           string
		first, second         time.Time
		wantFirst, wantSecond bool
	}{
		{"expiry", stampAt(0), refNow.Add(usageLimitMaxAge), refNow.Add(usageLimitMaxAge + time.Nanosecond), true, false},
		{"future becomes current", stampAt(time.Minute), refNow, refNow.Add(time.Minute), false, true},
		{"invalid timestamp", "invalid", refNow, refNow.Add(time.Hour), false, false},
		{"missing timestamp", "", refNow, refNow.Add(time.Hour), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeUsageLimitTranscript(t, recRateLimitAt(tc.stamp))
			cache, count := matrixCountedUsageCache(16)
			matrixRequireUsageFact(t, cache, path, tc.first, tc.wantFirst, true)
			matrixRequireUsageFact(t, cache, path, tc.second, tc.wantSecond, true)
			if scans := count.Load(); scans != 1 {
				t.Fatalf("clock change rescanned unchanged file %d times", scans)
			}
		})
	}
}

func TestMatrixUsageLimitCacheAppendClears429(t *testing.T) {
	path := writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute)))
	cache, count := matrixCountedUsageCache(16)
	matrixRequireUsageFact(t, cache, path, refNow, true, true)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(recAssistantAt(stampAt(0), "done") + "\n")
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append: %v %v", writeErr, closeErr)
	}
	for n := 0; n < 4; n++ {
		matrixRequireUsageFact(t, cache, path, refNow, false, true)
	}
	if scans := count.Load(); scans != 2 {
		t.Fatalf("append scans=%d", scans)
	}
}

func TestMatrixUsageLimitCacheMissingTruncateRemoveAndFirstFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.jsonl")
	cache, count := matrixCountedUsageCache(16)
	for n := 0; n < 3; n++ {
		matrixRequireUsageFact(t, cache, path, refNow, false, false)
	}
	if count.Load() != 0 {
		t.Fatal("missing path started or cached a scan")
	}
	matrixWriteUsageBody(t, path, recRateLimitAt(stampAt(-time.Minute))+"\n")
	matrixRequireUsageFact(t, cache, path, refNow, true, true)
	matrixWriteUsageBody(t, path, "")
	for n := 0; n < 2; n++ {
		matrixRequireUsageFact(t, cache, path, refNow, false, false)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	matrixRequireUsageFact(t, cache, path, refNow, false, false)
	matrixWriteUsageBody(t, path, recAssistantAt(stampAt(0), "normal")+"\n")
	matrixRequireUsageFact(t, cache, path, refNow, false, true)
	if scans := count.Load(); scans != 4 {
		t.Fatalf("unknown results cached or first flush missed: scans=%d", scans)
	}
}

func TestMatrixUsageLimitCachePartialTailAndNoVerdictRetry(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		formed, limited bool
	}{
		{"torn newer record", recRateLimitAt(stampAt(-time.Minute)) + "\n" + `{"type":"assistant","timestamp":"partial`, true, true},
		{"valid without final newline", recRateLimitAt(stampAt(-time.Minute)), true, true},
		{"malformed terminated newer record", recRateLimitAt(stampAt(-time.Minute)) + "\n" + "bad-json\n", true, true},
		{"no assistant", recUserAt(stampAt(-time.Minute)) + "\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thread.jsonl")
			matrixWriteUsageBody(t, path, tc.body)
			cache, count := matrixCountedUsageCache(16)
			for n := 0; n < 3; n++ {
				matrixRequireUsageFact(t, cache, path, refNow, tc.limited, tc.formed)
			}
			if scans := count.Load(); scans != 3 {
				t.Fatalf("retryable file cached: scans=%d", scans)
			}
			matrixWriteUsageBody(t, path, tc.body+"\n"+recAssistantAt(stampAt(0), "flushed")+"\n")
			matrixRequireUsageFact(t, cache, path, refNow, false, true)
		})
	}
}

func TestMatrixUsageLimitCacheSameSizePreservedMtimeRewriteAndReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "same inode", true: "replacement inode"}[replace], func(t *testing.T) {
			body := recRateLimitAt(stampAt(-time.Minute)) + "\n"
			changed := strings.Replace(body, `"apiErrorStatus":429`, `"apiErrorStatus":401`, 1)
			path := filepath.Join(t.TempDir(), "thread.jsonl")
			matrixWriteUsageBody(t, path, body)
			cache, count := matrixCountedUsageCache(16)
			matrixRequireUsageFact(t, cache, path, refNow, true, true)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !replace {
				if _, _, ok := matrixTurnInterruptChangeTime(before); !ok {
					t.Skip("same-inode preserved-mtime detection needs platform ctime")
				}
			}
			if replace {
				temp := path + ".replacement"
				matrixWriteUsageBody(t, temp, changed)
				if err := os.Chtimes(temp, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(temp, path); err != nil {
					t.Fatal(err)
				}
			} else {
				matrixWriteUsageBody(t, path, changed)
				if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			for n := 0; n < 3; n++ {
				matrixRequireUsageFact(t, cache, path, refNow, false, true)
			}
			if scans := count.Load(); scans != 2 {
				t.Fatalf("preserved-mtime change scans=%d", scans)
			}
		})
	}
}

func TestMatrixUsageLimitCacheSeparatesHomesAndCanonicalAliases(t *testing.T) {
	first := filepath.Join(t.TempDir(), "same-id.jsonl")
	second := filepath.Join(t.TempDir(), "same-id.jsonl")
	matrixWriteUsageBody(t, first, recRateLimitAt(stampAt(-time.Minute))+"\n")
	matrixWriteUsageBody(t, second, recAssistantAt(stampAt(-time.Minute), "normal")+"\n")
	alias := filepath.Join(t.TempDir(), "alias.jsonl")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	cache, count := matrixCountedUsageCache(16)
	for n := 0; n < 3; n++ {
		matrixRequireUsageFact(t, cache, first, refNow, true, true)
		matrixRequireUsageFact(t, cache, alias, refNow, true, true)
		matrixRequireUsageFact(t, cache, second, refNow, false, true)
	}
	if count.Load() != 2 {
		t.Fatalf("aliases failed to share or homes collided: %d", count.Load())
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	matrixRequireUsageFact(t, cache, alias, refNow, false, true)
	if count.Load() != 2 {
		t.Fatal("retargeted alias reused the wrong home or rescanned warm target")
	}
}

func TestMatrixUsageLimitCacheConcurrentReadersShareOneRealScan(t *testing.T) {
	path := writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute)))
	cache, count := matrixCountedUsageCache(16)
	entered := make(chan struct{})
	release := make(chan struct{})
	scan := cache.scan
	cache.scan = func(path string) matrixUsageLimitScanResult { close(entered); <-release; return scan(path) }
	var wg sync.WaitGroup
	for n := 0; n < 32; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); matrixRequireUsageFact(t, cache, path, refNow, true, true) }()
	}
	<-entered
	close(release)
	wg.Wait()
	if scans := count.Load(); scans != 1 {
		t.Fatalf("concurrent scans=%d", scans)
	}
}

func TestMatrixUsageLimitCacheChangeDuringReadDoesNotStoreOldFact(t *testing.T) {
	path := writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute)))
	cache, count := matrixCountedUsageCache(16)
	scan := cache.scan
	first := true
	cache.scan = func(path string) matrixUsageLimitScanResult {
		result := scan(path)
		if first {
			first = false
			matrixWriteUsageBody(t, path, recAssistantAt(stampAt(0), "new")+"\n")
		}
		return result
	}
	matrixRequireUsageFact(t, cache, path, refNow, true, true)
	for n := 0; n < 3; n++ {
		matrixRequireUsageFact(t, cache, path, refNow, false, true)
	}
	if scans := count.Load(); scans != 2 {
		t.Fatalf("read-window mutation cached stale fact: %d", scans)
	}
}

func TestMatrixUsageLimitCacheReplacementDuringBoundedReadRetries(t *testing.T) {
	path := writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute)))
	cache, count := matrixCountedUsageCache(16)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".new"
	matrixWriteUsageBody(t, replacement, recAssistantAt(stampAt(0), "normal")+"\n")
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed := false
	usageLimitScanObserver = func(read usageLimitScanRead) {
		if read.Kind == "chunk" && !changed {
			changed = true
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { usageLimitScanObserver = nil })
	matrixRequireUsageFact(t, cache, path, refNow, true, true)
	if !changed {
		t.Fatal("replacement was not injected into the actual read")
	}
	for n := 0; n < 3; n++ {
		matrixRequireUsageFact(t, cache, path, refNow, false, true)
	}
	if count.Load() != 2 {
		t.Fatalf("old descriptor result cached under replacement: %d", count.Load())
	}
}

func TestMatrixUsageLimitCacheRebindAtoBtoAWarmFiles(t *testing.T) {
	project := t.TempDir()
	inst := NewInstanceWithTool("matrix-usage-cache-rebind", project, "claude")
	firstID, secondID := "matrix-usage-cache-A", "matrix-usage-cache-B"
	dir := filepath.Join(GetClaudeConfigDirForInstance(inst), "projects", ConvertToClaudeDirName(project))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	firstPath := filepath.Join(dir, firstID+".jsonl")
	secondPath := filepath.Join(dir, secondID+".jsonl")
	matrixWriteUsageBody(t, firstPath, recRateLimitAt(now.Add(-time.Minute).Format(time.RFC3339Nano))+"\n")
	matrixWriteUsageBody(t, secondPath, recAssistantAt(now.Add(-time.Minute).Format(time.RFC3339Nano), "normal")+"\n")
	if limited, formed := latestAssistantTurnIsRateLimited(firstPath, now); !limited || !formed {
		t.Fatal("warm A did not form rejection")
	}
	if limited, formed := latestAssistantTurnIsRateLimited(secondPath, now); limited || !formed {
		t.Fatal("warm B did not form normal result")
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{{firstID, true}, {secondID, false}, {firstID, true}} {
		inst.mu.Lock()
		inst.ClaudeSessionID = tc.id
		inst.mu.Unlock()
		if got := inst.usageLimited(); got != tc.want {
			t.Fatalf("warm rebind %s returned %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestMatrixUsageLimitCacheFailedScanRemainsRetryable(t *testing.T) {
	path := writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute)))
	cache, count := matrixCountedUsageCache(16)
	scan := cache.scan
	first := true
	cache.scan = func(path string) matrixUsageLimitScanResult {
		if first {
			first = false
			return matrixUsageLimitScanResult{}
		}
		return scan(path)
	}
	matrixRequireUsageFact(t, cache, path, refNow, false, false)
	for n := 0; n < 3; n++ {
		matrixRequireUsageFact(t, cache, path, refNow, true, true)
	}
	if count.Load() != 1 {
		t.Fatalf("failed result suppressed the real retry: %d", count.Load())
	}
}

func TestMatrixUsageLimitCacheBoundedEntries(t *testing.T) {
	cache, count := matrixCountedUsageCache(2)
	paths := []string{writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute))), writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute))), writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute)))}
	for _, path := range paths {
		matrixRequireUsageFact(t, cache, path, refNow, true, true)
	}
	cache.mu.Lock()
	entries := len(cache.entries)
	cache.mu.Unlock()
	if entries != 2 {
		t.Fatalf("cache entries=%d", entries)
	}
	matrixRequireUsageFact(t, cache, paths[0], refNow, true, true)
	if count.Load() != 4 {
		t.Fatalf("LRU entry was not retired: %d", count.Load())
	}
}

func TestMatrixUsageLimitCachePinsBusyEntryAtCapacity(t *testing.T) {
	first := writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute)))
	second := writeUsageLimitTranscript(t, recRateLimitAt(stampAt(-time.Minute)))
	cache, count := matrixCountedUsageCache(1)
	entered, release := make(chan struct{}), make(chan struct{})
	scan := cache.scan
	var blocked atomic.Bool
	cache.scan = func(path string) matrixUsageLimitScanResult {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return scan(path)
	}
	done := make(chan struct{}, 3)
	go func() { matrixRequireUsageFact(t, cache, first, refNow, true, true); done <- struct{}{} }()
	<-entered
	go func() { matrixRequireUsageFact(t, cache, first, refNow, true, true); done <- struct{}{} }()
	go func() { matrixRequireUsageFact(t, cache, second, refNow, true, true); done <- struct{}{} }()
	deadline := time.Now().Add(time.Second)
	for {
		cache.mu.Lock()
		users := 0
		for _, entry := range cache.entries {
			users = entry.users
		}
		cache.mu.Unlock()
		if users == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("same-file waiter did not pin entry")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for n := 0; n < 3; n++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cache capacity wait did not finish")
		}
	}
	if scans := count.Load(); scans != 2 {
		t.Fatalf("pinned entry was evicted and scanned twice: %d", scans)
	}
}
