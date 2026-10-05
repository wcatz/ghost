package main

// `ghost context --audit` and `ghost prune` each refuse a --project (or --cwd) given
// twice, and each refusal QUOTES the reader's own argument so they can see the typo
// they made. docs/invariants.md's #839 rule is explicit about the shape that takes:
// memory.ProjectArg, which returns exactly `%q` for a value the secret guard does not
// recognise — so an ordinary refusal is byte-identical — and a placeholder naming the
// argument and the credential's format, and no part of the value, for one it does.
//
// One of the two parsers had it and the other did not, which is what the two tests
// here are for. The first is behavioural and holds every duplicate-scope refusal in the
// package to the same promise; the second is structural and is why a THIRD parser
// cannot reintroduce the hole without failing a build. The invariant called a package
// that interpolates the operand with `%q` "a hole in this rule, not an exception to
// it", and an exception is exactly what one parser out of two had become.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDuplicateScopeRefusalsWithholdACredential: one assertion over every parser that
// can refuse a duplicate scope, because "one parser at a time" is how the invariant
// came to be half-true.
//
// A credential in --project is not hypothetical here: it is what an agent produces when
// it pastes a config value into the wrong flag, and the refusal is printed to the
// terminal and lands in the log. The withheld half has to be there too — the reader is
// told WHICH argument was held back, or the sentence stops being actionable — so that
// is asserted and not inferred.
func TestDuplicateScopeRefusalsWithholdACredential(t *testing.T) {
	const key = "sk-ant-api03-Zz09XxYyWwVvUuTtSsRrQqPpOoNnMmLlKkJj01"
	for _, tc := range []struct {
		name  string
		field string
		parse func([]string) error
		args  []string
	}{
		{
			name:  "ghost context --audit --project",
			field: "project",
			parse: func(args []string) error { _, err := parseContextAuditArgs(args); return err },
			args:  []string{"--audit", "--project=" + key, "--project=ghost"},
		},
		{
			name:  "ghost context --audit --cwd",
			field: "cwd",
			parse: func(args []string) error { _, err := parseContextAuditArgs(args); return err },
			args:  []string{"--audit", "--cwd=" + key, "--cwd=/tmp"},
		},
		{
			// prune has no --cwd, and its duplicate guard is `opts.Project != ""`
			// rather than an occurrence count, so it is a separate case rather than
			// a flag column on the audit parser's.
			name:  "ghost prune --project",
			field: "project",
			parse: func(args []string) error { _, err := parsePruneArgs(args); return err },
			args:  []string{"--project=" + key, "--project=ghost"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.parse(tc.args)
			if err == nil {
				t.Fatalf("%v was accepted with the same scope twice", tc.args)
			}
			msg := err.Error()
			if strings.Contains(msg, key) {
				t.Errorf("the refusal quotes the credential the reader passed:\n%s", msg)
			}
			if !strings.Contains(msg, "twice") {
				t.Errorf("the refusal no longer names the duplicate:\n%s", msg)
			}
			if !strings.Contains(msg, "<"+tc.field+" withheld") {
				t.Errorf("the refusal does not say which argument was withheld:\n%s", msg)
			}
		})
	}

	// And the ordinary case, per parser, as an EXACT sentence. ProjectArg returns %q
	// for a value it does not recognise, so a refusal that routes through it must read
	// exactly as it did before; a test that only checked for "twice" would pass on a
	// sentence that had quietly stopped quoting the reader's own value, which is how
	// an agent finds its typo in the first place.
	for _, tc := range []struct {
		name  string
		parse func([]string) error
		args  []string
		want  string
	}{
		{
			name:  "ghost context --audit --project",
			parse: func(args []string) error { _, err := parseContextAuditArgs(args); return err },
			args:  []string{"--audit", "--project", "ghost", "--project", "other"},
			want:  `--project was given twice ("ghost" and "other")`,
		},
		{
			name:  "ghost prune --project",
			parse: func(args []string) error { _, err := parsePruneArgs(args); return err },
			args:  []string{"--project", "ghost", "--project", "other"},
			want:  `--project was given twice ("ghost" and "other")`,
		},
	} {
		t.Run("an ordinary value is still quoted verbatim: "+tc.name, func(t *testing.T) {
			err := tc.parse(tc.args)
			if err == nil {
				t.Fatalf("%v was accepted with the same scope twice", tc.args)
			}
			if err.Error() != tc.want {
				t.Errorf("refusal = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// TestNoDuplicateFlagRefusalQuotesWithPercentQ: the structural half, over every
// non-test file in this package, and it is what stops the third parser.
//
// The rule it enforces is narrow on purpose — a "was given twice" refusal that prints
// any `%q` operand at all must feed every one of them through memory.ProjectArg — and
// the narrowness is stated so a future reader does not mistake it for more than it is:
//
//   - It holds the SHAPE the invariant names. A refusal written as
//     fmt.Errorf("...given twice (%q and %q)", opts.Project, value) is a hole by
//     definition, whatever the surrounding code does, and this is the only check that
//     can fail on a parser that has no test case of its own.
//   - It does not try to judge non-%q arguments. `--since was given twice (%s and
//     %s)` prints two time.Durations, which are not caller-supplied text and have
//     nothing for the guard to recognise, and telling those apart statically would
//     need a type checker to be worth anything.
//   - The VALUES are held by the test above, not by this one. A scan cannot tell
//     whether an operand happens to hold a credential; only running the refusal can.
//   - A refusal that routes every value through ProjectArg prints `%s` and has no
//     `%q` to be wrong about, so it passes trivially — which is the state the fix
//     drives every parser to, and the reason the scan exists for the NEXT parser
//     rather than for these two.
func TestNoDuplicateFlagRefusalQuotesWithPercentQ(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	scanned := 0
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
			if !ok || !isFmtErrorf(call) || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			format, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(format, "was given twice") {
				return true
			}
			scanned++
			verbs := strings.Count(format, "%q")
			rendered := 0
			for _, arg := range call.Args[1:] {
				if isMemoryProjectArg(arg) {
					rendered++
				}
			}
			// Only a refusal that actually prints a bare operand is in scope. One
			// that routes every value through ProjectArg prints %s and has no %q
			// to be wrong about, which is the state this drives the package to.
			if verbs > 0 && verbs != rendered {
				t.Errorf("%s: a \"was given twice\" refusal prints %d %%q operand(s) but only "+
					"%d of them go through memory.ProjectArg, so a credential in the flag is "+
					"echoed verbatim (%s)", fset.Position(call.Pos()), verbs, rendered, lit.Value)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Error("no \"was given twice\" refusal was found to scan, so this guard is holding nothing")
	}
}

func isFmtErrorf(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Errorf" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt"
}

func isMemoryProjectArg(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ProjectArg" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "memory"
}
