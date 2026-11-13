package memory

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// A `verified` evidence record is what a reader counts in
// EvidenceCounts.Verified, and it is the only account of "somebody checked this"
// that survives a second save. The memories column answers a different question
// — the LATEST state — so a store that writes verified_at and never appends a
// record reports "1 observation, 0 verified" for a fact somebody just checked,
// and a reader has no way to tell that from a fact nobody checked.
//
// #673 left this to the validity writers on purpose (evidence.go's
// evidenceVerified comment reserves the kind for them), so these tests are the
// half of the contract that #673 could not write alone.

// verifiedStamp is a stored-layout verified_at as a caller states it.
func verifiedStamp(v string) *string { return &v }

// recordsByKind counts a memory's evidence records per kind.
//
// The counts alone cannot answer what these tests ask. EvidenceCounts counts
// EVERY record as an observation "whatever its kind", so a verified record is
// counted twice over and a change in Observations cannot tell a new report from a
// new check. The kind is the thing worth asserting, so this reads the records.
func recordsByKind(t *testing.T, s *Store, id string) map[string]int {
	t.Helper()
	records, err := s.MemoryProvenance(t.Context(), id)
	if err != nil {
		t.Fatalf("MemoryProvenance(%s): %v", id, err)
	}
	got := map[string]int{}
	for _, r := range records {
		got[r.Kind]++
	}
	return got
}

