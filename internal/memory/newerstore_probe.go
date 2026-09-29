package memory

// The counting seam behind TestStoreNewerCheckReadsUserVersionOnEveryWrite.
//
// The claim under test is a COST claim — exactly one PRAGMA user_version read
// per write, with no second pragma and no cache — so it has to be measured at
// the statement rather than inferred from the code's shape. A shape test would
// keep passing if a future change moved the read somewhere the shape cannot see,
// which is exactly the drift that turns "cheap" into "one extra round trip per
// write".
//
// The hook is a package-level atomic rather than a field on Store for the reason
// writelock's observer is one: the probe helper is called from a parameterless
// function on the write path, and a Store field would have to be threaded through
// it for no production benefit. Nothing in production installs a counter, so the
// cost there is one atomic load and a nil check per probe — the same price
// writelock's observer already pays per transaction.

import (
	"sync/atomic"
	"testing"
)

// storeVersionProbes counts reads of the pragma the check issues. A count rather
// than a flag because the assertion is about how MANY times user_version is read,
// which is the whole point of the gate.
var storeVersionProbes atomic.Pointer[probeCounter]

// probeCounter is boxed so the pointer is a single comparable word: a test
// installing a counter and the write path loading it must not tear.
type probeCounter struct {
	// userVersion is the ONLY pragma the check issues, and it is here alone on
	// purpose. An earlier version carried a data_version counter as well, left
	// over from the cache that was removed as unsound — a field named after the
	// pragma no longer involved is worse than no field, because the next person
	// looking for that cache finds one and concludes it is still there.
	userVersion atomic.Int64
}

// countStoreVersionProbes installs a fresh counter and returns the restore. It
// starts from zero, so a test that reads the totals part-way through sees only
// its own writes.
func countStoreVersionProbes(t *testing.T) func() {
	t.Helper()
	prev := storeVersionProbes.Load()
	storeVersionProbes.Store(&probeCounter{})
	return func() { storeVersionProbes.Store(prev) }
}

// storeVersionProbeCounts returns the totals read so far, keyed by pragma.
func storeVersionProbeCounts() map[string]int {
	c := storeVersionProbes.Load()
	if c == nil {
		return map[string]int{}
	}
	return map[string]int{"user_version": int(c.userVersion.Load())}
}

// recordStoreVersionProbe is called by the check for each pragma it issues. It
// is the only production-visible cost, and it is a no-op without a counter.
func recordStoreVersionProbe(name string) {
	c := storeVersionProbes.Load()
	if c == nil {
		return
	}
	if name == "user_version" {
		c.userVersion.Add(1)
	}
}
