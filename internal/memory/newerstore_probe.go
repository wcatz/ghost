package memory

import (
	"sync/atomic"
	"testing"
)

var storeVersionProbes atomic.Pointer[probeCounter]

type probeCounter struct {
	userVersion atomic.Int64
}

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

func recordStoreVersionProbe(name string) {
	c := storeVersionProbes.Load()
	if c == nil {
		return
	}
	if name == "user_version" {
		c.userVersion.Add(1)
	}
}