// The reach enumeration, as a test rather than a comment.
//
// `appendVerificationIfStatedTx`'s comment claims a set of writers, and the two
// times that comment has been wrong were both because prose is not checkable: it
// listed CreateFromCorpus as reaching a verified_at "without calling" it when the
// shared insertMemory appended anyway, and then listed the set as two writers when
// ImportMemory is a third. A claim about reach has to enumerate the set, and the
// only enumeration that stays true is one a test walks.
//
// So: every writer that stores memories.verified_at, and what each leaves behind.
// The rule being pinned is the OUTCOME, not the mechanism — a record carrying a
// verification stamp, in the same transaction — because the mechanisms differ on
// purpose (the import rides its own `imported` record; the corpus route has no
// event to record) and a test asserting "a `verified` record" would be asserting
// an implementation detail and would be wrong about the import.
//
// The corpus row is the one that records nothing, and the reason is the whole
// point: a third-party dataset's verified_at is a value in a column with NO
// observation behind it. Nobody checked anything through this store, so there is
// no event, and the only stamp available to a record would be the store's clock —
// which would manufacture the event. The import is the contrast case and shows why
// the distinction is real: its artifact carries an OBSERVATION, so a check
// happened and is attested, and the store keeps that attestation (deliberately
// stamping its arrival with its own clock rather than copying a date in from a
// file, per #682).
func TestEveryVerifiedAtWriterIsEnumerated(t *testing.T) {
	const claimed = "2026-09-20 00:00:00"

	for _, tc := range []struct {
		name string
		// write stores a verified_at through this writer.
		write func(*testing.T, *Store) string
		// wantVerifiedKinds are the record kinds that may carry the stamp. Empty
		// means the writer must leave NO record carrying one.
		wantVerifiedKinds []string
		// wantVerified is the EXACT number of stamped records the writer must leave.
		// Exact rather than a floor because every row here writes one row and one
		// write produces one record: a floor cannot tell a correct single stamp from
		// a doubled one, and a doubled stamp is the bug this table exists over.
		wantVerified int
		// why is the reason this writer is or is not in the set, kept next to the
		// assertion so the table cannot drift from the comment.
		why string
	}{
		{
			name: "Create",
			write: func(t *testing.T, s *Store) string {
				id, err := s.Create(context.Background(), testProject, Memory{
					Category: "fact", Content: "a fact checked by its author",
					Source: "mcp", VerifiedAt: verifiedStamp(claimed),
				})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				return id
			},
			wantVerifiedKinds: []string{evidenceVerified},
			wantVerified:      1,
			why:               "a live save; Ghost recorded the check, so the store's clock is the honest stamp",
		},
		{
			name: "CreateWithID",
			write: func(t *testing.T, s *Store) string {
				const id = "enumcreatewithid0000000000AA"
				got, err := s.CreateWithID(context.Background(), testProject, id, Memory{
					Category: "fact", Content: "a corpus row whose author checked it",
					Source: "mcp", VerifiedAt: verifiedStamp(claimed),
				})
				if err != nil {
					t.Fatalf("CreateWithID: %v", err)
				}
				if got != id {
					t.Fatalf("CreateWithID stored the row as %q, want the caller's id %q — this row is not the one the assertions below read", got, id)
				}
				return id
			},
			wantVerifiedKinds: []string{evidenceVerified},
			wantVerified:      1,
			why:               "a live write on the harness's behalf, so the store's clock is the honest stamp — it is Create's write with a named row, and a check stated on it is a check Ghost observed",
		},
		{
			name: "CreateWithIDFromCorpus",
			write: func(t *testing.T, s *Store) string {
				const id = "enumcorpusid000000000000AA"
				got, err := s.CreateWithIDFromCorpus(context.Background(), testProject, id, Memory{
					Category: "fact", Content: "a named dataset row carrying a verified_at value",
					Source: "mcp", VerifiedAt: verifiedStamp(claimed),
				})
				if err != nil {
					t.Fatalf("CreateWithIDFromCorpus: %v", err)
				}
				if got != id {
					t.Fatalf("CreateWithIDFromCorpus stored the row as %q, want the caller's id %q — this row is not the one the assertions below read", got, id)
				}
				return id
			},
			wantVerifiedKinds: nil,
			wantVerified:      0,
			why:               "the corpus route under a caller-chosen id, so the same value-with-nothing-observed-behind-it as CreateFromCorpus: naming the row changes nothing about whose claim the stamp would be",
		},
		{
			name: "CreateFromCorpus",
			write: func(t *testing.T, s *Store) string {
				id, err := s.CreateFromCorpus(context.Background(), testProject, Memory{
					Category: "fact", Content: "a dataset row carrying a verified_at value",
					Source: "mcp", VerifiedAt: verifiedStamp(claimed),
				})
				if err != nil {
					t.Fatalf("CreateFromCorpus: %v", err)
				}
				return id
			},
			wantVerifiedKinds: nil,
			wantVerified:      0,
			why:               "a bare value in a third-party dataset with no observation behind it — no event, and the only stamp available would manufacture one",
		},
		{
			name: "ImportMemory",
			write: func(t *testing.T, s *Store) string {
				const id = "enumimport00000000000000000AA"
				_, _, _, err := s.ImportMemory(context.Background(), PortableMemory{
					ID: id, ProjectID: testProject, Category: "fact",
					Content: "a fact an artifact records as checked", Source: "onboarding",
					VerifiedAt: verifiedStamp(claimed),
				}, ImportOptions{Apply: true})
				if err != nil {
					t.Fatalf("ImportMemory: %v", err)
				}
				return id
			},
			wantVerifiedKinds: []string{evidenceImported},
			wantVerified:      1,
			why:               "the artifact carries an OBSERVATION, so a check happened and is attested; it rides the import's own record rather than adding a `verified` one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			id := tc.write(t, s)

			records, err := s.MemoryProvenance(context.Background(), id)
			if err != nil {
				t.Fatalf("MemoryProvenance: %v", err)
			}
			var stamped []string
			for _, r := range records {
				if r.VerifiedAt != nil {
					stamped = append(stamped, r.Kind)
				}
			}

			// The outcome the comment claims, and the one a reader filtering by
			// kind actually sees.
			counts, err := s.MemoryEvidenceCounts(context.Background(), id)
			if err != nil {
				t.Fatalf("MemoryEvidenceCounts: %v", err)
			}
			if len(tc.wantVerifiedKinds) == 0 {
				if counts.Verified != tc.wantVerified {
					t.Errorf("Verified = %d, want exactly %d — %s", counts.Verified, tc.wantVerified, tc.why)
				}
				if len(stamped) != 0 {
					t.Errorf("records carrying a verification stamp = %v, want none — %s", stamped, tc.why)
				}
			} else {
				if counts.Verified != tc.wantVerified {
					t.Errorf("Verified = %d, want exactly %d — %s", counts.Verified, tc.wantVerified, tc.why)
				}
				var found bool
				for _, want := range tc.wantVerifiedKinds {
					for _, got := range stamped {
						if got == want {
							found = true
						}
					}
				}
				if !found {
					t.Errorf("stamped record kinds = %v, want one of %v — %s", stamped, tc.wantVerifiedKinds, tc.why)
				}
			}
		})
	}
}

