package mcpserver

// The store-version line in ghost_health (#746).
//
// A write against a store a newer Ghost owns is refused, and that refusal is
// the only place the reason appears. Without it here, the state a running
// server spends hours in — refusing every save, silently — looks exactly like a
// broken install: ghost_health reports the counts, the counts are stale, and
// both an agent and an operator read that as lost data.
//
// So this is a report, not a check. It asks the same question the write path
// asks and renders the answer, and it is deliberately separate from
// checkStoreNotNewer: a reader must not be refused, because the whole point is
// that reads keep working while writes do not. An agent that cannot search and
// cannot ask why is worse off than one that can only save nothing.

import (
	"context"
	"fmt"
	"strings"
)

// storeVersionReporter is the one method this needs, type-asserted at call time
// like every other store capability here (see updateCapableStore): it is not on
// provider.MemoryStore, and a store that cannot report a version is a store
// with nothing to report rather than a reason to fail the whole tool.
type storeVersionReporter interface {
	StoreVersionStatus(ctx context.Context) (storeVersion, buildVersion int, newer bool, err error)
}

// writeStoreVersionLine renders the store's schema version against this build's.
//
// It says nothing at all on a healthy store. A permanent line reading
// "schema v18 (current)" would be a line every operator learns to skip, and the
// one time the answer matters is the one time it would be ignored.
func (s *Server) writeStoreVersionLine(ctx context.Context, sb *strings.Builder) {
	reporter, ok := s.store.(storeVersionReporter)
	if !ok {
		return
	}
	storeVersion, buildVersion, newer, err := reporter.StoreVersionStatus(ctx)
	if err != nil {
		// An unanswerable question is reported as unanswerable. Rendering
		// nothing would let a server that cannot read its own version look
		// identical to a healthy one, which is the confusion this line exists
		// to remove.
		fmt.Fprintf(sb, "⚠ could not read the store's schema version: %v\n\n", err)
		return
	}
	if !newer {
		return
	}
	// Both numbers, because "the store moved" and "this build is behind" are
	// different problems with different fixes, and the reader cannot tell which
	// without them. The remedy names the RUNNING server rather than the binary
	// on disk: an operator whose binary is already current would run
	// `ghost upgrade`, watch it report "already up to date", and learn nothing.
	// Reads are named as still working, so the report reads as "restart me"
	// rather than as a dead server.
	fmt.Fprintf(sb,
		"⚠ **Writes are being refused.** The store is at schema v%d and this ghost build writes v%d, "+
			"so a newer Ghost migrated the store under this running server. Searches and reads still work; "+
			"saves, edits and deletes do not, and nothing they would have written is being recorded.\n"+
			"  → restart the client that runs this ghost server so it starts the newer binary\n\n",
		storeVersion, buildVersion)
}
