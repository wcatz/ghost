package mcpserver

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func stampDecisionCreatedAt(t *testing.T, db *sql.DB, id, mod string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE decisions SET created_at = datetime('now', ?) WHERE id = ?`, mod, id); err != nil {
		t.Fatalf("stamp created_at on decision %q: %v", id, err)
	}
}

func TestDecisionsResourceShowsRevisitDecisions(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)

	plantDecision(t, db, "abc123", "A1", "keep sqlite", "sqlite stays", "no cgo", "active")
	plantDecision(t, db, "abc123", "R1", "revisit the cache", "reconsider rrf weights", "tuning changed", "revisit")
	plantDecision(t, db, "abc123", "S1", "retired choice", "we dropped this", "reversed", "superseded")
	stampDecisionCreatedAt(t, db, "A1", "-1 day")
	stampDecisionCreatedAt(t, db, "R1", "-1 minute")

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
	if !strings.Contains(out, "**«revisit the cache»** (revisit):") {
		t.Errorf("a revisit decision is not shown and labelled:\n%s", out)
	}
	if strings.Contains(out, "retired choice") {
		t.Errorf("a superseded decision reached the resource:\n%s", out)
	}
	assertRowOrder(t, out, "keep sqlite", "revisit the cache")
}

func TestDecisionsResourceCapKeepsActiveAheadOfRevisit(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)

	for i := 0; i < 20; i++ {
		id := "A" + string(rune('0'+i%10)) + string(rune('a'+i/10))
		plantDecision(t, db, "abc123", id, "active decision "+id, "body "+id, "why", "active")
		stampDecisionCreatedAt(t, db, id, "-1 day")
	}
	plantDecision(t, db, "abc123", "R1", "revisit beyond the cap", "body R1", "why", "revisit")
	stampDecisionCreatedAt(t, db, "R1", "-1 minute")

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

	for i := 0; i < 20; i++ {
		id := "A" + string(rune('0'+i%10)) + string(rune('a'+i/10))
		if !strings.Contains(out, "active decision "+id) {
			t.Errorf("active decision %s was pushed out by a revisit row:\n%s", id, out)
		}
	}
	if strings.Contains(out, "revisit beyond the cap") {
		t.Errorf("a revisit row displaced an active decision inside a full window:\n%s", out)
	}
}

func TestProjectContextShowsRevisitDecisions(t *testing.T) {
	db, _, srv := newStoreWithDB(t)

	plantDecision(t, db, "abc123", "A1", "keep sqlite", "sqlite stays", "no cgo", "active")
	plantDecision(t, db, "abc123", "R1", "revisit the cache", "reconsider rrf weights", "tuning changed", "revisit")
	plantDecision(t, db, "abc123", "S1", "retired choice", "we dropped this", "reversed", "superseded")
	stampDecisionCreatedAt(t, db, "A1", "-1 day")
	stampDecisionCreatedAt(t, db, "R1", "-1 minute")

	text, err := srv.buildProjectContext(context.Background(), "abc123", "", searchResponseMaxBytes)
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
	assertRowOrder(t, text, "keep sqlite", "revisit the cache")
}

func TestProjectContextCapKeepsActiveAheadOfRevisit(t *testing.T) {
	db, _, srv := newStoreWithDB(t)

	for i := 0; i < 5; i++ {
		id := "A" + string(rune('0'+i))
		plantDecision(t, db, "abc123", id, "active decision "+id, "body "+id, "why", "active")
		stampDecisionCreatedAt(t, db, id, "-1 day")
	}
	plantDecision(t, db, "abc123", "R1", "revisit beyond the cap", "body R1", "why", "revisit")
	stampDecisionCreatedAt(t, db, "R1", "-1 minute")

	text, err := srv.buildProjectContext(context.Background(), "abc123", "", searchResponseMaxBytes)
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

func assertRowOrder(t *testing.T, text string, firstTitle, secondTitle string) {
	t.Helper()
	first := strings.Index(text, firstTitle)
	second := strings.Index(text, secondTitle)
	if first < 0 {
		t.Fatalf("row %q is missing from the output:\n%s", firstTitle, text)
	}
	if second < 0 {
		t.Fatalf("row %q is missing from the output:\n%s", secondTitle, text)
	}
	if first > second {
		t.Errorf("%q must render before %q when both are in the window (first at %d, second at %d)", firstTitle, secondTitle, first, second)
	}
}
