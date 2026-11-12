package memory

import (
	"testing"
	"time"
)

func passiveKeyScanReads() int64 { return passiveKeyScanVisited.Load() }

func resetPassiveKeyScan() { passiveKeyScanVisited.Store(0) }

func countStoreVersionProbes(t *testing.T) func() {
	t.Helper()
	prev := storeVersionProbes.Load()
	storeVersionProbes.Store(&probeCounter{})
	return func() { storeVersionProbes.Store(prev) }
}

func storeVersionProbeCounts() map[string]int {
	c := storeVersionProbes.Load()
	if c == nil {
		return map[string]int{}
	}
	return map[string]int{"user_version": int(c.userVersion.Load())}
}

func setPruneBeforeDelete(fn func() error) (restore func()) {
	prev := pruneBeforeDelete
	pruneBeforeDelete = fn
	return func() { pruneBeforeDelete = prev }
}

// RetentionValues returns the supported retention tiers.
func RetentionValues() []string {
	out := make([]string, len(retentionTiers))
	copy(out, retentionTiers)
	return out
}

func resetQueryKeyCache() {
	queryKeyMu.Lock()
	defer queryKeyMu.Unlock()
	queryKeyLoaded, queryKeyBytes_ = false, nil
	queryKeyErr, queryKeyRetryAfter = nil, time.Time{}
}

// ParseScopeJSON turns a stored memories.scope column back into a map. It is
// parseScope for a caller that already holds the column text — a reader with its
// own read-only handle rather than a Store scan, such as the session-start
// loaders — so the two reads cannot disagree about what an unreadable column
// means. It stays nil, rather than failing, for a value it cannot decode.
func ParseScopeJSON(raw []byte) map[string]string { return parseScopeJSON(raw) }

func (c *vectorCache) setIdle(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.idle = d
}

func (s *vectorScratch) allocationCount() int64 {
	if s == nil {
		return 0
	}
	return s.allocs.Load()
}
