package session

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// matrix/hardened: use private file caches and synthetic transcripts so tests
// count actual parsing without replacing global state or reading live history.
func matrixCountedInterruptCache(limit int) (*matrixTurnInterruptFileCache, *atomic.Int64) {
	cache := newMatrixTurnInterruptFileCache(limit)
	count := new(atomic.Int64)
	cache.parse = func(tool, path string) matrixTurnInterruptFileResult {
		count.Add(1)
		return matrixParseTurnInterruptFile(tool, path)
	}
	return cache, count
}

func matrixWriteInterruptCacheBody(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func matrixInterruptCacheBody(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func matrixSeedInterruptCacheRollout(t *testing.T, home, sid string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "10", "12")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-fixture-"+sid+".jsonl")
	matrixWriteInterruptCacheBody(t, path, matrixInterruptCacheBody(lines...))
	return path
}

func matrixInterruptCacheInstance(home, sid string) *Instance {
	return &Instance{Tool: "codex", CodexSessionID: sid, Command: `CODEX_HOME="` + home + `" codex`}
}

func matrixRequireInterruptCacheResult(t *testing.T, got matrixTurnInterruptFileResult, found bool, detail string) {
	t.Helper()
	if got.found != found {
		t.Fatalf("found = %v, want %v (verdict=%+v)", got.found, found, got.verdict)
	}
	if found && got.verdict.Detail() != detail {
		t.Fatalf("Detail() = %q, want %q", got.verdict.Detail(), detail)
	}
}

func TestMatrixTurnInterruptFileCacheFreshInstancesShareParse(t *testing.T) {
	home := t.TempDir()
	const sid = "019f0000-0000-7000-8000-999999998001"
	matrixSeedInterruptCacheRollout(t, home, sid, cxMeta, cxStarted, cxAborted)
	cache, count := matrixCountedInterruptCache(16)
	for n := 0; n < 2; n++ {
		inst := matrixInterruptCacheInstance(home, sid)
		inst.matrixRefreshTurnInterruptWithCache(cache)
		v, found := inst.matrixTurnInterruptCached()
		if !found || v.Detail() != "turn_aborted: interrupted" {
			t.Fatalf("fresh instance %d: found=%v, verdict=%+v", n, found, v)
		}
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("fresh instances parsed the same stable rollout %d times, want 1", got)
	}
}

func TestMatrixTurnInterruptFileCacheSeparatesHomesAndMetadata(t *testing.T) {
	const sid = "019f0000-0000-7000-8000-999999998002"
	userHome, subagentHome := t.TempDir(), t.TempDir()
	userPath := matrixSeedInterruptCacheRollout(t, userHome, sid, cxMeta, cxAborted)
	subagentPath := matrixSeedInterruptCacheRollout(t, subagentHome, sid, cxSubMeta, cxStarted)
	cache, count := matrixCountedInterruptCache(16)
	for n := 0; n < 3; n++ {
		user := cache.load("codex", userPath)
		if !user.meta.valid || user.meta.ThreadSource != "user" || user.meta.ParentThreadID != "" {
			t.Fatalf("user home metadata = %+v", user.meta)
		}
		matrixRequireInterruptCacheResult(t, user, true, "turn_aborted: interrupted")
		subagent := cache.load("codex", subagentPath)
		if !subagent.meta.valid || subagent.meta.ThreadSource != "subagent" || subagent.meta.ParentThreadID != "parent" {
			t.Fatalf("subagent home metadata = %+v", subagent.meta)
		}
		matrixRequireInterruptCacheResult(t, subagent, false, "")
		for _, tc := range []struct {
			home string
			want bool
		}{{userHome, true}, {subagentHome, false}} {
			inst := matrixInterruptCacheInstance(tc.home, sid)
			inst.matrixRefreshTurnInterruptWithCache(cache)
			if _, found := inst.matrixTurnInterruptCached(); found != tc.want {
				t.Fatalf("home %q fresh instance interrupted=%v, want %v", tc.home, found, tc.want)
			}
		}
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("two stable homes parsed %d times, want 2", got)
	}
}

func TestMatrixTurnInterruptFileCacheCachesNormalNegativeVerdicts(t *testing.T) {
	for _, tc := range []struct {
		tool  string
		lines []string
	}{{"codex", []string{cxMeta, cxStarted}}, {"claude", []string{tiUser, tiAssistant}}} {
		t.Run(tc.tool, func(t *testing.T) {
			path := matrixWriteTranscript(t, tc.lines...)
			cache, count := matrixCountedInterruptCache(16)
			for n := 0; n < 10; n++ {
				got := cache.load(tc.tool, path)
				matrixRequireInterruptCacheResult(t, got, false, "")
				if !got.cacheable {
					t.Fatal("complete normal verdict was not cacheable")
				}
			}
			if got := count.Load(); got != 1 {
				t.Fatalf("normal negative parsed %d times, want 1", got)
			}
		})
	}
}

func TestMatrixTurnInterruptFileCacheAppendClearsWarmAbort(t *testing.T) {
	for _, event := range []string{"task_started", "turn_started", "user_message", "task_complete", "turn_complete"} {
		t.Run(event, func(t *testing.T) {
			path := matrixWriteTranscript(t, cxMeta, cxAborted)
			cache, count := matrixCountedInterruptCache(16)
			matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := fmt.Fprintf(f, "{\"type\":\"event_msg\",\"payload\":{\"type\":%q}}\n", event)
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("append: write=%v close=%v", writeErr, closeErr)
			}
			for n := 0; n < 2; n++ {
				matrixRequireInterruptCacheResult(t, cache.load("codex", path), false, "")
			}
			if got := count.Load(); got != 2 {
				t.Fatalf("append parsed %d times, want 2", got)
			}
		})
	}
}

