package reflection

// #648 slice 1: the rule BuildReflectionPrompt's own docstring states about
// InputSignature, and the exception to it.
//
// The docstring says any field rendered for ExistingMemories must be mirrored in
// InputSignature, which fingerprints the corpus for the --skip-unchanged gate.
// The slice adds Flags, which is rendered for ExistingMemories and is
// deliberately NOT mirrored — mirroring it would stop the gate skipping on any
// store that records verdicts, which is most of them, on every stop hook.
//
// That reasoning was correct and was written down at the call site. The rule in
// prompt.go was left stating the opposite, unchanged, in the function that
// renders the field. A rule stated there with no exception recorded is a rule the
// next reader will enforce: the obvious "fix" for the perceived asymmetry is to
// mirror Flags, and that silently breaks --skip-unchanged for every audited
// project. So the exception is recorded in the docstring, and THIS test is what
// keeps it recorded.
//
// It reads the docstring rather than trusting it, because a docstring is prose
// and prose does not fail a build on its own: the commit that added the exception
// was otherwise green, and a later edit that deleted the paragraph would have
// been green too. go/parser is used rather than os.ReadFile + a substring over
// the whole file, so the assertion is about THIS function's doc comment — a match
// anywhere else in prompt.go is not a match.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestTheDocstringExceptionSitsBesideTheRule: an exception recorded far from the
// rule it qualifies is an exception a reader never meets, and the whole point is
// that the two statements must not be able to contradict each other. Both halves
// are in ONE doc comment here — asserted by reading BuildReflectionPrompt's alone
// — so this test pins only that the doc comment carries the exception at all.
//
// The negative case is what makes it worth having: a comment that named
// --skip-unchanged and Flags in DIFFERENT doc comments, or in a comment on
// some other function, would leave the rule in prompt.go unqualified while every
// phrase-based check still passed.
func TestTheDocstringExceptionSitsBesideTheRule(t *testing.T) {
	if _, err := os.Stat("prompt.go"); err != nil {
		t.Fatalf("stat prompt.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "prompt.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse prompt.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "BuildReflectionPrompt" {
			fn = d
			break
		}
	}
	if fn == nil || fn.Doc == nil {
		t.Fatal("BuildReflectionPrompt's doc comment is missing")
	}
	// One comment, both halves: the rule sentence and the Flags exception
	// must be separated by at most one blank comment line. A comment that put the
	// exception in its own trailing paragraph after a blank line, a page away in
	// a long comment, is still readable here — this bound is the cheap form of
	// "the reader meets both", and the phrases above are what carry the content.
	text := fn.Doc.Text()
	rule := strings.Index(text, "InputSignature")
	useful := strings.Index(text, "Flags")
	if rule < 0 || useful < 0 {
		t.Fatalf("the doc comment must carry both the rule and the exception to be a place "+
			"they can be read together; rule at %d, exception at %d\n\ncomment:\n%s", rule, useful, text)
	}
	if useful > rule+400 {
		t.Errorf("the Flags exception is %d bytes after the rule it qualifies, so a reader "+
			"who stops at the rule never meets it:\n%s", useful-rule, text)
	}
}
