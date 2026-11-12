package mcpserver

import (
	"context"
	"fmt"
	"strings"
)

type storeVersionReporter interface {
	StoreVersionStatus(ctx context.Context) (storeVersion, buildVersion int, newer bool, err error)
}

func (s *Server) writeStoreVersionLine(ctx context.Context, sb *strings.Builder, reporter storeVersionReporter) {
	if reporter == nil {
		return
	}
	storeVersion, buildVersion, newer, err := reporter.StoreVersionStatus(ctx)
	if err != nil {
		fmt.Fprintf(sb, "⚠ could not read the store's schema version: %v\n\n", err)
		return
	}
	if !newer {
		return
	}
	fmt.Fprintf(sb,
		"⚠ **Writes are being refused.** The store is at schema v%d and this ghost build writes v%d, "+
			"so a newer Ghost migrated the store under this running server. Searches and reads still work; "+
			"saves, edits and deletes do not, and nothing they would have written is being recorded.\n"+
			"  → restart the client that runs this ghost server so it starts the newer binary\n\n",
		storeVersion, buildVersion)
}
