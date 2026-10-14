package portable

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestTheImportReportNamesEveryFieldACapCut is #1232 inside the portable
// package: a report line says WHICH field a cap cut, for a task and a decision
// alike, and it says it in the dry run and in the apply the dry run previews.
//
// #1226 made the importers cut a task's and a decision's text at the caps their
// own writers apply, and justified the cut by its being visible in the stored
// row — true of a body field, whose ClampContent marker is IN the value, and
// false of everything else. A title's cut appends nothing, and the alternatives
// past the twentieth are not in the row at all. So an artifact carrying 100 KB
// of task title, or twenty-five alternatives, imported as a plain `create` and
// the loss was discoverable only by noticing afterwards that the row was short.
// The report could not have said otherwise: it carried a boolean, and a boolean
// was set for memories alone, so the two importers discarded what they knew.
//
// The lengths here are written from the caps rather than as literal byte counts,
// because the alternative is a copy of MaxTitleLen in this file — the drift
// #1226 closed, reintroduced as a test that stops cutting when the cap moves.
func TestTheImportReportNamesEveryFieldACapCut(t *testing.T) {
	// Past MaxTitleLen by a title's worth, and past MaxContentLen by a body's.
	// The filler is a digit-and-dot run rather than words, for a reason this test
	// would otherwise learn the hard way: twenty-four short lowercase words are a
	// BIP-39 recovery mnemonic's exact shape, so space-separated filler is
	// refused by the credential guard above the clamp, which runs first by
	// design and which this test is not about.
	oversizeTitle := strings.Repeat("ti01.", memory.MaxTitleLen/5+1)
	oversizeBody := strings.Repeat("bo02.", memory.MaxContentLen/5+1)
	oversizeAlternative := strings.Repeat("al03.", memory.MaxTitleLen/5+1)
	// More entries than the bound keeps, each well within it — so the clamp is
	// the drop of the tail rather than a cut inside an entry, which is a
	// different loss and gets its own record below.
	manyAlternatives := make([]string, 0, memory.MaxAlternatives+5)
	for i := 0; i < memory.MaxAlternatives+5; i++ {
		manyAlternatives = append(manyAlternatives, "option "+strings.Repeat("alt ", i+1))
	}

	lines := []string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{
			"id": "t1", "project_id": "p1", "title": oversizeTitle,
			"description": "an ordinary description", "notes": "ordinary notes",
			"status": "pending", "priority": 1,
		}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{
			"id": "t2", "project_id": "p1", "title": "two bodies past the cap",
			"description": oversizeBody, "notes": oversizeBody,
			"status": "pending", "priority": 1,
		}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{
			"id": "t3", "project_id": "p1", "title": "an ordinary task",
			"description": "an ordinary description", "notes": "ordinary notes",
			"status": "pending", "priority": 1,
		}}),
		jsonLine(t, map[string]any{"type": "decision", "decision": map[string]any{
			"id": "d1", "project_id": "p1", "title": "more alternatives than the bound keeps",
			"decision": "do it", "rationale": "because", "alternatives": manyAlternatives,
			"status": "active",
		}}),
		jsonLine(t, map[string]any{"type": "decision", "decision": map[string]any{
			"id": "d2", "project_id": "p1", "title": "two bodies and one oversized entry",
			"decision": oversizeBody, "rationale": oversizeBody,
			"alternatives": []string{"a short option", oversizeAlternative},
			"status":       "active",
		}}),
		jsonLine(t, map[string]any{"type": "decision", "decision": map[string]any{
			"id": "d3", "project_id": "p1", "title": "an ordinary decision",
			"decision": "do it", "rationale": "because", "alternatives": []string{"the other option"},
			"status": "active",
		}}),
	}
	artifact := strings.Join(lines, "\n") + "\n"

	// In the record's own field order, which is the order the report prints them
	// in: title first, then the bodies, then the list.
	want := map[string][]string{
		"t1": {"title"},
		"t2": {"description", "notes"},
		"t3": nil,
		"d1": {"alternatives"},
		"d2": {"decision", "rationale", "alternatives"},
		"d3": nil,
	}

	ctx := context.Background()
	run := func(t *testing.T, apply bool) map[string][]string {
		t.Helper()
		_, store := newTestStoreWithDB(t)
		seen := map[string][]string{}
		opts := ImportOptions{Apply: apply}
		if _, err := Import(ctx, store, strings.NewReader(artifact), opts, func(r RecordResult) {
			if r.Action != ActionCreate {
				t.Errorf("%s: record %s was %s (%v), want created — the clamp must be reported on the write",
					r.Type, r.ID, r.Action, r.Error)
				return
			}
			seen[r.ID] = r.Clamped
		}); err != nil {
			t.Fatalf("Import (apply=%v): %v", apply, err)
		}
		return seen
	}

	dry := run(t, false)
	applied := run(t, true)

	for id, wantFields := range want {
		if !slices.Equal(dry[id], wantFields) {
			t.Errorf("dry run reported %v for %s, want %v", dry[id], id, wantFields)
		}
		if !slices.Equal(applied[id], wantFields) {
			t.Errorf("apply run reported %v for %s, want %v", applied[id], id, wantFields)
		}
	}
	// The two runs must agree, which is the whole point of a preview: the answer
	// is made by the same store call above the apply=false return, so a clamp the
	// dry run reports is the clamp the apply makes.
	for id := range want {
		if !slices.Equal(dry[id], applied[id]) {
			t.Errorf("the dry run reported %v for %s and the apply %v", dry[id], id, applied[id])
		}
	}
}

