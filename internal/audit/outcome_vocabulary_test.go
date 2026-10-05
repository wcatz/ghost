package audit

// The outcome vocabulary, and the proof that every value in it reaches a bucket.
//
// The store-wide reader and the per-project reader are two implementations of one
// figure, and the store-wide one spells the buckets as string literals while this
// package spells them as typed constants. Nothing joined the two: the aggregate matched
// `case "superseded"` against the `superseded_in_session` the comparison writes, so
// every superseded verdict was counted in Scored and in no bucket — the health block
// printed "0 superseded in session" beside a precision whose denominator included them,
// and the two readers stopped agreeing.
//
// Spelling the vocabulary once cannot be the whole fix, because the next bucket added to
// either side drifts the same way. So it is held from both ends:
//
//   - TestTheOutcomeVocabularyIsOneListSpelledOnce parses compare.go and holds its
//     constants to AllOutcomes and to internal/memory's list in both directions and in
//     order. Go cannot enumerate constants, so a list is a convention; this is what makes
//     the convention checkable, and it fails if a bucket is spelled here rather than
//     named from the package that owns the column.
//   - TestTheStoreWideAggregateMapsEveryOutcomeTheComparerCanStore drives the store-wide
//     aggregate with each outcome and fails if one lands in no bucket — the arithmetic of
//     the failure being that the four buckets no longer sum to Scored, so an unmapped
//     outcome cannot hide behind a count that looks plausible.

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestTheOutcomeVocabularyIsOneListSpelledOnce holds the three lists of outcome values
// to each other: the constants in compare.go, this package's AllOutcomes, and
// memory.AllVerdictOutcomes — which is what the store-wide aggregate switches on.
//
// Compared in BOTH directions and by POSITION. A set comparison would pass a list that
// had dropped a bucket the const block still declares, and a bucket dropped from the
// aggregate's switch is precisely the failure this exists to prevent. The order is part
// of the agreement too: the list is ordered as the four buckets are named in the
// figures, so a reordering is a reader and a writer disagreeing about the figures and is
// worth failing on rather than sorting away.
//
// It holds the DERIVATION the comments claim as well: every constant in compare.go has to
// NAME a constant in internal/memory rather than carry a literal, and the value is
// resolved from memory's own block — so the test fails if someone re-spells a bucket here
// even when the re-spelling happens to be correct today.
func TestTheOutcomeVocabularyIsOneListSpelledOnce(t *testing.T) {
	declared := declaredOutcomeConstants(t)
	memorySide := memoryOutcomeConstants(t)

	if len(declared) != len(AllOutcomes) {
		t.Errorf("compare.go declares %d Outcome constant(s) and AllOutcomes holds %d: a bucket added to one and not the other is a verdict this build writes and no reader counts", len(declared), len(AllOutcomes))
	}
	for i, d := range declared {
		if i >= len(AllOutcomes) {
			break
		}
		if AllOutcomes[i] != Outcome(d.Value) {
			t.Errorf("outcome %d: compare.go's %s is %q and AllOutcomes[%d] is %q", i, d.Name, d.Value, i, AllOutcomes[i])
		}
	}

	if len(memorySide.Values) != len(memory.AllVerdictOutcomes) {
		t.Errorf("internal/memory declares %d verdict outcome constant(s) and lists %d: a bucket declared and not listed is one the aggregate cannot be held to", len(memorySide.Values), len(memory.AllVerdictOutcomes))
	}
	if !reflect.DeepEqual(memory.AllVerdictOutcomes, memorySide.Values) {
		t.Errorf("memory.AllVerdictOutcomes and the constants it names disagree:\n  list:     %v\n  declared: %v", memory.AllVerdictOutcomes, memorySide.Values)
	}
	// And the order the two packages took their buckets in, so the aggregate's switch and
	// this package's constants are held to the SAME list rather than to two lists that
	// happen to agree today.
	for i, d := range declared {
		if i >= len(memorySide.Values) {
			break
		}
		if d.Value != memorySide.Values[i] {
			t.Errorf("outcome %d: compare.go's %s carries memory.%s, whose value is %q, but internal/memory's %dth outcome is %q", i, d.Name, d.From, d.Value, i, memorySide.Values[i])
		}
	}
}

// outcomeDecl is one Outcome constant as compare.go declares it, with the value RESOLVED
// from the constant it names.
type outcomeDecl struct {
	Name string
	// From is the internal/memory constant the value names.
	From string
	// Value is what the store holds for it, resolved through From.
	Value string
}

