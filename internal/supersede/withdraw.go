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
// forever. Only a person who can see both notes can name the pair.
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
)

// minRefLen is the shortest PREFIX a caller may pass. A full id is always
// accepted whatever its length or shape, because it names one row and the store
// holds it; a prefix has to be an identity rather than a class, and below this
// length it names a large slice of any corpus. Ids are hex(randomblob(16)), so
// eight characters is the first length at which a prefix of a real corpus is
// usually one row — and a corpus where it is not is answered with the ambiguity
// listing, which says so and asks for more characters.
const minRefLen = 8

// WithdrawStore is the subset of *memory.Store a targeted withdrawal needs;
// narrowed for testability. Nothing here judges an edge: the store is asked what
// is live and is asked to invalidate, and the decision is the caller's.
type WithdrawStore interface {
	// MemoryIDsByIDPrefix resolves one ref to the memory ids it can mean,
	// scoped to the project (plus _global).
	MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error)
	// SupersedesLinksInto returns the live 'supersedes' edges pointing at a
	// memory, restricted to the ones the project owns through their source.
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
			// count and the list go back WITH the error.
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

// resolveRef turns one ref into a memory id in the project.
//
// A ref is matched as a LITERAL PREFIX of a memory id, which is what lets one
// query answer both forms: a full id of any shape matches itself, so an id an
// imported artifact wrote verbatim — `ghost import` preserves the ids it reads,
// and nothing about an id column says they are hex — is nameable, while 8 or
// more characters of one is the shortest prefix that can serve as an identity
// (below it, a prefix names a large slice of any corpus, and a large slice is
// not a decision).
//
// The length floor applies to a PREFIX only. A ref the store holds verbatim is a
// full id whatever its length or shape, and refusing one would be a dead end on
// an imported corpus — so the two forms are told apart by comparing the match
// with the ref rather than by inspecting the ref's characters, which is what
// would reject an imported id for not being hex.
//
// An ambiguous ref is refused with the matches listed. The alternative is
// choosing which memory to delete a link from, which is not a decision this
// function may make.
//
// The refusals say which of the two forms failed, because they fail for
// different reasons and a reader who pasted a full id needs to be told that the
// store does not hold it, not that the string they pasted is malformed.
func resolveRef(ctx context.Context, store WithdrawStore, projectID, which, ref string) (string, error) {
	ids, err := store.MemoryIDsByIDPrefix(ctx, projectID, ref)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 1:
		if len(ref) < minRefLen && !strings.EqualFold(ids[0], ref) {
			// A short ref that is not itself an id names a slice rather than a
			// row, and this project happens to hold exactly one id in that slice.
			// In a larger corpus the same ref would be ambiguous, so accepting it
			// would make the answer depend on the size of the project.
			return "", fmt.Errorf("the %s ref %q matches memory %s, but %d character(s) is too short to be a prefix of one — a prefix needs %d or more",
				which, ref, short(ids[0]), len(ref), minRefLen)
		}
		return ids[0], nil
	case 0:
		if len(ref) < minRefLen {
			return "", fmt.Errorf("no memory in project %s has the id %q, and %d character(s) is too short to be a prefix of one — a prefix needs %d or more",
				projectID, ref, len(ref), minRefLen)
		}
		return "", fmt.Errorf("no memory in project %s has an id starting with %q (%s)", projectID, ref, which)
	}
	// Every match is named, because the answer to an ambiguity is more
	// characters and the reader has to know what to type.
	return "", fmt.Errorf("the %s ref %q is ambiguous in project %s: %s — pass more characters of the id to choose one",
		which, ref, projectID, strings.Join(ids, ", "))
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

// short is the report's id form: the first eight characters, which is what every
// Ghost report prints and therefore what an operator will have on screen.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
