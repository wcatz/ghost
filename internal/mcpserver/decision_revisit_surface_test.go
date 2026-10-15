package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDecisionsResourceShowsRevisitDecisions: the injected decisions resource
// reads a project's LIVE decisions rather than active-only, so a decision
// flagged for reconsideration is shown and labelled "(revisit)" instead of
// being hidden, while a superseded decision stays out. Before the change the
// resource answered "No active decisions for this project." about a project
// whose only decision was a revisit one, and ghost_decisions_list showed that
// same decision — two surfaces disagreeing about one store.
func TestDecisionsResourceShowsRevisitDecisions(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)

	plantDecision(t, db, "abc123", "A1", "keep sqlite", "sqlite stays", "no cgo", "active")
	plantDecision(t, db, "abc123", "R1", "revisit the cache", "reconsider rrf weights", "tuning changed", "revisit")
	plantDecision(t, db, "abc123", "S1", "retired choice", "we dropped this", "reversed", "superseded")

	rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "ghost://project/abc123/decisions",
	})
	if err != nil {
		t.Fatalf("ReadResource decisions: %v", err)
	}
	if len(rr.Contents) != 1 {
		t.Fatalf("expected 1 resource content, got %d", len(rr.Contents))
	}
	out := rr.Contents[0].Text

	if !strings.Contains(out, "keep sqlite") {
		t.Errorf("an active decision is missing from the resource:\n%s", out)
	}
	// The marker is asserted WITH the title so a stray "(revisit)" elsewhere in
	// the block cannot satisfy it.
	if !strings.Contains(out, "**«revisit the cache»** (revisit):") {
		t.Errorf("a revisit decision is not shown and labelled:\n%s", out)
	}
	if strings.Contains(out, "retired choice") {
		t.Errorf("a superseded decision reached the resource:\n%s", out)
	}
}

// TestProjectContextShowsRevisitDecisions: the `## Recent Decisions` section of
// `ghost_project_context` (and the context resource beside it, which shares this
// renderer) shows a revisit decision with the same label the decisions resource
// uses, and a superseded one stays out.
func TestProjectContextShowsRevisitDecisions(t *testing.T) {
	db, _, srv := newStoreWithDB(t)

	plantDecision(t, db, "abc123", "A1", "keep sqlite", "sqlite stays", "no cgo", "active")
	plantDecision(t, db, "abc123", "R1", "revisit the cache", "reconsider rrf weights", "tuning changed", "revisit")
	plantDecision(t, db, "abc123", "S1", "retired choice", "we dropped this", "reversed", "superseded")

	text, err := srv.buildProjectContext(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	if !strings.Contains(text, "## Recent Decisions") {
		t.Fatalf("Recent Decisions section missing:\n%s", text)
	}
	if !strings.Contains(text, "**«revisit the cache»** (revisit):") {
		t.Errorf("a revisit decision is not shown and labelled:\n%s", text)
	}
	if strings.Contains(text, "retired choice") {
		t.Errorf("a superseded decision reached the section:\n%s", text)
	}
}

// TestProjectContextCapKeepsActiveAheadOfRevisit: the section's five-row cap is
// filled by active decisions before any revisit one, so a decision flagged for
// reconsideration cannot push an active decision out of the window.
func TestProjectContextCapKeepsActiveAheadOfRevisit(t *testing.T) {
	db, _, srv := newStoreWithDB(t)

	for i := 0; i < 5; i++ {
		id := "A" + string(rune('0'+i))
		plantDecision(t, db, "abc123", id, "active decision "+id, "body "+id, "why", "active")
	}
	plantDecision(t, db, "abc123", "R1", "revisit beyond the cap", "body R1", "why", "revisit")

	text, err := srv.buildProjectContext(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	for i := 0; i < 5; i++ {
		id := "A" + string(rune('0'+i))
		if !strings.Contains(text, "active decision "+id) {
			t.Errorf("active decision %s was pushed out by a revisit row:\n%s", id, text)
		}
	}
	if strings.Contains(text, "revisit beyond the cap") {
		t.Errorf("a revisit row displaced an active decision inside a full window:\n%s", text)
	}
}
