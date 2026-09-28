package memory

import (
	"context"
	"strings"
	"testing"
)

// TestCreateWithIDStoresTheCallersID: the seam exists so a benchmark can make a
// store's contents a function of its corpus rather than of randomblob, and that
// only works if the caller's id is what the row carries — through the whole
// write, not just the memories row. The history and evidence rows are written in
// the same transaction from the same id, so they are where a partial fix would
// show: a row whose own id is the caller's while its history names a different
// one is a store the trace cannot explain.
func TestCreateWithIDStoresTheCallersID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const want = "bench:graded:node_port"

	id, err := s.CreateWithID(ctx, testProject, want, Memory{
		Category: "fact", Content: "the node port is 3001", Importance: 0.7, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	if id != want {
		t.Fatalf("CreateWithID returned %q, want the caller's id %q", id, want)
	}

	all, err := s.GetAll(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d memories, want 1", len(all))
	}
	if all[0].ID != want {
		t.Errorf("the stored row carries id %q, want %q", all[0].ID, want)
	}
	for _, table := range []string{"memory_history", "memory_provenance"} {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE memory_id = ?", want).Scan(&n); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		if n == 0 {
			t.Errorf("%s has no row for %q: the write recorded the row under a different id", table, want)
		}
	}
}

// TestCreateWithIDRefusesAnEmptyID: an empty id is refused rather than quietly
// falling back to the column default. The fallback is the right behaviour for
// Create, which is the writer that wants a random id, and the wrong one here: a
// caller that asked for a named row and got a random one would never find out,
// and the whole reason it asked is that the id has to be the same on every run.
func TestCreateWithIDRefusesAnEmptyID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.CreateWithID(ctx, testProject, "", Memory{Category: "fact", Content: "no id asked for", Source: "mcp"})
	if err == nil {
		t.Fatalf("CreateWithID with an empty id stored the row as %q", id)
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("the refusal does not name the id it was given: %v", err)
	}
	if n, err := s.CountMemories(ctx, testProject); err != nil {
		t.Fatalf("CountMemories: %v", err)
	} else if n != 0 {
		t.Errorf("a refused write stored %d memories", n)
	}
}

// TestCreateWithIDStillGuardsCredentials: the seam chooses the row's id, not
// whether the row is allowed. Every check Create applies is applied here, so a
// bench corpus cannot become a way to write a token into a store that Create
// would have refused.
func TestCreateWithIDStillGuardsCredentials(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.CreateWithID(ctx, testProject, "bench:graded:secret", Memory{
		Category: "fact", Content: "token gho_0123456789abcdef0123456789abcdef0123", Source: "mcp",
	})
	if err == nil {
		t.Fatalf("CreateWithID stored a credential as %q", id)
	}
	if n, err := s.CountMemories(ctx, testProject); err != nil {
		t.Fatalf("CountMemories: %v", err)
	} else if n != 0 {
		t.Errorf("a guarded write stored %d memories", n)
	}
}

// TestCreateWithIDRefusesAnIDAlreadyInUse: the id is a primary key, so a second
// row claiming one is an error rather than a second copy of the row. This is what
// makes a corpus-derived id safe to build from the item's own name: a fixture
// that claims the same name twice fails the seed loudly instead of silently
// halving the corpus.
func TestCreateWithIDRefusesAnIDAlreadyInUse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const id = "bench:graded:pledge"

	if _, err := s.CreateWithID(ctx, testProject, id, Memory{Category: "fact", Content: "the pledge is 5000", Source: "mcp"}); err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	_, err := s.CreateWithID(ctx, testProject, id, Memory{Category: "fact", Content: "a second row claiming the same id", Source: "mcp"})
	if err == nil {
		t.Fatal("a second row was stored under an id already in use")
	}
	if n, err := s.CountMemories(ctx, testProject); err != nil {
		t.Fatalf("CountMemories: %v", err)
	} else if n != 1 {
		t.Errorf("got %d memories, want 1: the duplicate replaced or added a row", n)
	}
}

// TestCreateDrawsItsOwnIDFromTheColumn: Create takes no id, and it must keep
// taking none — the id column's default is what mints it, and that is also what
// every other writer in the schema relies on. The explicit-id seam is threaded
// through the same INSERT, so this is the pin on the branch that would take
// production's id generation down with it.
func TestCreateDrawsItsOwnIDFromTheColumn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "a memory created without an id", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "another one, same shape", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first == second {
		t.Fatalf("Create minted %q for both rows: the id no longer comes from the column default", first)
	}
	if first == "" || second == "" {
		t.Fatalf("Create returned ids %q and %q", first, second)
	}
}
