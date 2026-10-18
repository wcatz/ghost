//go:build !windows

package config

import (
	"os"
	"syscall"
)

// configOpenFlags are the flags readConfigLayer opens a config layer with.
// O_NONBLOCK keeps a FIFO at the config path from blocking the open until a
// writer appears; the fstat on the open descriptor then refuses it. It changes
// nothing for a regular file. There is deliberately no O_NOFOLLOW, unlike the
// claudeimport, manifest and audit sidecar readers: a symlinked config.yaml is
// a legitimate setup, and the regular-file check applies to the link's target.
const configOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK
