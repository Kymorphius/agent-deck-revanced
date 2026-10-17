package web

import (
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/sysinfo"
)

// matrix/hardened: keep the existing five-second browser cadence while sharing
// nearby requests from multiple tabs. No timer runs when nobody requests stats.
const systemStatsCacheTTL = 2 * time.Second

// matrix/hardened: the cache belongs to one Server, never a process-wide profile
// map. Stats contains only values, so callers cannot mutate the stored snapshot.
type systemStatsCache struct {
	mu          sync.Mutex
	stats       sysinfo.Stats
	collectedAt time.Time
	valid       bool

	// Test seams are bound before the first request and remain fixed for the
	// lifetime of this cache, so snapshots never cross collector identities.
	collect func() sysinfo.Stats
	now     func() time.Time
}

func (c *systemStatsCache) load() sysinfo.Stats {
	// matrix/hardened: hold only this cache's mutex through collection. Requests
	// arriving during the same miss wait and reuse the completed sample.
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now
	if now == nil {
		now = time.Now
	}
	age := now().Sub(c.collectedAt)
	if c.valid && age >= 0 && age < systemStatsCacheTTL {
		return c.stats
	}

	collect := c.collect
	if collect == nil {
		collect = sysinfo.Collect
	}
	stats := collect()
	c.stats = stats
	c.collectedAt = now() // Slow collection must not consume the sample's TTL.
	c.valid = true
	return stats
}
