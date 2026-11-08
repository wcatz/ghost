package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestMCPDocNamesOnlyRoutableCLICommands holds every backtick command
// reference in docs/mcp.md to the dispatch, in doc-to-CLI direction only. The
// command-listing paragraph in the single-line-preview section named
// `ghost promote` next to `ghost reflect`, and promote is not a command: the
// dispatch has no case for it and `ghost promote` answers `unknown command`
// with exit 2 (#691's shape exactly — a command line the CLI cannot act on
// answered as though it succeeded is the milder version; here the document
// itself sent the reader to one). Promotion to `_global` is
// `ghost reflect --promote-globals`, which cli.md already documents, so the
// reference had nowhere to point.
//
// The names come from usageByCommand, the same registry the known-command
// tests and TestInvariantsListsEverySubcommand read, so a command cannot be
// routable and unregistered at the same time. A subcommand form routes
// through its group: usageFor resolves the LONGEST registered path prefix of
// the reference's leading words, so `ghost maintenance consolidate-global`
// resolves at `maintenance` and a reference naming an unregistered first word
// resolves at nothing.
//
// Doc-to-CLI only: a command the dispatch routes but docs/mcp.md does not
// name is fine — mcp.md is the MCP protocol document, not the CLI manual,
// and cli.md owns that inventory.
func TestMCPDocNamesOnlyRoutableCLICommands(t *testing.T) {
	const docPath = "../../docs/mcp.md"
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	doc := string(raw)

	// The reference the test anchors on: the command-listing sentence in the
	// single-line-preview paragraph lists `ghost reflect`, and it is the one
	// backtick sequence this test must keep seeing. Without it the scan below
	// could match nothing and pass without looking at anything, the way the
	// invariants subcommand list is guarded in TestInvariantsListsEverySubcommand.
	if !strings.Contains(doc, "`ghost reflect`") {
		t.Fatalf("docs/mcp.md no longer names `ghost reflect` in the single-line-preview paragraph; the scan below cannot tell whether it still sees CLI command references at all")
	}

	sawReflect := false
	for _, m := range mcpDocCLICommandRE.FindAllStringSubmatch(doc, -1) {
		words := mcpDocCommandPath(m[1])
		if words == nil {
			continue // no leading command word: a placeholder or URI form
		}
		if words[0] == "reflect" {
			sawReflect = true
		}
		if _, _, _, ok := usageFor(words); !ok {
			t.Errorf("docs/mcp.md names `ghost %s`, which is not a routable command: no usageByCommand path matches its leading words (%s), so the CLI answers it with `unknown command` and exit 2", strings.Join(words, " "), m[1])
		}
	}
	if len(mcpDocCLICommandRE.FindAllStringSubmatch(doc, -1)) == 0 {
		t.Fatal("docs/mcp.md holds no backtick `ghost …` command references the scan can check; the test cannot tell whether the doc still names the CLI at all")
	}
	if !sawReflect {
		t.Error("the scan checked references but never saw a `ghost reflect` one, so it is no longer anchored on the listing paragraph it exists to hold")
	}
}

// mcpDocCLICommandRE extracts the instruction text of a backtick reference
// opening with the CLI's own word, `ghost `, followed by a command-shaped
// first token. Tool names (`ghost_memory_promote`), URIs (`ghost://…`) and
// placeholders do not match: their first character after the space tests
// false, and the text after it is their business.
var mcpDocCLICommandRE = regexp.MustCompile("`ghost ([a-z0-9][^`]*)`")

// mcpDocCommandPath returns the leading words of a reference that name the
// command itself, for usageFor: every word up to the first that is a flag,
// carries a placeholder (`<project>`) or merely follows as data. Leading
// words only — usageFor resolves the longest registered prefix of the words
// it is given, so `ghost maintenance consolidate-global` stops harmless at
// `maintenance` if the leaf is not itself registered, while a bad FIRST word
// resolves at nothing and fails the holding test.
func mcpDocCommandPath(ref string) []string {
	var words []string
	for _, tok := range strings.Fields(ref) {
		if tok == "" || strings.HasPrefix(tok, "-") || strings.ContainsAny(tok, "<>") || !plainWordRE.MatchString(tok) {
			break
		}
		words = append(words, tok)
	}
	if len(words) == 0 {
		return nil
	}
	return words
}

var plainWordRE = regexp.MustCompile("^[a-z0-9][a-z0-9._-]*$")
