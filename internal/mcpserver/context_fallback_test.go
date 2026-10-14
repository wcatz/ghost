package mcpserver

import (
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

// The server falls back to config.FallbackConfig when the config does not
// load, and the fallback keeps the GHOST_* environment. An out-of-range
// context value there used to reach the assembler, whose request check then
// refused every search. The fallback replaces it with the compiled default, so
// the search answers.
func TestSearchAnswersOnAFallbackWithOutOfRangeContextValues(t *testing.T) {
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "-0.5")
	t.Setenv("GHOST_CONTEXT_NO_ANSWER_COSINE", "5")

	srv, session := newSearchSession(t, weakSet(weakRow("A1", "memory A1")))
	srv.SetContextConfig(config.FallbackConfig().Context)
	searchText(t, session, "ledger") // fails the test on an error result
}
