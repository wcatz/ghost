//go:build !unix

package memory

import "time"

func cpuClock() time.Duration { return time.Duration(time.Now().UnixNano()) }
