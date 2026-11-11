package ai

import "os"

// LiveTestsEnabled reports whether live LLM tests may run.
func LiveTestsEnabled() bool {
	return os.Getenv("GHOST_LIVE_TESTS") == "1"
}
