package mcpserver

import (
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

func TestSearchAnswersOnAFallbackWithOutOfRangeContextValues(t *testing.T) {
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "-0.5")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "5")

	srv, session := newSearchSession(t, weakSet(weakRow("A1", "memory A1")))
	srv.SetContextConfig(config.FallbackConfig().Context)
	searchText(t, session, "ledger") // fails the test on an error result
}
