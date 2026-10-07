package mcpserver

// The retrieval record names the session the call was made in.
//
// The audit judges only the calls a session made, so a record with an empty session id
// is never judged. Over stdio the transport reports no session, so the id comes from the
// host: Claude Code puts the id of the session in the server's environment, the same id
// it sends in every hook payload and names its session record after, and the stop hook's
// scan carries the payload's. Both halves must agree for a call to be judged at all.

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func lastRecordSession(t *testing.T, srv *Server) (session string, n int) {
	t.Helper()
	recs, err := srv.store.(*memory.Store).RetrievalRecords(context.Background(), 100)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) == 0 {
		return "", 0
	}
	return recs[0].SessionID, len(recs)
}

// TestSearchRecordsTheHostsSessionIDOverStdio: stdio assigns no transport id, so the
// session the host named in the server's environment is recorded.
func TestSearchRecordsTheHostsSessionIDOverStdio(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "host-session-1")
	srv, session := newCapSession(t)
	saveMem(t, session, "recorded corpus entry about the nightly export", nil)
	listingIDs(t, session, "nightly export", nil)
	if got, n := lastRecordSession(t, srv); n != 1 || got != "host-session-1" {
		t.Fatalf("search recorded %d row(s) with session %q, want 1 row naming host-session-1", n, got)
	}
}

// TestAHostThatNamesNoSessionLeavesTheRecordUnscoped: nothing is invented. Hosts whose
// server environment carries no session id (and a bridge that has none at all) record
// "", and such a call is left unjudged by the audit.
func TestAHostThatNamesNoSessionLeavesTheRecordUnscoped(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	srv, session := newCapSession(t)
	saveMem(t, session, "recorded corpus entry about the nightly export", nil)
	listingIDs(t, session, "nightly export", nil)
	if got, n := lastRecordSession(t, srv); n != 1 || got != "" {
		t.Fatalf("recorded %d row(s) with session %q, want 1 row with none", n, got)
	}
}

// TestProjectContextRecordsTheHostsSessionID: all four project-context surfaces reach
// the assembler through assembleProjectContext, so the process-level id is set there and
// covers the tool, both resources and the prompt alike.
func TestProjectContextRecordsTheHostsSessionID(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "host-session-2")
	st, _ := projectRecordStore(t)
	srv, session := validityServerFor(t, st)
	saveValidityRow(t, session, projectContextSentinel, nil)
	_ = callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"})
	if got, n := lastRecordSession(t, srv); n != 1 || got != "host-session-2" {
		t.Fatalf("project context recorded %d row(s) with session %q, want 1 row naming host-session-2", n, got)
	}
}
