// The targeted withdrawal: remove ONE named 'supersedes' edge, on an
// operator's say-so, without asking a model whether it should.
//
// `Reassess` (reassess.go) is the other half of the repair, and it cannot reach
// this case. It withdraws an edge the CURRENT RULES reject — the veto, or a
// classifier that now answers neither/causes/reversed. #686 measured 43%
// precision, and every wrong edge joined two notes that were both still true,
// so the rules now refuse most of them. What the rules do not know is that a
// specific pair is wrong for a reason no rubric can see: the newer note is not a
// replacement of the older one at all, it is the same fact recorded twice on
// either side of a release, or the "superseding" note is the one that went
// stale. A classifier that re-answers `supersedes` on that pair has not made a
// mistake by its own lights, so --reassess confirms the edge and the wrong edge
// keeps its target buried: demoted by SupersedePenalties, and stamped
// resolved_at by resolve's supersedes piggyback, which the repair pass
// deliberately honours as a floor. Nothing in the ordinary pass can undo that,
// because skip-if-unchanged holds an edge whose endpoints have not changed quiet
// forever — whichever way the scan proposes the pair (#787) and however long ago
// the edge was last confirmed (#784). Only a person who can see both notes can
// name the pair.
//
// So this is the operator-facing undo for one edge, and the difference from
// Reassess is the whole point: nothing is judged here. The caller has decided,
// the pair is named, and the edge goes through the same InvalidateLink path --
// which writes the `unsupersede` history row, so an audit shows the claim and
// the withdrawal. A dry run is the default, because the judgement being trusted
// is the caller's and the graph is the thing that has to survive being wrong
// about it.
//
// The ids are refs, not ids: a full id, or 8 or more characters of one. Every
// Ghost report shortens an id to eight characters, so requiring the full id
// would make this undrivable from the report that names the wrong edge. A full
// id is accepted whatever its shape, because `ghost import` writes an artifact's
// ids verbatim and the column only DEFAULTS to hex — a ref check that insisted
// on hex would leave an imported endpoint unnameable, which is a repair nobody
// can perform. An ambiguous ref is a refusal that lists the matches, never a
// choice: a guess here deletes a link nobody named.
//
// Withdrawing the edge is still only half the repair. A target that was stamped
// resolved_at on this edge's account keeps it until `ghost resolve --reassess`
// clears it, so every caller reports that step; see the chain test in
// withdraw_test.go, which runs both halves against a real store.
package supersede

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

// The ref rules are NOT here. A ref is a full id or an 8-or-more-character
// prefix of one, matched literally and case-insensitively inside the project
// plus _global; an ambiguous ref is a refusal listing the matches, never a
// choice. `ghost resolve --mark` (#714) names memories by ref through the same
// query and the same refusals, and two copies of these rules would eventually
// disagree about which id one spelling addresses — so they live in
// internal/memref, and the two callers share them.

// WithdrawStore is the subset of *memory.Store a targeted withdrawal needs;
// narrowed for testability. Nothing here judges an edge: the store is asked what
// is live and is asked to invalidate, and the decision is the caller's.
type WithdrawStore interface {
	// MemoryIDsByIDPrefix resolves one ref to the memory ids it can mean,
	// scoped to the project (plus _global).
	MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error)
	// MemoryIDsByIDPrefixAnyProject resolves one ref to the ids it can mean
	// across the whole store's live rows, with no project predicate. It is
	// reached only when the named project IS `_global` — the shared scope,
	// where an edge's endpoint may live in any project (#786).
	MemoryIDsByIDPrefixAnyProject(ctx context.Context, prefix string) ([]string, error)
	// LinksInto returns the live edges pointing at a memory for the given
	// relation, restricted to the ones the project owns through EITHER endpoint
	// (or through the shared scope).
	LinksInto(ctx context.Context, projectID, memoryID, relation string) ([]memory.Link, error)
	// GetByIDs loads the targets' text, so the report can answer "did I
	// withdraw the right edge?" from its own output.
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	// InvalidateLink is the ordinary soft-invalidation, which writes the
	// `unsupersede` history row for a supersedes edge.
	InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error)
}

// WithdrawPair is one edge to withdraw, as the caller named it: Source is the
// superseding memory (the edge is written newer→older) and Target the superseded
// one. Either may be a full id or an unambiguous 8-or-more-character prefix of
// one.
type WithdrawPair struct {
	Source   string
	Target   string
	Relation string // empty means auto-select (supersedes then causes)
}