func TestMatrixTurnInterruptFileCacheSameSizeRewrite(t *testing.T) {
	path := matrixWriteTranscript(t, cxMeta, cxAborted)
	cache, count := matrixCountedInterruptCache(16)
	matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(matrixInterruptCacheBody(cxMeta, cxAborted), "interrupted", "new_failure", 1)
	matrixWriteInterruptCacheBody(t, path, changed)
	if err := os.Chtimes(path, before.ModTime(), before.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !os.SameFile(before, after) || before.ModTime().Equal(after.ModTime()) {
		t.Fatal("fixture must rewrite the same inode and size with changed mtime")
	}
	matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: new_failure")
	if got := count.Load(); got != 2 {
		t.Fatalf("same-size rewrite parsed %d times, want 2", got)
	}
}

func TestMatrixTurnInterruptFileCacheSameSizeMtimeReplacement(t *testing.T) {
	path := matrixWriteTranscript(t, cxMeta, cxAborted)
	cache, count := matrixCountedInterruptCache(16)
	matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := path + ".replacement"
	changed := strings.Replace(matrixInterruptCacheBody(cxMeta, cxAborted), "interrupted", "new_failure", 1)
	matrixWriteInterruptCacheBody(t, replacement, changed)
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || os.SameFile(before, after) {
		t.Fatal("fixture must replace inode while preserving size and mtime")
	}
	matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: new_failure")
	if got := count.Load(); got != 2 {
		t.Fatalf("inode replacement parsed %d times, want 2", got)
	}
}

// matrix/hardened: inspect native ctime portably in the test, independently of
// the cache helper, so preserving mtime cannot accidentally mask a rewrite.
func matrixInterruptTestCtime(info os.FileInfo) (string, bool) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return "", false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := v.FieldByName(name)
		if field.IsValid() && field.CanInterface() {
			return fmt.Sprint(field.Interface()), true
		}
	}
	return "", false
}

func TestMatrixTurnInterruptFileCacheSameInodePreservedMtimeRewrite(t *testing.T) {
	path := matrixWriteTranscript(t, cxMeta, cxAborted)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldCtime, supported := matrixInterruptTestCtime(before)
	if !supported {
		t.Skip("native FileInfo exposes no portable ctime field")
	}
	cache, count := matrixCountedInterruptCache(16)
	matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
	changed := strings.Replace(matrixInterruptCacheBody(cxMeta, cxAborted), "interrupted", "new_failure", 1)
	matrixWriteInterruptCacheBody(t, path, changed)
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	newCtime, _ := matrixInterruptTestCtime(after)
	if oldCtime == newCtime {
		t.Skip("filesystem ctime precision cannot distinguish this rewrite")
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("fixture must preserve inode, size, and mtime")
	}
	matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: new_failure")
	if got := count.Load(); got != 2 {
		t.Fatalf("ctime-only rewrite parsed %d times, want 2", got)
	}
}

