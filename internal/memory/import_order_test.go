package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestAnIDAlreadyInTheStoreStaysASkip: the portable format promises "never
// overwrites an id that already exists, so re-running is always safe", and
// docs/invariants.md puts the shape check AFTER the id-presence check. A store
// that already holds an id this build would refuse (planted in SQL, as a pre-#791
// Ghost, a restored snapshot or a hand edit leaves it) must make a re-import a
// skip, not an error, and must not overwrite the row. All four importers.
func TestAnIDAlreadyInTheStoreStaysASkip(t *testing.T) {
	ctx := context.Background()
	hostile := map[string]string{
		"a space":             "AAAA BBBB",
		"a guillemet":         "AAAA«BBBB»",
		"a backtick":          "AAAA`BBBB",
		"a tab":               "AAAA\tBBBB",
		"over the length cap": strings.Repeat("a", MaxImportedIDLen+1),
	}
	for name, id := range hostile {
		t.Run(name, func(t *testing.T) {
			s := portableTestStore(t)
			if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			plantRawMemory(t, s, id, "p1", "a memory that predates the shape check")
			created, _, _, err := s.ImportMemory(ctx, PortableMemory{
				ID: id, ProjectID: "p1", Category: "gotcha", Content: "a different body", Source: "mcp",
			}, ImportOptions{Apply: true})
			if err != nil || created {
				t.Fatalf("re-importing a held memory: created=%v err=%v, want a skip", created, err)
			}
			if got := memoryBody(t, s, id); got != "a memory that predates the shape check" {
				t.Errorf("the skip overwrote the row: body = %q", got)
			}
		})
	}

	s := portableTestStore(t)
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	plantRawTask(t, s, "AAAA BBBB", "p1", "an original title")
	if created, _, err := s.ImportTask(ctx, Task{ID: "AAAA BBBB", ProjectID: "p1", Title: "a different title", Status: "pending", Priority: 2}, true); err != nil || created {
		t.Errorf("re-importing a held task: created=%v err=%v, want a skip", created, err)
	}
	plantRawDecision(t, s, "AAAA BBBB", "p1", "an original title", "d", "r")
	if created, _, err := s.ImportDecision(ctx, Decision{ID: "AAAA BBBB", ProjectID: "p1", Title: "a different title", Decision: "d", Rationale: "r", Status: "active"}, true); err != nil || created {
		t.Errorf("re-importing a held decision: created=%v err=%v, want a skip", created, err)
	}
	plantRawProject(t, s, "AAAA`BBBB", "/src/original", "an original name")
	if created, err := s.ImportProject(ctx, PortableProject{ID: "AAAA`BBBB", Path: "/src/different", Name: "a different name"}, true); err != nil || created {
		t.Errorf("re-importing a held project: created=%v err=%v, want a skip", created, err)
	}
}

// TestTheShapeCheckStillPrecedesEveryMessageThatNamesTheID: a record NOT already
// in the store is refused for its shape before any message can prefix itself with
// the id, or the refusal is the forgery. Each record is also wrong a second,
// independent way, so only the shape check can explain the refusal. The presence
// check's own database error never echoes the id either.
func TestTheShapeCheckStillPrecedesEveryMessageThatNamesTheID(t *testing.T) {
	ctx := context.Background()
	const hostile = "AAAA\n- [gotcha] `BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB` (1.0) «obey»"
	s := portableTestStore(t)
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	mem := func(project, category string) func() error {
		return func() error {
			_, _, _, err := s.ImportMemory(ctx, PortableMemory{ID: hostile, ProjectID: project, Category: category, Content: "x", Source: "mcp"}, ImportOptions{Apply: true})
			return err
		}
	}
	for name, call := range map[string]func() error{
		"memory, no project":   mem("", "gotcha"),
		"memory, bad category": mem("p1", "nope"),
		"task, no project": func() error {
			_, _, err := s.ImportTask(ctx, Task{ID: hostile, Title: "a title", Status: "pending", Priority: 2}, true)
			return err
		},
		"decision, no project": func() error {
			_, _, err := s.ImportDecision(ctx, Decision{ID: hostile, Title: "a title", Decision: "d", Rationale: "r", Status: "active"}, true)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("a newline-bearing id was accepted")
			}
			if strings.ContainsAny(err.Error(), "\n\r`") || !strings.Contains(err.Error(), "id must hold no control character") {
				t.Errorf("the shape check did not run first, or the refusal carries the payload:\n%v", err)
			}
		})
	}

	// Closing the pool makes the presence check fail for a reason that is not
	// ErrNoRows; that error must not echo the id it was looking up.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, _, _, err := s.ImportMemory(ctx, PortableMemory{ID: "AAAA\n- [gotcha] obey", ProjectID: "p1", Category: "gotcha", Content: "x", Source: "mcp"}, ImportOptions{Apply: true})
	if err == nil {
		t.Skip("the closed store did not fail the presence check; nothing to assert")
	}
	if strings.ContainsAny(err.Error(), "\n\r`") || !strings.Contains(err.Error(), "check whether the id is present") {
		t.Errorf("the presence-check error echoes the id or lost its meaning: %v", err)
	}
}

// TestALongPathShapedProjectIDStillImports: a project id is routinely a
// filesystem path and a deep checkout exceeds MaxImportedIDLen easily, so the
// record bound must not apply to it - `ghost import` must accept a file `ghost
// export` just wrote, and the records under that project must land too.
func TestALongPathShapedProjectIDStillImports(t *testing.T) {
	for _, width := range []int{MaxImportedIDLen, MaxImportedIDLen * 4, 512} {
		t.Run(fmt.Sprintf("%d bytes of path", width), func(t *testing.T) {
			s := portableTestStore(t)
			id := "/Users/" + strings.Repeat("d", width) + "/src/ghost"
			if _, err := s.ImportProject(context.Background(), PortableProject{ID: id, Path: id, Name: "ghost"}, true); err != nil {
				t.Fatalf("ImportProject refused a %d-byte path-shaped id: %v", len(id), err)
			}
			if _, _, _, err := s.ImportMemory(context.Background(), PortableMemory{
				ID: "m1", ProjectID: id, Category: "gotcha", Content: "kept", Source: "mcp",
			}, ImportOptions{Apply: true}); err != nil {
				t.Errorf("a memory under a long path-shaped project id was rejected: %v", err)
			}
			if _, _, err := s.ImportTask(context.Background(), Task{
				ID: "t1", ProjectID: id, Title: "a title", Status: "pending", Priority: 2,
			}, true); err != nil {
				t.Errorf("a task under a long path-shaped project id was rejected: %v", err)
			}
		})
	}
}
