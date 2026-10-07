package mcpinit

// The session-start record names the session it opened.
//
// The hook payload's session_id is the one id both ends of the audit can agree on: the
// stop hook's scan carries it, and so must the record of the block that session was
// opened with, or the audit would leave the session's own opening block unjudged.

import (
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

func TestTheSessionStartRecordCarriesThePayloadsSessionID(t *testing.T) {
	projDir, dbPath := sessionRecordFixture(t)
	loadSessionContextFor(projDir, config.LoadForHook(), "hook-session-7")
	calls := recordedCalls(t, dbPath)
	if len(calls) != 1 || calls[0].source != "session_start" {
		t.Fatalf("recorded %+v, want exactly one session_start call", calls)
	}
	if calls[0].sessionID != "hook-session-7" {
		t.Errorf("session_id = %q, want the payload's hook-session-7", calls[0].sessionID)
	}
}

// TestAContextRenderWithNoSessionRecordsNone: `ghost context` and opencode's plugin have
// no hook payload, so their block is recorded with no session and is left unjudged.
func TestAContextRenderWithNoSessionRecordsNone(t *testing.T) {
	projDir, dbPath := sessionRecordFixture(t)
	loadSessionContext(projDir, config.LoadForHook())
	calls := recordedCalls(t, dbPath)
	if len(calls) != 1 || calls[0].sessionID != "" {
		t.Fatalf("recorded %+v, want one call with no session", calls)
	}
}
