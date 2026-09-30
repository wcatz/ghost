package assemble

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/memory"
)

// TestAnOverLongTagFromANonMCPWriterIsBoundedByTheRenderer is #821's real subject,
// which a unit test on TagsLabel cannot reach on its own: the bound has to hold
// INDEPENDENTLY of which writer produced the row.
//
// `validateTags` caps a tag at 64 on the four MCP tools, and that cap is not what
// keeps the label small — it is only the reason an ordinary row is already small.
// Three writers deliberately do not reach it: `RestoreSnapshot` writes the column
// in SQL from the snapshot table, `CreateFromCorpus` reaches `insertMemory`
// directly, and `ReplaceNonManual` inherits the rewritten row's union of tags. An
// imported artifact writes a tag verbatim too. This drives `CreateFromCorpus`,
// which is the writer the secret guard's own exclusion list names, because a test
// that planted the value in SQL would prove only that SQL can hold a long string.
//
// The two halves are the property: the STORED value is byte-for-byte what the
// writer wrote, and what the renderer PRINTS of it is bounded and marked. A bound
// applied on the way in would pass the second half and fail the first — and it
// would have cost the user the tail of a label on a restore.
func TestAnOverLongTagFromANonMCPWriterIsBoundedByTheRenderer(t *testing.T) {
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// A nil logger is honoured as silence, and this store has nothing to report.
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, "ghost", "/src/ghost", "ghost"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// 300 bytes of a three-byte rune, so the bound's cut lands INSIDE one of them
	// and a byte slice would put invalid UTF-8 on the line. A trailing ordinary tag
	// as well, so the assertion that a later tag is not dropped has a subject.
	huge := strings.Repeat("日", 100)
	id, err := s.CreateFromCorpus(ctx, "ghost", memory.Memory{
		Category: "gotcha", Content: "a corpus row with an over-long tag",
		Source: "mcp", Importance: 0.5, Tags: []string{huge, "golden"},
	})
	if err != nil {
		t.Fatalf("CreateFromCorpus: %v", err)
	}

	rows, err := s.GetTopMemories(ctx, "ghost", 10)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	var stored []string
	for _, r := range rows {
		if r.ID == id {
			stored = r.Tags
		}
	}
	if len(stored) != 2 || stored[0] != huge || stored[1] != "golden" {
		t.Fatalf("the stored tags are %q (%d runes for the first), want the corpus writer's two values verbatim",
			stored, len([]rune(stored[0])))
	}

	// And the render of exactly that row is bounded, marked and still valid UTF-8.
	line := Item{ID: id, Category: "gotcha", Content: "a corpus row with an over-long tag",
		Importance: 0.5, Tags: stored}.Line()
	if !strings.Contains(line, "tag truncated") {
		t.Errorf("a %d-byte tag from a non-MCP writer is printed whole:\n%s", len(huge), line)
	}
	if !utf8.ValidString(line) {
		t.Errorf("the line is not valid UTF-8 after bounding a multi-byte tag: %q", line)
	}
	if len(line) > 600 {
		t.Errorf("the line is %d bytes, want the tag bounded rather than printed at full length", len(line))
	}
	if !strings.Contains(line, `"golden"`) {
		t.Errorf("the bound dropped the ordinary tag beside the over-long one:\n%s", line)
	}
	// The row is still a row: the content keeps its own data block, which is what a
	// reader parses the line around.
	if !strings.HasSuffix(line, "«a corpus row with an over-long tag»") {
		t.Errorf("the row does not end in the content's own data block:\n%s", line)
	}
}
