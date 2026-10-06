// Package resolve marks "resolved-evidence" memories so they drop out of the
// ranked session-start injection while staying searchable. It is the detection
// half of the resolution classifier; consumption is the AND resolved_at IS NULL
// predicate on the injection/browse queries.
//
// Design mirrors internal/supersede: a cheap local prefilter proposes
// candidates, a deterministic KEEP veto (veto.go) settles the notes that state a
// standing rule or an open problem without asking anything, an LLM Classifier
// adjudicates the rest in batches of up to eight with a numbered
// KEEP/RESOLVED question (biased to KEEP, and a RESOLVED must name what closed
// the note), and — with apply — the confirmed set is stamped via SetResolved
// while newly-judged KEEP verdicts are cached by content hash in
// memories.resolve_kept_hash so a converged project makes no classifier calls at
// all. The LLM Classifier implementation lives in resolution.go; the hosting
// binary supplies a CLI-harness provider (see internal/ai). The stop hook spawns
// `ghost lifecycle --project <id>` as a detached background process
// (internal/mcpinit/stophook.go), whose resolve phase runs this command with
// --apply. The pass is re-runnable and idempotent — already-resolved rows are
// excluded by ResolveCandidates, and Reassess (reassess.go) is the repair pass
// for the rows this one already got wrong.
package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// resolveKeywords bounds LLM calls to plausible candidates: memories whose text
// signals a concluded/closed thread. Case-insensitive substring match. Missing
// a keyword only costs recall (the memory stays injectable), so the set is
// deliberately conservative — false negatives are cheap, false positives reach
// the KEEP-biased LLM which is the real gate.
//
// All entries must be lowercase: they are matched against
// strings.ToLower(content) in Prefilter.
var resolveKeywords = []string{
	"no-go", "resolved", "shipped", "retracted", "superseded", "abandoned",
	"fixed in", "removed", "merged", "kill experiment", "root cause",
	"concluded", "closed", "reverted", "deprecated", "landed in",
	// Concluded-work genres the eval corpus showed slipping the net
	// (finding F2, issue #336). Multi-word phrases stay high-precision;
	// "completed" is broader but false positives only cost one KEEP-biased
	// LLM call.
	"cost estimate", "postmortem", "changelog:", "investigation note",
	"pr locator", "reference only", "history only", "no follow-up",
	"completed",
}

// Verdict is the classifier's answer for one memory. UNKNOWN means the reply
// did not contain an explicit, parseable KEEP or RESOLVED verdict; it is kept
// visible and is deliberately not entered into the KEEP cache so the next pass
// can ask again.
type Verdict string

const (
	// VerdictResolved means the note is resolved evidence and may be stamped.
	VerdictResolved Verdict = "resolved"
	// VerdictKeep means the note is an explicit terminal conclusion or other
	// still-active knowledge.
	VerdictKeep Verdict = "keep"
	// VerdictUnknown means no explicit verdict could be parsed.
	VerdictUnknown Verdict = "unknown"
)

// Classifier decides whether each memory is resolved evidence, an explicit
// terminal conclusion / still-active knowledge, or an unparseable answer. The
// LLM implementation lives in resolution.go; tests inject a deterministic fake.
// It is biased to KEEP when the model expresses uncertainty, but a parse
// failure is UNKNOWN rather than an implicit KEEP. Batched so one call
// adjudicates many notes; the KEEP-cache skip happens in Run.
type Classifier interface {
	IsResolvedBatch(ctx context.Context, contents []string) ([]Verdict, error)
}

// keepCacheHashVersion versions the KEEP-cache key. Bump it whenever a cached
// KEEP verdict would mean something new, so rows judged under the old rules are
// re-asked instead of trusted — the same reason v2 existed.
//
//   - v1: every non-RESOLVED result, including a parse failure, was KEEP.
//   - v2: only an explicit KEEP verdict is cached; a parse failure is UNKNOWN.
//   - v3: a KEEP verdict also requires the classifier to have named what closed
//     the note, and a note carrying an imperative or an open marker is KEEP
//     before the call (issue #640). Every v2 entry was judged without those
//     rules, so all of them are deliberately re-asked.
//
// Issue #674 added a rubric rule — a note that only restates the repository is
// RESOLVED evidence — and deliberately did NOT bump this prefix. The rule reads
// the fresh-session question the v3 rules already read, so a cached KEEP and a
// re-ask usually agree; where they do not, the stale entry leaves the note
// INJECTABLE rather than buried, which is the direction this pass errs in
// everywhere else. Bumping would re-ask every cached KEEP in every project for
// that, which is the far larger cost.
const keepCacheHashVersion = "v3"

