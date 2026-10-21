package mcpinit

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// errNotRegularFile marks the refusal of a host configuration path that is
// not a regular file. It is a sentinel rather than a bare message so a caller
// can word its advice for this case — the file has to be replaced, not
// edited — instead of for a document that does not parse.
var errNotRegularFile = errors.New("not a regular file")

// readHostConfigFile reads a file Ghost finds in the user's home — Claude
// Code's settings.json and plugins registry, codex's config.toml and
// hooks.json, opencode's config and the lifecycle plugin, goose's package
// files, and the MEMORY.md redirect — and refuses anything that is not a
// regular file.
//
// The path belongs to the host, and every reader used to read it with a bare
// os.ReadFile: opening a FIFO for reading blocks until a writer appears, so a
// FIFO at one of these paths stopped `ghost mcp init` and `ghost mcp status`
// with no output at all, and a character device such as /dev/zero was read
// until memory ran out.
//
// The open carries O_NONBLOCK on unix, so the open of a FIFO returns at once
// instead of waiting for a writer, and the fstat on the open descriptor
// refuses anything that is not a regular file BEFORE a byte is read. There is
// deliberately no O_NOFOLLOW: a symlinked settings.json or config.toml is an
// ordinary dotfiles setup, and the check is made on what the link names.
func readHostConfigFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, hostConfigOpenFlags, 0) // #nosec G304 -- the host's own config paths, not caller input
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
