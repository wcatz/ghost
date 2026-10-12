package memory

// The structural half of the #746 write refusal: EVERY write must ask whether
// the store is newer than this build.
//
// The behavioural tests prove the check refuses, and they name the paths they
// exercise. They cannot prove the OTHER twenty do, and they will not keep
// proving it as paths are added — so this walks the package with go/parser and
// requires every write to reach one of the two guarded seams.
//
// There are three shapes to catch, and the second and third are the ones a check
// placed in beginWrite misses entirely:
//
//   - a write TRANSACTION — `BeginTx(ctx, nil)`. A caller may reach it directly,
//     which is how the store's own operations are written.
//   - an AUTOCOMMIT STATEMENT — `Exec`/`ExecContext` on a database handle. A
//     single-statement write needs no transaction of its own, so it reaches
//     SQLite straight from the pool. Rerouting these was not optional: the
//     issue's own reproduction is a save through a running server, and the
//     check has to live in the SAME transaction as the write it guards, or a
//     migration can commit in the gap between them.
//   - a WRITE THROUGH `Query`/`QueryRow`/`QueryRowContext` — a `RETURNING`
//     statement. The method name says "query" while the statement says "write",
//     which is why this shape was a LIVE HOLE in the tree this test was built
//     for: `CreateTask` (behind the `ghost_task_create` tool) and
//     `IncrementInteraction` both wrote through `QueryRowContext`, both
//     succeeded against a store a newer Ghost owned, and a classifier that only
//     knew `Exec` waved both through. A name-only classifier is not describing
//     this package; it is describing the part of it that was already correct.
//
// The classification is deliberately mechanical rather than clever. A write
// transaction is `BeginTx(ctx, nil)` (or the same on a connection), NOT
// `BeginTx(ctx, &sql.TxOptions{ReadOnly: true})` — a read transaction is not a
// write and must keep working against a newer store, since refusing reads would
// turn "restart your client" into "you cannot search anything at all". Anything
// the scan cannot classify is a failure, because the alternative is a silent
// hole in a safety check.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// writeSeamCall is one write the scan found, with the verdict it reached.
type writeSeamCall struct {
	// kind distinguishes the three shapes in a failure message: a write
	// transaction is opened by beginGuardedWrite, an autocommit statement by
	// execGuardedWrite, and a message that said only "write" would leave the
	// reader guessing which one to go and look at. It also names the SHAPE,
	// which is the more useful half: a reader who has just been told a
	// `QueryRowContext` is a write learns that the method name is not the
	// classification, which is the fact this shape exists to record.
	kind string
	file string
	line int
	// guarded is the seam's own name, or "" when the call is not routed
	// through it.
	guarded string
	// why records the classification in the failure message, so a new
	// unclassified call explains itself rather than just failing.
	why string
}

// The seams. They are named rather than pattern-matched on their bodies so
// the scan pins the CALL SITE, which is what a future write path has to get
// right — and they are named separately so routing a statement to the wrong one
// is a failure rather than a pass.
//
// There are three, and the third exists because a seam is a CLAIM that a function
// checks the store's version: naming one lets every write in it pass without the
// scan knowing whether the claim is true. That is why
// TestEveryNamedSeamActuallyChecksTheStoreVersion walks each seam's call tree and
// requires the check — a seam that stops checking fails on its own account rather
// than quietly admitting every write routed through it.
//
// beginScopedWrite is the retrieval record's: it opens the write transaction on a
// PINNED connection with its own busy_timeout (see the concurrency contract in
// docs/architecture.md), so it cannot be beginGuardedWrite, and it runs the same
// checkStoreNotNewer in the same transaction.
const (
	guardedTxSeam        = "beginGuardedWrite"
	guardedScopedTxSeam  = "beginScopedWrite"
	guardedStatementSeam = "execGuardedWrite"
)