// TestAClampIsReportedOnlyOnTheWriteItNames: a record this store already holds
// is a skip, and a record refused by the artifact's own bytes is a rejection.
// Neither wrote a row, so neither cut anything, and a report line that claimed a
// clamp on one would send the operator looking for text that was never lost —
// and would do it on every re-run of an artifact, which is the one run a backup
// is most often re-run.
func TestAClampIsReportedOnlyOnTheWriteItNames(t *testing.T) {
	oversizeTitle := strings.Repeat("ti01.", memory.MaxTitleLen/5+1)
	artifact := strings.Join([]string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{
			"id": "t1", "project_id": "p1", "title": oversizeTitle,
			"description": "an ordinary description", "status": "pending", "priority": 1,
		}}),
	}, "\n") + "\n"

	ctx := context.Background()
	_, store := newTestStoreWithDB(t)

	// The refusal names the field and nothing of its value, so this record is
	// rejected on the status set rather than on anything the title carries.
	refused := strings.Join([]string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{
			"id": "t2", "project_id": "p1", "title": oversizeTitle,
			"description": "an ordinary description", "status": "sideways", "priority": 1,
		}}),
	}, "\n") + "\n"

	for name, tc := range map[string]struct {
		artifact string
		opts     ImportOptions
	}{
		"a record the store already holds": {artifact: artifact, opts: ImportOptions{Apply: true}},
		"a record the artifact's own bytes refuse": {
			artifact: refused,
			opts:     ImportOptions{Apply: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Applied once first where the point is a re-import, so the second
			// run meets a store that already holds the row.
			if tc.artifact == artifact {
				if _, err := Import(ctx, store, strings.NewReader(artifact), tc.opts, nil); err != nil {
					t.Fatalf("Import (first run): %v", err)
				}
			}
			var seen []RecordResult
			if _, err := Import(ctx, store, strings.NewReader(tc.artifact), tc.opts, func(r RecordResult) {
				seen = append(seen, r)
			}); err != nil {
				t.Fatalf("Import (second run): %v", err)
			}
			for _, r := range seen {
				if r.Type != TypeTask {
					continue
				}
				if len(r.Clamped) != 0 {
					t.Errorf("record %s was %s and reported a clamp of %v; nothing was written, so nothing was cut",
						r.ID, r.Action, r.Clamped)
				}
			}
		})
	}
}
