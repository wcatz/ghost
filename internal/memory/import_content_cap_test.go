package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// bulkyField is one field's worth of imported text far past every cap a task
// or a decision row carries: 100 KB, twelve times the content cap and three
// hundred times the title cap. The tokens are unique per index so one field
// cannot be mistaken for another in a failure message.
func bulkyField(i int, noun string) string {
	return fmt.Sprintf("%s %02d: %s", noun, i, strings.Repeat(fmt.Sprintf("%s%02d.", noun[:2], i), 12000))
}

// wantCutContent is what a writer stores for a body field: the first MaxContentLen
// bytes of the text plus the explicit marker naming the cut.
func wantCutContent(input string) string {
	return TruncateUTF8(input, MaxContentLen) + TruncationMarker()
}

// wantCutTitle is the same for a field the writers cut at the title cap, which
// carries no marker because a title's cut does not append one.
func wantCutTitle(input string) string {
	return TruncateUTF8(input, MaxTitleLen)
}

// TestImportTaskClampsTextToTheWriterCaps: a task's title, description and
// notes reach the store through `ghost_task_create` and
// `ghost_task_complete` already capped, and the row-rendering read tools
// assume a row is bounded by that construction. An artifact carrying 100 KB of
// each field arrived through ImportTask instead, and one `ghost_task_list` row
// then rendered about 200 KB against a 16000-byte response budget. The import
// applies the caps the owning writers apply, so the stored bytes are exactly
// what a save of the same text stores.
func TestImportTaskClampsTextToTheWriterCaps(t *testing.T) {
	const titleCap, contentCap = 300, 8000

	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	fields := map[string]string{
		"title":       bulkyField(1, "title"),
		"description": bulkyField(2, "description"),
		"notes":       bulkyField(3, "notes"),
	}
	if _, err := store.ImportTask(ctx, Task{
		ID: "t1", ProjectID: "p1",
		Title:       fields["title"],
		Description: fields["description"],
		Notes:       fields["notes"],
		Status:      "pending", Priority: 2,
	}, true); err != nil {
		t.Fatalf("ImportTask: %v", err)
	}

	got, err := store.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	for _, c := range []struct {
		field   string
		stored  string
		want    string
		cap     int
		isTitle bool
	}{
		{"title", got.Title, wantCutTitle(fields["title"]), titleCap, true},
		{"description", got.Description, wantCutContent(fields["description"]), contentCap + len(TruncationMarker()), false},
		{"notes", got.Notes, wantCutContent(fields["notes"]), contentCap + len(TruncationMarker()), false},
	} {
		if c.stored != c.want {
			t.Errorf("stored %s = %d bytes, want the first %d bytes of the artifact's own text%s; stored tail %q",
				c.field, len(c.stored), c.cap, markerClause(c.isTitle), tailBytes(c.stored, 48))
		}
		if len(c.stored) > c.cap {
			t.Errorf("stored %s = %d bytes, over the %d-byte bound the task's own writers apply",
				c.field, len(c.stored), c.cap)
		}
	}
}

