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
	// SupersedesLinksInto returns the live 'supersedes' edges pointing at a
	// memory, restricted to the ones the project owns through EITHER endpoint
	// (or through the shared scope).
	SupersedesLinksInto(ctx context.Context, projectID, memoryID string) ([]memory.Link, error)
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
	Source string
	Target string
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
	SourceID   string
	TargetID   string
	TargetText string  // the target's own content, as stored
	LinkSource string  // the edge's own `source` column
	Strength   float32 // the edge's stored similarity, as written
	Withdrawn  bool    // this call moved it out of the live set
	// WithdrawalFailed marks the row whose own write errored, and NotAttempted
	// the rows after it, which this run never reached because each invalidation
	// is its own transaction. They are separate states because a report that
	// described either as "already gone" would be claiming a concurrent pass
	// removed an edge this run did not touch — and for a not-attempted row that
	// edge is still live.
	WithdrawalFailed bool
	NotAttempted     bool
}

// RepairableTargets is the follow-up's id list: the targets of the edges a
// withdrawal reported, deduplicated, in the order the rows were reported, and in
// FULL — not the eight-character abbreviations the reports use, because a
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
// It is EXPORTED and lives here because two surfaces printed this exact rule
// (cmd/ghost for `ghost supersede --withdraw`, internal/mcpserver for
// ghost_link_withdraw) over `[]WithdrawnLink` — the same type, so nothing forced
// the duplication — and only the CLI's copy had a test. Two copies is two answers
// to which memories a repair can still clear, and a wrong answer does not print a
// wrong report, it clears the wrong memories. `TestRepairableTargets*` pins it
// once, in the package that owns the type.
func RepairableTargets(links []WithdrawnLink) []string {
	var out []string
	seen := make(map[string]bool, len(links))
	for _, l := range links {
		if l.TargetID == "" || seen[l.TargetID] || l.NotAttempted || l.WithdrawalFailed {
			continue
		}
		seen[l.TargetID] = true
		out = append(out, l.TargetID)
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
		n, err := store.InvalidateLink(ctx, res.Links[i].SourceID, res.Links[i].TargetID, string(RelationSupersedes))
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
			return res, fmt.Errorf("withdraw supersedes link %s→%s: %w", res.Links[i].SourceID, res.Links[i].TargetID, err)
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
	textByID := make(map[string]string, len(mems))
	for _, m := range mems {
		textByID[m.ID] = m.Content
	}
	for i := range links {
		links[i].TargetText = textByID[links[i].TargetID]
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
	sourceID, err := resolveRef(ctx, store, projectID, "source", pair.Source)
	if err != nil {
		return link, err
	}
	targetID, err := resolveRef(ctx, store, projectID, "target", pair.Target)
	if err != nil {
		return link, err
	}
	if sourceID == targetID {
		// Not a missing edge: CreateLink refuses self-links, so this pair can
		// never be one, and saying that is more use than "no live supersedes
		// link" for an operator who mistyped a ref.
		return link, fmt.Errorf("%s supersedes itself: both refs are memory %s", short(targetID), targetID)
	}

	into, err := store.SupersedesLinksInto(ctx, projectID, targetID)
	if err != nil {
		return link, err
	}
	for _, l := range into {
		if l.SourceID != sourceID {
			continue
		}
		return WithdrawnLink{
			SourceID:   l.SourceID,
			TargetID:   l.TargetID,
			LinkSource: l.Source,
			Strength:   l.Strength,
		}, nil
	}
	return link, fmt.Errorf("no live supersedes link %s → %s in project %s%s",
		short(sourceID), short(targetID), projectID, intoSuffix(into))
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
// at SupersedesLinksInto, whose ownership rule requires an endpoint in `_global`
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
// that follows a missing one. It is project-scoped by the read it came from, so
// it cannot report another project's edge — and every sentence it produces says
// so, because an edge whose source was promoted to `_global` exists without
// being visible here and "nothing supersedes that memory" would be false.
func intoSuffix(links []memory.Link) string {
	if len(links) == 0 {
		return " (no memory in this project supersedes it)"
	}
	parts := make([]string, 0, len(links))
	for _, l := range links {
		parts = append(parts, short(l.SourceID))
	}
	return " (superseded, from this project, by " + strings.Join(parts, ", ") +
		" — withdraw that pair as well, or note that the other edge still buries it)"
}

// short is the report's id form, measured in CHARACTERS. The rules and the
// reasoning are in internal/memref, which every surface that names a memory by
// ref reports through.
func short(id string) string {
	return memref.Short(id)
}
