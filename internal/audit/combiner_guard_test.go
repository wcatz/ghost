package audit

// Why this file exists: the report's own prose says there is no way to pool two
// sources — Report.Pooled returns nil for exactly that reason, and
// docs/invariants.md says the store-wide reader "REPLACED the summing function rather
// than joining it: a shipped combiner with no caller is how the next reader concludes
// it is the sanctioned way to pool". A SourceReport.AddInto shipped with that claim
// written above it: it took another source's figures and added them without checking
// that the two were the same source, which is the forbidden direction, and nothing in
// the tree called it.
//
// A prose claim about an API cannot fail a build. This scan can, and it is what makes
// the invariant checkable rather than aspirational — the same reasoning as the
// go/ast scan in outcome_vocabulary_test.go, applied to a shape rather than to a
// vocabulary.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestSourceReportHasNoExportedCombiner: SourceReport gains no exported method that
// could COMBINE figures. It keeps its read-only ones — NoRows, Unscored, Precision,
// PrecisionPercent, Summary — because a reader outside this package needs those.
//
// The shape rejected is precise, and each half of it has a reason:
//
//   - a POINTER receiver. Every read this type serves has a value receiver, so a
//     pointer receiver can only be for MUTATION, and a mutation a caller can name is
//     the half of a combiner that does the damage: a method that folds numbers into a
//     per-source figure in place is pooling by another name, and the caller cannot
//     tell from the signature that it also has to check the source matches.
//   - a SourceReport PARAMETER. A method handed another source's figures is the
//     combiner in its plainest form — and this is the form AddInto had, added under a
//     docstring that called itself "the ONE permitted direction" while taking the two
//     sources it did not check as the same source. A *SourceReport RESULT is NOT in
//     this half: Source(name) hands back one existing entry to be read, which is the
//     opposite of a combiner.
//
// A package-level exported function is checked for the slice form too — two or more
// SourceReport parameters, or one []SourceReport — because that is the shape a
// pooling helper would take, and it is reachable from another package without ever
// being a method.
//
// An unexported method is exempt on purpose: it cannot be reached from another
// package, so it cannot be mistaken for the sanctioned way to pool. The failure
// message prints the docstring's first line, because the claim is the failure — a
// reader has to see that the method said it was the right way to do this.
func TestSourceReportHasNoExportedCombiner(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "report.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse report.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !fn.Name.IsExported() {
			continue
		}
		switch {
		case fn.Recv != nil && len(fn.Recv.List) > 0:
			byPointer := receiverIsPointer(fn)
			if !byPointer && sourceReportsIn(fn.Type.Params) == 0 {
				continue
			}
			why := "it is handed another SourceReport's figures, which is the combiner in plain form"
			if byPointer {
				why = "it has a pointer receiver, and every read on this type has a value receiver, so it exists to mutate a per-source figure"
			}
			t.Errorf("SourceReport.%s is exported at %s, which cannot be a read: %s. Pooling over "+
				"projects is memory.RetrievalSourceTotals' job (one aggregate per table), and pooling "+
				"over sources is the figure this package exists not to be able to produce — "+
				"Report.Pooled returns nil for it.%s",
				fn.Name.Name, fset.Position(fn.Pos()), why, docFirstLine(fn.Doc.Text()))
		case sourceReportsIn(fn.Type.Params) >= 2, takesSourceReportSlice(fn):
			shape := fmt.Sprintf("%d SourceReport parameters", sourceReportsIn(fn.Type.Params))
			if takesSourceReportSlice(fn) {
				shape = "a []SourceReport parameter"
			}
			t.Errorf("%s is an exported function over %s at %s, which is the pooling function "+
				"docs/invariants.md says this package has none of: the figures are per source and "+
				"pooling them is a number about neither question.%s",
				fn.Name.Name, shape, fset.Position(fn.Pos()), docFirstLine(fn.Doc.Text()))
		}
	}
}

// receiverIsPointer reports whether the method's sole receiver is *T.
func receiverIsPointer(fn *ast.FuncDecl) bool {
	_, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	return ok
}

// sourceReportsIn counts the parameters of the field list that are a SourceReport or a
// *SourceReport, counting names rather than types where one field declares several.
func sourceReportsIn(fields *ast.FieldList) int {
	if fields == nil {
		return 0
	}
	n := 0
	for _, field := range fields.List {
		if !isSourceReport(field.Type) {
			continue
		}
		if len(field.Names) == 0 {
			n++
			continue
		}
		n += len(field.Names)
	}
	return n
}

// takesSourceReportSlice reports whether any parameter is []SourceReport: the slice
// form, which is how the summing function this package refuses to ship would take its
// input.
func takesSourceReportSlice(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		slice, ok := field.Type.(*ast.ArrayType)
		if ok && isSourceReport(slice.Elt) {
			return true
		}
	}
	return false
}

func isSourceReport(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "SourceReport"
}

// docFirstLine is the docstring's opening sentence, for the failure message. Empty when
// there is no docstring, which is itself worth seeing in the message.
func docFirstLine(text string) string {
	line := strings.TrimSpace(text)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		return " It carries no docstring at all."
	}
	return " Its docstring opens: " + line
}