func TestMatrixTurnInterruptFileCacheTruncateRemoveReappear(t *testing.T) {
	path := matrixWriteTranscript(t, cxMeta, cxAborted)
	cache, count := matrixCountedInterruptCache(16)
	matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
	matrixWriteInterruptCacheBody(t, path, matrixInterruptCacheBody(cxMeta))
	for n := 0; n < 2; n++ {
		matrixRequireInterruptCacheResult(t, cache.load("codex", path), false, "")
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("truncated complete file parsed %d times, want 2", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		got := cache.load("codex", path)
		matrixRequireInterruptCacheResult(t, got, false, "")
		if got.cacheable {
			t.Fatal("missing file result was cacheable")
		}
	}
	matrixWriteInterruptCacheBody(t, path, matrixInterruptCacheBody(cxMeta, cxAborted))
	beforeRestoreParse := count.Load()
	for n := 0; n < 2; n++ {
		matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
	}
	if got := count.Load() - beforeRestoreParse; got != 1 {
		t.Fatalf("restored stable file parsed %d times, want 1", got)
	}
}

func TestMatrixTurnInterruptFileCacheIncompleteFlushRetries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tool       string
		partial    string
		complete   string
		wantDetail string
	}{
		{"empty codex", "codex", "", matrixInterruptCacheBody(cxMeta, cxAborted), "turn_aborted: interrupted"},
		{"partial metadata", "codex", `{"type":"session_meta","payload":{"thread_source":`, matrixInterruptCacheBody(cxMeta, cxAborted), "turn_aborted: interrupted"},
		{"partial codex tail", "codex", matrixInterruptCacheBody(cxMeta) + `{"type":"event_msg","payload":{"type":"turn_aborted"`, matrixInterruptCacheBody(cxMeta, cxAborted), "turn_aborted: interrupted"},
		{"empty claude", "claude", "", matrixInterruptCacheBody(tiLost), "server_error: Connection lost mid-response."},
		{"partial claude tail", "claude", matrixInterruptCacheBody(tiUser) + `{"type":"assistant","isApiErrorMessage":true`, matrixInterruptCacheBody(tiUser, tiLost), "server_error: Connection lost mid-response."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "partial.jsonl")
			matrixWriteInterruptCacheBody(t, path, tc.partial)
			cache, count := matrixCountedInterruptCache(16)
			for n := 0; n < 2; n++ {
				got := cache.load(tc.tool, path)
				if got.cacheable {
					t.Fatal("incomplete file result was cacheable")
				}
			}
			if got := count.Load(); got != 2 {
				t.Fatalf("incomplete observations parsed %d times, want 2", got)
			}
			matrixWriteInterruptCacheBody(t, path, tc.complete)
			for n := 0; n < 2; n++ {
				got := cache.load(tc.tool, path)
				matrixRequireInterruptCacheResult(t, got, true, tc.wantDetail)
				if !got.cacheable {
					t.Fatal("complete first flush was not cacheable")
				}
			}
			if got := count.Load(); got != 3 {
				t.Fatalf("first complete flush parsed %d total times, want 3", got)
			}
		})
	}
}

func TestMatrixTurnInterruptFileCacheConcurrentLoadsMergeParse(t *testing.T) {
	path := matrixWriteTranscript(t, cxMeta, cxAborted)
	cache, count := matrixCountedInterruptCache(16)
	const workers = 32
	start := make(chan struct{})
	results := make(chan matrixTurnInterruptFileResult, workers)
	var group sync.WaitGroup
	for n := 0; n < workers; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- cache.load("codex", path)
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for got := range results {
		matrixRequireInterruptCacheResult(t, got, true, "turn_aborted: interrupted")
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("concurrent stable loads parsed %d times, want 1", got)
	}
}

func TestMatrixTurnInterruptFileCacheChangedDuringParseIsNotStored(t *testing.T) {
	path := matrixWriteTranscript(t, cxMeta, cxAborted)
	cache := newMatrixTurnInterruptFileCache(16)
	var count atomic.Int64
	var writeErr error
	cache.parse = func(tool, path string) matrixTurnInterruptFileResult {
		got := matrixParseTurnInterruptFile(tool, path)
		if count.Add(1) == 1 {
			writeErr = os.WriteFile(path, []byte(matrixInterruptCacheBody(cxMeta, cxAborted, cxStarted)), 0o600)
		}
		return got
	}
	cache.load("codex", path)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	for n := 0; n < 2; n++ {
		matrixRequireInterruptCacheResult(t, cache.load("codex", path), false, "")
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("unstable parse then stable loads parsed %d times, want 2", got)
	}
}

