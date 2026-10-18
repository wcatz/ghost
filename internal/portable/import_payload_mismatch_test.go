package portable

import (
	"context"
	"strings"
	"testing"
)

// TestALineWhosePayloadsDoNotMatchItsTypeIsRefused: each artifact line carries a
// type and exactly the one payload that type names. A line that carries another
// kind's payload as well — a merged or hand-edited line — used to import the
// payload its type named and drop the other one, and the run reported a clean
// import with data from the file missing. It is refused as a file, before any
// record is written, the same way a record with no payload is, and the dry run
// says so exactly as the apply does.
func TestALineWhosePayloadsDoNotMatchItsTypeIsRefused(t *testing.T) {
	const header = `{"type":"header","schema_version":2}` + "\n"
	const project = `{"type":"project","project":{"id":"pH","path":"/src/pH","name":"pH"}}` + "\n"
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "a memory line that also carries a task",
			body: header + project +
				`{"type":"memory","memory":{"id":"m1","project_id":"pH","category":"fact","content":"kept","source":"manual"},"task":{"id":"t1","project_id":"pH","title":"silently gone","status":"pending","priority":2}}` + "\n",
			want: []string{"line 3", `"task"`},
		},
		{
			name: "a task line that carries three payloads",
			body: header + project +
				`{"type":"task","task":{"id":"t1","project_id":"pH","title":"t","status":"pending","priority":2},"project":{"id":"pX","path":"/src/pX","name":"pX"},"decision":{"id":"d1","project_id":"pH","title":"d","decision":"x","status":"active"}}` + "\n",
			want: []string{"line 3", `"project"`, `"decision"`},
		},
		{
			name: "an extra payload that is an empty object",
			body: header + project +
				`{"type":"memory","memory":{"id":"m1","project_id":"pH","category":"fact","content":"kept","source":"manual"},"task":{}}` + "\n",
			want: []string{"line 3", `"task"`},
		},
		{
			// Already refused by the missing-payload check; kept as coverage of
			// the "payload of another kind" half.
			name: "a memory line whose only payload is a task",
			body: header + project +
				`{"type":"memory","task":{"id":"t1","project_id":"pH","title":"t","status":"pending","priority":2}}` + "\n",
			want: []string{"line 3", `"memory"`},
		},
		{
			// The memory id would be refused line by line on a real memory line;
			// on a task line it must not turn a malformed file into one damaged
			// line the rest of the file imports around.
			name: "a task line that also carries a memory with an id this build refuses",
			body: header + project +
				`{"type":"task","task":{"id":"t1","project_id":"pH","title":"t","status":"pending","priority":2},"memory":{"id":"bad id","project_id":"pH","category":"fact","content":"x","source":"manual"}}` + "\n",
			want: []string{"line 3", `"memory"`},
		},
		{
			name: "a memory line with an id this build refuses and a second payload",
			body: header + project +
				`{"type":"memory","memory":{"id":"bad id","project_id":"pH","category":"fact","content":"x","source":"manual"},"task":{"id":"t1","project_id":"pH","title":"t","status":"pending","priority":2}}` + "\n",
			want: []string{"line 3", `"task"`},
		},
		{
			name: "a header that carries a payload",
			body: `{"type":"header","schema_version":2,"task":{"id":"t1","project_id":"pH","title":"t","status":"pending","priority":2}}` + "\n" + project,
			want: []string{"line 1", `"task"`},
		},
	}
	for _, tc := range cases {
		for _, apply := range []bool{false, true} {
			mode := "dry run"
			if apply {
				mode = "apply"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				store := newTestStore(t)
				ctx := context.Background()
				report, err := Import(ctx, store, strings.NewReader(tc.body), ImportOptions{Apply: apply}, nil)
				if err == nil {
					t.Fatalf("import accepted the line: created=%v rejected=%d", report.Created, report.Rejected)
				}
				for _, w := range tc.want {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error = %v, want it to mention %s", err, w)
					}
				}
				projects, lErr := store.PortableProjects(ctx)
				if lErr != nil {
					t.Fatalf("PortableProjects: %v", lErr)
				}
				if len(projects) != 0 {
					t.Errorf("a refused file still wrote %d projects", len(projects))
				}
				memories, lErr := store.PortableMemories(ctx, nil)
				if lErr != nil {
					t.Fatalf("PortableMemories: %v", lErr)
				}
				if len(memories) != 0 {
					t.Errorf("a refused file still wrote %d memories", len(memories))
				}
			})
		}
	}
}