// writeSeamExemptions are the writes that must NOT ask, keyed by "file.go" for a
// whole file or "file.go:Func" for one function, and valued with the reason it is
// not a store write.
//
// An exemption is a decision on the record, not a gap: a new entry is a claim a
// reviewer can check, where an unlisted write fails the build. They are keyed by
// function rather than only by file wherever the file still holds store writes,
// so exempting one path cannot quietly exempt the next one added beside it.
var writeSeamExemptions = map[string]string{
	// The scoped write's busy_timeout pragma. It sets what THIS CONNECTION waits
	// for the write lock and writes nothing to the file, so there is no store
	// version for it to be behind. It is keyed to the helper rather than to
	// beginScopedWrite itself on purpose: the seam's transaction and its guarded
	// check stay in scope of this test, and only the pragma is exempt.
	"retrieval_record.go:setScopedBusyTimeout": "a PRAGMA that sets the connection's busy_timeout configures the connection, not the file",

	// The migration path. It is the thing that makes a store newer, so asking
	// it whether the store is newer is a deadlock in meaning: at the moment it
	// runs, the answer is by construction "no, not yet". It also runs on a
	// pinned *sql.Conn, not the store's handle, and before any Store exists.
	"migrate.go": "the migration path is what advances user_version, and it runs before a Store exists",

	// The open path. OpenDB refuses a store newer than this build itself,
	// before any DDL and after nothing has been written (schema.go: the check
	// precedes initSQL precisely so a refusal has not already written). Every
	// Exec here is either that refusal, the DDL it gates, or the
	// `PRAGMA user_version` stamp on a database it just created. A guard would
	// ask the question OpenDB has already answered, at a point where the
	// answer is by definition "not newer".
	"schema.go:OpenDB":              "OpenDB refuses a newer store itself, before writing anything",
	"schema.go:backupBeforeMigrate": "runs on OpenDB's handle, after the refusal, to make the pre-migration copy",

	// The post-migration index builder, for the same reason as OpenDB and the
	// migration path: both callers are inside OpenDB, so both run after the
	// open-time refusal, on a handle no Store exists behind yet, and what they
	// write is CREATE INDEX on a store this build has just migrated to its own
	// version. A guard would ask a question the open has already answered.
	"schema.go:ensurePostMigrationIndexes": "runs inside OpenDB after the open-time refusal, and a Store does not exist yet",

	// A backup copies the store out; it does not write into it. Refusing it
	// because the store is newer would refuse the exact copy an operator takes
	// BEFORE replacing a stale server, which is the one moment the copy
	// matters most. The destination is a new file, not this database.
	"backup.go:vacuumInto": "VACUUM INTO writes a new file and leaves the store's own rows untouched",

	// A hygiene row, written through a handle the CALLER opened with OpenDB at
	// that moment (internal/scratch/budget.go), so the open-time refusal
	// already covers it. Guarding it here would need a Store this function
	// does not have, to guard a table that records something about this
	// process rather than about the store's content.
	"maintenance.go:RecordMaintenanceRun": "the handle was opened by OpenDB at the call site, so the open-time refusal already applies",

	// A read the scan cannot read the SQL of. Both are here because the
	// classifier's rule is to call an unresolvable statement a WRITE, and both
	// would otherwise be failures every time the file is touched.
	//
	// The as-of reader opens with WITH, which is deliberately absent from the
	// read-only allow-list: it reads like a query opener and can introduce an
	// INSERT. This one is a read, and it takes its reader from the caller rather
	// than from the store's handle.
	"asof.go:ReadMemoriesAsOf": "a read through a caller-supplied Queryer; it opens with WITH, which is not on the allow-list because WITH can introduce a write",

	// A row count for a backup manifest, on a handle the caller opened. Every
	// statement in its table is a `SELECT count(*)`, read from a struct field
	// the scan cannot follow.
	"backup.go:CountRows": "counts rows for a backup manifest; every statement in its table is a SELECT count(*), read from a struct field the scan cannot follow",

	// The compact pass's own two reads (#730), and the same shape as the two
	// above: a `SELECT` the scan cannot read, because the statement is BUILT by a
	// helper and reaches the handle as a local name. `query, args :=
	// compactCountStmt(...)` binds two names from one call, so the scan resolves
	// the first from a multi-value RHS it deliberately does not follow, the head
	// comes back "", and an unresolvable statement counts as a WRITE.
	//
	// These are exempt as FUNCTIONS, not as a file, and that is the narrowest
	// form available: history_compact.go also holds the pass's two real writes,
	// and those are NOT exempt — they open through beginGuardedWrite, which is
	// what makes a compact --apply against a store a newer Ghost owns refuse
	// rather than delete rows under it. Naming the two read functions says the
	// check still covers every write in the file
	// (TestCompactHistoryRefusesToDeleteOnAStoreANewerGhostOwns is the behaviour
	// that says so, and it would fail if either write stopped being guarded).
	//
	// Both statements are `SELECT`s and both are reached through a Queryer the
	// caller owns, so they must keep working against a newer store: refusing
	// reads is what would turn "restart your client" into "you cannot count
	// anything at all".
	"history_compact.go:(*Store).compactHistoryPreview": "a read: SELECT count(*) built by compactCountStmt and reached through a caller-owned handle, so the scan resolves no leading keyword and counts an unresolvable statement as a write",
	"history_compact.go:(*Store).stampBatchQuery":       "a read: the candidates SELECT built by compactCandidatesStmt and run through a caller-supplied Queryer (the same shape as asof.go:ReadMemoriesAsOf), so the scan resolves no leading keyword and counts an unresolvable statement as a write",

	// The growth report's own two reads (#729), and the same shape as the two
	// above for the same reason: `query, args := historyWindowGrowthStmt()` and
	// `query, args := historyPerMemoryGrowthStmt()` each bind two names from one
	// call, so the scan deliberately does not follow the multi-value RHS, the head
	// comes back "", and an unresolvable statement counts as a WRITE.
	//
	// Exempt as FUNCTIONS, and each of those functions is ONE read and nothing
	// else — which is the whole reason history_growth.go reads through
	// `historyWindowCounts` and `historyPerMemoryCounts` rather than inlining both
	// statements into `HistoryGrowth`. An exemption is granted per FUNCTION, so a
	// read left inside the big method would exempt that method whole, and a write
	// added to it would pass. Narrowing each read to its own function is what keeps
	// the exemption honest, and it is checked rather than asserted: an unguarded
	// ExecContext added to `HistoryGrowth` still fails this test.
	//
	// The limit of that is the same limit the two compact exemptions above have, and
	// it is worth stating rather than leaving to be discovered: a write added INSIDE
	// one of these two functions would not be caught, because the exemption is the
	// function. What narrowing buys is that the two exempted functions are one read
	// each, so the hole is two statements rather than every line of the report, and
	// it is the same trade the compact pass already made for the same reason.
	//
	// Reads must keep working against a newer store. ErrStoreNewer tells an operator
	// to restart their CLIENT, and a report that refused to run would turn that
	// advice into "you cannot even see how full your history is" — which is exactly
	// the question the report exists to answer, and the one a stale server most
	// needs answered before it is replaced.
	"history_growth.go:(*Store).historyWindowCounts":    "a read: the window aggregate SELECT built by historyWindowGrowthStmt and run through the pool, so the scan resolves no leading keyword and counts an unresolvable statement as a write — and a report that refused to run against a newer store would be useless for diagnosing one",
	"history_growth.go:(*Store).historyPerMemoryCounts": "a read: the per-memory aggregate SELECT built by historyPerMemoryGrowthStmt, reached the same way and for the same reason",
}