// WithdrawnLink is one edge the request named, resolved to full ids. The link's
// own `source` column is carried because it decides the follow-up: resolve's
// supersedes piggyback only ever acts on 'supersedes'/'llm' edges, so an edge
// from another source is not what stamped its target, and a report that said
// "run ghost resolve --reassess" for one would be sending the operator after a
// step that has nothing to do. The target's text is carried for the same reason
// the report prints it: an operator withdrawing an edge they believe is wrong
// has to be able to check that from the output, not from a second command.
type WithdrawnLink struct {
	SourceID string
	TargetID string
	// Relation is the edge's relation ('supersedes' or 'causes'), carried from
	// the row the pair resolved to. It is what the write uses — InvalidateLink is
	// relation-scoped, and only a 'supersedes' invalidation writes the
	// `unsupersede` history row — so a 'causes' withdrawal is withdrawn AS a
	// 'causes' edge rather than as a supersession that does not exist.
	Relation   string  // 'supersedes' or 'causes'
	TargetText string  // the target's own content, as stored
	LinkSource string  // the edge's own `source` column
	Strength   float32 // the edge's stored similarity, as written
	Withdrawn  bool    // this call moved it out of the live set
	// TargetProjectID is the project the target lives in, and the follow-up the
	// withdrawal prints is SCOPED TO IT rather than to the project the command was
	// run against. That is the same project in every ordinary case and a
	// different one exactly where it has to be: a `ghost supersede _global
	// --withdraw` whose target stayed in a project, where `ResolvedCandidates`
	// filters `project_id = ?` and a `ghost resolve _global --reassess` can
	// therefore never see the memory. Scoping the repair to the wrong project
	// does not fail loudly — the selector resolves and the row is not in the pool
	// — so a block promising a clear that cannot happen is the one failure this
	// field exists to prevent (#786).
	TargetProjectID string
	// WithdrawalFailed marks the row whose own write errored, and NotAttempted
	// the rows after it, which this run never reached because each invalidation
	// is its own transaction. They are separate states because a report that
	// described either as "already gone" would be claiming a concurrent pass
	// removed an edge this run did not touch — and for a not-attempted row that
	// edge is still live.
	WithdrawalFailed bool
	NotAttempted     bool
}

// ProjectTargets is one project's share of a repairable set: the ids to name, and
// the project whose repair can reach them. It exists because the two are not
// separable — a resolve repair's pool is `ResolvedCandidates(projectID)`, which
// filters `project_id = ?`, so a selector resolved against one project and
// repaired against another is a silent no-op, not an error (#786).
type ProjectTargets struct {
	ProjectID string
	Targets   []string
}

