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

	"github.com/wcatz/ghost/internal/memory"
)

// capturedLog is a slog.Handler that keeps every record a pass emits, so a
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

// onlyRecordFor returns the single record whose message contains substr and whose
func (c *capturedLog) onlyRecordFor(t *testing.T, substr, key, value string) capturedRecord {
	t.Helper()
	var matches []capturedRecord
	for _, r := range c.find(substr) {
		if r.attrs[key] == value {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("%d record(s) matching %q with %s=%s, want exactly 1:\n%s",
			len(matches), substr, key, value, dumpRecords(c))
	}
	return matches[0]
}

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
func TestRunNamesTheKeyOfEveryValueItLogs(t *testing.T) {
	// ONE case, and the fixture is the only shape in which the line is reachable
	for _, tc := range []struct {
		name       string
		relation   Relation
		sourceText string
		targetText string
		// stamp is the edge's own created_at, so the pair is re-judged; the fresh
		// scan proposes the timestamps' direction and the edge names the other,
		// which is what makes them disagree.
		stamp string
	}{
		{
			name:       "a live supersedes link",
			relation:   RelationSupersedes,
			sourceText: "bug: the relay stalls on every consumer rebalance",
			targetText: "the relay rebalance stall is fixed: pin the consumer",
			stamp:      "2020-01-01 00:00:00",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()

			// One note a month old and one a month new, so the timestamps have a
			old := add(t, store, db, tc.sourceText, []float32{1, 0, 0}, "2026-01-01 00:00:00")
			recent := add(t, store, db, tc.targetText, []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")
			linkSource, linkTarget := old, recent
			if err := store.CreateLinkJudged(ctx, linkSource, linkTarget, string(tc.relation), 0.95, "llm", tc.stamp); err != nil {
				t.Fatal(err)
			}

			log := &capturedLog{}
			res, _, err := Run(ctx, store, &supersedesEverything{}, "p", 0.9, true, log.logger())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.OppositeLive != 1 {
				t.Fatalf("Result.OppositeLive = %d, want 1: the fixture must reach the reverse-of-a-live-link line, or this test asserts nothing", res.OppositeLive)
			}

			records := log.find("reverse of a live link")
			if len(records) != 1 {
				t.Fatalf("emitted %d record(s) about a reversed live link, want exactly 1: %v", len(records), log.records)
			}
			attrs := records[0].attrs
			// Each id under the key that says WHICH source asserted it, plus the
			want := map[string]string{
				"link_relation": string(tc.relation),
				"link_source":   memory.RefusalArg("memory_id", linkSource),
				"link_target":   memory.RefusalArg("memory_id", linkTarget),
				"scan_newer":    memory.RefusalArg("memory_id", recent),
				"scan_older":    memory.RefusalArg("memory_id", old),
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
			// The general form of the same rule, so the line is also covered by
			// the shape assertion its siblings get.
			assertLogKeysAreLabels(t, log)
		})
	}
}

// assertDrivenLoggers requires every function in this package that actually
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
		return loggers[baseIdentName(fun.X)]
	case *ast.Ident:
		return slogLevelNames[fun.Name] // slog.Info(...) at package level
	}
	return false
}

// declaredLoggers returns the names a file binds a *slog.Logger to: parameters,
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
func TestPackagePassesLogOnlyLabelKeys(t *testing.T) {
	cases := []struct {
		name   string
		covers []string
		run    func(t *testing.T, log *capturedLog)
		wants  []string
	}{
		// `Run` is a one-line wrapper since #799 moved the pass into RunWith
		{name: "Run", covers: []string{"RunWith"}, run: runPassesLogKeys, wants: []string{
			"reverse of a live link", // the #804 line
			"refusing a pair the graph claims in both directions",
			"vetoed pair whose older note states a rule",
			"skipping pair with an unclassifiable verdict",
			"supersede classified",
		}},
		// `Reassess` is a one-line wrapper since #862 added the consensus gate
		// (ReassessWith), the same shape Run has had since #799 — so the
		// function that logs is ReassessWith, which is the name the scan reports.
		{name: "Reassess", covers: []string{"Reassess", "ReassessWith"}, run: reassessPassesLogKeys, wants: []string{
			"a cyclic pair whose two rows share both timestamps",
			"the older note states a rule this edge does not retire",
			"the passes did not agree; no edge withdrawn", // the #862 gate
			"supersede reassess", // the summary line — the longest in the package
		}},
		{name: "Withdraw", covers: []string{"Withdraw"}, run: withdrawPassesLogKeys, wants: []string{
			"supersede withdrew a named edge",
		}},
		// The classifier's lines live in its retry wrapper and its per-chunk
		{name: "RelationClassifier", covers: []string{"ClassifyBatch", "call", "classifyChunk"}, run: classifierPassesLogKeys, wants: []string{
			"classify call failed; retrying once",
			"batch reply unparseable",
		}},
	}

	// Every pass in the package that logs must have a case, or this test is
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
	unclassified := log.onlyRecordFor(t, "skipping pair with an unclassifiable verdict", "newer", memory.RefusalArg("memory_id", blankNewer))
	if unclassified.attrs["older"] != memory.RefusalArg("memory_id", blankOlder) {
		t.Errorf("the unclassifiable line names older=%s, want %s: the fixture exists so the line's keys carry the pair that produced them",
			unclassified.attrs["older"], memory.RefusalArg("memory_id", blankOlder))
	}

	vetoed := log.onlyRecordFor(t, "vetoed pair whose older note states a rule", "newer", memory.RefusalArg("memory_id", vetoNewer))
	if vetoed.attrs["older"] != memory.RefusalArg("memory_id", vetoOlder) {
		t.Errorf("the veto line names older=%s, want %s", vetoed.attrs["older"], memory.RefusalArg("memory_id", vetoOlder))
	}
	if vetoed.attrs["reason"] == "" {
		t.Error("the veto line carries no reason; the whole point of the line is naming the imperative that fired")
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

	// A third edge, and the gated run that reaches #862's split line: two passes
	splitNewer := add(t, store, db, "the nightly export job writes to the vault bucket", []float32{0, 0, 1}, "2026-09-02 09:00:00")
	splitOlder := add(t, store, db, "the nightly export job writes to the shared volume", []float32{0, 0, 0.99}, "2026-02-02 09:00:00")
	if err := store.CreateLink(ctx, splitNewer, splitOlder, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	gated := &perPassClassifier{script: []func(string, string) Relation{
		neitherEverywhere, neitherEverywhere, supersedesEverywhere,
	}}
	if _, _, err := ReassessWith(ctx, store, gated, "p", ReassessOptions{Apply: true, Consensus: 3}, log.logger()); err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
}

// withdrawPassesLogKeys drives the named withdrawal
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