// TestEveryWriteRefusesANewerStore is the check that the safety property cannot
// be quietly lost by a new write path.
func TestEveryWriteRefusesANewerStore(t *testing.T) {
	calls := scanWrites(t)
	if len(calls) == 0 {
		t.Fatal("the scan found no write at all, so it is broken rather than the package being clean")
	}
	for _, c := range calls {
		if c.why != "" {
			continue // exempt, with the reason recorded above
		}
		if c.guarded == "" {
			t.Errorf("%s:%d %s does not go through a guarded write seam (%s) — a server behind this "+
				"write would write into a store a newer Ghost owns", c.file, c.line, c.kind,
				strings.Join([]string{guardedTxSeam, guardedStatementSeam}, ", "))
		}
	}
}

// TestTheScanCatchesTheWriteShapesItWasBlindTo pins the classifier against the
// exact shapes that were found to slip through it.
//
// This is the regression test for the test. Every case below is a write a stale
// server could not be stopped from making, and every one of them passed the
// first version of this scan silently — the two at the top were live
// production paths (ghost_task_create, and the ghost_state counter). A scanner
// that only notices `Exec` is a scanner that certifies the half of the package
// that was already correct.
func TestTheScanCatchesTheWriteShapesItWasBlindTo(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// The live one: a RETURNING insert whose SQL says INSERT while its
			// method says Query.
			name: "RETURNING insert through QueryRowContext",
			body: `func (s *Store) f(ctx context.Context) error {
				return s.db.QueryRowContext(ctx, ` + "`INSERT INTO tasks (t) VALUES (?) RETURNING id`" + `).Scan(&id)
			}`,
		},
		{
			// The other live one: a RETURNING update on ghost_state.
			name: "RETURNING update through QueryRowContext",
			body: `func (s *Store) f(ctx context.Context) error {
				return s.db.QueryRowContext(ctx, ` + "`UPDATE ghost_state SET n = n + 1 RETURNING n`" + `).Scan(&n)
			}`,
		},
		{
			name: "write through QueryContext",
			body: `func (s *Store) f(ctx context.Context) error {
				rows, err := s.db.QueryContext(ctx, ` + "`DELETE FROM memories WHERE id = ?`" + `, id)
				return err
			}`,
		},
		{
			// Parenthesised receiver: the same write, spelled so a matcher that
			// stops at Ident and SelectorExpr cannot see it.
			name: "parenthesised receiver",
			body: `func (s *Store) f(ctx context.Context) error {
				_, err := (s.db).ExecContext(ctx, ` + "`UPDATE memories SET pinned = 1`" + `)
				return err
			}`,
		},
		{
			// A bound method value: no selector at the call site at all.
			name: "method value alias",
			body: `func (s *Store) f(ctx context.Context) error {
				exec := s.db.ExecContext
				_, err := exec(ctx, ` + "`DELETE FROM memories WHERE id = ?`" + `, id)
				return err
			}`,
		},
		{
			// A CTE that writes, through a Query* method so the statement
			// text is what decides. WITH is not on the read-only allow-list
			// for precisely this reason, and an Exec spelling would not test
			// that: Exec is classified as a write whatever its SQL says.
			name: "WITH that inserts",
			body: `func (s *Store) f(ctx context.Context) error {
				return s.db.QueryRowContext(ctx, ` + "`WITH x AS (SELECT 1 AS id) INSERT INTO memories (id) SELECT id FROM x RETURNING id`" + `).Scan(&id)
			}`,
		},
		{
			// A statement the scan cannot read is a write candidate, not a
			// read — and again through a Query* so the statement text is what
			// decides.
			name: "unresolvable statement text",
			body: `func (s *Store) f(ctx context.Context, sqlText string) error {
				return s.db.QueryRowContext(ctx, sqlText).Scan(&id)
			}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package memory\n\nimport (\n\t\"context\"\n\t\"database/sql\"\n)\n\nvar id string\nvar n int\n\n" + tc.body + "\n"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "synthetic.go", src, 0)
			if err != nil {
				t.Fatalf("parse synthetic source: %v", err)
			}
			calls := scanFile(fset, "synthetic.go", file, map[string]bool{"BeginTx": true})
			if len(calls) != 1 {
				t.Fatalf("scan found %d writes, want exactly 1 — the shape is invisible to the scan", len(calls))
			}
			if calls[0].guarded != "" {
				t.Fatalf("scan reported the write as guarded by %q, but a synthetic file has no seam in it", calls[0].guarded)
			}
		})
	}
}

// TestTheScanStillCallsAReadARead is the other half of the test above, and the
// guard against fixing the holes by calling everything a write.
//
// A scan that flags every Query* would fail on the first read, which means
// "widen it until the failures stop" is not a way to pass — the classifier has
// to actually read the statement. These are the read shapes the package is full
// of, including the two spellings — a package const, and a local built up by
// concatenation — that the first version of the scan reported as writes.
func TestTheScanStillCallsAReadARead(t *testing.T) {
	src := `package memory

import (
	"context"
	"fmt"
)

const listedQuery = ` + "`SELECT id FROM memories WHERE 1 = 1`" + `
const pathQuery = ` + "`SELECT %s FROM projects`" + `

var remoteColumn = "x"

func (s *Store) reads(ctx context.Context, projectID, status string) error {
	var q = ` + "`SELECT id FROM decisions`" + `
	if status != "" {
		q += ` + "` AND status = ?`" + `
	}
	q += ` + "` ORDER BY created_at DESC`" + `
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	_ = rows
	if _, err := s.db.QueryRowContext(ctx, listedQuery).Scan(&n); err != nil {
		return err
	}
	if _, err := s.db.QueryContext(ctx, fmt.Sprintf(pathQuery, remoteColumn), projectID); err != nil {
		return err
	}
	return nil
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	if calls := scanFile(fset, "synthetic.go", file, map[string]bool{"BeginTx": true}); len(calls) != 0 {
		t.Errorf("scan reported %d reads as writes: %+v", len(calls), calls)
	}
}

// TestExemptionKeysNameExactlyOneFunction keeps the exemption map honest.
//
// A key is "file.go:Func", and this package declares a free function and a
// method with the same bare name (mergeProjectTx), so a key that names two
// declarations would exempt both while reading like a decision about one. The
// receiver is part of the key for that reason, and this test fails if a key
// resolves to more than one declaration — which is what a new collision looks
// like before anyone notices it is one.
func TestExemptionKeysNameExactlyOneFunction(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	decls := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			decls[name+":"+qualifiedFuncName(fn)]++
		}
	}
	for key := range writeSeamExemptions {
		// A whole-file key is the file's own name; it is not a function key and
		// has nothing to resolve.
		if !strings.Contains(key, ".go:") {
			continue
		}
		switch decls[key] {
		case 0:
			// A function-scoped key with no function behind it: a typo, or a
			// rename that left the reason pointing at nothing.
			t.Errorf("exemption %q names no function in the package", key)
		case 1:
		default:
			t.Errorf("exemption %q matches %d declarations — a reason recorded for one would exempt the others", key, decls[key])
		}
	}
}

