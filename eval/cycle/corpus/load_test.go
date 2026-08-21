package corpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validJSONL = `# comment line
{"key":"a","category":"fact","content":"alpha","importance":0.7,"expected_resolved":true}
{"key":"b","category":"gotcha","content":"beta","importance":0.8,"expected_superseded_by":"c"}

{"key":"c","category":"fact","content":"gamma newer","importance":0.7}
{"key":"d1","category":"convention","content":"merge one","importance":0.6,"merge_group":"g"}
{"key":"d2","category":"fact","content":"merge two","importance":0.6,"merge_group":"g"}
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMixedAnnotations(t *testing.T) {
	entries, err := Load(writeTemp(t, validJSONL))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("want 5, got %d", len(entries))
	}
	if !entries[0].ExpectedResolved || entries[1].ExpectedSupersededBy != "c" {
		t.Fatalf("annotations lost: %+v", entries[:2])
	}
	if entries[0].TagsOrEmpty() == nil {
		t.Fatal("TagsOrEmpty must never be nil")
	}
}

func TestLoadDuplicateKeyRejected(t *testing.T) {
	dup := `{"key":"x","category":"fact","content":"one","importance":0.7}
{"key":"x","category":"fact","content":"two","importance":0.7}`
	if _, err := Load(writeTemp(t, dup)); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want duplicate key error, got %v", err)
	}
}

func TestValidateOK(t *testing.T) {
	entries, _ := Load(writeTemp(t, validJSONL))
	if err := Validate(entries); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateUnknownSupersedeRef(t *testing.T) {
	bad := `{"key":"a","category":"fact","content":"x","importance":0.7,"expected_superseded_by":"missing"}`
	entries, _ := Load(writeTemp(t, bad))
	err := Validate(entries)
	if err == nil || !strings.Contains(err.Error(), "not in corpus") {
		t.Fatalf("want ref error, got %v", err)
	}
}

func TestValidateLoneMergeGroupAndBadCategory(t *testing.T) {
	lone := `{"key":"a","category":"fact","content":"x","importance":0.7,"merge_group":"solo"}`
	entries, _ := Load(writeTemp(t, lone))
	if err := Validate(entries); err == nil || !strings.Contains(err.Error(), "need >=2") {
		t.Fatalf("want group-size error, got %v", err)
	}
	badcat := `{"key":"a","category":"nonsense","content":"x","importance":0.7}`
	entries2, _ := Load(writeTemp(t, badcat))
	if err := Validate(entries2); err == nil || !strings.Contains(err.Error(), "invalid category") {
		t.Fatalf("want category error, got %v", err)
	}
}

// TestCommittedCorpusShape pins the committed dataset's composition so
// annotation drift fails loudly: 55 entries, 8 supersede-pair olders,
// 12 resolved-evidence, exactly 3 merge groups of 3, 18 unannotated
// distractors.
func TestCommittedCorpusShape(t *testing.T) {
	entries, err := Load("corpus.jsonl")
	if err != nil {
		t.Fatalf("load committed corpus: %v", err)
	}
	if err := Validate(entries); err != nil {
		t.Fatalf("validate committed corpus: %v", err)
	}
	supOlder, res := 0, 0
	grouped := map[string]int{}
	targets := map[string]bool{} // newer members of supersede pairs
	for _, e := range entries {
		switch {
		case e.ExpectedSupersededBy != "":
			supOlder++
			targets[e.ExpectedSupersededBy] = true
		case e.ExpectedResolved:
			res++
		}
		if e.MergeGroup != "" {
			grouped[e.MergeGroup]++
		}
	}
	if supOlder != 8 || res != 12 {
		t.Fatalf("composition drift: supersedes=%d resolved=%d", supOlder, res)
	}
	if len(grouped) != 3 {
		t.Fatalf("want 3 merge groups, got %d (%v)", len(grouped), grouped)
	}
	pairMembers := supOlder + len(targets)
	distractors := len(entries) - pairMembers - res
	for _, n := range grouped {
		distractors -= n
	}
	if pairMembers != 16 {
		t.Fatalf("want 16 supersede-pair members, got %d", pairMembers)
	}
	if distractors != 18 {
		t.Fatalf("want 18 distractors, got %d", distractors)
	}
}

// TestLoadRejectsUnknownFields: a typo'd annotation key must fail loudly —
// silently ignoring it would change grading semantics.
func TestLoadRejectsUnknownFields(t *testing.T) {
	typo := `{"key":"a","category":"fact","content":"x","importance":0.7,"expected_resovled":true}`
	if _, err := Load(writeTemp(t, typo)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown field error, got %v", err)
	}
}
