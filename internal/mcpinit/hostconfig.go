package mcpinit

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var errNotRegularFile = errors.New("not a regular file")

func readHostConfigFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, hostConfigOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w (%s)", errNotRegularFile, info.Mode().Type())
	}
	return io.ReadAll(f)
}