// TestOnlyTheMigrationIsExemptByFile keeps the whole-file exemption a single
// named decision rather than a shape that spreads.
//
// A file-level key exempts every write added to that file later, silently,
// which is the one way this test's coverage can rot without anybody deciding
// anything. migrate.go is the defensible case — it is the thing that advances
// user_version, so asking it whether the store is newer is a deadlock in
// meaning, and it runs before a Store exists at all — and it is enumerated
// explicitly below so that a SECOND file-level exemption has to be a deliberate
// edit to this test rather than a line in a map.
func TestOnlyTheMigrationIsExemptByFile(t *testing.T) {
	for key, why := range writeSeamExemptions {
		if strings.Contains(key, ".go:") {
			continue
		}
		if key != "migrate.go" {
			t.Errorf("whole-file exemption %q is not migrate.go — narrow it to the function that earns it, "+
				"so a write added to that file later cannot inherit this one", key)
		}
		if why == "" {
			t.Errorf("whole-file exemption %q has no reason recorded", key)
		}
	}
}

// scanWrites walks every non-test file in internal/memory and classifies each
// write as guarded, exempt, or a failure.
func scanWrites(t *testing.T) []writeSeamCall {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var calls []writeSeamCall
	// Which functions hand back a *sql.Tx, learned from the whole package before
	// any call is judged: `tx, _, err := s.beginWrite(...)` binds a transaction
	// just as surely as a `tx *sql.Tx` parameter does, and the two seams are
	// this package's only such producers.
	txProducers := txReturningFuncs(t, entries)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// testdata holds a separate main package (the multi-process helper),
		// which is skipped by the IsDir check above.
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		calls = append(calls, scanFile(fset, name, file, txProducers)...)
	}
	return calls
}

// scanFile classifies every write in one parsed file.
//
// It is separated from the directory walk so a test can hand it a synthetic
// source — which is how the shapes that were once missed are pinned (see
// TestTheScanCatchesTheWriteShapesItWasBlindTo). A scan whose holes can only be
// found by editing the real package is a scan whose holes stay found.
func scanFile(fset *token.FileSet, name string, file *ast.File, txProducers map[string]bool) []writeSeamCall {
	var calls []writeSeamCall
	txNames := txNamesIn(file, txProducers)
	aliases := writeAliasesIn(file)
	heads := sqlHeadsIn(file)
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		kind, recv, ok := classifyWriteCall(call, aliases, heads)
		if !ok {
			return true
		}
		if recv == nil || !isDatabaseHandle(recv) {
			return true
		}
		if kind == writeTxKind && isReadOnlyTx(call) {
			// A read transaction is not a write. It must keep working
			// against a newer store: that is what lets an agent still
			// search while every write is refused.
			return true
		}
		enclosing := enclosingFuncName(file, call)
		if kind != writeTxKind && isTransactionReceiver(recv, txNames) {
			// Already inside a transaction, which a caller opened at a
			// guarded seam. There is nothing left to guard: the check ran
			// when that transaction was opened.
			return true
		}
		calls = append(calls, writeSeamCall{
			kind:    kind,
			file:    name,
			line:    fset.Position(call.Pos()).Line,
			guarded: seamFor(kind, enclosing),
			why:     exemptionWhy(name, enclosing),
		})
		return true
	})
	return calls
}

// writeMethodNames is every method that can carry a write out of a handle. It is
// the set used to recognise a METHOD VALUE — `f := s.db.ExecContext; f(ctx, …)`
// — which reaches SQLite with no selector at the call site and so is invisible
// to a scan that only looks at `x.Method(...)` shapes.
var writeMethodNames = map[string]bool{
	"BeginTx": true, "Exec": true, "ExecContext": true,
	"Query": true, "QueryContext": true, "QueryRow": true, "QueryRowContext": true,
}

