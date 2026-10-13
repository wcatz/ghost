package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The artifact is what crosses a machine boundary, so the tier has to be in the
// FILE rather than only in the store: a key the record struct does not declare
// is a key an unmarshal drops, and an artifact written before the field existed
// states nothing at all. The import's default then downgrades it the way it
// downgrades source and pin, so an untrusted file cannot plant a keep-forever
// row or schedule one for deletion.

// TestTheArtifactCarriesTheRetentionTier: every memory record states its tier,
// including the ones holding the default, because a field that is omitted for
// the common case is a field a reader has to infer — and an artifact is
// inspected text.
func TestTheArtifactCarriesTheRetentionTier(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, _, _, err := src.UpsertWithOptions(ctx, "p1", "fact", "a keep-forever fact", "mcp", 0.5, nil,
		memory.UpsertOptions{Retention: memory.RetentionPersistent}); err != nil {
		t.Fatalf("UpsertWithOptions(persistent): %v", err)
	}
	if _, _, _, err := src.UpsertWithOptions(ctx, "p1", "fact", "an ordinary fact", "mcp", 0.5, nil,
		memory.UpsertOptions{Retention: memory.RetentionProject}); err != nil {
		t.Fatalf("UpsertWithOptions(project): %v", err)
	}
	artifact := exportBytes(t, src, "")

	for _, want := range []string{
		`"retention":"persistent"`,
		`"retention":"project"`,
	} {
		if !bytes.Contains(artifact, []byte(want)) {
			t.Errorf("the artifact does not carry %s:\n%s", want, artifact)
		}
	}
	// Two memory lines, one per row, and a grep of the kind the issue's repro
	// runs finds both.
	if got := bytes.Count(artifact, []byte(`"retention":`)); got != 2 {
		t.Errorf("the artifact carries %d retention keys, want one per memory", got)
	}
}

// TestImportRestoresTheTierUnderTrustAndDowngradesItWithout: the default is the
// safe one to ship, and the flag is the case a user restoring their own store
// needs. Both halves are asserted against a real export and a real import,
// because the two sides are what could disagree.
func TestImportRestoresTheTierUnderTrustAndDowngradesItWithout(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, _, _, err := src.UpsertWithOptions(ctx, "p1", "fact", "a keep-forever fact", "mcp", 0.5, nil,
		memory.UpsertOptions{Retention: memory.RetentionPersistent}); err != nil {
		t.Fatalf("UpsertWithOptions(persistent): %v", err)
	}
	artifact := exportBytes(t, src, "")

	for _, tc := range []struct {
		name  string
		trust bool
		want  string
	}{
		{name: "restored by its owner", trust: true, want: memory.RetentionPersistent},
		{name: "arrived from somewhere", want: memory.RetentionProject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := newTestStore(t)
			report, err := Import(ctx, dst, bytes.NewReader(artifact),
				ImportOptions{Apply: true, TrustProvenance: tc.trust}, nil)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if report.Rejected != 0 {
				t.Fatalf("rejected %d record(s): %v", report.Rejected, report.Errors)
			}
			got, err := dst.PortableMemories(ctx, nil)
			if err != nil {
				t.Fatalf("PortableMemories: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("the destination holds %d memories, want 1", len(got))
			}
			if got[0].Retention != tc.want {
				t.Errorf("restored retention = %q, want %q", got[0].Retention, tc.want)
			}
		})
	}
}

