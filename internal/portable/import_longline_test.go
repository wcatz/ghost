package portable

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestImportReadsARecordTheExporterCanWrite is the pin on the seam the artifact
// exists for. A memory's evidence list is unbounded — the artifact carries one
// record per observation, and content is capped at 8000 bytes — so an exported
// record from a real store can pass any line ceiling derived from the content
// cap. While readRecords held such a ceiling, the one bloated record refused the
// file as a whole BEFORE any record was read: export exited 0, named nothing
// left out, and every other record in the backup became unimportable (#1084).
//
// The fixture seeds that state through a real writer. ImportMemory lands every
// evidence record the artifact carries in one transaction; the issue's own
// fixture instead drives 15000 Upserts, which appends one evidence row each but
// also inserts a linked copy per save, so the candidate probe rescans a growing
// store (O(n²)) and the case takes minutes. The state is the same — one memory
// whose evidence list alone is over a megabyte — and the exporter cannot tell
// which writer produced it.
func TestImportReadsARecordTheExporterCanWrite(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	const evidenceRows = 15000
	evidence := make([]memory.PortableEvidence, 0, evidenceRows)
	for i := 0; i < evidenceRows; i++ {
		evidence = append(evidence, memory.PortableEvidence{
			ID:        fmt.Sprintf("ev-%d", i),
			Kind:      "observed",
			Agent:     "opencode",
			SessionID: fmt.Sprintf("session-%d", i),
		})
	}
	const memoryID = "0123456789abcdef0123456789abcdef"
	if _, _, _, err := src.ImportMemory(ctx, memory.PortableMemory{
		ID:        memoryID,
		ProjectID: "p1",
		Category:  "fact",
		Content:   "the same fact, saved again and again",
		Source:    "mcp",
		Retention: "project",
		Evidence:  evidence,
	}, memory.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("seed a memory with %d evidence rows: %v", evidenceRows, err)
	}

	var artifact bytes.Buffer
	if _, err := Export(ctx, src, &artifact, ""); err != nil {
		t.Fatalf("Export: %v", err)
	}
	longest := longestLine(artifact.Bytes())
	if longest <= 1<<20 {
		t.Fatalf("the fixture's longest line is %d bytes, not over the 1 MiB ceiling the test needs to exercise", longest)
	}

	dst := newTestStore(t)
	report, err := Import(ctx, dst, bytes.NewReader(artifact.Bytes()), ImportOptions{Apply: true}, nil)
	if err != nil {
		t.Fatalf("import refused the exporter's own artifact: %v", err)
	}
	if report.Rejected != 0 {
		t.Fatalf("import rejected %d record(s) of the exporter's own artifact: %v", report.Rejected, report.Errors)
	}
	if report.Created["memory"] == 0 {
		t.Fatalf("import created no memories from an artifact that holds them: %+v", report)
	}
	// The long record is the thing that has to survive, and what made it long is
	// its evidence, so the imported memory is read back rather than counted.
	provenance, err := dst.MemoryProvenance(ctx, memoryID)
	if err != nil {
		t.Fatalf("read back the imported memory's evidence: %v", err)
	}
	if len(provenance) < evidenceRows {
		t.Fatalf("imported memory carries %d evidence records, want at least the %d the artifact held", len(provenance), evidenceRows)
	}
}

// longestLine reports the largest line in an artifact, the measurement the issue
// uses to show the old ceiling is crossed.
func longestLine(b []byte) int {
	longest := 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) > longest {
			longest = len(line)
		}
	}
	return longest
}

// TestImportReadsEveryLineShapeTheReaderHasToAgreeWithTheOldOneOn: the reader
// that replaced bufio.Scanner must still agree with it about where a line ENDS,
// not only about how long it may be. A file that has been through an editor has
// blank lines and CRLF endings; a file written by a shell pipeline has no final
// newline. All three yield their records, blank lines still count toward the
// line numbers a report names, and the record the reader refuses still reports
// the line a user would see in their editor.
func TestImportReadsEveryLineShapeTheReaderHasToAgreeWithTheOldOneOn(t *testing.T) {
	dst := newTestStore(t)
	var artifact strings.Builder
	artifact.WriteString(`{"type":"header","schema_version":2}` + "\r\n")
	artifact.WriteString("\r\n") // a blank line, as an editor leaves
	artifact.WriteString(`{"type":"project","project":{"id":"pv","path":"/src/pv","name":"v","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}` + "\n")
	artifact.WriteString(`{"type":"memory","memory":{"id":"bad","project_id":"pv","category":"fact","content":"a fourth tier","source":"mcp","retention":"forever","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}` + "\r\n")
	// The last record carries no terminator at all.
	artifact.WriteString(`{"type":"memory","memory":{"id":"good","project_id":"pv","category":"fact","content":"the unterminated last line","source":"mcp","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`)

	var results []RecordResult
	report, err := Import(context.Background(), dst, strings.NewReader(artifact.String()),
		ImportOptions{Apply: true}, func(r RecordResult) { results = append(results, r) })
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 1 {
		t.Fatalf("rejected %d record(s), want the one carrying a fourth tier: %v", report.Rejected, report.Errors)
	}
	if report.Created["memory"] != 1 {
		t.Fatalf("created %v, want the one memory the file holds", report.Created)
	}
	// Line 4 in the editor's numbering, counting the blank line above it. A
	// reader that dropped blank lines from the count would name line 3, and
	// nobody can repair a file from the wrong line number.
	for _, r := range results {
		if r.ID == "bad" {
			if r.Line != 4 {
				t.Errorf("the refused record reports line %d, want 4 — blank lines have to count", r.Line)
			}
		}
	}
	got, err := dst.PortableMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("imported memories = %+v, want exactly the record with no trailing newline", got)
	}
}
