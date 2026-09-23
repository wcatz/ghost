package mcpinit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestHomeEnvIsolationGuard makes the home-directory isolation invariant
// self-enforcing: production resolves home via os.UserHomeDir, which reads
// HOME on Unix but USERPROFILE on Windows, so a sibling test that sets only
// one of them directly reads the runner's real profile on the other OS —
// exactly what the whole-package Windows legs must never do.
// homeenv_test.go is the sanctioned exclusion: it defines homeVars/setHome,
// the only sanctioned way to set these variables, so every other *_test.go
// in this package must go through setHome. The guard parses each sibling
// with go/ast and fails the file:line of any Setenv call (any receiver:
// t.Setenv or os.Setenv) whose first argument is the string literal "HOME"
// or "USERPROFILE" — an exact comparison, so near-miss names such as
// HOMEWORK cannot match, and comment or string-literal text cannot trigger
// a call that the AST does not contain. Names built dynamically (kv[0] in
// setHome's own loop) are intentionally out of scope: the invariant targets
// the literal-call smell this check catches.
func TestHomeEnvIsolationGuard(t *testing.T) {
	const helperFile = "homeenv_test.go" // where homeVars/setHome live

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") || name == helperFile {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// A sibling test file that does not parse is itself a problem.
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Setenv" || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			key, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if key == "HOME" || key == "USERPROFILE" {
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d sets %s directly; use setHome(t, dir) so HOME and USERPROFILE move together", pos.Filename, pos.Line, key)
			}
			return true
		})
	}
}