// declaredOutcomeConstants reads every constant in compare.go typed `Outcome`, in
// declaration order, and resolves each value against internal/memory.
//
// Parsed rather than hand-listed, because a hand-list cannot detect the constant it was
// written next to — and a hand-list of four is exactly what a fifth bucket slips past.
//
// A value that is a literal, or that names anything other than a constant in
// internal/memory, FAILS here rather than being read: the point of the derivation is that
// the two packages cannot spell a bucket differently, and a test that quietly accepted a
// second spelling would be the second spelling.
func declaredOutcomeConstants(t *testing.T) []outcomeDecl {
	t.Helper()
	memorySide := memoryOutcomeConstants(t)

	var out []outcomeDecl
	eachConst(t, "compare.go", typedAs("Outcome"), func(spec constSpec) {
		decl := outcomeDecl{Name: spec.name}
		switch v := spec.value.(type) {
		case *ast.SelectorExpr:
			pkg, ok := v.X.(*ast.Ident)
			if !ok || pkg.Name != "memory" {
				t.Fatalf("compare.go: %s names %s, which is not a constant of the package that owns the column; the comparison's buckets are memory's vocabulary", spec.name, exprText(v))
			}
			decl.From = v.Sel.Name
			value, known := memorySide.byName[decl.From]
			if !known {
				t.Fatalf("compare.go: %s names memory.%s, which internal/memory does not declare", spec.name, decl.From)
			}
			decl.Value = value
		case *ast.BasicLit:
			t.Fatalf("compare.go: %s spells its value as %s rather than naming internal/memory's constant for it; a literal here is a second spelling of a column two packages read, and the one already spent a release", spec.name, exprText(v))
		default:
			t.Fatalf("compare.go: %s has the value %s, which this test cannot read; the vocabulary must be a constant of the package that owns the column", spec.name, exprText(v))
		}
		out = append(out, decl)
	})
	if len(out) == 0 {
		t.Fatal("no constant in compare.go is typed Outcome; the scan found nothing to compare and would pass on an empty match")
	}
	return out
}

// memoryOutcome is internal/memory's verdict outcome vocabulary as the declaring file
// spells it.
type memoryOutcome struct {
	// Values is every VerdictOutcome constant's value, in declaration order.
	Values []string
	byName map[string]string
}

// memoryOutcomeConstants reads the VerdictOutcome* constants out of
// internal/memory/retrieval_audit.go.
//
// It parses the declaring file rather than reading the constants through the package's
// own identifiers, because a list built out of the very constants it is checked against
// proves nothing: adding a bucket to the list and to the block agrees with itself, and
// the check passes. A bucket declared and NOT listed is the drift that matters — the
// aggregate cannot be held to a value nothing lists — so the block is the source and the
// list is what has to match it.
func memoryOutcomeConstants(t *testing.T) memoryOutcome {
	t.Helper()
	out := memoryOutcome{byName: map[string]string{}}
	eachConst(t, filepath.Join("..", "memory", "retrieval_audit.go"), named("VerdictOutcome"), func(spec constSpec) {
		lit, ok := spec.value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Fatalf("memory/retrieval_audit.go: %s is not spelled as a string literal; the outcome column holds whatever it is given, so its vocabulary is literals and nothing else", spec.name)
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("memory/retrieval_audit.go: %s holds the unquotable literal %s: %v", spec.name, lit.Value, err)
		}
		out.Values = append(out.Values, value)
		out.byName[spec.name] = value
	})
	if len(out.Values) == 0 {
		t.Fatal("no constant named VerdictOutcome* is declared in memory/retrieval_audit.go; the vocabulary this file checks has moved")
	}
	return out
}

// constSpec is one constant's name and its value EXPRESSION, unevaluated: a string
// literal stays a BasicLit and a reference to another package's constant stays a
// SelectorExpr, because which of the two it is the whole question.
type constSpec struct {
	name  string
	value ast.Expr
}

// constSelector says which constants a scan wants, given a spec and the type written on
// it ("" for an untyped constant).
type constSelector func(constSpec, string) bool

// typedAs selects constants declared with this type. It is the selector for a named
// vocabulary: a bucket is whatever compare.go declares as an Outcome, whatever it is
// called, and the comparison's other const block — the token thresholds, untyped ints —
// is not one.
func typedAs(typeName string) constSelector {
	return func(_ constSpec, got string) bool { return got == typeName }
}

// named selects constants whose name carries this prefix. It is the selector for
// internal/memory's block, whose outcome constants are UNTYPED — the column holds text
// and the type is a reader's business — so the prefix is the whole selection rule, which
// is why the block those constants live in is the vocabulary's only home.
func named(prefix string) constSelector {
	return func(spec constSpec, _ string) bool { return strings.HasPrefix(spec.name, prefix) }
}

// eachConst parses one file in this package's tree and calls visit for every constant the
// selector accepts, in declaration order. It fails the test if a constant's names and
// values do not pair up one to one, because a scan that silently skipped one would report
// a shorter vocabulary and agree with it.
func eachConst(t *testing.T, path string, keep constSelector, visit func(constSpec)) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, s := range gen.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if len(vs.Values) != len(vs.Names) {
				t.Fatalf("%s: a constant is declared with %d name(s) and %d value(s); this scan reads one value per name", path, len(vs.Names), len(vs.Values))
			}
			typeName := ""
			if typ, ok := vs.Type.(*ast.Ident); ok {
				typeName = typ.Name
			}
			for i, name := range vs.Names {
				spec := constSpec{name: name.Name, value: vs.Values[i]}
				if !keep(spec, typeName) {
					continue
				}
				visit(spec)
			}
		}
	}
}

// exprText renders an expression back to source, for a failure message that has to name
// what it found.
func exprText(e ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), e); err != nil {
		return fmt.Sprintf("(%T)", e)
	}
	return buf.String()
}

// TestTheStoreWideAggregateMapsEveryOutcomeTheComparerCanStore: for every outcome the
// comparer can produce, the store-wide aggregate counts it in EXACTLY ONE bucket, and
// that bucket is the one the figures name.
//
// The aggregate runs only when a health check asks for the whole store, and it is the
// reader whose counts feed the precision denominator. A verdict it counts in Scored and
// in no bucket is not a rounding error: the four buckets stop summing to Scored,
// `ghost_health` prints "0 superseded in session" over a store holding superseded
// verdicts, and the store-wide figures stop equalling the sum of the per-project
// reports — the equality TestBuildStoreReportIsTheSumOfThePerProjectReports holds.
//
// Each outcome gets its own store, so none is asserted as the arithmetic of the others
// and the four buckets are not compared against each other by accident.
func TestTheStoreWideAggregateMapsEveryOutcomeTheComparerCanStore(t *testing.T) {
	for _, outcome := range AllOutcomes {
		t.Run(string(outcome), func(t *testing.T) {
			store, projectID, _ := reportStore(t)
			call := recordCall(t, store, projectID, "search", "USEDID")
			fileVerdict(t, store, memory.RetrievalAuditRow{
				ProjectID: projectID, SessionID: "s1", Source: "search", MemoryID: "USEDID",
				Outcome: string(outcome), RecordRowID: call,
			})

			totals, err := store.RetrievalSourceTotals(context.Background(), time.Time{})
			if err != nil {
				t.Fatalf("RetrievalSourceTotals: %v", err)
			}
			var search memory.RetrievalSourceTotals
			for _, s := range totals {
				if s.Source == "search" {
					search = s
				}
			}
			buckets := outcomeBuckets(search)
			in, mapped := buckets[string(outcome)]
			if !mapped {
				t.Fatalf("this test has no bucket for the outcome %q, so it cannot check it either; add one — a bucket nothing names is a bucket nothing counts", outcome)
			}
			if search.Scored != 1 {
				t.Errorf("Scored = %d, want 1: the verdict names a call this report counts", search.Scored)
			}
			if in != 1 {
				t.Errorf("a %q verdict was counted %d time(s) in its bucket and %d in Scored, with the four buckets at %v", outcome, in, search.Scored, buckets)
			}
			sum := 0
			for _, n := range buckets {
				sum += n
			}
			if sum != search.Scored {
				t.Errorf("the four buckets sum to %d against a Scored of %d: a verdict this build can store is in the denominator and in no bucket, which is the honest reading of a STRANGER store's row and a wrong one of our own", sum, search.Scored)
			}
			// And the ids, because a bucket counted and an id unnamed are two halves of
			// one figure and the second is what an operator acts on. Only the
			// contradicted verdict names one; every other bucket must name none, or the
			// listing is reading a verdict the count does not describe.
			wantIDs := []string(nil)
			if outcome == OutcomeContradicted {
				wantIDs = []string{"USEDID"}
			}
			if !reflect.DeepEqual(search.ContradictedIDs, wantIDs) {
				t.Errorf("a %q verdict named the ids %v, want %v", outcome, search.ContradictedIDs, wantIDs)
			}
		})
	}
}

// outcomeBuckets reads the four outcome buckets of one source's figures, keyed by the
// outcome VALUE rather than by the field name.
//
// A test that looked them up by field name would follow the aggregate's own naming, which
// is the thing that drifted: the field is Superseded and the value it must match is
// superseded_in_session, and nothing about those two strings tells a reader they are the
// same thing. The key is the stored value, so a bucket this test cannot name fails the
// test rather than passing it.
func outcomeBuckets(t memory.RetrievalSourceTotals) map[string]int {
	return map[string]int{
		memory.VerdictOutcomeUsed:         t.Used,
		memory.VerdictOutcomeIgnored:      t.Ignored,
		memory.VerdictOutcomeSuperseded:   t.Superseded,
		memory.VerdictOutcomeContradicted: t.Contradicted,
	}
}