// The scan that makes the enumeration in evidence.go a fact about the package
// rather than a list somebody maintains.
//
// The reach enumeration was wrong twice, and both times because a hand-written
// list cannot notice a writer that is not in it: first CreateFromCorpus, which
// reaches a verified_at through the shared insertMemory, then ImportMemory, which
// is in the set and not among the callers. Both times eleven or more tests passed
// while the paragraph was untrue, because no test read it.
//
// So this walks internal/memory with go/parser, finds every function that mentions
// the `verified_at` column or the VerifiedAt field IN ITS OWN BODY, and requires
// each to be in one of four classifications. A new writer, reader,
// migration or snapshot-side statement has to be placed, and an unplaced one fails
// here instead of quietly making the paragraph stale.
//
// What it does NOT do, stated plainly because an earlier version of this comment
// over-claimed it: it does not resolve the column name out of a package-level
// literal, so a function that reaches `verified_at` only through one is INVISIBLE
// to it. The tree already contains that case — migrateV10 ALTERs memories to add
// the column, naming it only through phase1aProvenanceColumns — and there is a
// second shape: AppendVerifiedEvidenceTx's body mentions no column at all, because
// it is the seam the writers reach the column THROUGH. Both are placed explicitly,
// and TestColumnListWritersAreClassified closes the literal indirection rather than
// pretending to solve the general one. "Cannot miss a writer" was never true. What
// is true is that a writer which NAMES the column in its own body cannot go
// unplaced, and the residual gap is one comment away from this file.
//
// Nor does it infer read-versus-write, or memories-versus-snapshot: that inference
// is the fragile part, and a wrong guess would let a real writer through as a
// "reader". Over-approximating to "mentions the column" costs one line per new
// reader and cannot misclassify one.
func TestEveryVerifiedAtMentionIsClassified(t *testing.T) {
	// Each set answers a different question, so a name in two of them is a
	// documentation failure as much as a name in none.
	writesMemory := map[string]bool{
		// Behind THREE callers, and the scan cannot see any of them: Create,
		// CreateWithID and CreateFromCorpus all reach the column through
		// insertMemory, and none of their bodies names it (measured — the scan
		// reports all three unseen, exactly as it reports
		// AppendVerifiedEvidenceTx and migrateV10). So this single entry is the
		// classification for all three, which is why their per-writer OUTCOMES are
		// enumerated where a test walks them —
		// TestEveryVerifiedAtWriterIsEnumerated — instead of being listed here.
		// The corpus route opts out of the stamp.
		"store.go:insertMemory":            true,
		"store.go:UpsertWithOptions":       true, // all three branches, one function
		"store.go:UpdateMemoryWithOptions": true,
		"portable.go:ImportMemory":         true, // attested by the artifact, on its own record
		"store.go:RestoreSnapshot":         true, // pure SQL from the snapshot; evidence travels with it
		// ReplaceNonManual's reuse branch: fills a reused row's unset verified_at
		// from the sources merged into it. Its caller carries those sources'
		// evidence (carryEvidenceTx) in the same transaction, as the fresh insert does.
		"store.go:fillReusedClaimsTx": true,
	}
	// snapshotSide touches a verified_at that is not a memory row: the snapshot
	// tables a restore reads back, and the evidence table's own copies. Here so a
	// new one is a decision rather than an omission.
	snapshotSide := map[string]bool{
		"evidence.go:appendEvidenceTx":          true,
		"evidence.go:carryEvidenceTx":           true,
		"evidence.go:importEvidenceTx":          true,
		"evidence.go:restoreSnapshotEvidenceTx": true,
		"portable.go:portableEvidence":          true,
		"store.go:snapshotReplaceableTx":        true, // memory_snapshots + memory_snapshot_evidence, shared by ReplaceNonManual and FoldRows
	}
	// migrations, readers, and the two functions the body scan cannot see.
	//
	// The scanners are the ones a hand-written list reliably misses, because they
	// carry the field through a struct or a SELECT list rather than naming the
	// column in a write — the scan found four on its first run against a list I had
	// written by reading greps, which is the argument for having it. migrateV10 and
	// AppendVerifiedEvidenceTx are here for the opposite reason: the scan cannot see
	// either, and each reaches the column in a way worth naming.
	// spansBoth is for a function that genuinely writes a memory row AND a snapshot
	// or evidence row, which the other two sets each describe half of.
	//
	// ReplaceNonManual is the one, and it moved into it in this PR: its fresh-insert
	// path now writes the validity triple and the provenance strings onto the row a
	// rewrite becomes, so it is a memories writer as well as the snapshot writer #673
	// recorded. The alternative was to leave it in snapshotSide and call it classified,
	// which is the kind of half-true claim this test exists to catch — and it is also
	// why a function in two sets is an error everywhere else.
	spansBoth := map[string]bool{
		"store.go:ReplaceNonManual":          true, // memory row (fresh insert) + memory_snapshots + memory_snapshot_evidence
		"store.go:inheritedClaims":           true, // READS the replaced rows' columns; the write is its caller's
		"portable.go:anyCarriedVerification": true, // reads PortableEvidence.VerifiedAt; the write is the import's
	}

	migrationsAndReaders := map[string]bool{
		// ALTERs memories to ADD the column, naming it only through
		// phase1aProvenanceColumns — invisible to the body scan, and the reason
		// TestColumnListWritersAreClassified exists.
		"migrate.go:migrateV10": true,
		// #683's historical read SELECTs the triple off the current row — a
		// reader, and the one that made this test earn its place on an upstream
		// merge rather than only on this branch's own additions. The window is then
		// either judged at the instant asked for (the as_of listings, through
		// memory.ValidityAt) or not judged at all (ghost_memory_search, whose
		// stage 2 records no verdict because the bounds are the present's, #910),
		// so this read carrying the present's boundaries is a documented decision
		// rather than an oversight.
		"asof.go:ReadMemoriesAsOf": true,
		// The exported seam the live writers reach verified_at THROUGH, so its own
		// body names no column and the scan cannot see it either. Listed so a future
		// writer that only calls the seam is a known shape, not a silent one.
		"evidence.go:AppendVerifiedEvidenceTx": true,
		"migrate.go:migrateV13":                true,
		"migrate.go:migrateV18":                true,
		"migrate.go:rebuildMemoriesV15":        true,
		"evidence.go:MemoryProvenance":         true,
		"evidence.go:evidenceCountsFor":        true,
		"evidence.go:evidenceCountsOne":        true,
		"evidence.go:scanEvidence":             true, // the evidence table's row scanner
		"portable.go:PortableMemories":         true,
		"portable.go:scanPortableMemory":       true, // the artifact's row scanner
		"store.go:IsZero":                      true, // Validity.IsZero — reads the field, no SQL
		"store.go:scanMemoryRow":               true, // the memories row scanner
		"store.go:GetAll":                      true,
		"store.go:GetByCategory":               true,
		"store.go:GetTopMemories":              true,
		"store.go:ResolveCandidates":           true,
		"store.go:ResolvedCandidates":          true,
		"store.go:SearchFTS":                   true,
		"store.go:SearchFTSAll":                true,
		"vector.go:GetByIDs":                   true,
		"pinned_overflow.go:pinnedOverflow":    true,
		// Normalizes a portable record's triple for the import predicate and for
		// ImportMemory's bind. A reader of the record: no SQL, no write; the write
		// is ImportMemory's, which is classified on its own.
		"import_check.go:ImportedValidity": true,
		// The same for an EVIDENCE record's own stamps: it normalizes them for the
		// import predicate and for the bind, so a reader of the record in the same
		// way. The write is ImportMemory's, which is classified on its own.
		"import_check.go:ImportedEvidence": true,
		// #581's passive read reaches the validity triple through the shared
		// `memoryColumns` list rather than naming it, which is why the FETCH has NO
		// entry here and the scan does not report it: this is the documented
		// residual gap of the body scan — a column reached only through a
		// package-level slice is invisible to it — and `store.go:GetTopMemories`
		// above is in exactly the same position for the same reason. An earlier
		// version of the passive fetch spelled the columns out and WAS classified;
		// selecting the shared list instead is what removed the need, and it
		// removed the second statement of what a Memory is at the same time.
		//
		// passiveColumnsFor is the one function in that file the scan DOES see, and
		// it is a reader: on a store below the validity floor it replaces the
		// triple with NULL literals so the fetch does not name columns that are not
		// there. It writes nothing, and the substitution is the validity sibling of
		// the scope and tier substitutions beside it.
		"candidates_passive.go:passiveColumnsFor": true,
		// passiveEligible hands a row's validity triple to ValidityState to decide
		// whether the row is inside its window before the passive demotions run
		// (#893). A reader of the hydrated struct: no SQL, no write.
		"candidates_passive.go:passiveEligible": true,
		// lexicalReplacements asks ValidityState the same question, so a row stage 2
		// would withhold is never one side of a lexical replacement pair. A reader
		// of the hydrated struct: no SQL, no write.
		"lexical_replacement.go:lexicalReplacements": true,
	}

	found := scanVerifiedAtMentions(t)

	exclusive := map[string]int{}
	for _, set := range []map[string]bool{writesMemory, snapshotSide, migrationsAndReaders} {
		for name := range set {
			exclusive[name]++
		}
	}
	for name, n := range exclusive {
		if n > 1 {
			t.Errorf("%s is in %d of the three exclusive sets — they answer different questions, and one of the placements is half-true. Use spansBoth if it genuinely does both", name, n)
		}
	}
	for name := range found {
		if !writesMemory[name] && !snapshotSide[name] && !migrationsAndReaders[name] && !spansBoth[name] {
			t.Errorf("%s mentions verified_at but is in no classification: place it, or the enumeration in evidence.go is wrong", name)
		}
	}
	// spansBoth is the only escape from the sets above, so a name in it AND in one
	// of them is the same half-true claim with a different spelling.
	for name := range spansBoth {
		if writesMemory[name] || snapshotSide[name] || migrationsAndReaders[name] {
			t.Errorf("%s is in spansBoth and in another set; it is either one or the other", name)
		}
	}
	// A name in a set but no longer in the code is the same drift in reverse: the
	// classification would be describing a function that no longer exists.
	//
	// The two names the scan CANNOT see are exempt from the reverse check, and that
	// is the whole reason they are named explicitly: they are placed because the scan
	// does not find them, so "the scan did not find it" is evidence about them
	// rather than about the classification. migrateV10 reaches the column through
	// phase1aProvenanceColumns and AppendVerifiedEvidenceTx is the seam the writers
	// reach it THROUGH, so neither names it in its own body. Exempting them by name
	// keeps the reverse check meaningful for everything the scan does cover.
	//
	// The eight FULL-ROW READERS are here for the third reason, and it is this
	// branch's: #587 gave the memories row two more columns and the six (now
	// eight) readers that hydrate a whole row one shared list, `memoryColumns`,
	// so a reader can no longer be seen naming `verified_at` in its own body —
	// the column arrives through the list. They are not here because the list
	// hid them: TestColumnListWritersAreClassified covers exactly that
	// indirection by requiring every function iterating the list to be
	// classified, which is the stronger statement of the same thing. They are
	// exempt here so the reverse check below keeps meaning "classified but gone"
	// rather than firing on readers the refactor deliberately made uniform.
	invisibleToScan := map[string]bool{
		"migrate.go:migrateV10":                true,
		"evidence.go:AppendVerifiedEvidenceTx": true,
		"store.go:GetAll":                      true, // every one of these hydrates a full row through memoryColumns
		"store.go:GetByCategory":               true,
		"store.go:GetTopMemories":              true,
		"store.go:ResolveCandidates":           true,
		"store.go:ResolvedCandidates":          true,
		"store.go:SearchFTS":                   true,
		"store.go:SearchFTSAll":                true,
		"vector.go:GetByIDs":                   true,
	}
	for _, set := range []map[string]bool{writesMemory, snapshotSide, migrationsAndReaders, spansBoth} {
		for name := range set {
			if invisibleToScan[name] {
				continue
			}
			if !found[name] {
				t.Errorf("%s is classified but no longer mentions the column — the classification describes a function that no longer exists", name)
			}
		}
	}
}

