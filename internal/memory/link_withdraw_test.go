package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
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
// _global, which a promotion moves a memory into without touching its links),
// matched literally (no LIKE wildcards) and case-insensitively, in ascending id
// order, and by RUNE count so a non-ASCII id imported verbatim still resolves.
func TestMemoryIDsByIDPrefixIsProjectScopedAndLiteral(t *testing.T) {
	s := linkTestStore(t)
	ctx := context.Background()
	mine := mustCreate(t, s, "p1", "The restore path on one spindle is safe.")
	other := mustCreate(t, s, "p2", "The restore path on two spindles is not.")
	global := mustCreate(t, s, GlobalProjectID, "A global note about restores.")
	for i := 0; i < 4; i++ {
		mustCreate(t, s, "p1", "Note number "+string(rune('a'+i)))
	}
	const japanese = "日本語-メモ"
	if _, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID: japanese, ProjectID: "p1", Category: "fact", Content: "An imported note whose id is not ASCII.", Source: "mcp",
	}, ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	for _, tc := range []struct {
		name, prefix string
		want         []string
	}{
		{"global reachable from a project", global[:8], []string{global}},
		{"another project is not", other[:8], nil},
		{"case-insensitive", upperHex(mine[:8]), []string{mine}},
		{"percent is literal", "%", nil},
		{"underscore is literal", "_", nil},
		{"miss is empty, not an error", "ffffffff", nil},
		{"non-ASCII full id", japanese, []string{japanese}},
		{"non-ASCII prefix", "日本語", []string{japanese}},
	} {
		got, err := s.MemoryIDsByIDPrefix(ctx, "p1", tc.prefix)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: MemoryIDsByIDPrefix(%q) = %v, %v, want %v", tc.name, tc.prefix, got, err, tc.want)
		}
	}
	all, err := s.MemoryIDsByIDPrefix(ctx, "p1", "")
	if err != nil || len(all) < 6 || !sort.StringsAreSorted(all) {
		t.Errorf("MemoryIDsByIDPrefix(\"\") = %v, %v, want every id, ascending", all, err)
	}
}

