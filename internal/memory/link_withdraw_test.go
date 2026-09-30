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

// The prefix bound is a RUNE COUNT, because memories.id is TEXT and SQLite's
// substr() slices by character. A byte length makes a multi-byte ref compare
// against the first N CHARACTERS of every id, which can never be equal — a false
// negative that refuses an id the store really holds, and refuses it while saying
// no memory has that id. The hex ids Ghost mints have byte length == rune count,
// so only a non-ASCII id can catch this, and `ghost import` writes ids verbatim.
func TestMemoryIDsByIDPrefixFindsANonASCIID(t *testing.T) {
	store := linkTestStore(t)
	ctx := context.Background()
	const japanese = "日本語-メモ"
	if _, _, _, err := store.ImportMemory(ctx, PortableMemory{
		ID: japanese, ProjectID: "p1", Category: "fact",
		Content: "An imported note whose id is not ASCII.", Source: "mcp",
	}, ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	for _, prefix := range []string{japanese, "日本語"} {
		got, err := store.MemoryIDsByIDPrefix(ctx, "p1", prefix)
		if err != nil {
			t.Fatalf("MemoryIDsByIDPrefix(%q): %v", prefix, err)
		}
		if len(got) != 1 || got[0] != japanese {
			t.Errorf("MemoryIDsByIDPrefix(%q) = %q, want [%s]", prefix, got, japanese)
		}
	}
}

// TestSupersedesLinksIntoReachesAGlobalSource is #786 at the read a withdrawal
// decides on. A promotion moves a memory into `_global` and KEEPS its links, so a
// live 'supersedes' edge is left with one endpoint in the project and one in the
// shared scope — and the project is the one whose memory the edge buries. The
// read's project predicate is on the SOURCE alone, so that edge was invisible from
// the project being harmed, which is what left `ghost supersede <project>
// --withdraw` and `ghost_link_withdraw` refusing an edge that was demoting a
// memory the operator could see missing from every session.
//
// `_global` is in scope from EVERY project, and no other project is: that is the
// same rule the ref resolver follows, so the two halves of a withdrawal agree
// about which memories an operator may name.
func TestSupersedesLinksIntoReachesAGlobalSource(t *testing.T) {
	s := linkTestStore(t)
	ctx := context.Background()
	source := mustCreate(t, s, "p1", "A note promoted to _global after it superseded another.")
	target := mustCreate(t, s, "p1", "The p1 note that the promoted one superseded.")
	other := mustCreate(t, s, "p1", "An unrelated p1 note, to prove the read is not a match test.")
	// A second project's edge into a memory of its own, and one into OURS: the
	// first must stay invisible from p1 (p1 does not own it), and the second is
	// p1's own memory, so p1 is entitled to withdraw the edge burying it.
	foreignNewer := mustCreate(t, s, "p2", "A p2 note that supersedes a p2 note.")
	foreignOlder := mustCreate(t, s, "p2", "A p2 note that is stale.")
	foreignIntoOurs := mustCreate(t, s, "p2", "A p2 note that supersedes one of p1's.")

	mustLink(t, s, source, target, "supersedes", 0.95, "llm")
	mustLink(t, s, foreignNewer, foreignOlder, "supersedes", 0.95, "llm")
	mustLink(t, s, foreignIntoOurs, target, "supersedes", 0.95, "llm")
	if err := s.PromoteToGlobal(ctx, "p1", source); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	links, err := s.SupersedesLinksInto(ctx, "p1", target)
	if err != nil {
		t.Fatalf("SupersedesLinksInto: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("SupersedesLinksInto(p1) = %+v, want 2 live edges into p1's target: the promoted source's and the p2 one that buries a p1 memory", links)
	}
	seen := map[string]bool{}
	for _, l := range links {
		seen[l.SourceID] = true
	}
	if !seen[source] {
		t.Errorf("SupersedesLinksInto(p1) cannot see the edge whose SOURCE was promoted to _global: %+v", links)
	}
	if !seen[foreignIntoOurs] {
		t.Errorf("SupersedesLinksInto(p1) cannot see the edge burying its OWN target: %+v", links)
	}

	// Neither of p2's OTHER memories is reachable from p1: "either endpoint" is
	// about the edge, not about the whole store.
	if got, err := s.SupersedesLinksInto(ctx, "p1", foreignOlder); err != nil {
		t.Fatalf("SupersedesLinksInto(p1, p2's target): %v", err)
	} else if len(got) != 0 {
		t.Errorf("SupersedesLinksInto(p1) reached an edge with no endpoint in p1 or _global: %+v", got)
	}
	if got, err := s.SupersedesLinksInto(ctx, "p1", other); err != nil {
		t.Fatalf("SupersedesLinksInto(p1, unrelated): %v", err)
	} else if len(got) != 0 {
		t.Errorf("SupersedesLinksInto(p1, unrelated) = %+v, want none", got)
	}
	// And the shared scope is not every project: naming `_global` reaches the
	// promoted source's edge, and p2's edge into p1's target is NOT one of them
	// even though p1 can see it — the rule is "either endpoint is OURS", and
	// neither endpoint of that edge is in `_global`.
	if got, err := s.SupersedesLinksInto(ctx, GlobalProjectID, foreignOlder); err != nil {
		t.Fatalf("SupersedesLinksInto(_global, p2's target): %v", err)
	} else if len(got) != 0 {
		t.Errorf("SupersedesLinksInto(_global) reached an edge with no endpoint in the shared scope: %+v", got)
	}
	if got, err := s.SupersedesLinksInto(ctx, GlobalProjectID, target); err != nil {
		t.Fatalf("SupersedesLinksInto(_global, p1's target): %v", err)
	} else if len(got) != 1 || got[0].SourceID != source {
		t.Errorf("SupersedesLinksInto(_global) = %+v, want only the edge whose source IS in the shared scope", got)
	}
}

// TestLinksByRelationSourceReachesAGlobalSource is the same rule on the read
// every OTHER supersede surface goes through: the repair pass's load, the
// ordinary pass's reclassify half, resolve's supersedes piggyback and the floor
// that holds a resolved row down. All four scope on the edge's SOURCE, so a
// promoted source put the edge outside all four at once — including the floor,
// which then released a target the ranking was still demoting. They read one
// method for exactly this reason, and the consistency is the invariant.
func TestLinksByRelationSourceReachesAGlobalSource(t *testing.T) {
	s := linkTestStore(t)
	ctx := context.Background()
	source := mustCreate(t, s, "p1", "A note promoted to _global after it superseded another.")
	target := mustCreate(t, s, "p1", "The p1 note that the promoted one superseded.")
	foreignNewer := mustCreate(t, s, "p2", "A p2 note that supersedes a p2 note.")
	foreignOlder := mustCreate(t, s, "p2", "A p2 note that is stale.")
	foreignIntoOurs := mustCreate(t, s, "p2", "A p2 note that supersedes one of p1's.")
	mustLink(t, s, source, target, "supersedes", 0.95, "llm")
	mustLink(t, s, foreignNewer, foreignOlder, "supersedes", 0.95, "llm")
	mustLink(t, s, foreignIntoOurs, target, "supersedes", 0.95, "llm")
	if err := s.PromoteToGlobal(ctx, "p1", source); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	links, err := s.LinksByRelationSource(ctx, "p1", "supersedes", "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(links) != 1 || links[0].SourceID != source || links[0].TargetID != target {
		t.Fatalf("LinksByRelationSource(p1) = %+v, want the %s→%s edge whose source is in the shared scope", links, source, target)
	}
	// p2 owns the two edges it sources, and it also sees p1's promoted one — the
	// shared scope is in scope from every project, so a claim a global note makes
	// is one every project can judge. What p2 does NOT get is p1's ownership of
	// its own target, which is the other read's rule and not this one's: this read
	// is about who can JUDGE the edge, and the source is who makes the claim.
	links, err = s.LinksByRelationSource(ctx, "p2", "supersedes", "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource(p2): %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("LinksByRelationSource(p2) = %+v, want p2's own two edges plus the one its shared-scope source makes", links)
	}
	for _, l := range links {
		if l.SourceID != foreignNewer && l.SourceID != foreignIntoOurs && l.SourceID != source {
			t.Errorf("LinksByRelationSource(p2) = %+v, want only edges sourced in p2 or in _global", links)
		}
	}
}
