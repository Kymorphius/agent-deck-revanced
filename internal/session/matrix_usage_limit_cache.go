package session

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// matrix/hardened: stable transcripts need only a signature check at the existing
// five-second cadence. Cache bounded raw assistant facts, including normal turns.
const matrixUsageLimitFileCacheLimit = 1024

type matrixUsageLimitFact struct {
	rejection      bool
	at             time.Time
	timestampKnown bool
}

func (f matrixUsageLimitFact) limited(now time.Time) bool {
	// Unknown or future timestamps never prove a current rejection. The parsed
	// timestamp is immutable; freshness is evaluated anew rather than cached.
	if !f.rejection || !f.timestampKnown {
		return false
	}
	age := now.Sub(f.at)
	return age >= 0 && age <= usageLimitMaxAge
}

type matrixUsageLimitScanResult struct {
	fact              matrixUsageLimitFact
	formed, cacheable bool
	source            matrixTurnInterruptFileSignature
}

type matrixUsageLimitFileEntry struct {
	mu        sync.Mutex
	lastUsed  uint64 // guarded by the cache mutex
	users     int    // pending readers pin the entry against eviction
	signature matrixTurnInterruptFileSignature
	result    matrixUsageLimitScanResult
	valid     bool
}

type matrixUsageLimitFileCache struct {
	mu      sync.Mutex
	entries map[string]*matrixUsageLimitFileEntry
	limit   int
	clock   uint64
	space   *sync.Cond
	// A per-cache seam permits real file/read counters without mutating globals.
	scan func(string) matrixUsageLimitScanResult
}

var matrixUsageLimitFiles = newMatrixUsageLimitFileCache(matrixUsageLimitFileCacheLimit)

func newMatrixUsageLimitFileCache(limit int) *matrixUsageLimitFileCache {
	c := &matrixUsageLimitFileCache{entries: make(map[string]*matrixUsageLimitFileEntry), limit: max(limit, 1), scan: scanLatestAssistantUsageLimitFact}
	c.space = sync.NewCond(&c.mu)
	return c
}

// load uses the actual canonical file path, rather than a session ID that may
// name different transcripts in separate account homes. Misses are not cached.
func (c *matrixUsageLimitFileCache) load(requested string) matrixUsageLimitScanResult {
	absolute, err := filepath.Abs(requested)
	if err != nil {
		return matrixUsageLimitScanResult{}
	}
	path, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return matrixUsageLimitScanResult{}
	}
	entry := c.entry(path)
	defer c.release(entry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() {
		entry.valid = false
		return matrixUsageLimitScanResult{}
	}
	signature := matrixTurnInterruptSignature(before)
	if entry.valid && entry.signature.equal(signature) {
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil && resolved == path {
			return entry.result
		}
		return matrixUsageLimitScanResult{}
	}
	entry.valid = false
	result := c.scan(path)
	// Check both the opened descriptor and the name after reading. A replace or
	// rewrite during the scan cannot publish facts under a different signature.
	if result.formed && result.cacheable && signature.equal(result.source) {
		if after, err := os.Stat(path); err == nil && signature.equal(matrixTurnInterruptSignature(after)) {
			entry.signature, entry.result, entry.valid = signature, result, true
		}
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err != nil || resolved != path {
		return matrixUsageLimitScanResult{}
	}
	return result
}

func (c *matrixUsageLimitFileCache) entry(path string) *matrixUsageLimitFileEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		c.clock++
		if entry := c.entries[path]; entry != nil {
			entry.lastUsed = c.clock
			entry.users++
			return entry
		}
		if len(c.entries) < c.limit {
			break
		}
		var oldestPath string
		var oldest *matrixUsageLimitFileEntry
		for key, entry := range c.entries {
			if entry.users == 0 && (oldest == nil || entry.lastUsed < oldest.lastUsed) {
				oldestPath, oldest = key, entry
			}
		}
		if oldest != nil {
			delete(c.entries, oldestPath)
			break
		}
		// Do not evict a scan or its waiting readers: otherwise another call
		// could start a second scanner for the same file at capacity.
		c.space.Wait()
	}
	entry := &matrixUsageLimitFileEntry{lastUsed: c.clock, users: 1}
	c.entries[path] = entry
	return entry
}

func (c *matrixUsageLimitFileCache) release(entry *matrixUsageLimitFileEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.users--
	if entry.users == 0 {
		c.space.Broadcast()
	}
}