func TestMatrixTurnInterruptFileCacheRetriesUncacheableIOResult(t *testing.T) {
	path := matrixWriteTranscript(t, cxMeta, cxAborted)
	cache := newMatrixTurnInterruptFileCache(16)
	var count atomic.Int64
	cache.parse = func(tool, path string) matrixTurnInterruptFileResult {
		if count.Add(1) == 1 {
			// A transient read failure must not freeze a normal false verdict
			// while the filesystem signature remains unchanged.
			return matrixTurnInterruptFileResult{}
		}
		return matrixParseTurnInterruptFile(tool, path)
	}
	got := cache.load("codex", path)
	if got.found || got.cacheable {
		t.Fatalf("simulated I/O failure = %+v", got)
	}
	for n := 0; n < 2; n++ {
		matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("transient I/O failure and recovery parsed %d times, want 2", got)
	}
	for _, tool := range []string{"claude", "codex"} {
		missing := matrixParseTurnInterruptFile(tool, path+".missing")
		if missing.found || missing.cacheable {
			t.Fatalf("%s missing-file parser result = %+v", tool, missing)
		}
	}
}

func TestMatrixTurnInterruptFileCacheBoundedEntries(t *testing.T) {
	const limit = 3
	cache, _ := matrixCountedInterruptCache(limit)
	for n := 0; n < 12; n++ {
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		matrixWriteInterruptCacheBody(t, path, matrixInterruptCacheBody(cxMeta, cxAborted))
		matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
		if got := len(cache.entries); got > limit {
			t.Fatalf("cache entries=%d after file %d, limit=%d", got, n, limit)
		}
	}
}

// matrix/hardened: observing Cond.Wait's unlock proves the capacity waiter
// really registered before assertions, without sleeps or scheduler guesses.
type matrixInterruptCacheWaitObserver struct {
	sync.Locker
	waiting chan struct{}
}

func (o *matrixInterruptCacheWaitObserver) Unlock() {
	o.Locker.Unlock()
	select {
	case o.waiting <- struct{}{}:
	default:
	}
}

func TestMatrixTurnInterruptFileCachePinsBusyEntryAtCapacity(t *testing.T) {
	pathA := matrixWriteTranscript(t, cxMeta, cxAborted)
	pathB := matrixWriteTranscript(t, cxMeta, cxStarted)
	canonicalA, err := filepath.EvalSymlinks(pathA)
	if err != nil {
		t.Fatal(err)
	}
	canonicalB, err := filepath.EvalSymlinks(pathB)
	if err != nil {
		t.Fatal(err)
	}
	keyA := matrixTurnInterruptFileKey{tool: "codex", path: canonicalA}
	keyB := matrixTurnInterruptFileKey{tool: "codex", path: canonicalB}
	cache := newMatrixTurnInterruptFileCache(1)
	waiting := make(chan struct{}, 1)
	cache.space = sync.NewCond(&matrixInterruptCacheWaitObserver{Locker: &cache.mu, waiting: waiting})
	var parses atomic.Int64
	entered := make(chan struct{})
	finishParse := make(chan struct{})
	var finishOnce, releaseOnce sync.Once
	cache.parse = func(tool, path string) matrixTurnInterruptFileResult {
		if parses.Add(1) == 1 && path == canonicalA {
			close(entered)
			<-finishParse
		}
		return matrixParseTurnInterruptFile(tool, path)
	}
	resultA, resultB := make(chan matrixTurnInterruptFileResult, 1), make(chan matrixTurnInterruptFileResult, 1)
	doneA, doneB := make(chan struct{}), make(chan struct{})
	var joined *matrixTurnInterruptFileEntry
	bStarted := false
	defer func() {
		finishOnce.Do(func() { close(finishParse) })
		if joined != nil {
			releaseOnce.Do(func() { cache.release(joined) })
		}
		// On an assertion failure, also unblock a broken release/broadcast
		// implementation so no capacity waiter survives this test.
		cache.mu.Lock()
		cache.limit = 2
		cache.space.Broadcast()
		cache.mu.Unlock()
		pending := []chan struct{}{doneA}
		if bStarted {
			pending = append(pending, doneB)
		}
		for _, done := range pending {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("load remained blocked during cleanup")
			}
		}
	}()
	go func() {
		defer close(doneA)
		resultA <- cache.load("codex", pathA)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first path never entered parser")
	}
	cache.mu.Lock()
	busy := cache.entries[keyA]
	cache.mu.Unlock()
	if busy == nil {
		t.Fatal("in-flight path A has no cache entry")
	}
	bStarted = true
	go func() {
		defer close(doneB)
		resultB <- cache.load("codex", pathB)
	}()
	select {
	case <-waiting:
	case <-doneB:
		t.Fatal("path B evicted busy A instead of waiting for capacity")
	case <-time.After(5 * time.Second):
		t.Fatal("path B never registered its capacity wait")
	}
	joined = cache.entry(keyA)
	cache.mu.Lock()
	entryCount, users, same := len(cache.entries), joined.users, cache.entries[keyA] == busy && joined == busy
	cache.mu.Unlock()
	if entryCount != 1 || !same || users != 2 {
		t.Fatalf("same-path join: entries=%d same=%v users=%d, want 1/true/2", entryCount, same, users)
	}
	finishOnce.Do(func() { close(finishParse) })
	select {
	case <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("path A did not finish after parser release")
	}
	matrixRequireInterruptCacheResult(t, <-resultA, true, "turn_aborted: interrupted")
	matrixRequireInterruptCacheResult(t, cache.load("codex", pathA), true, "turn_aborted: interrupted")
	if got := parses.Load(); got != 1 {
		t.Fatalf("busy and later same-path loads parsed %d times, want 1", got)
	}
	cache.mu.Lock()
	entryCount, users, same = len(cache.entries), joined.users, cache.entries[keyA] == joined
	cache.mu.Unlock()
	if entryCount != 1 || !same || users != 1 {
		t.Fatalf("active join lost its pin: entries=%d same=%v users=%d", entryCount, same, users)
	}
	select {
	case <-doneB:
		t.Fatal("path B proceeded before the last active reference released A")
	default:
	}
	releaseOnce.Do(func() { cache.release(joined) })
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("path B did not wake after A released capacity")
	}
	matrixRequireInterruptCacheResult(t, <-resultB, false, "")
	if got := parses.Load(); got != 2 {
		t.Fatalf("A then B parsed %d total times, want 2", got)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 1 || cache.entries[keyA] != nil || cache.entries[keyB] == nil || cache.entries[keyB].users != 0 {
		t.Fatalf("released capacity did not retain exactly one inactive B entry: entries=%v", cache.entries)
	}
}

