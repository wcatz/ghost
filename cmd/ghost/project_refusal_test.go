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
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoRefusalQuotesAScopeOrProjectWithPercentQ: the structural half, over every
// non-test file in this package, and it is what stops the third parser and the next
// raw project id.
//
// The rule: a refusal that names a project argument, or that refuses a duplicate
// command-line scope, must not print any operand with a %q-family verb unless that
// operand is a memory.ProjectArg or memory.RefusalArg call — matched POSITIONALLY,
// verb to argument. A project id is the value this holds: `ghost import` accepts an
// artifact's project id verbatim, so a stored id can be token-shaped or oversized,
// and the refusal is printed to the terminal and lands in the log. A raw `%q` of one
// is a hole in #839's rule, not an exception to it.
//
// WHY THOSE TWO SHAPES, and the second is the widening this guard needed. The
// duplicate-scope formats are the refusals an earlier round fixed one parser at a
// time (`--project was given twice`, `--cwd was given twice`, `--out`, `--interval`).
// The project-naming formats are every OTHER refusal whose sentence names the
// argument, which is where `ghost project merge`'s self-merge refusal was hiding: its
// format never said "was given twice", so a filter on that phrase alone never saw it.
// Both are recognised by their format TEXT, which is the honest limit of a scan with
// no type checker: a sentence that names a project some other way is out of scope
// here, and a project-naming sentence is read as if every %q verb printed the project
// itself, so one that quotes another value beside it has to be disambiguated here
// rather than written past. That is why the renderer is a coverage promise as well as
// an enforced one.
//
// The positional part is the whole test, and the first version of this guard did not
// have it: it counted `%q` substrings and counted ProjectArg arguments and compared
// the two for equality, which two shapes walked straight through.
//
//   - A MIXED refusal. `--project was given twice (%q and %s)` with
//     (opts.Project, memory.ProjectArg("project", value)) counts one verb and one
//     ProjectArg, so the counts are equal — while the credential is in operand 0,
//     printed raw. A count cannot see WHICH operand; an index can.
//   - A FLAGGED or width-constrained %q. `%-10q`, `%+q`, `%.3q` contain no literal
//     "%q" substring, so a substring count found zero verbs and skipped the refusal
//     entirely.
//
// What it does not hold, stated so nobody reads more into it:
//
//   - Non-%q operands. `--since was given twice (%s and %s)` prints two
//     time.Durations, which are not caller-supplied text and would need a type
//     checker to be told apart from a string. A `%s` fed a raw project value is
//     therefore out of scope here; the table test above is what holds the values.
//   - Verb shapes this parser refuses to guess at. `quoteOperandIndexes` returns an
//     error for a `*` width (which takes its width from another argument) rather than
//     guessing, and the guard reports that as a failure to be fixed here — a scan that
//     cannot attribute a verb to an argument must say so, not pass silently.
//
// It also fails if it finds no refusal of either kind, so it cannot pass by scanning
// nothing. The two counts are kept apart for that reason: a filter widened to a
// second shape must still have found something of each, or the widening could have
// dropped the shape it was holding.
func TestNoRefusalQuotesAScopeOrProjectWithPercentQ(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	duplicateScanned, projectScanned := 0, 0
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
			format, ok := literalFormat(call.Args[0])
			if !ok {
				return true
			}
			duplicate := strings.Contains(format, "was given twice")
			namesProject := strings.Contains(strings.ToLower(format), "project")
			if !duplicate && !namesProject {
				return true
			}
			if duplicate {
				duplicateScanned++
			}
			if namesProject {
				projectScanned++
			}
			quotes, perr := quoteOperandIndexes(format)
			if perr != nil {
				t.Errorf("%s: %v, so this scan cannot tell which operand the verb prints; "+
					"extend quoteOperandIndexes here rather than letting the refusal past (%q)",
					fset.Position(call.Pos()), perr, format)
				return true
			}
			for _, idx := range quotes {
				if idx+1 >= len(call.Args) {
					t.Errorf("%s: the format has %d operands and %d arguments, so verb %d prints "+
						"nothing at all (%q)", fset.Position(call.Pos()), len(quotes), len(call.Args)-1,
						idx, format)
					continue
				}
				if !isMemoryProjectRenderer(call.Args[idx+1]) {
					t.Errorf("%s: a refusal prints operand %d with %%q and it does not go through "+
						"memory.ProjectArg or memory.RefusalArg, so a caller-supplied or stored "+
						"project value is echoed verbatim (%q)", fset.Position(call.Pos()), idx, format)
				}
			}
			return true
		})
	}
	if duplicateScanned == 0 {
		t.Error("no \"was given twice\" refusal was found to scan, so this guard is holding " +
			"nothing on the duplicate-scope side")
	}
	if projectScanned == 0 {
		t.Error("no project-naming refusal was found to scan, so this guard is holding " +
			"nothing on the project side")
	}
}

// literalFormat returns the string value of a format expression: a single string
// literal, or a `+` chain of them, which is how a long refusal is wrapped across
// lines. A concatenation is the same sentence as one literal, and skipping it would
// be a hole in this guard rather than a limit of it.
func literalFormat(expr ast.Expr) (string, bool) {
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
		left, ok := literalFormat(e.X)
		if !ok {
			return "", false
		}
		right, ok := literalFormat(e.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	}
	return "", false
}

// quoteOperandIndexes walks a printf format the way fmt does and returns, in order,
// the indexes of the ARGUMENTS that a %q-family verb prints.
//
// Three rules make it agree with fmt, and each one is a shape the previous version of
// this scan got wrong:
//
//   - `%%` is an escaped percent. It prints a literal % and consumes NO argument, so
//     the first %q in "100%% of %q" is operand 0 — while a plain substring count would
//     have read the "%q" inside "%%q" as a verb and shifted every later index by one.
//   - Flags, width and precision sit between the % and the verb, and the verb letter
//     alone decides whether this is a quote verb: `%-10q` and `%+.3q` print a quoted
//     string and nothing about them contains the substring "%q".
//   - `*` is refused rather than consumed. It takes its width from an ARGUMENT, which
//     makes verb-to-argument indexing a guess, and a guess in this scan is a refusal
//     that walks through.
func quoteOperandIndexes(format string) ([]int, error) {
	const flags = "+-# 0123456789."
	var quotes []int
	operand := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		if i+1 >= len(format) {
			return nil, errors.New("the format ends in a bare percent, which fmt renders as a " +
				"missing-verb marker rather than the text")
		}
		if format[i+1] == '%' {
			i++ // an escaped percent consumes no operand
			continue
		}
		j := i + 1
		for j < len(format) {
			c := format[j]
			if c == '*' {
				return nil, fmt.Errorf("the format uses a * width, which fmt fills from another "+
					"argument, so verb %d cannot be attributed to one operand", operand)
			}
			if strings.IndexByte(flags, c) < 0 {
				break
			}
			j++
		}
		if j >= len(format) {
			return nil, fmt.Errorf("the format ends mid-verb at %q", format[i:])
		}
		if format[j] == 'q' {
			quotes = append(quotes, operand)
		}
		operand++
		i = j
	}
	return quotes, nil
}

func isFmtErrorf(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Errorf" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt"
}

func isMemoryProjectRenderer(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "ProjectArg" && sel.Sel.Name != "RefusalArg") {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "memory"
}
