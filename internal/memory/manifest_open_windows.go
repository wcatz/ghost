//go:build windows

package memory

import "os"

// manifestOpenFlags are the flags a manifest sidecar is created with.
const manifestOpenFlags = os.O_CREATE | os.O_WRONLY
