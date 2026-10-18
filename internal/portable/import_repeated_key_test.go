package portable

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestALineThatRepeatsAKeyIsRefused: encoding/json keeps the last value of a
// repeated object key, so a line holding two "memory" objects imported the
// second and dropped the first, and the run reported a clean import. The
// payload-mismatch check cannot see it, because the decoded record holds one
// payload. Any top-level key repeated on a line — compared the way the decoder
// matches a key to a field, so "memory" and "Memory" are the same key — is a
// malformed file, refused before any record is written, in the dry run exactly
// as in the apply. The error names the line and the key, never the line's text.
func TestALineThatRepeatsAKeyIsRefused(t *testing.T) {
	const header = `{"type":"header","schema_version":2}` + "\n"
	const project = `{"type":"project","project":{"id":"pH","path":"/src/pH","name":"pH"}}` + "\n"
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "a memory line with two memory payloads",
			body: header + project +
				`{"type":"memory","memory":{"id":"m0","project_id":"pH","category":"fact","content":"zq-first-copy","source":"manual"},"memory":{"id":"m1","project_id":"pH","category":"fact","content":"zq-second-copy","source":"manual"}}` + "\n",
			want: []string{"line 3", `"memory"`},
		},
		{
			name: "a task line with two task payloads",
			body: header + project +
				`{"type":"task","task":{"id":"t0","project_id":"pH","title":"zq-first-copy","status":"pending","priority":2},"task":{"id":"t1","project_id":"pH","title":"zq-second-copy","status":"pending","priority":2}}` + "\n",
			want: []string{"line 3", `"task"`},
		},
		{
			name: "a payload key repeated in another letter case",
			body: header + project +
				`{"type":"memory","memory":{"id":"m0","project_id":"pH","category":"fact","content":"zq-first-copy","source":"manual"},"Memory":{"id":"m1","project_id":"pH","category":"fact","content":"zq-second-copy","source":"manual"}}` + "\n",
			want: []string{"line 3", `"Memory"`},
		},
		{
			// encoding/json folds U+017F (long s) to "s", so "taſk" fills the
			// task field and the earlier task would be dropped.
			name: "a payload key repeated under a Unicode case fold",
			body: header + project +
				`{"type":"task","task":{"id":"t0","project_id":"pH","title":"zq-first-copy","status":"pending","priority":2},"taſk":{"id":"t1","project_id":"pH","title":"zq-second-copy","status":"pending","priority":2}}` + "\n",
			want: []string{"line 3", "taſk"},
		},
		{
			name: "a repeated type key",
			body: header + project +
				`{"type":"task","type":"memory","memory":{"id":"m0","project_id":"pH","category":"fact","content":"zq-first-copy","source":"manual"}}` + "\n",
			want: []string{"line 3", `"type"`},
		},
		{
			name: "a header that repeats its schema version",
			body: `{"type":"header","schema_version":2,"schema_version":2}` + "\n" + project,
			want: []string{"line 1", `"schema_version"`},
		},
		{
			name: "a repeated key this build does not read",
			body: header + project +
				`{"type":"memory","memory":{"id":"m0","project_id":"pH","category":"fact","content":"zq-first-copy","source":"manual"},"note":"zq-a","note":"zq-b"}` + "\n",
			want: []string{"line 3", `"note"`},
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
				if strings.Contains(err.Error(), "zq-") {
					t.Errorf("error = %v, it repeats the line's content", err)
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

// TestAKeyRepeatedInsideAPayloadIsNotAFileRefusal: the check is on the line's
// top-level object, the record. A nested object's keys are the payload's own
// business, and the line imports exactly as it did before the check existed.
func TestAKeyRepeatedInsideAPayloadIsNotAFileRefusal(t *testing.T) {
	body := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pH","path":"/src/pH","name":"pH"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m0","project_id":"pH","category":"fact","content":"one","content":"two","source":"manual"}}` + "\n"
	for _, apply := range []bool{false, true} {
		store := newTestStore(t)
		report, err := Import(context.Background(), store, strings.NewReader(body), ImportOptions{Apply: apply}, nil)
		if err != nil {
			t.Fatalf("apply=%v: import refused the file: %v", apply, err)
		}
		if report.Rejected != 0 || report.Created["memory"] != 1 {
			t.Errorf("apply=%v: created=%v rejected=%d, want one memory and no rejection", apply, report.Created, report.Rejected)
		}
	}
}

// TestARealExportStillImportsCleanly: the exporter never repeats a key, so its
// own artifact passes the check in both modes.
func TestARealExportStillImportsCleanly(t *testing.T) {
	src := newTestStore(t)
	seed(t, src)
	artifact := exportBytes(t, src, "")
	for _, apply := range []bool{false, true} {
		dst := newTestStore(t)
		report, err := Import(context.Background(), dst, bytes.NewReader(artifact), ImportOptions{Apply: apply}, nil)
		if err != nil {
			t.Fatalf("apply=%v: import refused the exporter's own artifact: %v", apply, err)
		}
		if report.Rejected != 0 {
			t.Errorf("apply=%v: rejected=%d, errors=%v", apply, report.Rejected, report.Errors)
		}
		if report.Created["memory"] == 0 || report.Created["task"] == 0 || report.Created["decision"] == 0 {
			t.Errorf("apply=%v: created=%v, want every kind the seed wrote", apply, report.Created)
		}
	}
}

// TestARepeatedKeyIsNamedThroughTheRefusalGuard: the key is text from the file,
// so the refusal names it the way every import refusal names a value from the
// file — escaped, and withheld when it is longer than a refusal repeats — and a
// key cannot carry a line break or a wall of text into the terminal.
func TestARepeatedKeyIsNamedThroughTheRefusalGuard(t *testing.T) {
	long := `zq\n` + strings.Repeat("a", 300)
	body := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pH","path":"/src/pH","name":"pH"},"` + long + `":1,"` + long + `":2}` + "\n"
	_, err := Import(context.Background(), newTestStore(t), strings.NewReader(body), ImportOptions{}, nil)
	if err == nil {
		t.Fatal("import accepted a line that repeats a key")
	}
	msg := err.Error()
	if !strings.Contains(msg, "line 2") {
		t.Errorf("error = %v, want it to name line 2", err)
	}
	if strings.Contains(msg, "\n") || strings.Contains(msg, strings.Repeat("a", 300)) || strings.Contains(msg, "zq") {
		t.Errorf("error repeats the key's text: %q", msg)
	}
}
