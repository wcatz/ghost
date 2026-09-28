package memory

import (
	"context"
	"testing"
)

// linkTestStore is a store with one project, used by the link-withdraw reads.
func linkTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(db, nil)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureProject(ctx, "p2", "/tmp/p2", "p2"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureProject(ctx, GlobalProjectID, "", "_global"); err != nil {
		t.Fatal(err)
	}
	return s
}

func mustCreate(t *testing.T, s *Store, projectID, content string) string {
	t.Helper()
	id, err := s.Create(context.Background(), projectID, Memory{Category: "fact", Content: content, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(%s): %v", projectID, err)
	}
	return id
}

// TestMemoryIDsByIDPrefixIsProjectScopedAndLiteral pins the resolution half of a
// link withdrawal: a ref is a prefix of a memory id in THIS project (or in
// _global, which a promotion moves a memory into without touching its links), it
// is matched literally rather than as a LIKE pattern, and it is matched
// case-insensitively the way memIDKey already compares ids.
func TestMemoryIDsByIDPrefixIsProjectScopedAndLiteral(t *testing.T) {
	s := linkTestStore(t)
	ctx := context.Background()
	mine := mustCreate(t, s, "p1", "The restore path on one spindle is safe.")
	other := mustCreate(t, s, "p2", "The restore path on two spindles is not.")
	global := mustCreate(t, s, GlobalProjectID, "A global note about restores.")

	// A promoted memory keeps its links, so a live edge can point INTO _global
	// from a project memory; the target's ref has to resolve there too.
	got, err := s.MemoryIDsByIDPrefix(ctx, "p1", global[:8])
	if err != nil {
		t.Fatalf("MemoryIDsByIDPrefix(global): %v", err)
	}
	if len(got) != 1 || got[0] != global {
		t.Errorf("a _global memory must be reachable from the project that links it: got %v, want [%s]", got, global)
	}

	// Another project's memory is not: the prefix is scoped, so a ref cannot
	// name a memory this project has no business touching.
	got, err = s.MemoryIDsByIDPrefix(ctx, "p1", other[:8])
	if err != nil {
		t.Fatalf("MemoryIDsByIDPrefix(other): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("MemoryIDsByIDPrefix returned another project's memory: %v", got)
	}

	// Case-insensitive, the way memIDKey compares ids: an id copied out of a
	// report in another case still resolves.
	got, err = s.MemoryIDsByIDPrefix(ctx, "p1", upperHex(mine[:8]))
	if err != nil {
		t.Fatalf("MemoryIDsByIDPrefix(upper): %v", err)
	}
	if len(got) != 1 || got[0] != mine {
		t.Errorf("uppercase prefix = %v, want [%s]", got, mine)
	}

	// A ref is a PREFIX, not a pattern: the two SQL wildcards are matched
	// literally, so neither can widen the search.
	for _, wildcard := range []string{"%", "_"} {
		got, err = s.MemoryIDsByIDPrefix(ctx, "p1", wildcard)
		if err != nil {
			t.Fatalf("MemoryIDsByIDPrefix(%q): %v", wildcard, err)
		}
		if len(got) != 0 {
			t.Errorf("MemoryIDsByIDPrefix(%q) = %v, want no match: a ref is literal text, not a LIKE pattern", wildcard, got)
		}
	}

	// A prefix of nothing is an empty answer, not an error — the caller decides
	// what a miss means.
	got, err = s.MemoryIDsByIDPrefix(ctx, "p1", "ffffffff")
	if err != nil {
		t.Fatalf("MemoryIDsByIDPrefix(miss): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("MemoryIDsByIDPrefix(miss) = %v, want empty", got)
	}
}

// TestMemoryIDsByIDPrefixIsOrdered pins that the answer is a function of the
// stored data, not of the row order the query happened to return: an ambiguity
// report lists the matches, and a list whose order moved between two calls on
// the same data is a report that cannot be compared.
func TestMemoryIDsByIDPrefixIsOrdered(t *testing.T) {
	s := linkTestStore(t)
	ctx := context.Background()
	// Two memories whose ids share a prefix is not something the store can force
	// (ids are random), so the ordering is pinned over the whole project
	// instead: the returned ids must be sorted.
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, mustCreate(t, s, "p1", "Note number "+string(rune('a'+i))))
	}
	got, err := s.MemoryIDsByIDPrefix(ctx, "p1", "")
	if err != nil {
		t.Fatalf("MemoryIDsByIDPrefix: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("MemoryIDsByIDPrefix(\"\") = %d id(s), want %d", len(got), len(ids))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("ids are not in ascending order: %v", got)
		}
	}
}

// TestSupersedesLinksInto pins the read a withdrawal decides on. It returns the
// live 'supersedes' edges pointing INTO a memory, restricted to the edges the
// project owns through their SOURCE endpoint — the same scoping
// LinksByRelationSource applies to every other supersede reader, so a project
// can neither withdraw nor learn about another project's edge, and it excludes
// an edge the target itself is the source of.
func TestSupersedesLinksInto(t *testing.T) {
	s := linkTestStore(t)
	ctx := context.Background()
	newer := mustCreate(t, s, "p1", "The ingest service now runs Redis 7.2.")
	older := mustCreate(t, s, "p1", "The ingest service runs Redis 6.2.")
	cause := mustCreate(t, s, "p1", "A note the newer one merely cites.")
	foreignNewer := mustCreate(t, s, "p2", "Another project's replacement note.")
	foreignOlder := mustCreate(t, s, "p2", "Another project's stale note.")
	dropped := mustCreate(t, s, "p1", "A pair whose edge was already withdrawn.")

	mustLink(t, s, newer, older, "supersedes", 0.95, "llm")
	mustLink(t, s, cause, older, "causes", 0.9, "llm")
	mustLink(t, s, foreignNewer, foreignOlder, "supersedes", 0.95, "llm")
	mustLink(t, s, newer, dropped, "supersedes", 0.95, "llm")
	if n, err := s.InvalidateLink(ctx, newer, dropped, "supersedes"); err != nil || n != 1 {
		t.Fatalf("InvalidateLink = %d, %v; want 1 and no error", n, err)
	}

	links, err := s.SupersedesLinksInto(ctx, "p1", older)
	if err != nil {
		t.Fatalf("SupersedesLinksInto: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("SupersedesLinksInto = %+v, want exactly the one live supersedes edge into the target", links)
	}
	if links[0].SourceID != newer || links[0].TargetID != older || links[0].Relation != "supersedes" || links[0].Source != "llm" {
		t.Errorf("SupersedesLinksInto returned %+v, want the %s→%s llm supersedes edge", links[0], newer, older)
	}
	if links[0].InvalidatedAt != nil {
		t.Errorf("SupersedesLinksInto returned an invalidated edge: %+v", links[0])
	}

	// A target that is itself the source of an edge has no live edge pointing
	// at it from that direction, and the other project's edge into its own
	// target is not visible from p1 at all.
	links, err = s.SupersedesLinksInto(ctx, "p1", newer)
	if err != nil {
		t.Fatalf("SupersedesLinksInto(source endpoint): %v", err)
	}
	if len(links) != 0 {
		t.Errorf("SupersedesLinksInto into the source endpoint = %+v, want none", links)
	}
	links, err = s.SupersedesLinksInto(ctx, "p1", foreignOlder)
	if err != nil {
		t.Fatalf("SupersedesLinksInto(foreign): %v", err)
	}
	if len(links) != 0 {
		t.Errorf("SupersedesLinksInto reached another project's edge: %+v", links)
	}
}

func mustLink(t *testing.T, s *Store, sourceID, targetID, relation string, strength float32, source string) {
	t.Helper()
	if err := s.CreateLink(context.Background(), sourceID, targetID, relation, strength, source); err != nil {
		t.Fatalf("CreateLink(%s, %s, %s): %v", sourceID, targetID, relation, err)
	}
}

// upperHex uppercases the hex characters of an id, leaving anything else alone.
func upperHex(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'f' {
			out[i] = c - 'a' + 'A'
		}
	}
	return string(out)
}