// callReceiver returns the expression a write call is invoked on, or nil for a
// bare call. For the selector shape that is sel.X; for a method value it is the
// handle the method was taken from, which is how a call through an alias is
// still attributable to a receiver the scan can judge.
func callReceiver(call *ast.CallExpr) ast.Expr {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return sel.X
	}
	return nil
}

// writeAlias is a local bound to a database handle's method: the handle it was
// taken from, and which method it was.
//
// The method name has to be carried, not re-derived from the handle: the handle
// expression is `s.db`, which names no method, and a version that read the name
// back off it would classify every alias as a `Query` and then read the
// statement text for an `Exec` that has none.
type writeAlias struct {
	recv   ast.Expr
	method string
}

// classifyWriteCall classifies a call, following the alias form, and returns the
// handle expression the write reaches SQLite through.
//
// The alias exists because a bound method value is an ordinary thing to write and
// an invisible one to a structural scan: `f := s.db.ExecContext` binds the method
// to the handle, and every later `f(...)` reads as a call to a local function.
// Resolving what a bare call points at needs type inference the parser does not
// do, so the binding carries its own method name instead — and its handle, which
// is what the returned receiver is for. A bare call has no receiver of its own,
// so without the alias's handle the caller would see nil and skip the very shape
// this exists to catch.
func classifyWriteCall(call *ast.CallExpr, aliases map[string]writeAlias, known sqlHeads) (string, ast.Expr, bool) {
	if kind, ok := classifyWrite(call, known); ok {
		return kind, callReceiver(call), true
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return "", nil, false
	}
	alias, bound := aliases[id.Name]
	if !bound || !isDatabaseHandle(alias.recv) {
		return "", nil, false
	}
	if alias.method == "BeginTx" {
		return writeTxKind, alias.recv, isReadOnlyTx(call)
	}
	if alias.method == "Exec" || alias.method == "ExecContext" {
		return autocommitKind, alias.recv, true
	}
	if !statementSQLIsWrite(call, alias.method, known) {
		return "", nil, false
	}
	return queryWriteKind, alias.recv, true
}

// writeAliasesIn collects the local names bound to a database handle's method,
// mapping each to the handle and method it was taken from.
//
// Only LHS[0] is read, for the reason txNamesIn gives: a multi-value assignment
// puts the meaningful value first and the rest are errors.
func writeAliasesIn(file *ast.File) map[string]writeAlias {
	aliases := map[string]writeAlias{}
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 || len(assign.Rhs) == 0 {
			return true
		}
		sel, ok := assign.Rhs[0].(*ast.SelectorExpr)
		if !ok || !writeMethodNames[sel.Sel.Name] {
			return true
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			aliases[id.Name] = writeAlias{recv: sel.X, method: sel.Sel.Name}
		}
		return true
	})
	return aliases
}

// txReturningFuncs names every function in the package whose results include a
// *sql.Tx, including the standard library's own BeginTx by name.
//
// It is the second half of telling a transaction from a handle, and the half
// that matters: a transaction is almost never a parameter in this package, it is
// the first value a write seam returns.
func txReturningFuncs(t *testing.T, entries []os.DirEntry) map[string]bool {
	t.Helper()
	producers := map[string]bool{"BeginTx": true}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Results == nil {
				continue
			}
			for _, field := range fn.Type.Results.List {
				if isSqlTxType(field.Type) {
					producers[fn.Name.Name] = true
				}
			}
		}
	}
	return producers
}

// isTransactionReceiver reports whether a call receiver is a *sql.Tx, or
// something derived from one, and so a statement already inside a transaction a
// guarded seam opened.
//
// A receiver it cannot prove is a transaction is treated as a HANDLE, not the
// other way round. That direction is deliberate: the cost of guessing wrong is a
// test failure naming a line, while guessing the other way silently accepts a
// write through a handle the scan never modelled — the precise hole this test
// exists to close. A transaction reaches a statement in three ways in this
// package, and all three are recognised: declared as a parameter or local of
// type *sql.Tx, bound from a function that returns one, or derived from one by
// the conversions and preparers that database/sql hands out.
func isTransactionReceiver(recv ast.Expr, txNames map[string]bool) bool {
	ident, ok := recv.(*ast.Ident)
	if !ok {
		// A selector is a Store field (s.db, s.readDB) or a package handle. No
		// transaction in this package is reached through one, and treating it
		// as a transaction would be the unsafe direction.
		return false
	}
	return txNames[ident.Name]
}

// txNamesIn collects every name in a file that holds, or is derived from, a
// *sql.Tx.
//
// The three sources are run to a fixed point, because derivation chains: a
// helper may take the prepared statement a transaction produced and hand that
// on. The result is scoped to the file, which over-collects a name reused for a
// different purpose in the same file — the same deliberate bias as above, and a
// name serving both roles in one file shows up as a failure to resolve rather
// than a silent pass.
func txNamesIn(file *ast.File, txProducers map[string]bool) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Field:
			if isSqlTxType(v.Type) {
				for _, id := range v.Names {
					names[id.Name] = true
				}
			}
		case *ast.ValueSpec:
			if isSqlTxType(v.Type) {
				for _, id := range v.Names {
					names[id.Name] = true
				}
			}
		}
		return true
	})
	// Seed from producers, then close over derivation: `db := sqlExecutor(tx)`
	// only becomes a transaction name once `tx` is one, and a helper that takes
	// that `db` and returns a statement of its own is derived in turn.
	//
	// Only the FIRST value of an assignment is considered. Every producer and
	// every derivation in this package puts the transaction-scoped value first —
	// `tx, _, err := s.beginWrite(...)`, `stmt, err := tx.PrepareContext(...)` —
	// and the later values are the error or the result code. Reading past the
	// first would mark `err` a transaction, which is harmless today and
	// indistinguishable from the bug it would become tomorrow.
	for changed := true; changed; {
		changed = false
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) == 0 {
				return true
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := assign.Lhs[0].(*ast.Ident); ok {
				if names[id.Name] {
					return true
				}
				if isTxProducerCall(call, txProducers) || isTxDerivedCall(call, names) {
					names[id.Name] = true
					changed = true
				}
			}
			return true
		})
	}
	return names
}