func TestMatrixTurnInterruptFileCacheCanonicalPathAndToolIdentity(t *testing.T) {
	path := matrixWriteTranscript(t, cxMeta, cxAborted)
	alias := filepath.Join(filepath.Dir(path), "alias.jsonl")
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cache, count := matrixCountedInterruptCache(16)
	matrixRequireInterruptCacheResult(t, cache.load("codex", path), true, "turn_aborted: interrupted")
	matrixRequireInterruptCacheResult(t, cache.load("codex", alias), true, "turn_aborted: interrupted")
	if got := count.Load(); got != 1 {
		t.Fatalf("canonical and symlink paths parsed %d times, want 1", got)
	}
	matrixRequireInterruptCacheResult(t, cache.load("claude", path), false, "")
	if got := count.Load(); got != 2 {
		t.Fatalf("distinct parser tool identities parsed %d times, want 2", got)
	}
}

func TestMatrixTurnInterruptFileCacheRebindRejectsOldInflightVerdict(t *testing.T) {
	home := t.TempDir()
	const oldSID = "019f0000-0000-7000-8000-999999998003"
	const newSID = "019f0000-0000-7000-8000-999999998004"
	oldPath := matrixSeedInterruptCacheRollout(t, home, oldSID, cxMeta, cxAborted)
	oldPath, err := filepath.EvalSymlinks(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	matrixSeedInterruptCacheRollout(t, home, newSID, cxMeta, cxStarted)
	cache := newMatrixTurnInterruptFileCache(16)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	cache.parse = func(tool, path string) matrixTurnInterruptFileResult {
		got := matrixParseTurnInterruptFile(tool, path)
		if path == oldPath {
			close(entered)
			<-release
		}
		return got
	}
	inst := matrixInterruptCacheInstance(home, oldSID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		inst.matrixRefreshTurnInterruptWithCache(cache)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old refresh did not enter parser")
	}
	inst.mu.Lock()
	inst.CodexSessionID = newSID
	inst.mu.Unlock()
	inst.matrixRefreshTurnInterruptWithCache(cache)
	if _, found := inst.matrixTurnInterruptCached(); found {
		t.Fatal("new active thread inherited interrupted verdict")
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old refresh did not complete")
	}
	if _, found := inst.matrixTurnInterruptCached(); found {
		t.Fatal("old in-flight scan published its verdict after rebind")
	}
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	if got := inst.matrixInterrupt.key; got != "codex:"+newSID {
		t.Fatalf("memo key = %q, want codex:%s", got, newSID)
	}
}

func TestMatrixTurnInterruptFileCacheInstanceThrottlePreserved(t *testing.T) {
	home := t.TempDir()
	const sid = "019f0000-0000-7000-8000-999999998005"
	path := matrixSeedInterruptCacheRollout(t, home, sid, cxMeta, cxAborted)
	cache, count := matrixCountedInterruptCache(16)
	inst := matrixInterruptCacheInstance(home, sid)
	inst.matrixRefreshTurnInterruptWithCache(cache)
	matrixWriteInterruptCacheBody(t, path, matrixInterruptCacheBody(cxMeta, cxStarted))
	inst.matrixRefreshTurnInterruptWithCache(cache)
	if _, found := inst.matrixTurnInterruptCached(); !found {
		t.Fatal("same instance bypassed its existing scan throttle")
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("throttled instance parsed %d times, want 1", got)
	}
	fresh := matrixInterruptCacheInstance(home, sid)
	fresh.matrixRefreshTurnInterruptWithCache(cache)
	if _, found := fresh.matrixTurnInterruptCached(); found {
		t.Fatal("fresh instance failed to see changed file immediately")
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("fresh instance parsed %d total times, want 2", got)
	}
}

func TestMatrixTurnInterruptFileCacheWarmInstanceClearsMissingOrSubagent(t *testing.T) {
	for _, tc := range []string{"removed", "replaced by subagent"} {
		t.Run(tc, func(t *testing.T) {
			home := t.TempDir()
			const sid = "019f0000-0000-7000-8000-999999998006"
			path := matrixSeedInterruptCacheRollout(t, home, sid, cxMeta, cxAborted)
			cache, _ := matrixCountedInterruptCache(16)
			inst := matrixInterruptCacheInstance(home, sid)
			inst.matrixRefreshTurnInterruptWithCache(cache)
			if _, found := inst.matrixTurnInterruptCached(); !found {
				t.Fatal("fixture failed to warm interrupted Instance")
			}
			if tc == "removed" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				replacement := path + ".replacement"
				matrixWriteInterruptCacheBody(t, replacement, matrixInterruptCacheBody(cxSubMeta, cxAborted))
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			}
			inst.mu.Lock()
			inst.matrixInterrupt.scannedAt = time.Now().Add(-matrixTurnInterruptScanInterval)
			inst.mu.Unlock()
			inst.matrixRefreshTurnInterruptWithCache(cache)
			if _, found := inst.matrixTurnInterruptCached(); found {
				t.Fatalf("warm Instance retained old interrupted verdict after rollout %s", tc)
			}
		})
	}
}

