package statedb

// matrix/hardened: expose the exact recent-config key for targeted permanent
// deletion, rather than deleting every recent item with a similar title/path.
func RecentSessionConfigID(row *RecentSessionRow) string { return recentSessionDedupID(row) }
