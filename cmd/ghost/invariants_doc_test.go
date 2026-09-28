package main

import (
	"os"
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
