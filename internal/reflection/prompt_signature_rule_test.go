package reflection

// #648 slice 1: the rule BuildReflectionPrompt's own docstring states about
// InputSignature, and the exception to it.
//
// The docstring says any field rendered for ExistingMemories must be mirrored in
// InputSignature, which fingerprints the corpus for the --skip-unchanged gate.
// The slice adds Usefulness, which is rendered for ExistingMemories and is
// deliberately NOT mirrored — mirroring it would stop the gate skipping on any
// store that records verdicts, which is most of them, on every stop hook.
//
// That reasoning was correct and was written down at the call site. The rule in
// prompt.go was left stating the opposite, unchanged, in the function that
// renders the field. A rule stated there with no exception recorded is a rule the
// next reader will enforce: the obvious "fix" for the perceived asymmetry is to
// mirror Usefulness, and that silently breaks --skip-unchanged for every audited
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

// TestThePromptDocstringRecordsTheInputSignatureException: the mirroring rule and
// both of its exceptions are stated in the same doc comment, and the Usefulness
// one names the field and the gate it would break.
//
// The phrases are required rather than paraphrased because each carries a fact
// the rule cannot be reconstructed from: the RULE (a rendered field must be
// mirrored in InputSignature, and why — the --skip-unchanged gate), the ACCESS
// COUNT exception and its reason (ordinary reads increment it), and the USEFULNESS
// exception with the same shape of reason (verdicts arrive from retrieval, not
// from a session saving something, so the gate would fire on a session that saved
// nothing).
func TestThePromptDocstringRecordsTheInputSignatureException(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "prompt.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse prompt.go: %v", err)
	}
	var doc *ast.CommentGroup
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "BuildReflectionPrompt" {
			continue
		}
		if doc != nil {
			t.Fatal("two BuildReflectionPrompt declarations; the docstring under test is ambiguous")
		}
		doc = fn.Doc
	}
	if doc == nil {
		t.Fatal("BuildReflectionPrompt has no doc comment, so it cannot state the mirroring " +
			"rule or record the exceptions to it")
	}
	text := doc.Text()

	for _, want := range []struct {
		phrase string
		why    string
	}{
		{"InputSignature", "the docstring must name what the rule requires, so a reader who finds " +
			"Usefulness rendered and not mirrored can see which rule to read before enforcing it"},
		{"--skip-unchanged", "the rule must say what the mirroring is FOR, or \"must be mirrored\" " +
			"reads as style rather than as the gate"},
		{"access count", "the pre-existing exception must still be stated: the rule now says it has " +
			"exceptions, and losing this one would make it the only rule left"},
		{"Usefulness", "the exception this slice adds must name the FIELD it is about, since the " +
			"field is what a reader sees rendered and unmirrored"},
		{"NOT mirrored", "the exception must be stated as an exception and not merely implied — a " +
			"reader who does not see it denied will read the rule as mandatory"},
	} {
		if !strings.Contains(text, want.phrase) {
			t.Errorf("BuildReflectionPrompt's doc comment does not mention %q: %s\n\ncomment:\n%s",
				want.phrase, want.why, text)
		}
	}
}

// TestTheDocstringExceptionSitsBesideTheRule: an exception recorded far from the
// rule it qualifies is an exception a reader never meets, and the whole point is
// that the two statements must not be able to contradict each other. Both halves
// are in ONE doc comment here — asserted by reading BuildReflectionPrompt's alone
// — so this test pins only that the doc comment carries the exception at all.
//
// The negative case is what makes it worth having: a comment that named
// --skip-unchanged and Usefulness in DIFFERENT doc comments, or in a comment on
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
	// One comment, both halves: the rule sentence and the Usefulness exception
	// must be separated by at most one blank comment line. A comment that put the
	// exception in its own trailing paragraph after a blank line, a page away in
	// a long comment, is still readable here — this bound is the cheap form of
	// "the reader meets both", and the phrases above are what carry the content.
	text := fn.Doc.Text()
	rule := strings.Index(text, "InputSignature")
	useful := strings.Index(text, "Usefulness")
	if rule < 0 || useful < 0 {
		t.Fatalf("the doc comment must carry both the rule and the exception to be a place "+
			"they can be read together; rule at %d, exception at %d\n\ncomment:\n%s", rule, useful, text)
	}
	if useful > rule+400 {
		t.Errorf("the Usefulness exception is %d bytes after the rule it qualifies, so a reader "+
			"who stops at the rule never meets it:\n%s", useful-rule, text)
	}
}
