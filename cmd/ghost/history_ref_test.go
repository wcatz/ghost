package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

// refTestLiveID is the live memory's id in refTestStore, named so the purge tests
// in history_test.go address the same row. It is 32 characters, the length
// hex(randomblob(16)) mints, and deliberately not asserted to be: the purge
// decision is a membership test, so a fixture of another length would pass for a
// different reason.
const refTestLiveID = "L1A2B3C4D5E6F70819293A4B5C6D7E8F9"

// refTestStore is a store holding one live memory, one deleted memory, and one
// memory from another project, all with history behind them: the three states a
// `ghost history` ref has to reach, and the reach is the whole change (#720).
//
// The ids are imported under ids the fixture chose rather than minted, because a
// random 8-character prefix cannot be made to collide on demand and the ambiguity
// refusal is a case that has to be driven.
func refTestStore(t *testing.T) (store *memory.Store, live, gone string) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store = memory.NewStore(db, nil)
	ctx := context.Background()
	for _, p := range []string{"p", "other"} {
		if err := store.EnsureProject(ctx, p, "/tmp/"+p, p); err != nil {
			t.Fatalf("EnsureProject(%s): %v", p, err)
		}
	}

	live = refTestLiveID
	for _, f := range []struct{ id, project, content string }{
		{live, "p", "the live memory a report would name by eight characters"},
		{"DEADBEEFDEADBEEFDEADBEEFDEADBEEF", "p", "a memory deleted on purpose, whose text the history keeps"},
		{"FEEDFACE0000000000000000000000AA", "other", "a memory in a project the reader did not name"},
	} {
		if _, _, _, err := store.ImportMemory(ctx, memory.PortableMemory{
			ID: f.id, ProjectID: f.project, Category: "fact", Content: f.content, Source: "mcp",
		}, memory.ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
			t.Fatalf("ImportMemory(%s): %v", f.id, err)
		}
	}
	gone = "DEADBEEFDEADBEEFDEADBEEFDEADBEEF"
	if err := store.Delete(ctx, gone); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	return store, live, gone
}

// TestResolveHistoryRefAcceptsWhatTheReportsPrint: `ghost resolve --mark` and
// `ghost supersede --withdraw` print EIGHT characters, and `ghost history` used to
// answer one of them with "no memory and no history recorded" — reading as though
// the memory had never existed. The eight characters a report prints have to
// resolve here, and a full id has to keep working exactly as it did.
func TestResolveHistoryRefAcceptsWhatTheReportsPrint(t *testing.T) {
	s, live, gone := refTestStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		what string
		ref  string
		want string
	}{
		{"the eight characters a report prints of a live memory", memref.Short(live), live},
		{"sixteen characters", live[:16], live},
		{"the full id", live, live},
		{"the full id in another case", strings.ToUpper(live), live},
		{"a DELETED memory's eight characters, which reach its tombstone", memref.Short(gone), gone},
		// `ghost history` has no project operand and a full id has always
		// reached any row in the store, so a prefix confined to one project
		// would make the two forms of one ref disagree about where a memory may
		// be looked for.
		{"another project's eight characters", "FEEDFACE0000", "FEEDFACE0000000000000000000000AA"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := resolveHistoryRef(ctx, s, tc.ref)
			if err != nil {
				t.Fatalf("resolveHistoryRef(%q): %v", tc.ref, err)
			}
			if got != tc.want {
				t.Errorf("resolveHistoryRef(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

// TestResolveHistoryRefReachesADeletedMemory: the tombstone is the feature the
// command exists for, and it lives in memory_history — not in memories, where a
// deleted row is gone. A ref resolved against the live rows only would report a
// deleted memory as never written, which is the exact claim #720 calls out.
func TestResolveHistoryRefReachesADeletedMemory(t *testing.T) {
	s, _, gone := refTestStore(t)
	ctx := context.Background()

	id, err := resolveHistoryRef(ctx, s, memref.Short(gone))
	if err != nil {
		t.Fatalf("resolveHistoryRef on a deleted memory's prefix: %v", err)
	}
	view, err := readHistoryView(ctx, s, id, 0)
	if err != nil {
		t.Fatalf("readHistoryView: %v", err)
	}
	if len(view.Entries) == 0 {
		t.Fatal("the resolved id printed no history; the tombstone is unreachable")
	}
	if view.Live != nil {
		t.Error("a deleted memory was reported as live")
	}
	var buf strings.Builder
	if err := printMemoryHistory(&buf, view); err != nil {
		t.Fatalf("printMemoryHistory: %v", err)
	}
	if !strings.Contains(buf.String(), "no longer live") {
		t.Errorf("the report does not say the memory is gone:\n%s", buf.String())
	}
}

// TestResolveHistoryRefRefusesAnAmbiguousPrefixWithoutChoosing: two memories
// behind one prefix is a question with two answers, and this command reads one
// memory's recorded text — so a choice would print the wrong memory's history as
// though it were the one asked for. The refusal names the matches so the reader
// can add characters.
func TestResolveHistoryRefRefusesAnAmbiguousPrefixWithoutChoosing(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, id := range []string{"CAFE0000CAFE0000CAFE0000CAFE0000", "CAFE0000CAFE0000CAFE0000CAFE0001"} {
		if _, _, _, err := s.ImportMemory(ctx, memory.PortableMemory{
			ID: id, ProjectID: "p", Category: "fact", Content: "a memory behind a shared prefix", Source: "mcp",
		}, memory.ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
			t.Fatalf("ImportMemory(%s): %v", id, err)
		}
	}

	_, err = resolveHistoryRef(ctx, s, "CAFE0000")
	if err == nil {
		t.Fatal("resolveHistoryRef chose between two memories behind one prefix")
	}
	for _, want := range []string{"CAFE0000CAFE0000CAFE0000CAFE0000", "CAFE0000CAFE0000CAFE0000CAFE0001", "more characters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q: %v", want, err)
		}
	}
}

// TestResolveHistoryRefSaysAPrefixNoIdStartsWith: the other half of the miss. A
// short argument that matches nothing must not be reported as a memory that was
// never written — it is a prefix, and no id starts with it, which is a different
// claim with a different remedy. The ref below the floor is refused the same way,
// and refused WITHOUT a match list, because printing one turns the refusal into a
// dump of the ids a caller could not otherwise enumerate.
func TestResolveHistoryRefSaysAPrefixNoIdStartsWith(t *testing.T) {
	s, live, _ := refTestStore(t)
	ctx := context.Background()

	t.Run("eight characters, matching nothing", func(t *testing.T) {
		_, err := resolveHistoryRef(ctx, s, "F0F0F0F0")
		if err == nil {
			t.Fatal("a prefix no id starts with was resolved to something")
		}
		if !strings.Contains(err.Error(), "starting with") {
			t.Errorf("the refusal does not say the ref is a prefix no id starts with: %v", err)
		}
		if !strings.Contains(err.Error(), "F0F0F0F0") {
			t.Errorf("the refusal does not name the ref: %v", err)
		}
	})

	t.Run("below the floor", func(t *testing.T) {
		_, err := resolveHistoryRef(ctx, s, "F0F0F0")
		if err == nil {
			t.Fatal("a three-character ref was accepted")
		}
		if !strings.Contains(err.Error(), "too short to be a prefix") {
			t.Errorf("the refusal does not say the ref is too short to be a prefix: %v", err)
		}
		// Every id in the store shares nothing with this ref, so the listing to
		// forbid is the one a wide match would produce. Asserted against the ids
		// themselves: a refusal that quoted one would hand out part of the set.
		if strings.Contains(err.Error(), live) {
			t.Errorf("the refusal listed an id, turning it into a dump of the set: %v", err)
		}
	})
}

// TestResolveHistoryRefPassesAFullIDItCannotFindStraightThrough: a full id the
// store does not hold is not a prefix miss, and it is not this command's error to
// raise. `ghost history <full id>` has always reported it as "never written, or
// its history has been pruned", and a caller that has the whole id has said
// everything there is to say — there is no more identity to ask for. Routing it
// through the prefix rules would report the id as a PREFIX of nothing, which is a
// claim about a string the caller did not shorten.
//
// The distinction is made by asking the store, not by measuring the argument: what
// separates "the whole id" from "part of one" is whether THIS store holds an id
// beginning with the ref, and nothing about a string's length can answer that.
// `ghost import` writes an artifact's ids verbatim, so a store full of imported
// notes holds eight-character ids, and those eight characters ARE a whole id.
func TestResolveHistoryRefPassesAFullIDItCannotFindStraightThrough(t *testing.T) {
	s, _, _ := refTestStore(t)
	ctx := context.Background()

	// A full-length id nothing was ever written under. Longer than the store's own
	// ids, which is the case that must NOT be treated as a truncation of some other
	// id: no id in this store begins with it, and a ref no id begins with is a whole
	// id this store does not hold, whatever its length.
	const never = "0000000000000000000000000000000FFF"
	if got, err := s.AnyMemoryIDsByIDPrefix(ctx, never); err != nil {
		t.Fatalf("AnyMemoryIDsByIDPrefix: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("the fixture is not absent: %q", got)
	}
	got, err := resolveHistoryRef(ctx, s, never)
	if err != nil {
		t.Fatalf("a full id the store does not hold was refused: %v", err)
	}
	if got != never {
		t.Errorf("resolveHistoryRef(%q) = %q, want it unchanged", never, got)
	}
	// And the report still answers it the way it always has, rather than with a
	// prefix claim.
	view, err := readHistoryView(ctx, s, got, 0)
	if err != nil {
		t.Fatalf("readHistoryView: %v", err)
	}
	var buf strings.Builder
	if err := printMemoryHistory(&buf, view); err != nil {
		t.Fatalf("printMemoryHistory: %v", err)
	}
	if !strings.Contains(buf.String(), "never written") {
		t.Errorf("the report changed its answer for an id it does not hold:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "starting with") {
		t.Errorf("the report called a full id a prefix:\n%s", buf.String())
	}
}

// TestResolveHistoryRefRefusesAnEmptyRef: parseHistoryArgs requires an id, so this
// is the seam's own floor rather than the parser's — and a caller reaching it must
// not be handed the first id in the store.
func TestResolveHistoryRefRefusesAnEmptyRef(t *testing.T) {
	s, _, _ := refTestStore(t)
	if _, err := resolveHistoryRef(context.Background(), s, ""); err == nil {
		t.Fatal("an empty ref resolved to an id")
	}
}

// TestResolveHistoryRefRefusesAnAmbiguousWholeLengthRef: the full-id pass-through
// is a pass-through on a MISS, not on every refusal. `ghost import` writes an
// artifact's ids verbatim, so a store can hold two 40-character ids sharing 32
// characters — and a gate on LENGTH alone would let that through, hand the read an
// id in neither table, and print "never written" about two memories the store
// plainly holds. The reader cannot tell that from the truth, so the ambiguity has
// to reach them the same way a shorter ambiguous prefix does: refused, with the
// matches named.
func TestResolveHistoryRefRefusesAnAmbiguousWholeLengthRef(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// Two ids LONGER than a minted full id, sharing a 32-character prefix. The
	// shape is reachable: an artifact may hold ids of any length and any case.
	const (
		first  = "AABBCCDDEEFF0011223344556677889900AA"
		second = "AABBCCDDEEFF0011223344556677889900BB"
	)
	for _, id := range []string{first, second} {
		if _, _, _, err := s.ImportMemory(ctx, memory.PortableMemory{
			ID: id, ProjectID: "p", Category: "fact", Content: "a memory behind a long shared prefix", Source: "mcp",
		}, memory.ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
			t.Fatalf("ImportMemory(%s): %v", id, err)
		}
	}
	// The prefix is 32 characters, so a length gate would wave it through.
	shared := first[:memref.FullIDLen]
	if got := utf8.RuneCountInString(shared); got != memref.FullIDLen {
		t.Fatalf("the fixture's shared prefix is %d characters, not the %d a length gate tests", got, memref.FullIDLen)
	}

	_, err = resolveHistoryRef(ctx, s, shared)
	if err == nil {
		t.Fatal("a whole-length ref naming two memories was passed through as a whole id")
	}
	// NOT a miss, which is the whole finding: a caller branching on ErrNoMatch
	// passes a genuine miss through to the read's own sentence and refuses this.
	if errors.Is(err, memref.ErrNoMatch) {
		t.Errorf("an ambiguous whole-length ref reports itself as a miss: %v", err)
	}
	for _, want := range []string{first, second, "more characters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q: %v", want, err)
		}
	}
}

// TestRunHistoryPurgePurgesTheStoredSpellingOfAFoldedRef: ids are matched
// case-insensitively everywhere a ref is resolved, and a report can print a hex id
// uppercased whatever the column holds. Every read a purge makes after the
// whole-id gate compares case-SENSITIVELY — neither `memories.id` nor
// `memory_history.memory_id` carries COLLATE NOCASE — so a gate that returned a
// boolean would accept the folded spelling and then erase nothing, reporting
// "nothing to purge" on the redaction path while the text sat in the database.
func TestRunHistoryPurgePurgesTheStoredSpellingOfAFoldedRef(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// A lower-case id, so the caller has to spell it in another case to reach it.
	const id = "abcdef0123456789abcdef0123456789"
	if _, _, _, err := s.ImportMemory(ctx, memory.PortableMemory{
		ID: id, ProjectID: "p", Category: "fact", Content: "a secret in a memory nobody must read", Source: "mcp",
	}, memory.ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}

	if err := purgeHistoryMemory(ctx, s, strings.ToUpper(id)); err != nil {
		t.Fatalf("a case-folded whole id was refused: %v", err)
	}
	if rows, err := s.GetByIDs(ctx, []string{id}); err != nil {
		t.Fatalf("GetByIDs: %v", err)
	} else if len(rows) != 0 {
		t.Error("the purge left the live row")
	}
	if entries, err := s.MemoryHistory(ctx, id, 0); err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("the purge left %d recorded version(s) of a memory the store still names as %s", len(entries), id)
	}
}

// TestRunHistoryPurgeRefusesWhenTwoIdsDifferOnlyInCase: whether a ref IS a stored
// id is a ref rule, and the purge gate used to re-derive it — a case-insensitive
// scan taking the first match. `ghost import` writes an artifact's ids verbatim and
// its presence probe is case-sensitive, so a store can legitimately hold both
// `ABC…` and `abc…`, and SQLite's BINARY collation sorts the upper case first. The
// scan would then hand the purge the wrong memory: a THIRD casing destroys `ABC…`'s
// row and recorded text irreversibly, while the memory the operator named keeps its
// secret. The read path gets this right through memref; the gate is the one place
// that must not re-derive the rule.
func TestRunHistoryPurgeRefusesWhenTwoIdsDifferOnlyInCase(t *testing.T) {
	s, ctx := casePairStore(t)
	const (
		upper = "ABCDEF0123456789ABCDEF0123456789"
		lower = "abcdef0123456789abcdef0123456789"
	)

	// A third casing reaches NEITHER stored id, and memref refuses it because of
	// that. A gate that did not would take whichever sort came first and erase it.
	err := purgeHistoryMemory(ctx, s, "AbcDef0123456789abcdef0123456789")
	if err == nil {
		t.Fatal("a third casing of two stored spellings was purged as though it named one")
	}
	for _, want := range []string{upper, lower} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, so the reader cannot choose: %v", want, err)
		}
	}
	for _, id := range []string{upper, lower} {
		if entries, err := s.MemoryHistory(ctx, id, 0); err != nil {
			t.Fatalf("MemoryHistory(%s): %v", id, err)
		} else if len(entries) == 0 {
			t.Fatalf("a refused purge erased the recorded text of %s", id)
		}
	}

	// Each stored spelling is then addressable, one row each — byte-exact beats the
	// fold, so the operator's own spelling is decisive rather than a guess about
	// which of two rows they meant.
	if err := purgeHistoryMemory(ctx, s, upper); err != nil {
		t.Fatalf("purging the stored upper-case id: %v", err)
	}
	if entries, err := s.MemoryHistory(ctx, lower, 0); err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	} else if len(entries) == 0 {
		t.Error("purging the upper-case id erased the lower-case memory's text too")
	}
}

// TestRunHistoryPurgeTakesTheStoredSpellingOverTheCallers: the byte-exact match
// wins and it is one row, so the caller's own spelling is also the right one — and
// when the store holds a single spelling in another case, that spelling is what the
// case-sensitive reads below need.
func TestRunHistoryPurgeTakesTheStoredSpellingOverTheCallers(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	const id = "abcdef0123456789abcdef0123456789"
	if _, _, _, err := s.ImportMemory(ctx, memory.PortableMemory{
		ID: id, ProjectID: "p", Category: "fact", Content: "the only spelling this store holds", Source: "mcp",
	}, memory.ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}

	if err := purgeHistoryMemory(ctx, s, strings.ToUpper(id)); err != nil {
		t.Fatalf("a case-folded whole id was refused: %v", err)
	}
	if rows, err := s.GetByIDs(ctx, []string{id}); err != nil {
		t.Fatalf("GetByIDs: %v", err)
	} else if len(rows) != 0 {
		t.Error("the purge left the live row")
	}
}

// TestRunHistoryPurgeReportsAFailedIDReadAsAFailure: the gate reads the id set
// before it can decide anything, and a read that fails has told us nothing about
// the argument. Folding that into "not a whole id" would report a prefix to an
// operator whose id was already correct — a wrong answer, in the one place where
// being wrong has consequences nobody can undo. A closed database is a real read
// failure, and runHistory's own os.Exit is not reachable from a test.
func TestRunHistoryPurgeReportsAFailedIDReadAsAFailure(t *testing.T) {
	s, _, _ := refTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	err := purgeHistoryMemory(context.Background(), s, refTestLiveID)
	if err == nil {
		t.Fatal("a failed id read was reported as a decision")
	}
	// Not the prefix refusal — that sentence sends the operator after a full id they
	// may already have, and it would be a claim about an argument nobody examined.
	if strings.Contains(err.Error(), "whole memory id") {
		t.Errorf("a failed read was reported as a prefix: %v", err)
	}
	if !strings.Contains(err.Error(), "against this store") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

// casePairStore holds two memories whose ids differ only in letter case, which is
// the shape `ghost import` can produce and the one a first-match scan gets wrong.
func casePairStore(t *testing.T) (*memory.Store, context.Context) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, f := range []struct{ id, content string }{
		{"ABCDEF0123456789ABCDEF0123456789", "a memory whose id is stored in upper case"},
		{"abcdef0123456789abcdef0123456789", "a memory whose id is stored in lower case"},
	} {
		if _, _, _, err := s.ImportMemory(ctx, memory.PortableMemory{
			ID: f.id, ProjectID: "p", Category: "fact", Content: f.content, Source: "mcp",
		}, memory.ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
			t.Fatalf("ImportMemory(%s): %v", f.id, err)
		}
	}
	return s, ctx
}

// TestReportHistoryRefusalUsesTheFormTheCallerAskedFor: a --json run's every line
// is otherwise an entry, so a refusal has to be the one `{"error": ...}` object a
// script can branch on — on stdout, because that is where the stream is, with
// stderr left quiet. The human form is a plain diagnostic on stderr. A --json run
// that put a refusal on stderr, or printed it as an entry, would break both
// readings at once.
func TestReportHistoryRefusalUsesTheFormTheCallerAskedFor(t *testing.T) {
	const message = "the id ref \"a1b2c3d4\" is ambiguous: pass more characters of the id to choose one"

	var out, errOut strings.Builder
	if err := reportHistoryRefusal(&out, &errOut, true, message); err != nil {
		t.Fatalf("reportHistoryRefusal(--json): %v", err)
	}
	var got struct {
		Error string `json:"error"`
	}
	if jsonErr := json.Unmarshal([]byte(out.String()), &got); jsonErr != nil {
		t.Fatalf("the --json refusal is not one JSON object: %v\n%s", jsonErr, out.String())
	}
	if got.Error != message {
		t.Errorf("--json refusal = %q, want %q", got.Error, message)
	}
	if errOut.String() != "" {
		t.Errorf("the --json refusal also wrote to stderr, so a script reading the stream sees it twice: %q", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if err := reportHistoryRefusal(&out, &errOut, false, message); err != nil {
		t.Fatalf("reportHistoryRefusal: %v", err)
	}
	if out.String() != "" {
		t.Errorf("the human refusal wrote to stdout, where a history is printed: %q", out.String())
	}
	if !strings.Contains(errOut.String(), message) {
		t.Errorf("the human refusal is not on stderr: %q", errOut.String())
	}
}

// TestReportHistoryRefusalReportsAWriteFailure: a failed write on the stdout path
// is returned rather than dropped, because the caller exits non-zero on it and a
// silently dropped error would make a refused --json run look like a clean empty
// stream — the one answer a script cannot act on. failingWriter fails its first
// write, which is the refusal object on the --json path.
func TestReportHistoryRefusalReportsAWriteFailure(t *testing.T) {
	if err := reportHistoryRefusal(&failingWriter{failOn: 1}, &strings.Builder{}, true, "x"); err == nil {
		t.Fatal("a failed write was reported as a printed refusal")
	}
}
