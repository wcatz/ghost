package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestInvariantsListsEverySubcommand holds docs/invariants.md's one-line command
// inventory (the package-map bullet for cmd/ghost, moved there from CLAUDE.md)
// to the dispatch, in both directions. The line went stale four times before
// this test existed (#561 found lifecycle, maintenance, history and opencode
// missing from it) and nothing noticed, because the list is prose next to a
// switch statement and no build step reads either.
//
// The names come from usageByCommand, which is the registry every `-h` answer
// comes from, so a subcommand cannot be routable and undocumented at the same
// time. `help` is deliberately not in that table: it prints the top-level
// command list rather than a usage block of its own, so it has no entry to
// derive a name from — it is checked by name below instead.
func TestInvariantsListsEverySubcommand(t *testing.T) {
	const docPath = "../../docs/invariants.md"
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	line, ok := subcommandListBody(string(raw))
	if !ok {
		t.Fatalf("no subcommand list line in %s; the test cannot tell whether it is stale", docPath)
	}

	listed := map[string]bool{}
	for _, name := range strings.Split(line, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			listed[name] = true
		}
	}

	for path := range usageByCommand {
		command, _, _ := strings.Cut(path, " ")
		if !listed[command] {
			t.Errorf("docs/invariants.md's subcommand list does not name %q (registered as %q)", command, path)
		}
	}
	if !listed["help"] {
		t.Error(`docs/invariants.md's subcommand list does not name "help"`)
	}

	// The other direction: a name in the list that dispatches to nothing is a
	// reader sent looking for a command that is not there.
	for name := range listed {
		if name == "help" {
			continue // not in usageByCommand, by design; see the comment above
		}
		if _, found := usageByCommand[name]; !found {
			t.Errorf("docs/invariants.md's subcommand list names %q, which is not a registered command", name)
		}
	}
}

// subcommandListBody finds the docs/invariants.md bullet that inventories the commands
// and returns just the names. Anchored on the `cmd/ghost/main.go` reference
// rather than on the word "subcommands" alone, because a sentence containing
// that word anywhere above the bullet would otherwise redirect the test at the
// wrong line — and a test that silently checks the wrong sentence is worse than
// one that fails.
var subcommandListRE = regexp.MustCompile("(?m)^- `cmd/ghost/main\\.go`[^\\n]*subcommands: ([^.]+)\\.")

func subcommandListBody(doc string) (string, bool) {
	m := subcommandListRE.FindStringSubmatch(doc)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// TestInvariantsListsEveryInternalPackage holds docs/invariants.md's package
// map to the tree under internal/, in both directions, for the reason the
// subcommand list above is held to the dispatch: the map is prose next to the
// filesystem and no build step reads either, so a package under internal/ can
// exist with nothing in the file the PR reviewer workflow reads as its
// conventions. #764 listed three such packages at once (procstat, scratch,
// repo) and named the map as the fix; the fourth (secret) was not in the item
// and is only here because this test walks the tree instead of trusting the
// list of things noticed.
func TestInvariantsListsEveryInternalPackage(t *testing.T) {
	const docPath = "../../docs/invariants.md"
	const internalDir = "../../internal"
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}

	listed := internalPackageList(string(raw))
	if len(listed) == 0 {
		t.Fatalf("no `internal/<pkg>/` bullets in %s; the test cannot tell whether the map is stale", docPath)
	}

	entries, err := os.ReadDir(internalDir)
	if err != nil {
		t.Fatalf("read %s: %v", internalDir, err)
	}
	found := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkg := filepath.Join(internalDir, entry.Name())
		if !hasGoFiles(pkg) {
			continue
		}
		found++
		if !listed[entry.Name()] {
			t.Errorf("docs/invariants.md's package map has no bullet for internal/%s/", entry.Name())
		}
	}
	if found == 0 {
		t.Fatalf("no package directories under %s; the test passed without looking at anything", internalDir)
	}

	// The other direction: a bullet naming a directory that is gone is a
	// reader sent looking for a package that is not there.
	for name := range listed {
		if _, err := os.Stat(filepath.Join(internalDir, name)); errors.Is(err, fs.ErrNotExist) {
			t.Errorf("docs/invariants.md's package map names internal/%s/, which is not a directory under %s", name, internalDir)
		}
	}
}

// internalPackageList returns the package names the doc's package map claims a
// bullet for. Anchored on a line that STARTS with the bullet and names the
// directory, so a package path mentioned mid-sentence in another bullet is not
// counted as a bullet of its own — a test that credits a prose mention is worse
// than one that fails.
var internalPackageRE = regexp.MustCompile("(?m)^- `internal/([a-z0-9]+)/`")

func internalPackageList(doc string) map[string]bool {
	listed := map[string]bool{}
	for _, m := range internalPackageRE.FindAllStringSubmatch(doc, -1) {
		listed[m[1]] = true
	}
	return listed
}

// hasGoFiles reports whether the directory holds at least one Go file, test
// files included: internal/adversarial is called only from _test.go files and
// still owns a bullet.
func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			return true
		}
	}
	return false
}
