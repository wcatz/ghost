package mcpserver

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoToolResultPrintsAStoredIDOutsideTheRenderer is the structural half of
// #1377: over every non-test source file in this package, a stored memory id
// passed bare to a formatting call fails a build.
//
// The rule it enforces is the one the work changed: an id that reaches a tool
// result goes through assemble.Token (or shortID where a listing abbreviates, or
// memory.RefusalArg / memory.ProjectArg in a refusal). It is structural rather
// than behavioural because the defect the issue found is a HANDLER printing an
// id, and a behavioural test can only cover the handlers somebody remembered to
// write a case for — the next handler that prints a caller-addressed id is the
// one this holds.
//
// The operands it recognises are the identifiers and selectors this package
// uses for an id it did not itself mint: `id`, `duplicateOf`, `memoryID`,
// `memoryIDs`, `decisionID`, `args.MemoryID` and `args.MemoryIDs`. That set is
// the honest limit of a scan with no type checker, and it is deliberately the
// expressions a stored id is spelled with HERE: a value that is not one of them
// is out of scope, and the set grows with a new spelling rather than the guard
// being widened to guess.
//
// A verb is not consulted. Any verb prints a bare string, and `%q` is no safer
// than `%s` — it leaves a printable non-ASCII rune as itself — so the question is
// whether the value reached fmt at all, not which letter followed the percent.
//
// It also fails if it finds no rendered id operand, so it cannot pass by
// scanning nothing.
func TestNoToolResultPrintsAStoredIDOutsideTheRenderer(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	rendered := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			operands, ok := idGuardOperands(call)
			if !ok {
				return true
			}
			for _, arg := range operands {
				switch {
				case idGuardOperand(arg):
					t.Errorf("%s: a formatting call prints a stored memory id bare; route it "+
						"through assemble.Token (or shortID / memory.RefusalArg) so a line break "+
						"or a « cannot forge a line in the answer: %s",
						fset.Position(arg.Pos()), idGuardRender(arg))
				case idGuardRenders(arg):
					rendered++
				}
			}
			return true
		})
	}
	if rendered == 0 {
		t.Error("no stored id rendered through assemble.Token, shortID or memory.RefusalArg was " +
			"found, so this guard is holding nothing")
	}
}

// idGuardOperands returns the operands of a fmt formatting call whose format is
// a literal string, so the scan only judges a call it can actually read.
func idGuardOperands(call *ast.CallExpr) ([]ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return nil, false
	}
	// Fprintf carries its writer first; Sprintf and Errorf do not.
	formatIndex := 0
	switch sel.Sel.Name {
	case "Sprintf", "Errorf":
	case "Fprintf":
		formatIndex = 1
	default:
		return nil, false
	}
	if len(call.Args) <= formatIndex {
		return nil, false
	}
	if _, ok := idGuardLiteral(call.Args[formatIndex]); !ok {
		return nil, false
	}
	return call.Args[formatIndex+1:], true
}

// idGuardLiteral returns the value of a format string literal, or of a `+`
// chain of them, which is how a long sentence is wrapped across lines.
func idGuardLiteral(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := idGuardLiteral(e.X)
		if !ok {
			return "", false
		}
		right, ok := idGuardLiteral(e.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	}
	return "", false
}

// idGuardOperand reports whether expr is one of the spellings this package gives
// a stored memory id.
func idGuardOperand(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		switch e.Name {
		case "id", "duplicateOf", "memoryID", "memoryIDs", "decisionID":
			return true
		}
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok && pkg.Name == "args" {
			switch e.Sel.Name {
			case "MemoryID", "MemoryIDs":
				return true
			}
		}
	}
	return false
}

// idGuardRenders reports whether expr is a renderer call over a stored id: the
// two key renderers take the value first, the two refusal renderers take a label
// first and the value second.
func idGuardRenders(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch {
	case pkg.Name == "assemble" && (sel.Sel.Name == "Token" || sel.Sel.Name == "ShortID"):
		return len(call.Args) == 1 && idGuardOperand(call.Args[0])
	case pkg.Name == "memory" && (sel.Sel.Name == "RefusalArg" || sel.Sel.Name == "ProjectArg"):
		return len(call.Args) == 2 && idGuardOperand(call.Args[1])
	}
	return false
}

// idGuardRender prints an expression for a failure message, so the reader is
// shown the offending operand rather than its position alone.
func idGuardRender(expr ast.Expr) string {
	var b strings.Builder
	_ = printer.Fprint(&b, token.NewFileSet(), expr)
	return b.String()
}
