package portable

import (
	"strings"
	"testing"
)

// TestParseRefusesOnlyWholeFileProblems: Parse is the file-level half of Import,
// so it refuses exactly the problems that stop a run before any record is
// written and leaves every per-line refusal to the run that reports it.
func TestParseRefusesOnlyWholeFileProblems(t *testing.T) {
	const header = `{"type":"header","schema_version":1}` + "\n"
	const project = `{"type":"project","project":{"id":"p1","path":"/src/p1","name":"p1"}}` + "\n"

	cases := []struct {
		name string
		body string
		want string // empty means Parse must accept the file
	}{
		{
			name: "a header with no records",
			body: header,
		},
		{
			name: "a damaged line is not a file refusal",
			body: header + "not json\n",
		},
		{
			name: "an id this build will not store is a per-line refusal",
			body: header + `{"type":"memory","memory":{"id":"bad id","project_id":"p1","category":"fact","content":"x","source":"manual"}}` + "\n",
		},
		{
			name: "no header",
			body: project,
			want: "not a ghost artifact",
		},
		{
			name: "a second header",
			body: header + header,
			want: "a second",
		},
		{
			name: "an unknown record type",
			body: header + `{"type":"bogus"}` + "\n",
			want: "unknown record type",
		},
		{
			name: "a payload that does not match the type",
			body: header + `{"type":"memory","task":{"id":"t1","project_id":"p1","title":"t","status":"pending","priority":2}}` + "\n",
			want: `"memory" payload`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.body))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Parse refused a file the run would read: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Parse accepted a file Import refuses")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