// TestLinksInto pins the read a withdrawal decides on: the live 'supersedes'
// edges pointing INTO a memory, scoped like LinksByRelationSource (an edge is
// the project's when an endpoint is its own or _global's, never another
// project's), excluding invalidated edges and ones the target merely sources.
// The empty relation returns supersedes then causes (#833) and never
// 'contradicts', a relation no withdrawal can name. A promotion to _global keeps
// its links (#786), so such an edge must stay visible from the project it buries.
func TestLinksInto(t *testing.T) {
	s := linkTestStore(t)
	ctx := context.Background()
	newer := mustCreate(t, s, "p1", "The ingest service now runs Redis 7.2.")
	older := mustCreate(t, s, "p1", "The ingest service runs Redis 6.2.")
	cause := mustCreate(t, s, "p1", "A note the newer one merely cites.")
	dropped := mustCreate(t, s, "p1", "A pair whose edge was already withdrawn.")
	foreignNewer := mustCreate(t, s, "p2", "Another project's replacement note.")
	foreignOlder := mustCreate(t, s, "p2", "Another project's stale note.")

	mustLink(t, s, newer, older, "supersedes", 0.95, "llm")
	mustLink(t, s, cause, older, "causes", 0.9, "llm")
	mustLink(t, s, foreignNewer, foreignOlder, "supersedes", 0.95, "llm")
	mustLink(t, s, newer, dropped, "supersedes", 0.95, "llm")
	if n, err := s.InvalidateLink(ctx, newer, dropped, "supersedes"); err != nil || n != 1 {
		t.Fatalf("InvalidateLink = %d, %v; want 1", n, err)
	}
	into := func(project, target, rel string) []Link {
		t.Helper()
		links, err := s.LinksInto(ctx, project, target, rel)
		if err != nil {
			t.Fatalf("LinksInto(%s): %v", project, err)
		}
		return links
	}
	if links := into("p1", older, "supersedes"); len(links) != 1 || links[0].SourceID != newer || links[0].TargetID != older ||
		links[0].Relation != "supersedes" || links[0].Source != "llm" || links[0].InvalidatedAt != nil {
		t.Fatalf("LinksInto = %+v, want exactly the one live llm supersedes edge", links)
	}
	for _, target := range []string{newer, foreignOlder, dropped} {
		if links := into("p1", target, "supersedes"); len(links) != 0 {
			t.Errorf("LinksInto(p1, %s) = %+v, want none", target, links)
		}
	}
	if links := into("p1", older, ""); len(links) != 2 || links[0].Relation != "supersedes" || links[1].Relation != "causes" {
		t.Fatalf("LinksInto(both relations) = %+v, want supersedes first then causes", links)
	}
	if links := into("p1", older, "causes"); len(links) != 1 || links[0].SourceID != cause {
		t.Errorf("LinksInto(causes) = %+v, want only the causes edge", links)
	}
	mustLink(t, s, newer, older, "contradicts", 0.8, "llm")
	if links := into("p1", older, ""); len(links) != 2 {
		t.Errorf("LinksInto(both relations) = %+v, want 'contradicts' left out", links)
	}

	// A source promoted to _global keeps its edge, and p2's edge into p1's target
	// buries a p1 memory, so both are visible from p1.
	source := mustCreate(t, s, "p1", "A note promoted to _global after it superseded another.")
	target := mustCreate(t, s, "p1", "The p1 note that the promoted one superseded.")
	foreignIntoOurs := mustCreate(t, s, "p2", "A p2 note that supersedes one of p1's.")
	mustLink(t, s, source, target, "supersedes", 0.95, "llm")
	mustLink(t, s, foreignIntoOurs, target, "supersedes", 0.95, "llm")
	if err := s.PromoteToGlobal(ctx, "p1", source); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	links := into("p1", target, "supersedes")
	seen := map[string]bool{}
	for _, l := range links {
		seen[l.SourceID] = true
	}
	if len(links) != 2 || !seen[source] || !seen[foreignIntoOurs] {
		t.Errorf("LinksInto(p1) = %+v, want the promoted source's edge and the p2 edge burying p1's target", links)
	}
	if got := into(GlobalProjectID, target, "supersedes"); len(got) != 1 || got[0].SourceID != source {
		t.Errorf("LinksInto(_global) = %+v, want only the edge whose source IS in the shared scope", got)
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

// TestLinksByRelationSourceReachesAGlobalSource is the same scoping on the read
// every other supersede surface uses (repair load, reclassify, resolve's
// piggyback, the resolved-row floor): an edge whose source was promoted to
// _global still loads for the project, an edge with an endpoint outside the
// caller's project is that other project's to correct, and _global does not load
// an edge whose target is a p1 memory.
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
	for _, tc := range []struct {
		project string
		want    string // source id of the single expected edge, "" for none
	}{{"p1", source}, {"p2", foreignNewer}, {GlobalProjectID, ""}} {
		links, err := s.LinksByRelationSource(ctx, tc.project, "supersedes", "llm")
		if err != nil {
			t.Fatalf("LinksByRelationSource(%s): %v", tc.project, err)
		}
		if tc.want == "" && len(links) != 0 || tc.want != "" && (len(links) != 1 || links[0].SourceID != tc.want) {
			t.Errorf("LinksByRelationSource(%s) = %+v, want source %q", tc.project, links, tc.want)
		}
	}
}

// TestLinksByRelationSourceReturnsEdgesInInsertionOrder pins the order the repair
// pass applies edges in: the order they were written, not the order the query
// plan happens to visit the two memories joins in. The edges are written with
// their source ids in DESCENDING order, so a plan that walks memories by id (as an
// index on memories makes cheap) returns them reversed.
func TestLinksByRelationSourceReturnsEdgesInInsertionOrder(t *testing.T) {
	s := linkTestStore(t)
	ctx := context.Background()
	const n = 6
	ids := make([]string, 0, 2*n)
	for i := 0; i < 2*n; i++ {
		ids = append(ids, mustCreate(t, s, "p1", fmt.Sprintf("Ordering note number %d with its own wording %d.", i, i*7919)))
	}
	sort.Strings(ids)
	var want []string
	for i := n - 1; i >= 0; i-- {
		src, tgt := ids[i], ids[i+n]
		mustLink(t, s, src, tgt, "supersedes", 0.9, "llm")
		want = append(want, src+">"+tgt)
	}
	for round := 0; round < 3; round++ {
		links, err := s.LinksByRelationSource(ctx, "p1", "supersedes", "llm")
		if err != nil {
			t.Fatalf("LinksByRelationSource: %v", err)
		}
		var got []string
		for _, l := range links {
			got = append(got, l.SourceID+">"+l.TargetID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("edges came back as %v, want insertion order %v", got, want)
		}
	}
}