// TestImportDecisionClampsTextToTheWriterCaps: the decision half, and the
// alternatives list with it. `ghost_decision_record` truncates each entry and
// keeps at most MaxAlternatives of them, and `ghost_decisions_list` renders the
// whole list back, so an artifact carrying 25 oversized entries produced a
// single row of megabytes. Tags are the one column left alone: a tag is carried
// byte for byte in both directions (#822) and the import must not change that.
func TestImportDecisionClampsTextToTheWriterCaps(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	alternatives := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		alternatives = append(alternatives, bulkyField(i, "alt"))
	}
	tags := []string{"alpha", "beta"}
	fields := map[string]string{
		"title":     bulkyField(6, "title"),
		"decision":  bulkyField(7, "decision"),
		"rationale": bulkyField(8, "rationale"),
	}
	if _, err := store.ImportDecision(ctx, Decision{
		ID: "d1", ProjectID: "p1",
		Title:        fields["title"],
		Decision:     fields["decision"],
		Alternatives: alternatives,
		Rationale:    fields["rationale"],
		Status:       "active",
		Tags:         tags,
	}, true); err != nil {
		t.Fatalf("ImportDecision: %v", err)
	}

	got := findDecision(t, store, "d1")
	for _, c := range []struct {
		field   string
		stored  string
		want    string
		isTitle bool
	}{
		{"title", got.Title, wantCutTitle(fields["title"]), true},
		{"decision", got.Decision, wantCutContent(fields["decision"]), false},
		{"rationale", got.Rationale, wantCutContent(fields["rationale"]), false},
	} {
		if c.stored != c.want {
			t.Errorf("stored %s = %d bytes, want the writer's cut of the artifact's own text%s; stored tail %q",
				c.field, len(c.stored), markerClause(c.isTitle), tailBytes(c.stored, 48))
		}
	}
	if len(got.Alternatives) != MaxAlternatives {
		t.Fatalf("stored %d alternatives, want the %d `ghost_decision_record` keeps",
			len(got.Alternatives), MaxAlternatives)
	}
	for i, alt := range got.Alternatives {
		// The entries the writer keeps are the FIRST MaxAlternatives in artifact
		// order, each cut at the title cap, so entry i is the prefix of the i-th
		// entry the artifact carried.
		if want := wantCutTitle(alternatives[i]); alt != want {
			t.Errorf("alternatives[%d] = %d bytes, want the %d-byte title cap applied to the entry the artifact carried",
				i, len(alt), MaxTitleLen)
		}
	}
	if len(got.Tags) != len(tags) {
		t.Errorf("stored %d tags, want %d — a tag list is carried byte for byte (#822)",
			len(got.Tags), len(tags))
	}
}

// TestImportTaskAndDecisionLeaveSubCapTextByteIdentical: the clamp is a clamp
// and not a rewrite. An ordinary artifact — the overwhelmingly common case, and
// every artifact this build has ever written — must come back byte for byte,
// with no marker and no cut, or a restore stops being a restore.
func TestImportTaskAndDecisionLeaveSubCapTextByteIdentical(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	task := Task{
		ID: "t1", ProjectID: "p1", Title: "ship it", Description: "a description",
		Notes: "some notes", Status: "done", Priority: 2, CompletedAt: "2026-01-02 00:00:00",
	}
	if _, err := store.ImportTask(ctx, task, true); err != nil {
		t.Fatalf("ImportTask: %v", err)
	}
	gotTask, err := store.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotTask.Title != task.Title || gotTask.Description != task.Description || gotTask.Notes != task.Notes {
		t.Errorf("imported task text = %q / %q / %q, want %q / %q / %q byte for byte",
			gotTask.Title, gotTask.Description, gotTask.Notes, task.Title, task.Description, task.Notes)
	}

	dec := Decision{
		ID: "d1", ProjectID: "p1", Title: "a title", Decision: "do it",
		Alternatives: []string{"one", "two"}, Rationale: "because", Status: "active",
		Tags: []string{"tag"},
	}
	if _, err := store.ImportDecision(ctx, dec, true); err != nil {
		t.Fatalf("ImportDecision: %v", err)
	}
	gotDec := findDecision(t, store, "d1")
	if gotDec.Title != dec.Title || gotDec.Decision != dec.Decision || gotDec.Rationale != dec.Rationale {
		t.Errorf("imported decision text = %q / %q / %q, want byte for byte",
			gotDec.Title, gotDec.Decision, gotDec.Rationale)
	}
	if len(gotDec.Alternatives) != 2 || gotDec.Alternatives[0] != "one" || gotDec.Alternatives[1] != "two" {
		t.Errorf("imported alternatives = %q, want [one two] byte for byte", gotDec.Alternatives)
	}
}

// markerClause names the marker a cut carries, so a failure message says which
// bound is missing rather than only that two byte strings differ.
func markerClause(isTitle bool) string {
	if isTitle {
		return ""
	}
	return " plus the " + TruncationMarker() + " marker"
}

// tailBytes is the last n bytes of s, for a failure message that must not print
// a 100 KB field.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