// scanVerifiedAtMentions finds every function in internal/memory whose body
// mentions the verified_at column or the VerifiedAt field, keyed "file.go:Func".
func scanVerifiedAtMentions(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if mentionsVerifiedAt(fn.Body) {
				found[name+":"+fn.Name.Name] = true
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("the scan found nothing, so it is broken rather than the package being clean")
	}
	return found
}

// mentionsVerifiedAt reports whether a function body touches the column or the
// field — the SQL identifier inside a string, or the Go field name.
func mentionsVerifiedAt(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING && strings.Contains(v.Value, "verified_at") {
				found = true
			}
		case *ast.Ident:
			if v.Name == "VerifiedAt" {
				found = true
			}
		}
		return !found
	})
	return found
}

// The one indirection the body scan cannot see, closed directly rather than by
// generalising the scan to resolve constants — which would be a heuristic with
// holes of its own, and the review that found migrateV10 was right that naming the
// hole is cheaper than pretending to have solved it.
//
// The shape is a function that ALTERs memories to add a column whose name lives in
// a package-level slice. The body scan sees neither the name nor the field, so
// migrateV10 — the migration that brings verified_at into existence — was
// unclassified and the test stayed green. This finds every function that ITERATES
// phase1aProvenanceColumns and requires each to be in the classification sets, so
// the next migration written that way has to be placed.
//
// It is deliberately narrow. A function that reaches a column through a const, a
// format string or a helper is still invisible, and that residual gap is stated in
// the scan's own comment rather than papered over.
func TestColumnListWritersAreClassified(t *testing.T) {
	const listName = "phase1aProvenanceColumns"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	referencing := map[string]bool{}
	listed := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := name + ":" + fn.Name.Name
			if mentionsIdent(fn.Body, listName) {
				referencing[key] = true
			}
			// Every function the scan DOES see is classified by the other test; the
			// only thing this one owns is the set the scan cannot reach.
			if mentionsVerifiedAt(fn.Body) {
				listed[key] = true
			}
		}
	}

	// The list must contain the column, or this test is guarding nothing.
	if !listContainsColumn(t, listName, "verified_at") {
		t.Fatalf("%s no longer contains verified_at, so this test guards an indirection that does not exist", listName)
	}

	if len(referencing) == 0 {
		t.Fatal("no function references " + listName + ", so the scan is broken rather than the package being clean")
	}
	for key := range referencing {
		if listed[key] {
			continue // the body scan already covers and classifies it
		}
		if !classifiedNames[key] {
			t.Errorf("%s iterates %s, which contains verified_at, but is classified nowhere — the body scan cannot see it", key, listName)
		}
	}
}