// RepairableTargets is the follow-up: the targets of the edges a withdrawal
// reported, DEDUPLICATED, in FULL, and GROUPED by the project each one lives in,
// in the order the rows were reported. A caller prints one scoped repair per
// group, and every group is a project the repair can actually reach.
//
// In FULL, not the eight-character abbreviations the reports use, because a
// selector is a repair about to be run and a prefix that is unambiguous now may
// not be after the operator's next save.
//
// Every row counts except the two whose edge is STILL LIVE. A row this call never
// WROTE counts when its edge is gone, because a concurrent pass that took it
// first left the same state behind: no live edge, and a resolved_at nothing
// defends any more — exactly the state the repair clears, so dropping it would
// leave a just-as-repairable memory out of the list the caller is about to run. A
// row never reached, or one whose write failed, is the opposite case: its edge is
// still live, so the target is still held down on purpose and naming it would
// send the repair after a row its own floor reports as still asserted. A row
// with no resolved target is out because there is nothing to name.
//
// The grouping is nearly always ONE project — the one the command named — and a
// caller that gets a single group cannot tell the difference from today's flat
// list, which is the point: `ghost supersede _global --withdraw` on a pair whose
// target stayed in a project is the case that needs the split, and it is exactly
// the case a flat list got wrong.
//
// It is EXPORTED and lives here because two surfaces printed this exact rule
// (cmd/ghost for `ghost supersede --withdraw`, internal/mcpserver for
// ghost_link_withdraw) over `[]WithdrawnLink` — the same type, so nothing forced
// the duplication — and only the CLI's copy had a test. Two copies is two answers
// to which memories a repair can still clear, and a wrong answer does not print a
// wrong report, it clears the wrong memories. `TestRepairableTargets*` pins it
// once, in the package that owns the type.
//
// It counts 'supersedes' withdrawals ALONE, and the relation is a FILTER rather
// than an assumption. The repair is a `ghost resolve --reassess`, resolve's
// supersedes piggyback acts on 'supersedes'/'llm' edges only, and a 'causes'
// claim never stamped the `resolved_at` the repair clears — so naming a 'causes'
// target sends the operator to clear a memory nothing is holding down. #833 made
// the filter load-bearing rather than vacuous: before it every link reaching this
// function was a 'supersedes' edge by construction (the relation on the row was
// never even read), so a 'causes' withdrawal did not exist to be filtered. An
// EMPTY relation counts, because that is the zero value of a struct a caller may
// have built by hand and the withdrawal's own default is 'supersedes'.
func RepairableTargets(links []WithdrawnLink) []ProjectTargets {
	rows := make([]RepairTarget, 0, len(links))
	seen := make(map[string]bool, len(links))
	for _, l := range links {
		if l.Relation != "" && l.Relation != string(RelationSupersedes) {
			continue
		}
		if l.TargetID == "" || seen[l.TargetID] || l.NotAttempted || l.WithdrawalFailed {
			continue
		}
		seen[l.TargetID] = true
		rows = append(rows, RepairTarget{ID: l.TargetID, ProjectID: l.TargetProjectID})
	}
	return GroupByProject(rows)
}

// RepairTarget is one memory a repair can still clear, with the project it lives
// in — the pair a follow-up command has to name, and the only two things a
// grouping needs. It is its own type so one grouping rule serves both withdrawal
// row types (`WithdrawnLink` and `WithdrawnEdge`) rather than being written twice
// over two structs that differ in everything but these two fields.
type RepairTarget struct {
	ID        string
	ProjectID string
}

// GroupByProject groups a repairable set by the project that owns each memory,
// preserving the order the rows were reported in, and in that same order among the
// projects — so a report's list of repairs reads in the order the rows did.
//
// A row with no project keeps its own single-member group under the empty id, and
// the id is never dropped: a caller that cannot name the project it repairs in
// has to render the unscoped form, and dropping the row would hide a memory that
// is still stuck. A caller that groups by the project it was run against instead
// is the bug this replaces.
func GroupByProject(rows []RepairTarget) []ProjectTargets {
	var out []ProjectTargets
	index := make(map[string]int, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.ID == "" || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		i, ok := index[r.ProjectID]
		if !ok {
			i = len(out)
			index[r.ProjectID] = i
			out = append(out, ProjectTargets{ProjectID: r.ProjectID})
		}
		out[i].Targets = append(out[i].Targets, r.ID)
	}
	return out
}

// WithdrawResult summarizes a request. Resolved counts the pairs that named a
// live edge; Withdrawn counts the edges this call actually invalidated, which is
// 0 for a dry run and can be lower than Resolved under --apply when a
// concurrent pass took an edge between the check and the write — InvalidateLink
// is guarded on invalidated_at IS NULL precisely so that case is countable.
type WithdrawResult struct {
	Resolved  int
	Withdrawn int
	Links     []WithdrawnLink
}

