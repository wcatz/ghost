package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestInvariantsCitedTestsExist holds every `TestName` that docs/invariants.md
// cites in backticks to a test function that exists. A renamed test left the
// doc naming one nothing defines (TestRetrievalAuditsStoresNoText for
// TestRecordRetrievalAuditsStoresNoText), and a reader who greps for the
// cited name finds nothing.
func TestInvariantsCitedTestsExist(t *testing.T) {
	const root = "../.."
	raw, err := os.ReadFile(filepath.Join(root, "docs", "invariants.md"))
	if err != nil {
		t.Fatalf("read invariants.md: %v", err)
	}
	cited := map[string]bool{}
	for _, m := range regexp.MustCompile("`(Test[A-Za-z0-9_]+)`").FindAllStringSubmatch(string(raw), -1) {
		cited[m[1]] = true
	}
	if len(cited) == 0 {
		t.Fatal("docs/invariants.md cites no tests; the pattern no longer matches anything")
	}

	defined := map[string]bool{}
	defRE := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == ".worktrees" || n == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range defRE.FindAllStringSubmatch(string(b), -1) {
			defined[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Names the doc cites on purpose as the tests that were replaced.
	retired := map[string]bool{
		"TestDecisionRecordCannotReachARefusedNameBinding": true,
		"TestTheCausesWriteIsNotGuarded":                   true,
	}
	for name := range cited {
		if !defined[name] && !retired[name] {
			t.Errorf("docs/invariants.md cites %s, which no _test.go file defines", name)
		}
	}
}
