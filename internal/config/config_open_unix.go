//go:build !windows

package config

import (
	"os"
	"syscall"
)

const configOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK
