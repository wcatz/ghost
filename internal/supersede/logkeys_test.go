package supersede

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// capturedLog is a slog.Handler that keeps every record a pass emits, so a
// test can read the KEYS a line carries rather than re-deriving them from the
// call site.
//
// It exists because a slog level call takes alternating key/value arguments
// and the pairing happens inside slog, from the ARGUMENT LIST: a call that
// writes `"link", a, b, "scan", c, d` compiles, vets, and runs, and slog pairs
// it as link=a, b=scan, c=d — so `b` is read as a key while the author read it
// as a value. Nothing downstream of the handler can tell, because by then the
// record is well-formed.
//
// The one thing a handler CAN see is the pairing itself, which is why the
// assertions below are on keys.
type capturedLog struct {
	mu      sync.Mutex
	records []capturedRecord
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (c *capturedLog) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (c *capturedLog) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	return nil
}

func (c *capturedLog) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *capturedLog) WithGroup(_ string) slog.Handler      { return c }

func (c *capturedLog) logger() *slog.Logger { return slog.New(c) }

// find returns every record whose message contains substr.
func (c *capturedLog) find(substr string) []capturedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedRecord
	for _, r := range c.records {
		if strings.Contains(r.msg, substr) {
			out = append(out, r)
		}
	}
	return out
}

// Every key the pass logs is a lowercase label — "newer", "older", "reason",
// "error" — and that is the whole rule assertLogKeysAreLabels enforces. It is
// one rule because it covers both ways a key/value list can fail to line up:
//
//   - A memory id in the KEY slot is a value that landed there, which is what
//     #804 was: slog paired `"link", a, b, "scan", c, d` as link=a, b=scan, c=d.
//     go vet's slog check cannot see that, because the argument count is even
//     and every argument is a string — the pairing the author meant is not
//     recoverable from the types.
//   - slog's own !BADKEY marker is an argument list that does not pair at all —
//     an odd number of trailing args. vet does reject that one ("missing a final
//     value"), so a handler never sees it in this tree; the rule covers it so
//     the assertion does not depend on that staying true.
func assertLogKeysAreLabels(t *testing.T, log *capturedLog) {
	t.Helper()
	for _, r := range log.records {
		for key := range r.attrs {
			if !isLogKeyLabel(key) {
				t.Errorf("log line %q carries the key %q, which is not a lowercase label: a memory id or a value here means this call's arguments do not pair the way the author read them (slog renders an unpaired trailing argument as %q). attrs=%v",
					r.msg, key, badKeyMarker, r.attrs)
			}
		}
	}
}

// badKeyMarker is the key log/slog gives an argument it could not pair, per its
// documented handling of a key that is not a string. Named here so a failure
// message can say what the alternative to a label looks like.
const badKeyMarker = "!BADKEY"

// isLogKeyLabel reports whether s is a key a person wrote: a lowercase word,
// optionally with digits and underscores, and never a bare number.
func isLogKeyLabel(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		case c == '_' && i > 0:
		default:
			return false
		}
	}
	return true
}

// TestRunNamesTheKeyOfEveryValueItLogs is #804.
//
// The line it pins is the one that reports a scan proposing the REVERSE of a
// live supersedes link: the only log line in the package whose subject is two
// disagreeing orientations of one pair, so the only one where naming the source
// of each id is the whole point. It passed `"link", l.SourceID, l.TargetID,
// "scan", cand.NewerID, cand.OlderID` — six values, four of them with a name and
// two without, and slog paired l.TargetID as a KEY, so a dry run printed
//
//	link=02EA044F… 3092A7BE…=scan 3092A7BE…=74CE9D10…
//
// which reads as three unrelated facts and names neither endpoint.
func TestRunNamesTheKeyOfEveryValueItLogs(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// The #641 damage already in the graph: a live link stale→fix, while the
	// scan reads the timestamps and proposes the pair the other way round. The
	// pass refuses that orientation and logs why, which is the record under
	// test.
	stale := add(t, store, db, "bug: the relay stalls on every consumer rebalance", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	fix := add(t, store, db, "the relay rebalance stall is fixed: pin the consumer", []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")
	if err := store.CreateLink(ctx, stale, fix, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, stale, fix)

	log := &capturedLog{}
	res, _, err := Run(ctx, store, &supersedesEverything{}, "p", 0.9, true, log.logger())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OppositeLive != 1 {
		t.Fatalf("Result.OppositeLive = %d, want 1: the fixture must reach the reverse-of-a-live-link line, or this test asserts nothing", res.OppositeLive)
	}

	records := log.find("reverse of a live supersedes link")
	if len(records) != 1 {
		t.Fatalf("emitted %d record(s) about a reversed live link, want exactly 1: %v", len(records), log.records)
	}
	attrs := records[0].attrs
	// Each id under the key that says WHICH source asserted it. The link's own
	// direction and the scan's proposal disagree, so "newer"/"older" alone would
	// leave a reader unable to tell which id came from where.
	want := map[string]string{
		"link_source": stale,
		"link_target": fix,
		"scan_newer":  fix,
		"scan_older":  stale,
	}
	if len(attrs) != len(want) {
		t.Errorf("the line carries %d attribute(s) %v, want exactly %d %v: every value needs a key of its own, and no value may become one",
			len(attrs), attrs, len(want), want)
	}
	for key, wantValue := range want {
		if got, ok := attrs[key]; !ok {
			t.Errorf("no %q attribute; the line's attrs are %v", key, attrs)
		} else if got != wantValue {
			t.Errorf("%s = %q, want %q", key, got, wantValue)
		}
	}
	// The general form of the same rule, so the line is also covered by the
	// shape assertion its siblings get.
	assertLogKeysAreLabels(t, log)
}

// assertDrivenLoggers requires every function in this package that actually
// LOGS to be covered by one of the cases, so the guard's scope cannot quietly
// outgrow the code it guards.
//
// It walks the package with go/parser rather than transcribing the list, which is
// the same technique TestEveryHarnessCommandCallSiteChecksItsError uses in
// internal/ai and TestEveryVerifiedAtMentionIsClassified uses in
// internal/memory: the point of a guard like this is that it fails when the code
// moves, and a transcribed list only fails when someone remembers to edit it —
// which is the discipline the guard exists to remove.
//
// The set is the functions that make a slog LEVEL CALL, not the functions that
// merely take a *slog.Logger, and the receiver is resolved to a logger the file
// actually declares. Both halves of that were wrong in the first version, in the
// direction that looks like a pass: scanning declarations reports
// RelationClassifier.SetLogger, a setter that logs nothing, and matching the
// selector name alone counts `err.Error()` as a log call, which credits the
// Withdraw case with a line it never reaches. A guard that over-counts is worse
// than none, because it reports coverage it does not have.
//
// Finding ZERO logging functions is itself a failure: a scan that recognised
// nothing is indistinguishable from a package that is clean.
func assertDrivenLoggers(t *testing.T, cases []struct {
	name   string
	covers []string
	run    func(t *testing.T, log *capturedLog)
	wants  []string
}) {
	t.Helper()
	covered := map[string]string{} // logging function -> the case that drives it
	for _, tc := range cases {
		for _, fn := range tc.covers {
			covered[fn] = tc.name
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// The enclosing FuncDecl of every level call. A method or closure that
		// logs is attributed to the function whose name a caller already knows,
		// so the table is keyed on entry points rather than on every small
		// function that happens to emit a line.
		loggers := declaredLoggers(file)
		seenInFile := 0
		var stack []*ast.FuncDecl
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				stack = append(stack, node)
			case *ast.CallExpr:
				if !isSlogLevelCall(node, loggers) || len(stack) == 0 {
					return true
				}
				found++
				seenInFile++
				fn := stack[len(stack)-1].Name.Name
				if _, ok := covered[fn]; !ok {
					t.Errorf("%s:%d: %s logs, but no case covers it: its log lines are outside assertLogKeysAreLabels",
						name, fset.Position(node.Pos()).Line, fn)
				}
			}
			return true
		})
		// Per FILE, not just per package: a file that declares a logger and is
		// then seen making no level call is a gap in the scan, not a file that
		// happens not to log. With only a package-wide count, that loss is
		// silent — the other files keep `found` above zero, the file's cases stop
		// being load-bearing, and deleting them from the table would still pass.
		if len(loggers) > 0 && seenInFile == 0 {
			t.Errorf("%s declares a *slog.Logger (%v) but the scan found no level call in it: the scan is not reading this file's logging, so its case covers nothing",
				name, sortedKeys(loggers))
		}
	}
	if found == 0 {
		t.Fatal("no slog level call found in the package: this test has stopped seeing the code it guards")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// slogLevelNames are the methods and functions that emit a record. Anything else
// on a logger (With, Handler, Enabled) emits nothing.
var slogLevelNames = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true,
}

// isSlogLevelCall reports whether call is a level call on this package's logger,
// or on the slog package itself.
//
// The receiver is checked against the set of names the FILE declares a
// *slog.Logger under, because "the selector's name is a level method" is not
// enough on its own: `err.Error()` satisfies it, `slog` shares its level names
// with error and with a dozen other types, and a false positive here credits a
// case with covering a function that logs nothing — which is the exact failure
// the scan exists to prevent, in the direction that looks like a pass.
func isSlogLevelCall(call *ast.CallExpr, loggers map[string]bool) bool {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.SelectorExpr:
		if !slogLevelNames[fun.Sel.Name] {
			return false
		}
		if pkg, isIdent := fun.X.(*ast.Ident); isIdent && pkg.Name == "slog" {
			return true
		}
		// A method on a logger the file declares: a parameter, a struct field
		// (`h.logger`), or a local. The receiver's name is resolved against a
		// declaration in the same file, so `err.Error()` cannot pass, and a
		// logger reached through a field or a local can.
		return loggers[baseIdentName(fun.X)]
	case *ast.Ident:
		return slogLevelNames[fun.Name] // slog.Info(...) at package level
	}
	return false
}

// declaredLoggers returns the names a file binds a *slog.Logger to: parameters,
// results, struct fields, var declarations and locals. A name bound to a logger
// ANYWHERE in the file counts.
//
// The struct-field arm is load-bearing, not decoration. `RelationClassifier`
// holds its logger in a field, so without that arm declaredLoggers(relation.go)
// sees only SetLogger's parameter — named `l` — while every one of that file's
// five log calls is `h.logger.Warn(...)`, whose receiver name is `logger`. The
// scan then reports no logging function in the file at all, which is a silent
// loss rather than a visible one, so the per-file check in assertDrivenLoggers
// is the second half of the same fix: a file that declares a logger and is seen
// making no level call is reported, and a scan that recognises nothing in a file
// it has a logger in cannot hide behind the files that work.
func declaredLoggers(file *ast.File) map[string]bool {
	names := map[string]bool{}
	bind := func(expr ast.Expr, ident *ast.Ident) {
		if ident != nil && isSlogLoggerType(expr) {
			names[ident.Name] = true
		}
	}
	bindFieldList := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, field := range list.List {
			for _, ident := range field.Names {
				bind(field.Type, ident)
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			bindFieldList(node.Recv)
			bindFieldList(node.Type.Params)
			bindFieldList(node.Type.Results)
		case *ast.StructType:
			// A logger held as a field: RelationClassifier's `logger`, which is
			// how this package's one long-lived component gets one.
			bindFieldList(node.Fields)
		case *ast.ValueSpec:
			for i, ident := range node.Names {
				if i < len(node.Values) {
					bind(node.Values[i], ident)
				} else {
					bind(node.Type, ident)
				}
			}
		case *ast.AssignStmt:
			// A local logger: `logger := slog.New(...)`. The RHS is not checked
			// for *slog.Logger — that needs a type checker — so this is admitted
			// only for an assignment from the slog package.
			if len(node.Lhs) != len(node.Rhs) {
				return true
			}
			for i, lhs := range node.Lhs {
				ident, isIdent := lhs.(*ast.Ident)
				if !isIdent {
					continue
				}
				if rhsIsSlogNew(node.Rhs[i]) {
					names[ident.Name] = true
				}
			}
		}
		return true
	})
	return names
}

// rhsIsSlogNew reports whether expr is a call to slog.New, which is the only way
// this package ever obtains a logger.
func rhsIsSlogNew(expr ast.Expr) bool {
	call, isCall := ast.Unparen(expr).(*ast.CallExpr)
	if !isCall {
		return false
	}
	sel, isSel := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !isSel {
		return false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	return isIdent && pkg.Name == "slog" && sel.Sel.Name == "New"
}

// isSlogLoggerType reports whether expr names a *slog.Logger (or a slog.Logger).
func isSlogLoggerType(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return isSlogLoggerType(e.X)
	case *ast.SelectorExpr:
		pkg, isIdent := e.X.(*ast.Ident)
		return isIdent && pkg.Name == "slog" && e.Sel.Name == "Logger"
	}
	return false
}

// baseIdentName returns the receiver's own name, so `h.logger` and `logger` both
// yield "logger". A computed receiver with no identifier behind it returns "".
func baseIdentName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.ParenExpr:
		return baseIdentName(e.X)
	}
	return ""
}

// TestPackagePassesLogOnlyLabelKeys is the package-wide half of #804: the same
// shape assertion, driven through every entry point in this package that logs —
// the creation pass, the repair pass, the operator's named withdrawal, and the
// harness classifier's own diagnostics — so a future mispaired call on any of
// them fails here rather than in a log file nobody reads.
//
// Its fixtures are built to hit lines with DIFFERENT key vocabularies on
// purpose: a refusal naming a link's direction, a second refusal, a veto, an
// unclassifiable verdict, a cyclic pair ("a"/"b", not "newer"/"older"), a
// withdrawal ("source"/"target"), and a retry ("unjudged"/"settled"). One line's
// keys being right says nothing about the next one's, which is the whole reason
// #804 survived in a tree where every other line was fine.
//
// WHAT IT DOES AND DOES NOT CATCH, since the boundary is the useful part: a
// mispairing is caught when the value that lands in the KEY slot is a memory id,
// which is every value in the lines above except the classifier's own (a retry
// delay, a message, an error). A mispairing among plain words is NOT caught —
// `"pair", "delay", "why", "the call failed"` is all labels and all keys, and
// nothing in a Record distinguishes it from what was meant. That is why the
// per-line key/value assertions live in the tests beside each pass's own
// behaviour, and why the audit this test came from was a type-checked read of
// every call rather than a runtime signal.
//
// The table is the coverage claim made checkable: a function in this package
// that logs, and no case covers it, is named by the check below. That set is
// read out of the package's own source, so it grows when a fifth logging entry
// point is added rather than when someone remembers this file.
func TestPackagePassesLogOnlyLabelKeys(t *testing.T) {
	cases := []struct {
		name   string
		covers []string
		run    func(t *testing.T, log *capturedLog)
		wants  []string
	}{
		// `Run` is a one-line wrapper since #799 moved the pass into RunWith
		// (the per-call decisions became an Options value), so the function that
		// logs is RunWith and that is the name the scan reports. The fixture
		// still calls Run, because Run is the entry point a caller reads.
		{name: "Run", covers: []string{"RunWith"}, run: runPassesLogKeys, wants: []string{
			"reverse of a live supersedes link", // the #804 line
			"refusing a pair the graph claims in both directions",
			"vetoed pair whose older note states a rule",
			"skipping pair with an unclassifiable verdict",
			"supersede classified",
		}},
		{name: "Reassess", covers: []string{"Reassess"}, run: reassessPassesLogKeys, wants: []string{
			"a cyclic pair whose two rows share both timestamps",
			"the older note states a rule this edge does not retire",
			"supersede reassess", // the summary line — the longest in the package
		}},
		{name: "Withdraw", covers: []string{"Withdraw"}, run: withdrawPassesLogKeys, wants: []string{
			"supersede withdrew a named edge",
		}},
		// The classifier's lines live in its retry wrapper and its per-chunk
		// helper, not in the public Classify/ClassifyBatch a caller names — the
		// scan reads that off the source, which is why these two are spelled the
		// way they are rather than the way the case is named.
		{name: "RelationClassifier", covers: []string{"ClassifyBatch", "call", "classifyChunk"}, run: classifierPassesLogKeys, wants: []string{
			"classify call failed; retrying once",
			"batch reply unparseable",
		}},
	}

	// Every pass in the package that logs must have a case, or this test is
	// quietly narrower than it says. The list is DERIVED from the package's own
	// source rather than transcribed, so a fifth logging entry point added later
	// fails here instead of logging into an unguarded void. (The first version of
	// this check transcribed the four names and a reviewer was right that it
	// could only ever catch a case being removed, not a logger being added.)
	assertDrivenLoggers(t, cases)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &capturedLog{}
			tc.run(t, log)

			// The shape assertion, over every line the fixture reached.
			assertLogKeysAreLabels(t, log)

			// And a floor on WHICH lines it reached: a fixture that quietly
			// stopped producing log lines would pass the shape assertion while
			// testing nothing, which is how a guard like this rots.
			for _, want := range tc.wants {
				if len(log.find(want)) == 0 {
					t.Errorf("no record matching %q; the fixture must still reach the lines it is meant to cover. Records seen:\n%s", want, dumpRecords(log))
				}
			}
		})
	}
}

// runPassesLogKeys drives the creation pass over a corpus built to reach five
// lines with different key vocabularies.
func runPassesLogKeys(t *testing.T, log *capturedLog) {
	t.Helper()
	store, db := seed(t)
	ctx := context.Background()

	// Pair 1: a live link whose direction the scan contradicts → the
	// reverse-of-a-live-link line.
	stale := add(t, store, db, "bug: the relay stalls on every consumer rebalance", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	fix := add(t, store, db, "the relay rebalance stall is fixed: pin the consumer", []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")
	if err := store.CreateLink(ctx, stale, fix, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, stale, fix)

	// Pair 2: both directions live → the bidirectional-refusal line.
	biNewer := add(t, store, db, "kubernetes now on 1.31", []float32{1, 0.02, 0}, "2026-09-02 00:00:00")
	biOlder := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0.98, 0.03, 0}, "2026-01-02 00:00:00")
	for _, dir := range [][2]string{{biNewer, biOlder}, {biOlder, biNewer}} {
		if err := store.CreateLink(ctx, dir[0], dir[1], "supersedes", 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}
	backdateLink(t, db, biNewer, biOlder)

	// Pair 3: an older note that states a rule the newer one never retires, so
	// the deterministic veto fires and the pair never reaches a classifier.
	vetoOlder := add(t, store, db, "never deploy on a Friday: the release train does not run", []float32{0.96, 0.06, 0}, "2026-02-01 00:00:00")
	vetoNewer := add(t, store, db, "the friday release train now ships from the automated pipeline", []float32{0.95, 0.07, 0}, "2026-09-03 00:00:00")

	// Pair 4: an ordinary fresh pair the classifier cannot parse a verdict for
	// → the unclassifiable line, and then the classified line for pair 1's
	// reclassification.
	blankNewer := add(t, store, db, "grafana listens on port 8080", []float32{0, 1, 0.01}, "2026-09-04 00:00:00")
	blankOlder := add(t, store, db, "grafana listens on port 80", []float32{0, 0.99, 0.02}, "2026-03-04 00:00:00")

	cls := &mockClassifier{verdict: func(newer, older string) Relation {
		if strings.Contains(newer, "grafana listens on port 8080") {
			return "" // the unparseable verdict a single bad phrasing produces
		}
		return RelationSupersedes
	}}
	if _, _, err := Run(ctx, store, cls, "p", 0.9, true, log.logger()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// A line that names a pair is only useful if it names the RIGHT pair, and
	// the shape assertion cannot tell a correctly-keyed id from a swapped one.
	// The unclassifiable pair and the vetoed pair are the two whose ids the
	// fixture knows, so read both back — each guarded by its own count, so a
	// line that stopped firing is reported by the case's own coverage loop.
	if got := log.find("skipping pair with an unclassifiable verdict"); len(got) == 1 {
		attrs := got[0].attrs
		if attrs["newer"] != blankNewer || attrs["older"] != blankOlder {
			t.Errorf("the unclassifiable line names (%s, %s), want the grafana pair (%s, %s): the fixture exists so the line's keys carry the pair that produced them",
				attrs["newer"], attrs["older"], blankNewer, blankOlder)
		}
	}
	if got := log.find("vetoed pair whose older note states a rule"); len(got) == 1 {
		attrs := got[0].attrs
		if attrs["older"] != vetoOlder || attrs["newer"] != vetoNewer {
			t.Errorf("the veto line names (%s, %s), want the friday-train pair (%s, %s)",
				attrs["newer"], attrs["older"], vetoNewer, vetoOlder)
		}
		if attrs["reason"] == "" {
			t.Error("the veto line carries no reason; the whole point of the line is naming the imperative that fired")
		}
	}
}

// reassessPassesLogKeys drives the repair pass (`ghost resolve --reassess`).
// It logs the same kinds of facts as Run on the same ids, so a mispairing here
// would be invisible to a guard that only drove the creation pass.
func reassessPassesLogKeys(t *testing.T, log *capturedLog) {
	t.Helper()
	store, db := seed(t)
	ctx := context.Background()

	// A cycle whose two rows share both timestamps, so the repair pass reports
	// it as unframeable rather than asking about it. This is the only line in
	// Reassess whose keys are "a"/"b" rather than "newer"/"older", which is
	// exactly the kind of local vocabulary a shared guard has to survive.
	const bulkStamp = "2026-09-20 09:26:05"
	cycA := add(t, store, db, "prod db timeout is 30s", []float32{1, 0, 0}, bulkStamp)
	cycB := add(t, store, db, "prod db timeout is 5s", []float32{0.99, 0.01, 0}, bulkStamp)
	for _, dir := range [][2]string{{cycA, cycB}, {cycB, cycA}} {
		if err := store.CreateLink(ctx, dir[0], dir[1], "supersedes", 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}

	// A live edge whose older note states a rule the newer one never retires, so
	// the same deterministic veto the creation pass applies settles this one too.
	vetoNewer := add(t, store, db, "the nightly batch writes to the audit table", []float32{0, 1, 0}, "2026-09-01 00:00:00")
	vetoOlder := add(t, store, db, "never truncate the audit table; it is the only record we keep", []float32{0, 0.99, 0.01}, "2026-01-01 00:00:00")
	if err := store.CreateLink(ctx, vetoNewer, vetoOlder, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, vetoNewer, vetoOlder)

	cls := NewRelationClassifier(&fakeProvider{resp: resolveAnswerNEITHER})
	cls.SetRetryDelay(0)
	if _, _, err := Reassess(ctx, store, cls, "p", true, log.logger()); err != nil {
		t.Fatalf("Reassess: %v", err)
	}
}

// withdrawPassesLogKeys drives the named withdrawal
// (`ghost_link_withdraw` / `ghost resolve --withdraw`). Its line carries three
// ids and a running count, and its keys are "source"/"target" — a fourth
// vocabulary in a package that already had three.
func withdrawPassesLogKeys(t *testing.T, log *capturedLog) {
	t.Helper()
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "the relay rebalance stall is fixed: pin the consumer", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "bug: the relay stalls on every consumer rebalance", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: newer, Target: older}}, true, log.logger()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
}

// classifierPassesLogKeys drives the harness classifier's own lines, the ones in
// the package made by a type that is not one of the three passes
// (relation.go's RelationClassifier, which owns its logger because an
// unparseable reply and a failed call are its own business). Its keys are a
// fifth vocabulary — "unjudged", "settled", "batch", "reply" — so it is the one
// entry that would have gone uncovered had the table stopped at the passes.
func classifierPassesLogKeys(t *testing.T, log *capturedLog) {
	t.Helper()
	ctx := context.Background()
	pairs := []Candidate{
		{NewerContent: "the newer note", OlderContent: "the older note"},
		{NewerContent: "a second newer note", OlderContent: "a second older note"},
		{NewerContent: "a third newer note", OlderContent: "a third older note"},
	}

	// (1) The retry line: a provider that fails, so the one retry it is allowed
	// fires. A single pair, because the batch path has its own lines.
	failing := NewRelationClassifier(&fakeProvider{err: errFakeClassify})
	failing.SetRetryDelay(0)
	failing.SetLogger(log.logger())
	if _, err := failing.Classify(ctx, pairs[0]); err == nil {
		t.Fatal("Classify: want the provider's error after its single retry, or the retry line was never reached")
	}

	// (2) The batch fallback line: a provider that answers with something the
	// parser does not know, which is the shape that sends a batch reply down
	// the per-pair path and logs the transition.
	garbled := NewRelationClassifier(&fakeProvider{resp: "no verdict here at all"})
	garbled.SetRetryDelay(0)
	garbled.SetLogger(log.logger())
	// A batch is only taken above one pair, so this is what reaches
	// ClassifyBatch's reply handling.
	if _, err := garbled.ClassifyBatch(ctx, pairs); err != nil {
		t.Fatalf("ClassifyBatch: %v", err)
	}
}

// resolveAnswerNEITHER and errFakeClassify are the two fake-provider outcomes
// these fixtures need: a verdict the parser accepts, and a failure.
const resolveAnswerNEITHER = "NEITHER"

var errFakeClassify = errors.New("fake provider failure")

// dumpRecords renders a captured log for a failure message.
func dumpRecords(log *capturedLog) string {
	log.mu.Lock()
	defer log.mu.Unlock()
	var b bytes.Buffer
	for _, r := range log.records {
		b.WriteString("  " + r.msg + " ")
		for k, v := range r.attrs {
			b.WriteString(k + "=" + v + " ")
		}
		b.WriteByte('\n')
	}
	return b.String()
}
