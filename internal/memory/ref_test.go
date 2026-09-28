package memory

import (
	"context"
	"testing"
)

// TestAnyMemoryIDsByIDPrefixReachesEveryIdAMemoryHasEverHeld pins the id set a
// `ghost history` ref resolves against: every id memory_history records — the
// deleted ones included, since the tombstone is the whole reason to read a
// history — and every id still live, across every project.
//
// The two halves are both load-bearing and the second one is the easy miss. A
// memory that predates the history table has NO history row at all: migrateV17
// deliberately does not backfill, so `ghost history <full id>` answers "nothing
// has written it since this store reached schema v17" while an id-set read of
// memory_history alone would report that no id starts with its first eight
// characters — a memory the store plainly holds, named as though it were never
// written. The live half is a direct INSERT for exactly that reason.
//
// The project-free scope is pinned here too, and it is a decision rather than an
// oversight: `ghost history` has no project operand and a full id has always
// reached any row in the store, so a PREFIX confined to one project would make
// the two forms of one ref disagree about where a memory may be looked for. The
// prefix rules still refuse anything ambiguous, and the whole store is one
// answer to that question.
func TestAnyMemoryIDsByIDPrefixReachesEveryIdAMemoryHasEverHeld(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "other-project", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	live := mustCreate(t, s, testProject, "a live memory whose id every report shortens")
	// Several writes, so the id stands behind several history rows: a set that
	// listed one row per write would report the id twice and a prefix naming it
	// would be refused as ambiguous — the memory's own history would make it
	// unaddressable.
	for _, content := range []string{"an edit of the live memory", "a second edit of the live memory"} {
		if err := s.UpdateMemory(ctx, testProject, live, &content, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
	}

	gone := mustCreate(t, s, testProject, "a memory deleted on purpose, whose text the history keeps")
	if err := s.Delete(ctx, gone); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// A pre-history memory: a live row with no history row, which is what every
	// memory carried into schema v17 is until something writes to it.
	const preHistory = "AABBCCDDEEFF00112233445566778899"
	if _, err := s.db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source)
		 VALUES (?, ?, 'fact', 'a memory from before the history table', 'mcp')`,
		preHistory, testProject); err != nil {
		t.Fatalf("seed the pre-v17 row: %v", err)
	}

	// Another project's memory. The history report is store-wide, because the
	// command has no project operand and always has been able to read any id in
	// the store; scoping a PREFIX to a project while a full id reached every
	// project would make the two forms of one ref disagree about the store.
	theirs := mustCreate(t, s, "other-project", "a memory in a project the reader did not name")

	for _, tc := range []struct {
		what string
		id   string
	}{
		{"live", live},
		{"deleted", gone},
		{"pre-history", preHistory},
		{"another project's", theirs},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := s.AnyMemoryIDsByIDPrefix(ctx, tc.id[:8])
			if err != nil {
				t.Fatalf("AnyMemoryIDsByIDPrefix(%q): %v", tc.id[:8], err)
			}
			if len(got) != 1 || got[0] != tc.id {
				t.Fatalf("AnyMemoryIDsByIDPrefix(%q) = %q, want [%s]", tc.id[:8], got, tc.id)
			}
		})
	}

	t.Run("one id behind many history rows is named once", func(t *testing.T) {
		if n := historyRowCount(t, s, live); n < 3 {
			t.Fatalf("the fixture recorded %d history row(s) for the live id, want at least 3", n)
		}
		got, err := s.AnyMemoryIDsByIDPrefix(ctx, live[:8])
		if err != nil {
			t.Fatalf("AnyMemoryIDsByIDPrefix: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("AnyMemoryIDsByIDPrefix(%q) = %q, want the id once", live[:8], got)
		}
	})

	t.Run("a full id and a fold match", func(t *testing.T) {
		for _, ref := range []string{live, upperHex(live)} {
			got, err := s.AnyMemoryIDsByIDPrefix(ctx, ref)
			if err != nil {
				t.Fatalf("AnyMemoryIDsByIDPrefix(%q): %v", ref, err)
			}
			if len(got) != 1 || got[0] != live {
				t.Errorf("AnyMemoryIDsByIDPrefix(%q) = %q, want [%s]", ref, got, live)
			}
		}
	})

	t.Run("literal text, not a LIKE pattern", func(t *testing.T) {
		if got, err := s.AnyMemoryIDsByIDPrefix(ctx, live[:4]+"%"); err != nil {
			t.Fatalf("AnyMemoryIDsByIDPrefix: %v", err)
		} else if len(got) != 0 {
			t.Errorf("AnyMemoryIDsByIDPrefix matched %q for a ref holding a LIKE wildcard", got)
		}
	})

	t.Run("a miss is an empty slice and no error", func(t *testing.T) {
		got, err := s.AnyMemoryIDsByIDPrefix(ctx, "ffffffff")
		if err != nil {
			t.Fatalf("AnyMemoryIDsByIDPrefix(miss): %v", err)
		}
		if len(got) != 0 {
			t.Errorf("AnyMemoryIDsByIDPrefix(miss) = %q, want empty", got)
		}
	})
}

// TestAnyMemoryIDsByIDPrefixMeasuresTheBoundInCharacters: the id column is TEXT
// and SQLite's substr() slices by CHARACTER, so a byte-counted bound compares a
// multi-byte ref against the first N characters of every id and can never match —
// refusing an id the store holds. The hex ids Ghost mints have byte length ==
// rune count, so only an imported id can catch it.
func TestAnyMemoryIDsByIDPrefixMeasuresTheBoundInCharacters(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const japanese = "日本語-メモ"
	if _, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID: japanese, ProjectID: testProject, Category: "fact",
		Content: "An imported note whose id is not ASCII.", Source: "mcp",
	}, ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	for _, prefix := range []string{japanese, "日本語"} {
		got, err := s.AnyMemoryIDsByIDPrefix(ctx, prefix)
		if err != nil {
			t.Fatalf("AnyMemoryIDsByIDPrefix(%q): %v", prefix, err)
		}
		if len(got) != 1 || got[0] != japanese {
			t.Errorf("AnyMemoryIDsByIDPrefix(%q) = %q, want [%s]", prefix, got, japanese)
		}
	}
}

// historyRowCount counts the history rows behind one id, so a fixture can prove
// the case that distinguishes a distinct id set from a row-per-write one.
func historyRowCount(t *testing.T, s *Store, memoryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, memoryID).Scan(&n); err != nil {
		t.Fatalf("count history rows for %s: %v", memoryID, err)
	}
	return n
}