// TestAnArtifactRecordThatStatesNoRetentionStillImports: a record written
// before the field existed — the whole of a v1 file — states no tier, and an
// absent optional field is the documented no-change case rather than a
// malformed record. It imports as the durable default.
func TestAnArtifactRecordThatStatesNoRetentionStillImports(t *testing.T) {
	dst := newTestStore(t)
	// Written by hand, in the shape a v1 artifact carries: no retention key.
	artifact := strings.Join([]string{
		`{"type":"header","schema_version":2}`,
		`{"type":"project","project":{"id":"pv","path":"/src/pv","name":"v","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
		`{"type":"memory","memory":{"id":"mv","project_id":"pv","category":"fact","content":"a record that states no tier","source":"mcp","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
	}, "\n") + "\n"

	report, err := Import(context.Background(), dst, strings.NewReader(artifact),
		ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 0 {
		t.Fatalf("rejected %d record(s): %v", report.Rejected, report.Errors)
	}
	if report.Created["memory"] != 1 {
		t.Fatalf("created %v, want one memory", report.Created)
	}
	got, err := dst.PortableMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(got) != 1 || got[0].Retention != memory.RetentionProject {
		t.Errorf("retention = %+v, want the durable default for a record that states none", got)
	}
}

// TestAnArtifactStatingAnUnknownRetentionIsRejectedByName: a hand-edited file
// can carry a fourth tier. It is refused per record rather than failing the
// INSERT with the schema's own wording, the rest of the file still imports, and
// the run exits non-zero — which is what lets the operator fix the line.
func TestAnArtifactStatingAnUnknownRetentionIsRejectedByName(t *testing.T) {
	dst := newTestStore(t)
	artifact := strings.Join([]string{
		`{"type":"header","schema_version":2}`,
		`{"type":"project","project":{"id":"pv","path":"/src/pv","name":"v","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
		`{"type":"memory","memory":{"id":"bad","project_id":"pv","category":"fact","content":"a fourth tier","source":"mcp","retention":"forever","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
		`{"type":"memory","memory":{"id":"good","project_id":"pv","category":"fact","content":"a real tier","source":"mcp","retention":"persistent","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
	}, "\n") + "\n"

	var results []RecordResult
	report, err := Import(context.Background(), dst, strings.NewReader(artifact),
		ImportOptions{Apply: true, TrustProvenance: true}, func(r RecordResult) {
			results = append(results, r)
		})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 1 {
		t.Fatalf("rejected %d record(s), want exactly the one with the bad tier: %v", report.Rejected, report.Errors)
	}
	if report.Created["memory"] != 1 {
		t.Errorf("created %v, want the good record alone", report.Created)
	}
	for _, r := range results {
		if r.ID == "bad" {
			if r.Error == nil || !strings.Contains(r.Error.Error(), "must be one of") {
				t.Errorf("the bad record's error does not name the accepted values: %v", r.Error)
			}
			continue
		}
		if r.Action != ActionCreate {
			t.Errorf("the good record was not created: %+v", r)
		}
	}
	got, err := dst.PortableMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" || got[0].Retention != memory.RetentionPersistent {
		t.Errorf("the destination holds %+v, want only the good persistent record", got)
	}
}

// TestARejectedArtifactRecordNeverPrintsTheValueItRejected: the report is the
// only place the artifact's own words are echoed back — every rejected record's
// error is carried in it and the CLI prints each one — so it is the surface a
// value would actually reach a terminal through. A hand-edited file can put a
// credential in any column, and the predicate that judges a record runs before
// the secret guard, so each of the three value-set refusals must withhold the
// value it refused rather than quote it back.
func TestARejectedArtifactRecordNeverPrintsTheValueItRejected(t *testing.T) {
	const token = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// One record per column a value SET guards, each holding the token and
	// otherwise valid, so the refusal under test is the column's own. The record
	// is built as JSON rather than a string so a duplicate key cannot hide which
	// value the decoder actually saw.
	line := func(field string) string {
		rec := map[string]any{
			"id": "bad-" + field, "project_id": "pv", "category": "fact", "content": "x",
			"source": "mcp", "created_at": "2026-01-01 00:00:00", "updated_at": "2026-01-01 00:00:00",
			field: token,
		}
		b, err := json.Marshal(map[string]any{"type": "memory", "memory": rec})
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		return string(b)
	}
	artifact := strings.Join([]string{
		`{"type":"header","schema_version":2}`,
		`{"type":"project","project":{"id":"pv","path":"/src/pv","name":"v","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
		line("retention"),
		line("category"),
		line("source"),
	}, "\n") + "\n"

	dst := newTestStore(t)
	report, err := Import(context.Background(), dst, strings.NewReader(artifact),
		ImportOptions{Apply: true}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 3 {
		t.Fatalf("rejected %d record(s), want the three holding a token: %v", report.Rejected, report.Errors)
	}
	for _, e := range report.Errors {
		if strings.Contains(e.Error(), token) {
			t.Errorf("a report error printed the credential: %v", e)
		}
	}
	// And rendered the way the CLI renders it, since %v on the error is what a
	// user sees: an assertion on the error objects alone would miss a print
	// path that re-encoded them.
	var out strings.Builder
	for _, e := range report.Errors {
		fmt.Fprintf(&out, "%v\n", e)
	}
	if strings.Contains(out.String(), token) {
		t.Errorf("the rendered report printed the credential:\n%s", out.String())
	}
}