// isTxDerivedCall reports whether a call produces something that belongs to a
// transaction rather than a handle of its own.
//
// Only two shapes qualify, and both are named rather than guessed at: a
// CONVERSION to a type this package declares over the shared executor surface
// (`sqlExecutor(tx)`), and the preparers and connection pinner on database/sql
// itself (Prepare, PrepareContext, Conn, ConnContext). Each is a write surface
// that cannot outlive the transaction it came from. A call in any other shape is
// not accepted, so a helper that quietly opened its own handle has to be named
// in the exemption list instead of being waved through.
func isTxDerivedCall(call *ast.CallExpr, txNames map[string]bool) bool {
	if !anyInputIsTx(call, txNames) {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		// A conversion: a bare type name applied to a value.
		return true
	case *ast.SelectorExpr:
		switch fun.Sel.Name {
		case "Prepare", "PrepareContext", "Conn", "ConnContext":
			return true
		}
	}
	return false
}

// anyInputIsTx reports whether a call is handed a transaction — as an argument,
// or, for a method call, as the receiver it is invoked on. Both spellings occur:
// `sqlExecutor(tx)` passes one, and `tx.PrepareContext(...)` is called on one.
func anyInputIsTx(call *ast.CallExpr, txNames map[string]bool) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if recv, ok := sel.X.(*ast.Ident); ok && txNames[recv.Name] {
			return true
		}
	}
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok && txNames[id.Name] {
			return true
		}
	}
	return false
}

// isTxProducerCall reports whether a call is to a function that returns a
// *sql.Tx, covering both a bare call and a method call on any receiver.
func isTxProducerCall(call *ast.CallExpr, txProducers map[string]bool) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return txProducers[fun.Name]
	case *ast.SelectorExpr:
		return txProducers[fun.Sel.Name]
	}
	return false
}

// isSqlTxType reports whether a type expression is *sql.Tx.
func isSqlTxType(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Tx"
}

const (
	writeTxKind    = "opens a write transaction"
	autocommitKind = "writes an autocommit statement"
	// queryWriteKind is a write that arrives through a Query* method, which in
	// practice means a RETURNING clause. It has to be its own kind because it is
	// not the same problem: execGuardedWrite returns an sql.Result and has no
	// rows to hand back, so a RETURNING write cannot be routed there and its
	// only guarded home is a transaction.
	queryWriteKind = "runs a write statement through a handle"
)

// seamFor returns the seam that is supposed to be performing a write of this
// kind, when the enclosing function IS that seam. A seam's own body is the one
// place allowed to reach the handle directly, and it is recognised by the
// function it sits in rather than by a marker comment — which a formatter can
// move, and which would not fail when the seam's body grew a second write.
//
// The comparison is on the BARE method name: the seams are methods, so the
// enclosing name arrives receiver-qualified for the exemption map's sake, and
// matching the qualified form here would make both seams unrecognisable and
// report their own bodies as failures.
//
// The kinds are not interchangeable: a BeginTx found inside the STATEMENT seam is
// not accepted, because that seam is where single statements are wrapped and a
// transaction opened there would be a write escaping the check this test exists
// to enforce. A Query* write is accepted only in the TRANSACTION seam, for the
// reason its kind records.
func seamFor(kind, enclosing string) string {
	bare := bareFuncName(enclosing)
	switch kind {
	case writeTxKind, queryWriteKind:
		if bare == guardedTxSeam || bare == guardedScopedTxSeam {
			return bare
		}
	case autocommitKind:
		if bare == guardedStatementSeam {
			return guardedStatementSeam
		}
	}
	return ""
}

// bareFuncName strips the receiver qualifier qualifiedFuncName adds, so a
// receiver-qualified name can be compared against a plain method name.
func bareFuncName(name string) string {
	if i := strings.LastIndex(name, ")."); i >= 0 {
		return name[i+2:]
	}
	return name
}

// classifyWrite reports whether a call is a store write, and which of the three
// shapes it is.
//
// The Query* methods are here because SQLite runs an INSERT or an UPDATE
// through them perfectly happily — that is what `RETURNING` is — and a write
// that arrives by a name this function did not know about is exactly the hole
// the test exists to close. Deciding whether a `Query*` is a write is
// statementSQLIsWrite's job, not this function's: it cannot be done from the
// method name, only from the SQL.
func classifyWrite(call *ast.CallExpr, known sqlHeads) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	switch sel.Sel.Name {
	case "BeginTx":
		return writeTxKind, true
	case "Exec", "ExecContext":
		return autocommitKind, true
	case "Query", "QueryContext", "QueryRow", "QueryRowContext":
		if !statementSQLIsWrite(call, sel.Sel.Name, known) {
			return "", false
		}
		return queryWriteKind, true
	}
	return "", false
}

