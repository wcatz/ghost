package portable

import (
	"context"
	"strings"
	"testing"
)

// TestImportRefusalsNeverEchoHostileColumnValues pins the import report for the
// columns whose refusals once quoted the offending value with %q before the
// token guard ran: evidence kind, task and decision status, the declared record
// type and a record's project_id. Each is fed a
// token-shaped value and a value over the 256-byte bound; the report, dry run
// and apply, must carry neither and must still name the field.
func TestImportRefusalsNeverEchoHostileColumnValues(t *testing.T) {
	token := credentialInArtifact()
	token = token[strings.Index(token, "ghp_"):]
	long := strings.Repeat("Z", 300)

	const header = `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n"
	cases := []struct {
		field string
		body  string // %V is replaced by the hostile value
	}{
		{"kind", `{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact","content":"plain fact","source":"mcp","tags":[],"evidence":[{"kind":"%V"}]}}`},
		{"status", `{"type":"task","task":{"id":"t1","project_id":"p1","title":"a task","status":"%V"}}`},
		{"status", `{"type":"decision","decision":{"id":"d1","project_id":"p1","title":"a decision","decision":"do it","rationale":"because","status":"%V"}}`},
		{"type", `{"type":"%V"}`},
		{"project_id", `{"type":"task","task":{"id":"t1","project_id":"%V","title":"a task","status":"pending"}}`},
	}
	for _, c := range cases {
		for name, value := range map[string]string{"token": token, "long": long} {
			for _, apply := range []bool{false, true} {
				mode := "dry run"
				if apply {
					mode = "apply"
				}
				t.Run(c.field+"/"+name+"/"+mode, func(t *testing.T) {
					body := header + strings.ReplaceAll(c.body, "%V", value) + "\n"
					store := newTestStore(t)
					var results []RecordResult
					report, importErr := Import(context.Background(), store, strings.NewReader(body),
						ImportOptions{Apply: apply}, func(r RecordResult) { results = append(results, r) })

					var printed []string
					for _, e := range report.Errors {
						printed = append(printed, e.Error())
					}
					for _, r := range results {
						printed = append(printed, r.Detail)
						if r.Error != nil {
							printed = append(printed, r.Error.Error())
						}
					}
					// A refused record type aborts the import rather than rejecting
					// one record, so its refusal is the returned error.
					if importErr != nil {
						printed = append(printed, importErr.Error())
					}
					joined := strings.Join(printed, "\n")
					if report.Rejected == 0 && len(report.Errors) == 0 && importErr == nil {
						t.Fatalf("the artifact was not refused:\n%s", joined)
					}
					if strings.Contains(joined, value) || strings.Contains(joined, value[:40]) {
						t.Errorf("the report echoes the hostile %s value:\n%s", c.field, joined)
					}
					if !strings.Contains(joined, c.field) {
						t.Errorf("the report does not name the field %q:\n%s", c.field, joined)
					}
				})
			}
		}
	}
}