// classifiedNames is every key the three classification sets in
// TestEveryVerifiedAtMentionIsClassified accept, hoisted so this test can check a
// name that test cannot reach. Kept as a literal rather than a computed union
// because the two tests failing for different reasons is the point: a name missing
// from here and from there should be visible in both.
var classifiedNames = map[string]bool{
	"store.go:ReplaceNonManual":             true,
	"store.go:snapshotReplaceableTx":        true,
	"store.go:inheritedClaims":              true,
	"portable.go:anyCarriedVerification":    true,
	"asof.go:ReadMemoriesAsOf":              true,
	"migrate.go:migrateV10":                 true,
	"migrate.go:migrateV13":                 true,
	"migrate.go:migrateV18":                 true,
	"migrate.go:rebuildMemoriesV15":         true,
	"evidence.go:AppendVerifiedEvidenceTx":  true,
	"evidence.go:MemoryProvenance":          true,
	"evidence.go:evidenceCountsFor":         true,
	"evidence.go:evidenceCountsOne":         true,
	"evidence.go:scanEvidence":              true,
	"portable.go:PortableMemories":          true,
	"portable.go:scanPortableMemory":        true,
	"portable.go:portableEvidence":          true,
	"store.go:IsZero":                       true,
	"store.go:scanMemoryRow":                true,
	"store.go:GetAll":                       true,
	"store.go:GetByCategory":                true,
	"store.go:GetTopMemories":               true,
	"store.go:ResolveCandidates":            true,
	"store.go:ResolvedCandidates":           true,
	"store.go:SearchFTS":                    true,
	"store.go:SearchFTSAll":                 true,
	"store.go:insertMemory":                 true,
	"store.go:UpsertWithOptions":            true,
	"store.go:UpdateMemoryWithOptions":      true,
	"store.go:RestoreSnapshot":              true,
	"vector.go:GetByIDs":                    true,
	"evidence.go:appendEvidenceTx":          true,
	"evidence.go:carryEvidenceTx":           true,
	"evidence.go:importEvidenceTx":          true,
	"evidence.go:restoreSnapshotEvidenceTx": true,
	"portable.go:ImportMemory":              true,
}