// ContentHash is the KEEP-cache key: resolve's question is content-only, so a
// tag or importance edit must not invalidate a cached verdict. The version
// prefix is hashed in with the content (see keepCacheHashVersion).
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(keepCacheHashVersion + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

// resolveStore is the subset of *memory.Store the pass needs; narrowed for
// testability.
type resolveStore interface {
	ResolveCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	SetResolved(ctx context.Context, ids []string) (int, error)
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	ResolveKeptHashes(ctx context.Context, projectID string) (map[string]string, error)
	MarkResolveKept(ctx context.Context, projectID string, hashes map[string]string) error
	// UsefulnessByMemory is #648's negative evidence: what the retrieval audit has
	// recorded about each memory. It is on the ordinary pass's store and NOT on
	// reassessStore, because the repair pass exists to UNDO resolutions and the
	// evidence points the other way — telling a repair that a memory was
	// contradicted three times argues for leaving it resolved.
	UsefulnessByMemory(ctx context.Context, projectID string) (map[string]memory.UsefulnessEvidence, error)
}

// linkScopeReader is the one method scopeCompatibleSupersedes needs. Both
// resolveStore and reassessStore satisfy it, so the rule is stated once for the
// ordinary pass and the repair pass instead of twice.
type linkScopeReader interface {
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
}

// scopeCompatibleSupersedes filters live 'supersedes'/'llm' links down to the
// ones the pass may act on.
//
// An edge is a verdict that one claim replaced another, and it is only that
// when both endpoints could be the same claim. A development row never replaced
// a production one, so an edge between them asserts nothing — yet the classifier
// that wrote it could not have known: it is handed the two note bodies and
// nothing else, and scope is a column beside the text. Any store consolidated
// before internal/supersede learned to refuse the pair can hold such an edge,
// and read as sound it retires a live memory: the ordinary pass stamps
// resolved_at on it, which is persisted, removes it from ranked injection
// everywhere, and is undone only by an explicit repair pass.
//
// The endpoints' scopes are read from the store rather than from the link,
// because the link row carries neither. A link whose source cannot be loaded is
// dropped rather than trusted: memory_links cascades with its memories, so the
// case should be unreachable, and if it is reached the two failure modes are not
// equal — a pair the classifier never sees can be judged on its own merits next
// pass, while a row resolved on a verdict that turned out to be unsound needs
// --reassess to undo.
func scopeCompatibleSupersedes(ctx context.Context, store linkScopeReader, links []memory.Link) ([]memory.Link, error) {
	if len(links) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(links)*2)
	seen := make(map[string]bool, len(links)*2)
	for _, l := range links {
		for _, id := range []string{l.SourceID, l.TargetID} {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("load supersedes link endpoints: %w", err)
	}
	scopeByID := make(map[string]map[string]string, len(mems))
	for _, m := range mems {
		scopeByID[m.ID] = m.Scope
	}

	out := make([]memory.Link, 0, len(links))
	for _, l := range links {
		newer, ok := scopeByID[l.SourceID]
		if !ok {
			continue
		}
		older, ok := scopeByID[l.TargetID]
		if !ok {
			// The target is the row the pass acts on, so a missing one is not a
			// pair to judge; the caller finds it in its own pool or not at all.
			continue
		}
		if memory.ScopesConflict(newer, older) {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// Result summarizes a pass.
type Result struct {
	Loaded     int // candidates returned by the store (already category/pin/NULL filtered)
	Candidates int // survived the keyword prefilter (demoted + then classified)
	Confirmed  int // classified as resolved evidence by the LLM
	Superseded int // older endpoint of a live 'supersedes'/'llm' link, demoted deterministically
	Corrected  int // older prefilter-passing memory tied to a correction, demoted deterministically
	Vetoed     int // candidates settled KEEP by the deterministic veto, no classifier call
	Skipped    int // candidates skipped via the KEEP cache
	Unknown    int // candidates whose verdict could not be parsed; eligible for a later pass
	Resolved   int // rows written (0 in dry-run)
}

// Prefilter keeps only memories whose content contains a resolution keyword.
// Case-insensitive. Order is preserved.
func Prefilter(mems []memory.Memory) []memory.Memory {
	var out []memory.Memory
	for _, m := range mems {
		lc := strings.ToLower(m.Content)
		for _, kw := range resolveKeywords {
			if strings.Contains(lc, kw) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// Run loads eligible candidates, applies two deterministic demotion signals,
// prefilters the rest, settles the KEEP vetoes and the KEEP cache, classifies
// what is left in batches, and — when apply is true — stamps resolved_at on
// every confirmed memory in one batch and records the newly-judged KEEP hashes.
// Dry-run (apply=false) writes nothing but returns the confirmed set for
// preview. A classifier error on any batch is fatal so a partial pass is never
// silently applied.
//
// The veto is not a demotion: a candidate VetoKeep settles is KEEP, so it is
// never classified and never cached. Issue #640 measured the pass burying about
// one note in three, overwhelmingly notes that stated a rule in imperative form
// or an unfinished problem in open-marker form, so that signal is now read
// before the model is asked. It does not touch the two deterministic demotions
// below, which act on evidence resolve never asked about.
//
// Deterministic demotions (no LLM call, so no extra CLI spend):
//
//   - Supersedes-edge piggyback: the older endpoint of a live 'supersedes'/'llm'
//     link is already-adjudicated evidence — supersede's LLM decided the newer
//     replaces the older, so resolve acts on that verdict instead of re-asking.
//   - Correction pairing: an explicit correction memory ("CORRECTION/RESOLUTION",
//     "already fixed on main", "no PR is needed") demotes the OLDER
//     prefilter-passing memories sharing its rare subject tokens. The correction
//     itself stays live; only the claims it invalidates are demoted.
func Run(ctx context.Context, store resolveStore, cls Classifier, projectID string, apply bool, logger *slog.Logger) (Result, []memory.Memory, error) {
	loaded, err := store.ResolveCandidates(ctx, projectID)
	if err != nil {
		return Result{}, nil, fmt.Errorf("load candidates: %w", err)
	}
	byID := make(map[string]memory.Memory, len(loaded))
	for _, m := range loaded {
		byID[m.ID] = m
	}
	cands := Prefilter(loaded)
	res := Result{Loaded: len(loaded), Candidates: len(cands)}
	if logger != nil {
		logger.Info("resolve prefilter",
			"loaded", len(loaded), "kept", len(cands), "skipped", len(loaded)-len(cands))
	}

	confirmed := make([]memory.Memory, 0, len(cands))
	confirmedSet := make(map[string]bool, len(cands))
	addConfirmed := func(m memory.Memory) {
		if confirmedSet[m.ID] {
			return
		}
		confirmedSet[m.ID] = true
		confirmed = append(confirmed, m)
	}

	// Mechanism 1: supersedes-edge piggyback. The link source is the NEWER
	// memory ('supersedes' is written newer→older), so its TargetID is the older
	// now-obsolete claim. Only demote links whose older endpoint is still an
	// eligible candidate (unresolved, unpinned, non-exempt category) — anything
	// else is already handled or out of scope — and whose endpoints do not
	// conflict on scope, which is not a verdict this pass may act on
	// (scopeCompatibleSupersedes).
	links, err := store.LinksByRelationSource(ctx, projectID, "supersedes", "llm")
	if err != nil {
		return res, nil, fmt.Errorf("load supersedes links: %w", err)
	}
	links, err = scopeCompatibleSupersedes(ctx, store, links)
	if err != nil {
		return res, nil, err
	}
	for _, l := range links {
		older, ok := byID[l.TargetID]
		if !ok {
			continue
		}
		res.Superseded++
		addConfirmed(older)
	}

	// Mechanism 2: correction pairing. correctionPairTargets already returns
	// only prefilter-passing loaded candidates, so each is demoted deterministically.
	for _, m := range correctionPairTargets(loaded, cands) {
		res.Corrected++
		addConfirmed(m)
	}

	// Classify the remaining prefilter candidates; deterministically-demoted
	// memories are excluded so a concurrent verdict can't land twice. The KEEP
	// cache drops candidates whose content already earned a KEEP verdict, so a
	// converged project makes no calls at all. The key is the content hash:
	// resolve's question is content-only, so tag/importance edits do not
	// invalidate a cached verdict.
	keptHashes, err := store.ResolveKeptHashes(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load resolve kept hashes: %w", err)
	}
	var pending []memory.Memory
	var pendingContents []string
	for _, m := range cands {
		if confirmedSet[m.ID] {
			continue
		}
		// The veto runs before the cache lookup and before the classifier: a
		// note that states a rule or an open problem on its face is KEEP, so
		// it costs no harness call and no cache entry (the veto is free to
		// recompute every pass).
		if reason, vetoed := VetoKeep(m.Content); vetoed {
			res.Vetoed++
			if logger != nil {
				logger.Debug("resolve veto kept memory", "id", m.ID, "pattern", reason)
			}
			continue
		}
		if keptHashes[m.ID] == ContentHash(m.Content) {
			res.Skipped++
			// THE LIMIT OF #648's EVIDENCE HERE, STATED BECAUSE IT IS ABOVE THIS
			// LINE: a cached KEEP is dropped from the pending set BEFORE the audit's
			// evidence is read, so a memory whose content already earned a KEEP never
			// reaches the classifier again — and never reaches it WITH a contradiction
			// either. In a converged corpus, which is the state a project reaches once
			// the first pass has run, that is most of the corpus, so the evidence is
			// delivered only to the memories this pass is already going to re-ask
			// about. Resolve does not ALWAYS tell the classifier about a contradiction;
			// it tells it about contradictions on candidates this pass is asking about
			// anyway.
			//
			// The cache is deliberately NOT changed here. It is a content-keyed memo of
			// a judgement resolve already paid for, and the key is deliberately the
			// CONTENT alone so a retag or a re-weight re-asks nothing — which is
			// correct for the cache's own purpose and wrong for this one, because an
			// audit verdict arrives independently of the content. Making the evidence
			// invalidate the cache is a change to what the cache MEANS, and the fix
			// belongs beside it rather than inside this slice: #880.
			continue
		}
		pending = append(pending, m)
		pendingContents = append(pendingContents, m.Content)
	}

	// newKept collects KEEP verdicts to cache; written only on apply.
	newKept := make(map[string]string)
	var llmConfirmed int
	if len(pending) > 0 {
		// #648: one read of the audit's negative evidence for the whole pass,
		// asked only once there is something to ask about. It is read here,
		// after the pending set is known, so a converged project pays nothing —
		// and it is a single statement over the project, never one query per
		// candidate.
		//
		// It FAILS OPEN. The evidence is an addition to a judgement that already
		// works without it, so a store that cannot answer costs the pass the
		// annotation and nothing else; failing the pass would turn a missing
		// audit into a missing maintenance phase, which makes the audit a
		// dependency of resolve rather than an input to it.
		//
		// The line is appended to the CONTENT, not passed beside it, so it lands
		// inside the «...» the classifier's own quoteData renders and is read
		// under that rule with the rest of the note rather than as a line of
		// instructions from the harness. ContentHash still keys on m.Content, so
		// the KEEP cache is unaffected.
		//
		// Read this as a bound on what the evidence does, not a statement that the
		// classifier hears about every contradiction: the cache gate above drops a
		// memory with a cached KEEP before this read, so nothing here reaches a
		// candidate this pass was not going to re-ask about anyway (#880).
		evidence, evErr := store.UsefulnessByMemory(ctx, projectID)
		if evErr != nil {
			if logger != nil {
				logger.Warn("resolve usefulness evidence unavailable", "error", evErr)
			}
			evidence = nil
		}
		asked := make([]string, len(pendingContents))
		for i, m := range pending {
			asked[i] = pendingContents[i]
			if line := evidence[m.ID].Line(); line != "" {
				asked[i] = asked[i] + "\n" + line
			}
		}
		verdicts, err := cls.IsResolvedBatch(ctx, asked)
		if err != nil {
			return res, nil, fmt.Errorf("classify %d candidate(s): %w", len(pendingContents), err)
		}
		if len(verdicts) != len(pendingContents) {
			return res, nil, fmt.Errorf("classify %d candidate(s): classifier returned %d verdict(s)", len(pendingContents), len(verdicts))
		}
		for i, m := range pending {
			switch verdicts[i] {
			case VerdictKeep:
				newKept[m.ID] = ContentHash(m.Content)
			case VerdictResolved:
				res.Confirmed++
				llmConfirmed++
				addConfirmed(m)
			default:
				// UNKNOWN (or an invalid classifier value) is not an implicit
				// KEEP. Leave the memory unclassified so a later pass can ask
				// it again instead of locking a parse failure into the cache.
				res.Unknown++
			}
		}
	}
	if logger != nil {
		logger.Info("resolve classified",
			"confirmed", llmConfirmed, "superseded", res.Superseded,
			"corrected", res.Corrected, "vetoed", res.Vetoed,
			"cached", res.Skipped, "unknown", res.Unknown)
	}

	if apply {
		if len(confirmed) > 0 {
			ids := make([]string, len(confirmed))
			for i, m := range confirmed {
				ids[i] = m.ID
			}
			n, err := store.SetResolved(ctx, ids)
			if err != nil {
				return res, nil, fmt.Errorf("set resolved: %w", err)
			}
			res.Resolved = n
			if logger != nil {
				logger.Info("resolve applied", "resolved", res.Resolved)
			}
		}
		if len(newKept) > 0 {
			if err := store.MarkResolveKept(ctx, projectID, newKept); err != nil {
				// The resolved rows are already correct; losing derived cache
				// state only costs a re-classify next pass, so warn rather
				// than fail a pass whose primary effect landed.
				if logger != nil {
					logger.Warn("resolve kept cache write failed", "error", err)
				}
			}
		}
	}
	return res, confirmed, nil
}