// Withdraw withdraws the named 'supersedes' edges and returns them resolved to
// full ids. A dry run (apply=false) resolves and reports without writing.
//
// The whole request is settled before anything is written, and a refusal writes
// NOTHING — not even the pairs that were fine. Several pairs in one command are
// an operator correcting a list they read off a report, and withdrawing four of
// five while reporting an error about the fifth is a graph state nobody asked
// for and cannot get back without knowing which four moved. So every ref is
// resolved, every self-pair and every pair with no live edge is collected, and
// only a request with no problem in it is applied.
//
// Every problem in the request is reported, not just the first: an operator
// correcting five edges learns from all five that they are wrong at once.
//
// A write that fails part-way through still returns what landed, because each
// invalidation is its own transaction and a later pass will not see those edges
// again — a repair that partly happened has to be visible as such, the same
// reason Reassess returns its result beside its error.
//
// A pair with no live edge is an error rather than a success that withdrew
// nothing, and the refusal names the target's live edges: "no live supersedes
// link A→B" is a dead end, and "B is superseded by C and D" is an answer.
func Withdraw(ctx context.Context, store WithdrawStore, projectID string, pairs []WithdrawPair, apply bool, logger *slog.Logger) (WithdrawResult, error) {
	var res WithdrawResult
	if len(pairs) == 0 {
		return res, fmt.Errorf("no pair to withdraw: name an edge as --withdraw <source-id> <target-id>")
	}

	var problems []string
	links := make([]WithdrawnLink, 0, len(pairs))
	for _, pair := range pairs {
		link, err := resolvePair(ctx, store, projectID, pair)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		links = append(links, link)
	}
	if len(problems) > 0 {
		return res, fmt.Errorf("nothing withdrawn: %s", strings.Join(problems, "; "))
	}
	res.Resolved = len(links)
	res.Links = links
	// The targets' text, read once for the whole request and only after it is
	// known to be sound: the report quotes the memory each edge was burying, so
	// the operator can tell from the output whether that was the one they meant.
	if err := attachTargetText(ctx, store, res.Links); err != nil {
		return WithdrawResult{}, err
	}

	if !apply {
		return res, nil
	}
	for i := range res.Links {
		n, err := store.InvalidateLink(ctx, res.Links[i].SourceID, res.Links[i].TargetID, res.Links[i].Relation)
		if err != nil {
			// The edges before this one are gone and cannot be un-gone, so the
			// count and the list go back WITH the error — and the list says WHICH
			// rows those are. The rows after this one were never reached, so they
			// are marked rather than left to be read as edges something else
			// removed.
			res.Links[i].WithdrawalFailed = true
			for j := i + 1; j < len(res.Links); j++ {
				res.Links[j].NotAttempted = true
			}
			return res, fmt.Errorf("withdraw %s link %s→%s: %w", res.Links[i].Relation, res.Links[i].SourceID, res.Links[i].TargetID, err)
		}
		if n == 0 {
			// A concurrent pass withdrew it first. The edge is gone either way,
			// so this is not a failure — but it is not this call's write and the
			// report must not claim it, the same distinction Reassess draws.
			continue
		}
		res.Links[i].Withdrawn = true
		res.Withdrawn++
		if logger != nil {
			logger.Info("supersede withdrew a named edge",
				"source", res.Links[i].SourceID, "target", res.Links[i].TargetID,
				"relation", res.Links[i].Relation,
				"link_source", res.Links[i].LinkSource, "withdrawn", res.Withdrawn)
		}
	}
	return res, nil
}

// attachTargetText fills in each row's target text in place. A target that
// cannot be loaded leaves its row's text empty rather than failing the request:
// the edge was found through a live link row, so its endpoint exists, and a
// report that refused to print the edge over a text it could not read would be
// refusing to answer the question the operator asked.
func attachTargetText(ctx context.Context, store WithdrawStore, links []WithdrawnLink) error {
	ids := make([]string, 0, len(links))
	seen := make(map[string]bool, len(links))
	for _, l := range links {
		if l.TargetID == "" || seen[l.TargetID] {
			continue
		}
		seen[l.TargetID] = true
		ids = append(ids, l.TargetID)
	}
	if len(ids) == 0 {
		return nil
	}
	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("load withdrawn targets: %w", err)
	}
	type loaded struct {
		content string
		project string
	}
	byID := make(map[string]loaded, len(mems))
	for _, m := range mems {
		byID[m.ID] = loaded{content: m.Content, project: m.ProjectID}
	}
	for i := range links {
		links[i].TargetText = byID[links[i].TargetID].content
		links[i].TargetProjectID = byID[links[i].TargetID].project
	}
	return nil
}

