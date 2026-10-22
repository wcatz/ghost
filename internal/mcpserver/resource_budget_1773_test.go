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

// cutNoticeRe is the exact cut notice a resource ends with: the shared sentence
// from assemble.CutNotice, wrapped in its blank line, closing the answer. The
// advice is the resource's own and is matched whole, so a fragment of it, or
// another tool's advice, does not pass.
func cutNoticeRe(advice string) *regexp.Regexp {
	return regexp.MustCompile(`\n\((\d+) rows were cut from the end of this answer to stay within the (\d+)-byte ` +
		`response budget, which bounds the answer and not what is stored\. ` + regexp.QuoteMeta(advice) + `\)\n$`)
}

// checkCutResource asserts what a trimmed resource must be: the text before the
// notice is whole rows only and ends on the closing of a complete last row
// (rowTail, which every planted row ends with), the notice is the exact
// sentence, and the count it states is the number of rows not shown. HasSuffix
// on a newline would prove nothing: the notice itself ends in one.
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

	// 45 tasks with ~1100-byte descriptions = ~50KB total, enough to exceed 16KB budget
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
	// Ensure the tool-specific advice about `limit` and `status` is NOT present
	if strings.Contains(text, "lower `limit`") || strings.Contains(text, "narrow with `status`") {
		t.Errorf("tasks resource contains tool-specific advice instead of resource advice")
	}
	// At least one row is present
	if !strings.Contains(text, "## Open Tasks") {
		t.Errorf("tasks resource missing heading")
	}
	_ = srv
}

func TestDecisionsResourceFitsResponseBudget1773(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	ctx := context.Background()

	// 20 decisions with ~1300-byte fields = ~52KB, enough to exceed 16KB budget
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
	// Ensure the tool-specific advice about `limit` and `status` is NOT present
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

	// One decision with 15000-byte fields (much larger than 16KB budget)
	plantDecision(t, db, "abc123", "big1", "title", strings.Repeat("d", 15000), strings.Repeat("r", 15000), "active")

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/decisions"})
	if err != nil {
		t.Fatalf("ReadResource decisions: %v", err)
	}
	text := res.Contents[0].Text

	// Row alone exceeds budget - should show whole row with over-budget notice
	if !strings.Contains(text, "shown in full rather than dropped") {
		t.Errorf("decisions resource missing over-budget notice for oversized row: %s", text)
	}
	// The row content should be present
	if !strings.Contains(text, strings.Repeat("d", 100)) {
		t.Errorf("decisions resource missing oversized row content")
	}
	// The row is whole: its full closing precedes the notice, and the notice
	// is the over-budget sentence at the very end.
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

	// One task with 20000-byte description (larger than 16KB budget)
	plantTask(t, db, "abc123", "big1", "title", strings.Repeat("d", 20000), "active")

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/tasks"})
	if err != nil {
		t.Fatalf("ReadResource tasks: %v", err)
	}
	text := res.Contents[0].Text

	// Row alone exceeds budget - should show whole row with over-budget notice
	if !strings.Contains(text, "shown in full rather than dropped") {
		t.Errorf("tasks resource missing over-budget notice for oversized row: %s", text)
	}
	// The description content should be present
	if !strings.Contains(text, strings.Repeat("d", 100)) {
		t.Errorf("tasks resource missing oversized row content")
	}
	// The row is whole: its full description precedes the notice, and the
	// notice is at the very end.
	if !strings.Contains(text, "  \u00ab"+strings.Repeat("d", 20000)+"\u00bb\n\n(") {
		t.Errorf("tasks resource does not show the oversized row whole before its notice")
	}
	if !strings.HasSuffix(text, ")\n") {
		t.Errorf("tasks resource does not end in its notice")
	}
}
