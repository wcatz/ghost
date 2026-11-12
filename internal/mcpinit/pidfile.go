package mcpinit

import (
	"io"
	"os"
)

const maxPIDFileBytes = 4096

func readPIDFile(path string) ([]byte, bool) {
	f, err := os.OpenFile(path, markerOpenFlags, 0) // #nosec G304 -- a PID file inside the Ghost data directory, not caller input
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPIDFileBytes+1))
	if err != nil || len(data) > maxPIDFileBytes {
		return nil, false
	}
	return data, true
}
