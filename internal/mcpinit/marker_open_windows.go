//go:build windows

package mcpinit

import "os"

const markerOpenFlags = os.O_RDONLY
const momentAppendOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY
const logAppendOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY
const operatorLogOpenFlags = os.O_APPEND | os.O_CREATE | os.O_WRONLY
