package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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

// TestResolveHistoryRefPassesAFullIDItCannotFindStraightThrough: a full id the
// store does not hold is not a prefix miss, and it is not this command's error to
// raise. `ghost history <full id>` has always reported it as "never written, or
// its history has been pruned", and a caller that has the whole id has said
// everything there is to say — there is no more identity to ask for. Routing it
// through the prefix rules would report the id as a PREFIX of nothing, which is a
// claim about a string the caller did not shorten.
func TestResolveHistoryRefPassesAFullIDItCannotFindStraightThrough(t *testing.T) {
	s, _, _ := refTestStore(t)
	ctx := context.Background()

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
