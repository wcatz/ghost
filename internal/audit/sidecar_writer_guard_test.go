package audit

// Why this file exists: the sidecar is written on the stop-hook path, which runs
// synchronously while the user is waiting on the turn, and one turn can hold every
// distinct token of a large assistant line — a tool call's whole argument body is
// one `AddToolArgs`, and a single line is bounded at 64 KiB by the scanner before
// it is even parsed. So the per-turn line is written straight into the buffer: a
// `line += " " + fp` in that loop copies the whole prefix on every fingerprint and
// is quadratic in the turn's own size, where the buffer is linear.
//
// A claim about complexity cannot be read out of the code by a reader, and this
// shape was written the expensive way once already — under review, where the only
// evidence was the loop itself. This scan is the same reasoning as
// combiner_guard_test.go applied to a shape rather than to an API: the writer's
// linear form fails a build if it is ever concatenated back.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTheSidecarWriterBuildsNoStringInALoop: WriteSidecar contains no additive
// assignment, in either spelling — `line += x` and `line = line + x` — because
// both build a growing string out of a turn's fingerprints. A concatenation that
// cannot grow with a turn (one field, one value) is fine and stays.
func TestTheSidecarWriterBuildsNoStringInALoop(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "signals.go", nil, 0)
	if err != nil {
		t.Fatalf("parse signals.go: %v", err)
	}
	var found bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "WriteSidecar" {
			continue
		}
		found = true
		ast.Inspect(fn, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 {
				return true
			}
			name, ok := as.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			if as.Tok == token.ADD_ASSIGN {
				t.Errorf("WriteSidecar: %s += ... at %s: a per-entry "+
					"concatenation is quadratic in the turn it prints; write into the buffer",
					name.Name, fset.Position(as.Pos()))
				return true
			}
			for _, rhs := range as.Rhs {
				if growsIdent(rhs, name.Name) {
					t.Errorf("WriteSidecar: %s = %s + ... at %s: the same "+
						"quadratic build in its other spelling; write into the buffer",
						name.Name, name.Name, fset.Position(as.Pos()))
					break
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("WriteSidecar not found in signals.go")
	}
}

// growsIdent reports whether the expression adds the named variable to something,
// which is how the quadratic form is written when += is not used.
func growsIdent(expr ast.Expr, name string) bool {
	bin, ok := expr.(*ast.BinaryExpr)
	if !ok || bin.Op != token.ADD {
		return false
	}
	for _, operand := range []ast.Expr{bin.X, bin.Y} {
		if id, ok := operand.(*ast.Ident); ok && id.Name == name {
			return true
		}
		if growsIdent(operand, name) {
			return true
		}
	}
	return false
}
