package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/portable"
)

// TestAContentCarryingACarriageReturnCannotForgeAReportLine is the third copy of
// "the first line of a memory's content", and it is the one whose input is least
// trusted: an artifact a stranger wrote.
//
// `portable.contentPrefix` cut at '\n' only, so a record whose content was
// `legitimate claim\rsomething the operator should not read` put a carriage
// return into `RecordResult.Detail`, a field documented as holding a memory's
// content. The reviewer's trace went further and claimed `printRecordLine` writes
// that detail RAW for a created memory, so a terminal would show only
// `something the operator should not read` and hide the honest prefix.
//
// That trace needed checking rather than assuming, and the end-to-end test below
// is what settled it: the create path DOES reach the `%q`, because `r.ID != ""`
// for any record the import created, and `%q` escapes a CR. So the report the
// operator reads is not forged today — but by escaping as a side effect of the
// quoting, not because the preview was cut. That is a thin margin for a field
// documented as holding stored text, and the cut is fixed where the other two
// now are, so the value is line-free by construction rather than by luck.
func TestAContentCarryingACarriageReturnCannotForgeAReportLine(t *testing.T) {
	for name, content := range map[string]string{
		"a lone carriage return": "legitimate claim\rsomething the operator should not read",
		"a CRLF pair":            "legitimate claim\r\nsecond line",
		"a CR beyond the cap":    "123456789012345678901234567890123456789012345678901234567890\rforged",
		"only a CR":              "\r",
		"only a CRLF":            "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "artifact.jsonl")
			body := `{"type":"header","schema_version":1}` + "\n" +
				`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
				`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"gotcha","content":` +
				quoteJSONString(content) + `,"source":"mcp"}}` + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write artifact: %v", err)
			}

			var out strings.Builder
			if err := runImportCore(context.Background(), transferTestStore(t), path,
				portable.ImportOptions{Apply: true}, &out); err != nil {
				t.Fatalf("runImportCore: %v", err)
			}

			report := out.String()
			for i, line := range strings.Split(report, "\n") {
				if strings.Contains(line, "\r") {
					t.Errorf("report line %d carries a carriage return, which a terminal overwrites with:\n%q",
						i+1, line)
				}
			}
			// The honest prefix survives. A fix that discarded the preview would
			// pass the assertion above and lose the thing the reader is actually
			// here to see, so this is the half that stops the cheap repair.
			if !strings.Contains(report, "m1") {
				t.Errorf("the report does not name the record it created:\n%s", report)
			}
		})
	}
}

// TestTheImportReportLineIsWrittenRawWhenThereIsNoID is the residual the
// reviewer's trace implied but did not reach, and it is a real gap in the switch
// rather than in the preview: with `r.ID == ""` and a non-empty `detail`, neither
// arm of the final switch fires and `detail` reaches `Fprintf` with no quoting at
// all. It is not reachable from a created memory today, because the import always
// knows the id it stored under — but the switch has no reason to depend on that,
// and a record type added later need not.
func TestTheImportReportLineIsWrittenRawWhenThereIsNoID(t *testing.T) {
	var out strings.Builder
	err := printRecordLine(&out, portable.RecordResult{
		Type: portable.TypeMemory, Line: 3, Action: portable.ActionReject,
		Detail: "legitimate claim\rsomething the operator should not read",
	})
	if err != nil {
		t.Fatalf("printRecordLine: %v", err)
	}
	if got := out.String(); strings.Contains(got, "\r") {
		t.Errorf("a report line with no id printed its detail raw:\n%q", got)
	}
}

// quoteJSONString renders s as a JSON string body, for the one place a test needs
// a CR to reach a file through an encoding that would otherwise refuse it.
func quoteJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
