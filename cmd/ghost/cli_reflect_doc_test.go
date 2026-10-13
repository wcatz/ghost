package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheCLIReferenceStatesReflectRefusesASecondProject pins the documented
// contract to the parser. docs/cli.md claimed `ghost reflect` was the exception
// among the operand parsers and consolidated the LAST of two positionals, which
// no build has ever done: parseReflectArgs refuses a second project with
// `expected exactly one project`, exactly as every other scope parser does, and
// docs/invariants.md already said so. The two documents contradicted each other
// and this is the one that was wrong.
//
// The parser is the authority, so the test first states what it does and then
// holds the paragraph to it. A reference a caller follows expecting last-one-wins
// otherwise gets exit 1 and no consolidation, which is the reproduction the
// issue recorded.
func TestTheCLIReferenceStatesReflectRefusesASecondProject(t *testing.T) {
	// The three orders the issue reproduced, each refused by the parser.
	for _, args := range [][]string{
		{"alpha", "beta"},
		{"--project", "alpha", "beta"},
		{"alpha", "--project", "beta"},
	} {
		if _, err := parseReflectArgs(args); err == nil || !strings.Contains(err.Error(), "expected exactly one project") {
			t.Fatalf("parseReflectArgs(%v) error = %v, want it to contain %q; this test's premise is the parser's, so a parser change has to revisit the reference",
				args, err, "expected exactly one project")
		}
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatalf("read docs/cli.md: %v", err)
	}

	// Scoped to the one paragraph that lists what each operand parser reports,
	// so the assertion is about that sentence rather than about the page.
	const operandLead = "A word after a command that takes an *operand*"
	var paragraph string
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, operandLead) {
			paragraph = line
			break
		}
	}
	if paragraph == "" {
		t.Fatalf("docs/cli.md no longer has the operand-parsers paragraph beginning %q", operandLead)
	}

	// The retired claim, in the two forms it shipped.
	for _, stale := range []string{
		"`ghost reflect` is the exception among those parsers",
		"given two positionals it consolidates the last one",
	} {
		if strings.Contains(paragraph, stale) {
			t.Errorf("docs/cli.md still carries the false claim %q", stale)
		}
	}

	// And the paragraph names reflect among the parsers that refuse a second
	// project, with the sentence the parser produces.
	if !strings.Contains(paragraph, "`ghost reflect`") {
		t.Errorf("docs/cli.md does not name `ghost reflect` among the parsers that refuse a second project:\n%s", paragraph)
	}
	if !strings.Contains(paragraph, "expected exactly one project") {
		t.Errorf("docs/cli.md does not state the `expected exactly one project` refusal the parser produces:\n%s", paragraph)
	}
}