// resolvePair turns one pair of refs into the live edge it names, or explains
// why there is none. It reads only: every problem a request can have is found
// here, before Withdraw writes anything.
func resolvePair(ctx context.Context, store WithdrawStore, projectID string, pair WithdrawPair) (WithdrawnLink, error) {
	var link WithdrawnLink
	if pair.Source == "" || pair.Target == "" {
		empty := "source"
		if pair.Target == "" {
			empty = "target"
		}
		return link, fmt.Errorf("the %s ref is empty in the pair (%q, %q)", empty, pair.Source, pair.Target)
	}
	// The TARGET is resolved first, and the order is the point rather than a
	// convenience. It is the memory the caller is un-burying, so it is the one
	// whose project the command has to be able to name; and once it is named, the
	// live edges pointing at it are in scope, which is what makes a SOURCE in
	// another project nameable. `ghost project merge` moves a memory between
	// projects and leaves its links, so an edge can end up with its target here
	// and its source in `q` — a claim that buries one of this project's memories
	// and that neither project's own ref scope could name, which is the same
	// "demoted by a claim no command can reach" state #786 removed for a promoted
	// source. Resolving the source against the target's own holders closes it,
	// and it is a narrow widening: the id set is derived from an edge the caller
	// can already SEE, so it cannot be used to ask what else exists in `q`.
	targetID, err := resolveRef(ctx, store, projectID, "target", pair.Target)
	if err != nil {
		return link, err
	}
	into, err := store.LinksInto(ctx, projectID, targetID, "") // fetch all relations for auto/inspection
	if err != nil {
		return link, err
	}
	sourceID, err := resolveSource(ctx, store, projectID, into, pair.Source)
	if err != nil {
		return link, err
	}
	if sourceID == targetID {
		return link, fmt.Errorf("%s supersedes itself: both refs are memory %s", short(targetID), targetID)
	}
	// The RELATION this pair resolves to, and the reason the loop is ordered
	// rather than a single scan: an unset Relation means "whichever of the two the
	// pair has", and the two are not equal — 'supersedes' is the one that buries
	// its target and the one every pre-#833 call site meant, so it wins. A pair
	// holding both is exactly the case where the order decides which edge moves,
	// and the order has to be a stated rule rather than whatever the read
	// happened to return.
	//
	// A PINNED Relation is a filter rather than a preference, so it skips every
	// other relation instead of merely sorting behind this one: falling back is
	// the wrong-edge withdrawal the pin exists to prevent.
	for _, want := range []string{string(RelationSupersedes), string(RelationCauses)} {
		if pair.Relation != "" && want != pair.Relation {
			continue
		}
		for _, l := range into {
			if l.SourceID == sourceID && l.Relation == want {
				return WithdrawnLink{
					SourceID:   l.SourceID,
					TargetID:   l.TargetID,
					Relation:   want,
					LinkSource: l.Source,
					Strength:   l.Strength,
				}, nil
			}
		}
	}
	return link, fmt.Errorf("no live %s link %s → %s in project %s%s",
		relationLabel(pair.Relation), short(sourceID), short(targetID), projectID, intoSuffix(into))
}

// relationLabel names the edges the refusal is about: the pinned relation, or
// both when the pair named neither. It is what keeps a refusal honest about what
// was searched — "no live supersedes link" over a pair holding only a 'causes'
// edge sent an operator to the graph instead of telling them the edge they want
// needs --relation, which is the whole of the #833 fix.
func relationLabel(relation string) string {
	if relation == "" {
		return "supersedes or causes"
	}
	return relation
}

// resolveSource turns the SOURCE ref into a memory id, in the project's own scope
// first and then against the memories that hold the target.
//
// The second attempt is what lets `ghost supersede p --withdraw` reach an edge a
// note in `q` makes about a note in `p` — a shape `ghost project merge` leaves
// behind, and one the project-scoped read cannot name from either end: `p` cannot
// resolve a `q` memory, and `q` cannot resolve a `p` one. The target's own read
// already returned the holder, so the id is in hand and only the REF standing for
// it was missing; the operator has that id on screen, because the refusal that
// would have named it is built from the same read.
//
// The project's scope is tried FIRST so the refusal a mistyped ref gets is the
// project-scoped one, naming the project the operator was working in. The holders
// are a fallback, never a replacement, and the rules are memref's either way — so
// an ambiguous holder is still a refusal with the matches listed, and a ref that
// names nothing is still told it names nothing.
//
// The holder set is NOT narrowed by the caller's pinned relation, and that is
// deliberate: a source resolved through a 'causes' holder and then refused for
// having no 'supersedes' edge is a PRECISE refusal ("no live supersedes link
// A→B, still caused by A"), while narrowing the set first refuses the SOURCE ref
// as unresolvable and sends the operator looking for the wrong problem.
func resolveSource(ctx context.Context, store WithdrawStore, projectID string, into []memory.Link, ref string) (string, error) {
	id, scopedErr := resolveRef(ctx, store, projectID, "source", ref)
	if scopedErr == nil {
		return id, nil
	}
	// Only a MISS falls through to the holders. An AMBIGUOUS ref is a refusal
	// about a real ambiguity in the project's own ids, and answering it from a
	// different id set would resolve the very spelling the refusal said cannot
	// address either of its matches — so a ref too short to be a prefix stays
	// refused too, and neither reaches the fallback.
	if !errors.Is(scopedErr, memref.ErrNoMatch) {
		return "", scopedErr
	}
	ids := make([]string, 0, len(into))
	for _, l := range into {
		ids = append(ids, l.SourceID)
	}
	if id, err := memref.ResolveIn(ids, "source", ref); err == nil {
		return id, nil
	}
	return "", scopedErr
}

