package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/eval/cycle/corpus"
)

const supOut = `acme-migration: 3 candidate pairs, 2 supersedes, 1 causes, 0 reclassified, linked
  aabbccdd11223344  supersedes  556677889900aabb
  deadbeefdeadbeef  supersedes  0011223344556677
  feedfacefeedface  causes  1111222233334444
`

func TestParseSupersedeLines(t *testing.T) {
	pairs := parseSupersedeLines(supOut)
	if len(pairs) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(pairs), pairs)
	}
	if pairs[0].NewerID8 != "aabbccdd" || pairs[0].OtherID8 != "55667788" || pairs[0].Relation != "supersedes" {
		t.Fatalf("pair0 wrong: %+v", pairs[0])
	}
	if pairs[2].Relation != "causes" {
		t.Fatalf("pair2 wrong: %+v", pairs[2])
	}
}

const resOut = `acme-migration: 30 loaded, 12 after prefilter, 2 confirmed evidence, would resolve 2
  aabbccdd11223344  [fact]  Some content line here
  9988776655443322  [gotcha]  Another content line
Re-run with --apply to mark these resolved.`

func TestParseResolveIDs(t *testing.T) {
	got := parseResolveIDs(resOut)
	want := []string{"aabbccdd", "99887766"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// grade fixtures: ids are 16-hex whose first 8 chars are the pair prefixes.
var gradeIDs = map[string]string{
	"old1": "11111111aaaaaaaa",
	"new1": "22222222aaaaaaaa",
	"old2": "33333333aaaaaaaa",
	"new2": "44444444aaaaaaaa",
	"ev1":  "55555555aaaaaaaa",
}

func gradeEntries() []corpus.Entry {
	return []corpus.Entry{
		{Key: "old1", Content: "deploy to fly today", ExpectedSupersededBy: "new1"},
		{Key: "new1", Content: "deploy to hetzner now"},
		{Key: "old2", Content: "queue uses lists", ExpectedSupersededBy: "new2"},
		{Key: "new2", Content: "queue uses streams"},
		{Key: "ev1", Content: "closed spike note", ExpectedResolved: true},
	}
}

func TestGradeSupersede_MixedOutcomes(t *testing.T) {
	pairs := []supPair{
		{NewerID8: "22222222", Relation: "supersedes", OtherID8: "11111111"}, // correct TP
		{NewerID8: "33333333", Relation: "supersedes", OtherID8: "44444444"}, // reversed → FP + FN(old2)
		{NewerID8: "feedface", Relation: "causes", OtherID8: "11112222"},     // counted, not scored
	}
	st := gradeSupersede(gradeIDs, gradeEntries(), pairs)
	if st.TP != 1 || st.FP != 1 || st.FN != 1 || st.Causes != 1 {
		t.Fatalf("counts wrong: %+v", st)
	}
	if st.Precision() != 0.5 || st.Recall() != 0.5 {
		t.Fatalf("P/R wrong: %.2f/%.2f", st.Precision(), st.Recall())
	}
	kinds := map[string]bool{}
	for _, m := range st.Misses {
		kinds[m.Kind] = true
	}
	if !kinds["unexpected-link"] || !kinds["missed-link"] {
		t.Fatalf("missing miss kinds: %+v", st.Misses)
	}
}

func TestGradeSupersede_Perfect(t *testing.T) {
	pairs := []supPair{
		{NewerID8: "22222222", Relation: "supersedes", OtherID8: "11111111"},
		{NewerID8: "44444444", Relation: "supersedes", OtherID8: "33333333"},
	}
	st := gradeSupersede(gradeIDs, gradeEntries(), pairs)
	if st.TP != 2 || st.FP != 0 || st.FN != 0 {
		t.Fatalf("counts wrong: %+v", st)
	}
}

func TestGradeResolve_Mixed(t *testing.T) {
	confirmed := []string{"55555555", "44444444"} // ev1 correct; new2 unexpected
	st := gradeResolve(gradeEntries(), confirmed, gradeIDs)
	if st.TP != 1 || st.FP != 1 || st.FN != 0 {
		t.Fatalf("counts wrong: %+v", st)
	}
	found := map[string]bool{}
	for _, m := range st.Misses {
		found[m.Kind] = true
	}
	if !found["unexpected-resolve"] || found["missed-resolve"] {
		t.Fatalf("miss kinds wrong: %+v", st.Misses)
	}
}

func TestGradeResolve_Missed(t *testing.T) {
	st := gradeResolve(gradeEntries(), nil, gradeIDs)
	if st.TP != 0 || st.FN != 1 {
		t.Fatalf("counts wrong: %+v", st)
	}
}

func TestExcerpt(t *testing.T) {
	long := strings.Repeat("x", 100) + "\nsecond line"
	if got := excerpt(long); len(got) != 70 || strings.Contains(got, "\n") {
		t.Fatalf("excerpt wrong: %q", got)
	}
	if got := excerpt("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
}
