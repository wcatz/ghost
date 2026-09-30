package mcpserver

// The package's XDG sandbox, and the reason it is here rather than in each test.
//
// These tests build real `*memory.Store`s and call real tools. Since #646 a
// search also writes a retrieval record, and that record's query digest is an
// HMAC under a PER-INSTALL KEY that lives in a file beside the database. Resolving
// that key reads $XDG_DATA_HOME, so without this the suite would CREATE A KEY IN
// THE DEVELOPER'S REAL DATA DIRECTORY on every run — and worse, every run would
// find the one a previous run left and quietly use it, so the suite's behaviour
// would depend on the state of the machine it ran on.
//
// A TestMain is the right shape for this rather than a t.Setenv per test: it has
// to hold for every test in the package, including ones added later, and a helper
// a test can forget is not a sandbox. t.Setenv cannot be used from TestMain
// anyway (it needs a *testing.T), which is the other reason.

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ghost-mcpserver-xdg-")
	if err != nil {
		panic("mcpserver tests: sandbox the XDG roots: " + err.Error())
	}
	// Both roots, because a test that reaches for either must reach this tree:
	// XDG_DATA_HOME is where the retrieval key goes, and pointing only one of the
	// two leaves the other aimed at the developer's home.
	for _, kv := range [][2]string{
		{"XDG_DATA_HOME", dir},
		{"XDG_CONFIG_HOME", dir},
		{"XDG_STATE_HOME", dir},
		{"XDG_CACHE_HOME", dir},
	} {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			panic("mcpserver tests: set " + kv[0] + ": " + err.Error())
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