// resolveRef turns one ref into a memory id in the project, through the shared
// rules in internal/memref. It stays a named function so the two call sites in
// resolvePair read as what they are — the SOURCE and the TARGET — and so `which`
// reaches the refusal.
//
// `_global` is the one project whose refs are not project-scoped, and the reason
// is the same one makes it the shared scope everywhere else: an edge whose source
// was promoted out of a project is a claim made FROM `_global`, and the endpoint
// it buries may be in any project. Naming such a pair from `_global` — which is
// where `ghost supersede _global --reassess` finds it, and what an operator
// reaches for after the project's own surfaces refuse — needs both refs to resolve
// without a project predicate, or the pair is unnameable and the repair #786 is
// about does not exist (#786).
//
// This is the WIDENING of a ref's scope and nothing else. It decides which
// memories may be NAMED, never which edge may be changed: the request still ends
// at LinksInto, whose ownership rule requires an endpoint in `_global`
// itself, so a ref that resolves into some other project buys the operator
// nothing but an honest refusal about the edge. And it is not reachable for a
// named project — `ghost supersede p --withdraw` still cannot name a memory in q.
func resolveRef(ctx context.Context, store WithdrawStore, projectID, which, ref string) (string, error) {
	if projectID == memory.GlobalProjectID {
		ids, err := store.MemoryIDsByIDPrefixAnyProject(ctx, ref)
		if err != nil {
			return "", err
		}
		// The rules, not a second copy: ResolveIn is memref's unscoped entry
		// point, so a ref is judged identically here and a refusal names no
		// project, because the set it searched is not confined to one.
		return memref.ResolveIn(ids, which, ref)
	}
	return memref.Resolve(ctx, store, projectID, which, ref)
}

// intoSuffix names the live edges that DO point at a target, for the refusal
// that follows a missing one.
//
// It names the holders and says NOTHING about whose they are, which is a change
// and not an omission. The read is scoped to "either endpoint is ours", so a
// holder is as likely to be a memory in `_global` — promoted there with its links
// intact — as one in the project itself, and the sentence that claimed otherwise
// was asserting an ownership the read does not establish (#786's review). Every
// claim it does make is about the TARGET, which the caller named and which is in
// scope by construction: these edges bury that memory, and one of them may still
// hold it down after the pair they asked about is withdrawn.
//
// The empty case is scoped on purpose and says "in this project": with no holder
// the sentence is a claim about the whole graph, and one is false whenever the
// edge exists under a scope this read cannot see.
func intoSuffix(links []memory.Link) string {
	if len(links) == 0 {
		return " (no memory in this project links it)"
	}
	parts := make([]string, 0, len(links))
	for _, l := range links {
		parts = append(parts, short(l.SourceID))
	}
	return " (still " + relationWords(links) + " by " + strings.Join(parts, ", ") +
		" — withdraw that pair as well, or note that the other edge still buries it)"
}

// relationWords picks the verb phrase that matches the relations actually present.
func relationWords(links []memory.Link) string {
	hasSup, hasCauses := false, false
	for _, l := range links {
		switch l.Relation {
		case string(RelationSupersedes):
			hasSup = true
		case string(RelationCauses):
			hasCauses = true
		}
	}
	switch {
	case hasSup && hasCauses:
		return "linked (superseded or caused)"
	case hasCauses:
		return "caused"
	default:
		return "superseded"
	}
}

// short is the report's id form, measured in CHARACTERS. The rules and the
// reasoning are in internal/memref, which every surface that names a memory by
// ref reports through.
func short(id string) string {
	return memref.Short(id)
}