// readOnlyLeadingKeywords are the only first words this scan accepts as proof
// that a `Query*` cannot write.
//
// The list is short on purpose. It is an ALLOW-list, so a statement this
// function cannot read is a write candidate rather than a read, and every write
// keyword — INSERT, UPDATE, DELETE, REPLACE, CREATE, DROP, ALTER, VACUUM — plus
// every statement the scan failed to resolve falls into it. `WITH` is absent
// deliberately: it reads like a query opener but can introduce an INSERT, and
// the one read in this package that uses it carries a recorded reason instead
// (see writeSeamExemptions) rather than being waved through by a rule that would
// also admit `WITH ... INSERT`.
var readOnlyLeadingKeywords = map[string]bool{
	"SELECT":  true,
	"PRAGMA":  true,
	"EXPLAIN": true,
	"VALUES":  true,
}

// statementSQLIsWrite reports whether the statement a Query* call carries is a
// write, and says YES for anything it cannot resolve.
//
// Resolvable means: a string literal, the format string of an `fmt.Sprintf`, a
// concatenation rooted in one of those, or a name bound to one — a package-level
// string constant, or a local assigned from a literal. A statement this function
// cannot read is reported as a write, and the asymmetry is the whole point. A
// false positive costs a test failure naming a line and a recorded reason; a
// false negative is a write nothing checks, which is the outcome the issue was
// filed about.
func statementSQLIsWrite(call *ast.CallExpr, method string, known sqlHeads) bool {
	// One expression, deliberately: "" is not in the allow-list, so a statement
	// this function could not read is a write. Splitting that into a separate
	// `keyword == ""` branch would state the rule twice, and the safety
	// property would then be pinned by neither branch alone.
	return !readOnlyLeadingKeywords[leadingKeywordOf(queryArgument(call, method), known)]
}

// sqlHeads maps a name to the leading SQL word of the string it is bound to, or
// to "" when the binding could not be read.
type sqlHeads map[string]string

// leadingKeywordOf returns the first SQL word of a statement expression, or "".
func leadingKeywordOf(expr ast.Expr, known sqlHeads) string {
	switch v := expr.(type) {
	case *ast.Ident:
		// A name the scan could not resolve yields "", which the caller reads as
		// "write" — see the note on statementSQLIsWrite.
		return known[v.Name]
	case *ast.ParenExpr:
		return leadingKeywordOf(v.X, known)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return leadingKeywordOf(v.X, known)
		}
	case *ast.CallExpr:
		// A format string is always the first argument of Sprintf.
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" && len(v.Args) > 0 {
			return leadingKeywordOf(v.Args[0], known)
		}
	}
	return leadingSQLWord(literalText(expr))
}

// literalText returns the quoted text of a string literal, or "".
func literalText(expr ast.Expr) string {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	return lit.Value
}

// leadingSQLWord returns the first word of a quoted SQL literal, uppercased, or
// "" when the literal says nothing.
func leadingSQLWord(lit string) string {
	if lit == "" {
		return ""
	}
	s, err := strconv.Unquote(lit)
	if err != nil {
		// A raw string literal unquotes fine, but a concatenation fragment that
		// is not a complete literal does not. Strip the quotes and try anyway,
		// because refusing to read it means reporting a read as a write.
		s = strings.Trim(lit, "`\"")
	}
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' {
			continue
		}
		word := s[i:]
		if j := strings.IndexAny(word, " \t\n\r("); j >= 0 {
			word = word[:j]
		}
		return strings.ToUpper(word)
	}
	return ""
}

// sqlHeadsIn collects the statement heads a file's names resolve to.
//
// It reads two sources. A package-level `const`/`var` of string type is the
// statement itself (`const pathCandidatesQuery = "SELECT …"`). A local
// assignment is read for its FIRST binding only, and `+=` is skipped outright:
// appending cannot change the leading word, so a query assembled as
// `q := "SELECT …"` followed by three `q += " AND …"` keeps SELECT. Reading the
// last binding instead would report the tail, and a statement whose tail is
// `RETURNING id` is the exact shape this classifier exists to catch.
func sqlHeadsIn(file *ast.File) sqlHeads {
	heads := sqlHeads{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.GenDecl:
			for _, spec := range v.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) == 0 {
					continue
				}
				// An untyped const (`const q = "SELECT …"`) has no type
				// expression at all, and requiring one would silently skip
				// exactly the form this package uses.
				if vs.Type != nil && !isStringTyped(vs.Type) {
					continue
				}
				for _, name := range vs.Names {
					if _, seen := heads[name.Name]; !seen {
						heads[name.Name] = leadingSQLWord(literalText(vs.Values[0]))
					}
				}
			}
		case *ast.AssignStmt:
			// An append is not a first binding, and its token is ADD_ASSIGN.
			if v.Tok == token.ADD_ASSIGN || len(v.Lhs) == 0 {
				return true
			}
			id, ok := v.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			if _, seen := heads[id.Name]; seen {
				return true
			}
			if len(v.Rhs) == 0 {
				return true
			}
			// A multi-value call binds several names; only the first receives
			// this RHS, so the rest are left unresolved rather than all being
			// told the same thing.
			heads[id.Name] = leadingKeywordOf(v.Rhs[0], heads)
		}
		return true
	})
	return heads
}

// isStringTyped reports whether a declared type is `string`, or a named type
// this package could still hold one in.
func isStringTyped(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name == "string"
	case *ast.SelectorExpr:
		return v.Sel.Name == "String"
	}
	return false
}

