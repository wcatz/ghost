package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// credentialInArtifact is a token-shaped value in the GitHub PAT format. It is
// assembled rather than written out because GitHub push protection matches that
// format anywhere in a diff and rejects the push (GH013) before review starts —
// the same constraint internal/secret's own fixtures work around, and the reason
// a detector's test data has to be built at all.
func credentialInArtifact() string {
	return "GITHUB_TOKEN=ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
}

// TestImportReportNeverEchoesARefusedCredential is the leak the credential
// guard created for itself.
//
// The guard stops the value being stored, and the report then prints it. Every
// rejection path is a printing path: the error list names the record with
// labelOrID, which is the record's content prefix or its title; and
// printRecordLine in cmd/ghost prints the same detail per record. A token pasted
// into an artifact was therefore refused by Ghost and then written to stdout in
// full by Ghost — and a dry run printed it too, which is the worse half: the
// point of a dry run is to be the safe way to look at an artifact you do not
// trust yet.
//
// So the detail a report line carries is not the record's text. Where that text
// holds a credential the detail is the record's id, and the error names the
// format, which is what the operator needs to fix the artifact.
func TestImportReportNeverEchoesARefusedCredential(t *testing.T) {
	const token = "ghp_"
	credential := credentialInArtifact()
	// Distinctive enough that a substring search over all the output cannot
	// match by accident, and long enough that a 60-rune contentPrefix would
	// print it whole.
	needle := credential[strings.Index(credential, token):]

	body := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact",` +
		`"content":"` + credential + `","source":"mcp","tags":[]}}` + "\n" +
		`{"type":"task","task":{"id":"t1","project_id":"p1","title":"rotate ` + credential + `"}}` + "\n" +
		`{"type":"decision","decision":{"id":"d1","project_id":"p1","title":"rotate ` + credential + `",` +
		`"decision":"rotate quarterly","rationale":"the value is ` + credential + `","status":"active"}}` + "\n"

	for _, apply := range []bool{false, true} {
		name := "dry run"
		if apply {
			name = "apply"
		}
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			var results []RecordResult
			report, _ := Import(context.Background(), store, strings.NewReader(body),
				ImportOptions{Apply: apply}, func(r RecordResult) { results = append(results, r) })

			var printed []string
			for _, e := range report.Errors {
				printed = append(printed, e.Error())
			}
			for _, r := range results {
				errText := ""
				if r.Error != nil {
					errText = r.Error.Error()
				}
				printed = append(printed, r.Detail, errText)
			}

			if report.Rejected != 3 {
				t.Fatalf("Rejected = %d, want 3 (one memory, one task, one decision): %v", report.Rejected, report.Errors)
			}
			for _, line := range printed {
				if strings.Contains(line, needle) {
					t.Errorf("the report echoed the refused credential:\n%s", line)
				}
			}
			// The rule still has to be named, or the operator cannot tell which
			// value in the artifact to remove.
			joined := strings.Join(printed, "\n")
			if !strings.Contains(joined, "GitHub personal access token") {
				t.Errorf("the report does not name the credential format it refused:\n%s", joined)
			}
		})
	}
}

// TestLabelOrIDPrintsOnlyTheIDForACredentialRefusal pins the invariant at the
// place that builds the error line, independently of safeDetail.
//
// The two overlap today: safeDetail has already reduced Detail to the id by the
// time labelOrID runs, so removing labelOrID's branch changes no current output.
// That is exactly why it is worth a direct test — a RecordResult carries a
// public Detail field, so any future caller that fills it without going through
// safeDetail would otherwise print a credential from the one function whose job
// is to describe the record being refused.
func TestLabelOrIDPrintsOnlyTheIDForACredentialRefusal(t *testing.T) {
	credential := credentialInArtifact()
	refusal := &memory.SecretContentError{Field: "content", Format: "GitHub personal access token"}
	ordinary := errors.New("memory m1: invalid category \"nope\"")

	cases := []struct {
		name   string
		result RecordResult
		err    error
		want   string
	}{
		{
			name:   "credential refusal ignores the detail",
			result: RecordResult{ID: "m1", Detail: credential},
			err:    refusal,
			want:   "m1",
		},
		{
			name:   "credential refusal with a wrapped error still ignores the detail",
			result: RecordResult{ID: "m1", Detail: credential},
			err:    fmt.Errorf("import memory m1: %w", refusal),
			want:   "m1",
		},
		{
			name:   "credential refusal with no id says so",
			result: RecordResult{Detail: credential},
			err:    refusal,
			want:   "(no id)",
		},
		{
			name:   "an ordinary rejection keeps the detail",
			result: RecordResult{ID: "m1", Detail: "an ordinary memory"},
			err:    ordinary,
			want:   `"an ordinary memory" (m1)`,
		},
		{
			name:   "an ordinary rejection with no detail falls back to the id",
			result: RecordResult{ID: "m1"},
			err:    ordinary,
			want:   "m1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := labelOrID(tc.result, tc.err)
			if got != tc.want {
				t.Errorf("labelOrID = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, credential) {
				t.Errorf("labelOrID echoed the credential: %q", got)
			}
		})
	}
}

// TestImportReportStillLabelsOrdinaryRecords is the other half: the safe-detail
// rule must not cost the report its usefulness for every record that is not a
// credential. A rejection over an ordinary record still gets its content prefix
// or its title, because that is how the reader knows which line to fix.
func TestImportReportStillLabelsOrdinaryRecords(t *testing.T) {
	body := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"not-a-category",` +
		`"content":"an ordinary memory about the relay port","source":"mcp","tags":[]}}` + "\n"

	store := newTestStore(t)
	var results []RecordResult
	report, _ := Import(context.Background(), store, strings.NewReader(body),
		ImportOptions{Apply: true}, func(r RecordResult) { results = append(results, r) })

	if report.Rejected != 1 {
		t.Fatalf("Rejected = %d, want 1: %v", report.Rejected, report.Errors)
	}
	if len(results) == 0 || !strings.Contains(results[len(results)-1].Detail, "an ordinary memory") {
		t.Errorf("an ordinary rejection lost its content prefix: %+v", results)
	}
}