// listContainsColumn reports whether a package-level slice literal of
// {name, typ} pairs contains the column. A literal walk, because the alternative
// is evaluating Go, and a heuristic that silently stops matching is worse than
// one that says it only handles the shape it can see.
func listContainsColumn(t *testing.T, listName, column string) bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, ident := range vs.Names {
					if ident.Name != listName {
						continue
					}
					for _, elt := range vs.Values {
						cl, ok := elt.(*ast.CompositeLit)
						if !ok {
							continue
						}
						for _, item := range cl.Elts {
							switch v := item.(type) {
							case *ast.KeyValueExpr:
								// {"name": "x", "typ": "TEXT"}
								if lit, ok := v.Key.(*ast.BasicLit); ok &&
									strings.Trim(lit.Value, `"`) == column {
									return true
								}
							case *ast.CompositeLit:
								// {"x", "TEXT"} — the shape phase1aProvenanceColumns
								// actually uses, so the first element is the name.
								if len(v.Elts) == 0 {
									continue
								}
								if lit, ok := v.Elts[0].(*ast.BasicLit); ok &&
									strings.Trim(lit.Value, `"`) == column {
									return true
								}
							}
						}
					}
				}
			}
		}
	}
	return false
}

// mentionsIdent reports whether a function body references a package-level name.
func mentionsIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// The fold is the case the whole mechanism is for, and the one this PR's
// validity merge made reachable: the row a fold leaves in the corpus is the
// target, not the copy the response names, so a verification that reached only
// the copy would leave the row search returns with no account of the check.
func TestUpsertFoldRecordsTheVerificationOnTheSurvivor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const text = "the vector identity is compared as an opaque string, never parsed"
	target, _, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		text, "mcp", 0.6, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}

	// FoldOnly so the fold is decided by foldOnlyEquivalent rather than a Jaccard
	// bar this PR does not own, and so no copy competes for the count.
	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"The vector identity is compared as an opaque string, never parsed",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{VerifiedAt: verifiedStamp("2026-09-20 00:00:00")},
			FoldOnly: true,
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (fold): %v", err)
	}
	if dup != target {
		t.Fatalf("fold returned duplicateOf %q, want the target %q — the fold branch was not reached", dup, target)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, target)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("Verified on the fold target = %d, want 1: the check was recorded somewhere other than the row the corpus keeps", counts.Verified)
	}
	// The first save's report, the fold's second report, and the check. Exactly
	// three records, so the fold's second report is not double-counted by the
	// verification landing next to it.
	if kinds := recordsByKind(t, s, target); kinds[evidenceObserved] != 2 || kinds[evidenceVerified] != 1 {
		t.Errorf("records = %v, want two observed (first save, fold's report) and one verified", kinds)
	}
}