// queryArgument returns the AST node holding the SQL of a Query* call, or nil
// when the call has no argument in that position.
//
// The position is named per method rather than guessed: `Query(query, …)` takes
// the statement first, `QueryContext(ctx, query, …)` takes a context first. Both
// spellings occur throughout this package, and reading the wrong index would make
// a context expression look like a statement — which resolves to no leading
// keyword, which by the rule above means "treat as a write". That would fail
// loudly, but it would fail on every read, so the index is worth being right
// about.
func queryArgument(call *ast.CallExpr, method string) ast.Expr {
	idx := 0
	if method == "QueryContext" || method == "QueryRowContext" {
		idx = 1
	}
	if idx >= len(call.Args) {
		return nil
	}
	return call.Args[idx]
}

// isDatabaseHandle reports whether an expression names something this package
// writes through: an identifier (a handle local or parameter), a selector (a
// Store field such as s.db), or a parenthesised one.
//
// The parentheses are not a curiosity. `(s.db).ExecContext(…)` is the same write
// as `s.db.ExecContext(…)`, and a receiver matcher that stops at `Ident` and
// `SelectorExpr` accepts it silently. That is a hole of exactly the kind this
// test is here to close, found by adding that one shape to a copy of the package
// and watching the scan pass it.
func isDatabaseHandle(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return true
	case *ast.ParenExpr:
		return isDatabaseHandle(v.X)
	}
	return false
}

// isReadOnlyTx reports whether a BeginTx call asks for a read-only transaction.
func isReadOnlyTx(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		opts, ok := arg.(*ast.UnaryExpr)
		if !ok || opts.Op != token.AND {
			continue
		}
		lit, ok := opts.X.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == "ReadOnly" {
				if bl, ok := kv.Value.(*ast.Ident); ok && bl.Name == "true" {
					return true
				}
			}
		}
	}
	return false
}

// exemptionWhy returns the recorded reason a write in file/func need not be
// guarded, or "" when there is none. A whole-file entry is the fallback, so a
// file that is entirely migration or open-path work needs one entry rather than
// one per function.
func exemptionWhy(file, funcName string) string {
	if why, ok := writeSeamExemptions[file+":"+funcName]; ok {
		return why
	}
	return writeSeamExemptions[file]
}

// enclosingFuncName returns the receiver-qualified name of the function a node
// sits in, or "".
//
// It walks the declarations rather than tracking the traversal, so a write at
// package scope — a var initialiser; an init function's body is a FuncDecl and
// does have a name — reports "" rather than a name borrowed from an unrelated
// declaration that happens to span it. A node that is inside no function is a
// write this test cannot reason about, and "" makes it land on the file-level
// exemption or fail.
//
// The receiver is part of the name because this package declares both a free
// function and a method called mergeProjectTx, and a bare name would let a
// reason recorded for one silently exempt the other. The format is the Go
// spelling of the method expression — `(*Store).mergeProjectTx` — so the key in
// the exemption map is the same text a reader would search the file for.
func enclosingFuncName(file *ast.File, node ast.Node) string {
	name := ""
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if node.Pos() >= fn.Body.Pos() && node.End() <= fn.Body.End() {
			name = qualifiedFuncName(fn)
		}
	}
	return name
}

// qualifiedFuncName renders a function declaration's name with its receiver, so
// two declarations that share a bare name are two distinct keys.
func qualifiedFuncName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	// The receiver is usually *Store, but a value receiver is legal and must not
	// render the same string as the pointer one or the two would collide.
	if star, ok := recv.(*ast.StarExpr); ok {
		return "(*" + typeText(star.X) + ")." + fn.Name.Name
	}
	return "(" + typeText(recv) + ")." + fn.Name.Name
}

// typeText renders a receiver type expression, which is an identifier for every
// receiver this package declares.
func typeText(expr ast.Expr) string {
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// TestEveryNamedSeamActuallyChecksTheStoreVersion: the other half of what naming a
// seam claims.
//
// seamFor recognises a function as a seam BY NAME, so naming one makes every write
// inside it pass this guard without the guard knowing whether the function really
// performs the check. A seam that is renamed, emptied of its check, or added by
// someone who believed their own comment would then admit every write routed
// through it, silently — the failure this file exists to prevent, reached from the
// other direction.
//
// So each named seam's CALL TREE is read and required to run the check. The tree
// rather than the body, because a seam may delegate: beginGuardedWrite and
// execGuardedWrite both hand off to finishGuardedWrite, and asserting a direct call
// would have failed on both seams that existed before this test — which is how a
// correct delegation looks like a broken seam.
func TestEveryNamedSeamActuallyChecksTheStoreVersion(t *testing.T) {
	// The same walk this scan already does: the package directory's non-test .go
	// files, parsed one by one. parser.ParseDir would be shorter and is deprecated
	// because it ignores build tags when associating files with packages, which
	// would make the seam list depend on which platform the test runs on.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	direct := map[string]bool{}
	calls := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		{
			for _, d := range file.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				var names []string
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						if fun.Sel.Name == "checkStoreNotNewer" {
							direct[fn.Name.Name] = true
						}
						names = append(names, fun.Sel.Name)
					case *ast.Ident:
						names = append(names, fun.Name)
					}
					return true
				})
				calls[fn.Name.Name] = names
			}
		}
	}
	var reaches func(string, int) bool
	reaches = func(name string, depth int) bool {
		if direct[name] {
			return true
		}
		if depth == 0 {
			return false
		}
		for _, called := range calls[name] {
			if reaches(called, depth-1) {
				return true
			}
		}
		return false
	}
	for _, seam := range []string{guardedTxSeam, guardedScopedTxSeam, guardedStatementSeam} {
		if !reaches(seam, 2) {
			t.Errorf("%s is named as a guarded seam but nothing in its call tree runs "+
				"checkStoreNotNewer — every write routed through it passes this guard unchecked", seam)
		}
	}
}
