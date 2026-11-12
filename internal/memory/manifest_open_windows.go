//go:build windows

package memory

import "os"

// manifestOpenFlags creates or opens a manifest sidecar for writing.
const manifestOpenFlags = os.O_CREATE | os.O_WRONLY