// matrix/hardened: synthetic large metadata and a stable trailing event stream
// expose both head and backward-tail allocations without touching live logs.
func BenchmarkMatrixTurnInterruptFileCache(b *testing.B) {
	path := filepath.Join(b.TempDir(), "rollout.jsonl")
	meta := `{"type":"session_meta","payload":{"id":"benchmark","thread_source":"user","base_instructions":"` + strings.Repeat("x", 768<<10) + `"}}`
	var body strings.Builder
	body.WriteString(matrixInterruptCacheBody(meta, cxAborted))
	for body.Len() < (1280 << 10) {
		body.WriteString(cxTokens)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	if info.Size() < (1 << 20) {
		b.Fatalf("benchmark fixture is only %d bytes, want >= 1 MiB", info.Size())
	}
	b.Run("OriginalParse", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			got := matrixParseTurnInterruptFile("codex", path)
			if !got.found || !got.cacheable || !got.meta.valid {
				b.Fatalf("parse result = %+v", got)
			}
		}
	})
	b.Run("CachedHit", func(b *testing.B) {
		cache := newMatrixTurnInterruptFileCache(16)
		if got := cache.load("codex", path); !got.found {
			b.Fatal("failed to warm cache")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			got := cache.load("codex", path)
			if !got.found || !got.cacheable || !got.meta.valid {
				b.Fatalf("cached result = %+v", got)
			}
		}
	})
	b.Run("CtimeSignature", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			signature := matrixTurnInterruptSignature(info)
			if signature.info != info {
				b.Fatal("signature changed file identity")
			}
		}
	})
}
