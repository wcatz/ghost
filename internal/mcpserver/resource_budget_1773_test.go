package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func cutNoticeRe(advice string) *regexp.Regexp {
	return regexp.MustCompile(`\n\((\d+) rows were cut from the end of this answer to stay within the (\d+)-byte ` +
		`response budget, which bounds the answer and not what is stored\. ` + regexp.QuoteMeta(advice) + `\)\n$`)
}

func checkCutResource(t *testing.T, text, advice, rowPrefix, rowTail string, planted int) {
	t.Helper()
	m := cutNoticeRe(advice).FindStringSubmatchIndex(text)
	if m == nil {
		t.Fatalf("resource does not end in the exact cut notice with advice %q; last 600 bytes: %q",
			advice, text[max(0, len(text)-600):])
	}
	stated, err := strconv.Atoi(text[m[2]:m[3]])
	if err != nil {
		t.Fatalf("cut count in notice: %v", err)
	}
	budget, err := strconv.Atoi(text[m[4]:m[5]])
	if err != nil || budget != searchResponseMaxBytes {
		t.Errorf("notice names budget %q, want %d", text[m[4]:m[5]], searchResponseMaxBytes)
	}
	body := text[:m[0]] // the notice opens with a blank line; the row's own newline stays
	if !strings.HasSuffix(body, rowTail) {
		t.Errorf("rows before the notice do not end on a complete last row; want suffix %q, last 300 bytes: %q",
			rowTail[max(0, len(rowTail)-60):], body[max(0, len(body)-300):])
	}
	shown := strings.Count(body, rowPrefix)
	whole := strings.Count(body, rowTail)
	if shown == 0 {
		t.Fatalf("no row shown before the notice")
	}
	if whole != shown {
		t.Errorf("%d rows shown but only %d of them are whole (end in %q)", shown, whole, rowTail[max(0, len(rowTail)-60):])
	}
	if stated < 1 || stated != planted-shown {
		t.Errorf("notice states %d rows cut, but %d planted - %d shown = %d not shown", stated, planted, shown, planted-shown)
	}
}

func TestTasksResourceFitsResponseBudget1773(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	ctx := context.Background()

	for i := 0; i < 15; i++ {
		plantTask(t, db, "abc123", fmt.Sprintf("t%d", i), fmt.Sprintf("task %d", i), strings.Repeat("d", 1100), "active")
		plantTask(t, db, "abc123", fmt.Sprintf("b%d", i), fmt.Sprintf("task %d", i), strings.Repeat("d", 1100), "blocked")
		plantTask(t, db, "abc123", fmt.Sprintf("p%d", i), fmt.Sprintf("task %d", i), strings.Repeat("d", 1100), "pending")
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/tasks"})
	if err != nil {
		t.Fatalf("ReadResource tasks: %v", err)
	}
	text := res.Contents[0].Text
	t.Logf("tasks resource len=%d", len(text))

	if len(text) > searchResponseMaxBytes {
		t.Errorf("tasks resource is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	checkCutResource(t, text, taskResourceCutAdvice, "\n- [", "  \u00ab"+strings.Repeat("d", 1100)+"\u00bb\n", 45)
	if strings.Contains(text, "lower `limit`") || strings.Contains(text, "narrow with `status`") {
		t.Errorf("tasks resource contains tool-specific advice instead of resource advice")
	}
	if !strings.Contains(text, "## Open Tasks") {
		t.Errorf("tasks resource missing heading")
	}
	_ = srv
}

func TestDecisionsResourceFitsResponseBudget1773(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		plantDecision(t, db, "abc123", fmt.Sprintf("dec%d", i), fmt.Sprintf("title %d", i), strings.Repeat("d", 1300), strings.Repeat("r", 1300), "active")
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/decisions"})
	if err != nil {
		t.Fatalf("ReadResource decisions: %v", err)
	}
	text := res.Contents[0].Text
	t.Logf("decisions resource len=%d", len(text))

	if len(text) > searchResponseMaxBytes {
		t.Errorf("decisions resource is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	checkCutResource(t, text, decisionsResourceCutAdvice, "\n- `", "(rationale: \u00ab"+strings.Repeat("r", 1300)+"\u00bb)\n", 20)
	if strings.Contains(text, "lower `limit`") || strings.Contains(text, "narrow with `status`") {
		t.Errorf("decisions resource contains tool-specific advice instead of resource advice")
	}
	if !strings.Contains(text, "## Decisions") {
		t.Errorf("decisions resource missing heading")
	}
}

func TestDecisionsResourceSingleOversizedRowShownWhole(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	ctx := context.Background()

	plantDecision(t, db, "abc123", "big1", "title", strings.Repeat("d", 15000), strings.Repeat("r", 15000), "active")

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/decisions"})
	if err != nil {
		t.Fatalf("ReadResource decisions: %v", err)
	}
	text := res.Contents[0].Text

	if !strings.Contains(text, "shown in full rather than dropped") {
		t.Errorf("decisions resource missing over-budget notice for oversized row: %s", text)
	}
	if !strings.Contains(text, strings.Repeat("d", 100)) {
		t.Errorf("decisions resource missing oversized row content")
	}
	if !strings.Contains(text, "(rationale: \u00ab"+strings.Repeat("r", 15000)+"\u00bb)\n\n(") {
		t.Errorf("decisions resource does not show the oversized row whole before its notice")
	}
	if !strings.HasSuffix(text, ")\n") {
		t.Errorf("decisions resource does not end in its notice")
	}
}

func TestTasksResourceSingleOversizedRowShownWhole(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	ctx := context.Background()

	plantTask(t, db, "abc123", "big1", "title", strings.Repeat("d", 20000), "active")

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/tasks"})
	if err != nil {
		t.Fatalf("ReadResource tasks: %v", err)
	}
	text := res.Contents[0].Text

	if !strings.Contains(text, "shown in full rather than dropped") {
		t.Errorf("tasks resource missing over-budget notice for oversized row: %s", text)
	}
	if !strings.Contains(text, strings.Repeat("d", 100)) {
		t.Errorf("tasks resource missing oversized row content")
	}
	if !strings.Contains(text, "  \u00ab"+strings.Repeat("d", 20000)+"\u00bb\n\n(") {
		t.Errorf("tasks resource does not show the oversized row whole before its notice")
	}
	if !strings.HasSuffix(text, ")\n") {
		t.Errorf("tasks resource does not end in its notice")
	}
}
