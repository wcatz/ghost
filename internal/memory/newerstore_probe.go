package memory

import (
	"sync/atomic"
)

var storeVersionProbes atomic.Pointer[probeCounter]

type probeCounter struct {
	userVersion atomic.Int64
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
