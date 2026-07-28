# Supersede Relation-Type Fix — Design

## Problem

`ghost supersede`'s `Classifier` interface (`internal/supersede/supersede.go:43-45`) asks a single yes/no question: does the newer memory supersede the older one? `HaikuClassifier` (`internal/supersede/haiku.go`) answers it with one prompt, and `Run()` writes a `supersedes` link (source `llm`) for every `true`.

This conflates two distinct relationships:

1. **Genuine replacement** — the newer memory restates or corrects the same fact; the older one is now stale. `supersedes` is the right edge.
2. **Cited-as-evidence** — the newer memory (typically a decision) references the older one as its rationale, but the older memory remains independently true. Today's binary classifier says YES here too, because the newer memory *is* clearly related to and prompted by the older one — it just isn't a replacement of it.

The eval (`docs/superpowers/reports/2026-07-27-ghost-eval.md`, storyline `reversed-decision`) caught this concretely: a reversal decision cited a still-valid gotcha (NATS ordering limitation) as its rationale. The classifier said YES, and the gotcha — still true, still useful — got an `llm`-sourced `supersedes` link pointing at it. That's exactly the shape of edge `SupersedeDemote` (`internal/memory/vector.go`, `demoteSuperseded`) is built to bury from ranked results. `SupersedeDemote` defaults to `false` and isn't wired into the session-start hook's injection path today, so this is a latent defect, not a live outage — but it's the wrong edge regardless, and it will misfire the moment `SupersedeDemote` is enabled or a future injection-time dedup pass (the third spec in this sequence) starts walking the link graph.

## Goals

- Replace the binary supersedes/not-supersedes classification with a 3-way classification: `SUPERSEDES` / `CAUSES` / `NEITHER`.
- Cited-as-evidence pairs get a `causes` link (schema-defined, currently unused by any writer) instead of either a wrong `supersedes` link or no link at all.
- `SupersedesWithin`'s consumer query (`internal/memory/links.go:178-215`, hard-filtered `WHERE relation = 'supersedes'`) requires zero changes — `causes` links are automatically invisible to it.
- Previously-created `llm`-sourced `supersedes` links (written by the old binary classifier) get reclassified the next time `ghost supersede` runs, self-healing bad edges without a separate migration tool — and without paying that reclassification cost indefinitely on every subsequent run (see Skip-if-unchanged below).
- Preserve `Run()`'s existing semantics: dry-run by default, one call per candidate pair, fatal-on-error (no partial-pass silent success), idempotent/re-runnable.

## Non-goals

- No change to candidate *selection* (`SelectCandidates`, cosine-similarity + `maxNeighbors` bound) — only classification and link-writing change.
- No change to `SupersedeDemote`'s consumer logic or its default (`false`). Whether/when to enable it is a separate decision, out of scope here.
- No schema migration — `causes` is already a valid `relation` value in the `memory_links` CHECK constraint.
- No general-purpose link-editing API. The reclassify mechanism below is specific to this pass's own previously-written `llm`-sourced `supersedes` links, not a generalized "change a link's relation" feature.
- This spec does not touch injection-time dedup (spec #3 in the sequence) or the resolution-unification work (spec #1, already merged as PR #218).

## Design

### Classifier interface change

`Classifier.Supersedes(ctx, newer, older) (bool, error)` becomes:

```go
type Relation string

const (
	RelationSupersedes Relation = "supersedes"
	RelationCauses     Relation = "causes"
	RelationNeither    Relation = "neither"
)

type Classifier interface {
	Classify(ctx context.Context, newer, older string) (Relation, error)
}
```

`RelationNeither` is a package-internal sentinel, never written to the DB — only `supersedes` and `causes` are ever passed to `CreateLink`.

### Prompt redesign (`HaikuClassifier`)

`classifyPrompt` (`internal/supersede/haiku.go`) is rewritten from a yes/no question to a 3-way forced choice, preserving the existing `quoteData` delimiter-based prompt-injection defense around both memory contents. The prompt draws the line explicitly:

- **SUPERSEDES**: the newer memory restates, corrects, or replaces the same underlying fact/claim as the older one — the older one is now stale and shouldn't be trusted going forward.
- **CAUSES**: the newer memory was informed by, references, or acts on the older one as supporting evidence or rationale, but the older memory's content remains independently true and useful on its own.
- **NEITHER**: the two are unrelated or the relationship doesn't cleanly fit either category above.

The response is parsed as one of the three literal tokens; an unparseable response is a classifier error (fatal, per existing `Run()` semantics), not a silent default.

### `Run()` changes

`Run()` gains a second responsibility alongside classifying fresh candidates: reclassifying this project's existing `llm`-sourced `supersedes` links, since those were written under the old binary semantics and may be misclassified `causes` pairs.

1. **Fetch existing links to reclassify.** New store method:

   ```go
   func (s *Store) LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]Link, error)
   ```

   Scoped to a project (join through `memories`), filtered to `relation = ?` and `source = ?` and `invalidated_at IS NULL`. `Run()` calls this once with `("supersedes", "llm")` to get the set of previously-confirmed pairs.

2. **Union candidates.** Fresh candidates from `SelectCandidates` and existing `llm`-sourced `supersedes` pairs are merged into one classification list, deduped by `(NewerID, OlderID)` — a pair already covered by an existing link isn't re-fetched from `SelectCandidates`, but if it independently reappears there (e.g. similarity just crossed threshold), it's still just one classification call.

3. **Classify each pair once** via the new 3-way `Classify` call — same call count per pair as today's single-call `Supersedes`, so no cost regression per pair (the only increase is pairs added by step 1, which is bounded by however many `supersedes` links already exist).

4. **Act on the verdict** (when `apply` is true):
   - `SUPERSEDES` → `CreateLink(newer, older, "supersedes", similarity, "llm")`. If an existing `causes` link exists for the same pair (from a prior run's opposite verdict), invalidate it via `InvalidateLink(ctx, older, newer, "causes")` (note the reversed argument order — see direction note below).
   - `CAUSES` → `CreateLink(older, newer, "causes", similarity, "llm")` — the *older* memory is the cause (the evidence/rationale), the *newer* memory is the effect (the decision it informed), so `causes` points older→newer, the reverse of `supersedes`' newer→older. If an existing `supersedes` link exists for the same pair, invalidate it via `InvalidateLink(ctx, newer, older, "supersedes")`.
   - `NEITHER` → invalidate whichever of `supersedes` (`newer, older`) / `causes` (`older, newer`) currently exists for the pair (if any); write nothing new.

   Invalidation happens via `InvalidateLink` (`internal/memory/links.go:218-233`), which soft-invalidates by the exact `(source_id, target_id, relation)` composite key — the correct primitive here since `relation` is part of the primary key and can't be changed via `CreateLink`'s `ON CONFLICT` upsert (that clause only updates `strength`/`invalidated_at` for an *unchanged* relation).

5. **Result reporting.** `Result` gains fields distinguishing new links from reclassifications, e.g. `CausesCreated int`, `Reclassified int` (count of existing links whose relation changed or were invalidated), alongside the existing `Candidates`, `Confirmed`, `Created`.

### Content resolution for reclassified pairs

`Classify` takes memory *content*, but `LinksByRelationSource` returns only `Link` rows (IDs and metadata — no content). The narrowed `vectorStore` interface (`internal/supersede/supersede.go:49-54`) gains:

```go
GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
```

(already implemented on `*memory.Store` at `internal/memory/vector.go:377`, just not currently exposed through this interface). `Run()` collects every memory ID referenced by the reclassify set, calls `GetByIDs` once, and builds a local `byID` map — the same shape `SelectCandidates` already builds internally, just not shared across the package boundary today.

### Skip-if-unchanged

Reclassifying every existing `supersedes` link on every run means classification cost grows with total accumulated links, not just fresh candidates — most of that cost is spent re-confirming pairs whose content hasn't moved since they were last classified. `Run()` skips a pair from the reclassify set when neither endpoint's `Memory.UpdatedAt` is newer than the existing link's `CreatedAt` (both already returned by `GetByIDs` and `LinksByRelationSource` — no schema change needed). A link only gets re-classified via this path when one of its endpoints has actually changed (e.g. via `ghost_memory_update`) since the link was written — pre-fix links, whose endpoints predate the link itself by construction, are *not* swept by this path on the first post-fix run. The self-heal the eval called for instead rides on the fresh-candidate path: any mislabeled pair still at or above the cosine threshold is re-selected by `SelectCandidates` on every run regardless of link age, gets classified under the new 3-way prompt, and is corrected (or invalidated) the same way a genuinely fresh pair would be. A mislabeled pair whose similarity has since drifted below threshold is not actively swept and stays as-is until it either re-crosses the threshold or one of its endpoints is edited.

### Project scoping and resolved memories

`LinksByRelationSource` filters by the `source_id` endpoint's `project_id` (supersede links are always written within a single project's candidate set — cross-project pairs never occur via `SelectCandidates`, so no cross-project case needs handling). Resolved memories (`resolved_at` set) are not special-cased: `SelectCandidates`'s underlying `GetAll`/`GetByIDs` queries don't filter on `resolved_at` today, so a resolved memory is already an eligible candidate/reclassification endpoint under existing behavior — this fix doesn't change that.

### Consumer impact

No changes required to `SupersedesWithin`, `demoteSuperseded`, or any `SearchParams` wiring — `causes` links are a new relation type these functions don't query for, by construction of their existing `WHERE relation = 'supersedes'` filter.

## Testing

- Table-driven tests on `Run()` with a fake `Classifier` covering all three verdicts on fresh candidates: `SUPERSEDES` writes `supersedes` (source→newer, target→older); `CAUSES` writes `causes` with the reversed direction (source→older, target→newer) — assert the exact `SourceID`/`TargetID` written, not just that *a* link exists, since the direction bug this spec fixes would otherwise pass silently; `NEITHER` writes nothing.
- A reclassification test: seed an existing `llm`-sourced `supersedes` link (`newer→older`) between two memories, run `Run()` with a fake classifier returning `CAUSES` for that pair, assert the old `supersedes` link is invalidated and a new `causes` link exists with direction `older→newer`.
- A no-op reclassification test: seed an existing `supersedes` link, fake classifier returns `SUPERSEDES` again, assert the link is unchanged (still valid, no duplicate row, per `CreateLink`'s existing upsert idempotency).
- A `NEITHER`-on-existing-link test: seed a `causes` link, fake classifier returns `NEITHER`, assert it's invalidated and nothing new is written.
- A skip-if-unchanged test: seed an existing `supersedes` link whose `created_at` is newer than both endpoints' `updated_at`; assert the pair is never passed to `Classify` at all (fake classifier records calls; assert it wasn't invoked for this pair).
- A re-trigger test: seed the same setup, then bump one endpoint's `updated_at` past the link's `created_at` (e.g. via a stub `GetByIDs` returning an updated timestamp); assert the pair *is* passed to `Classify` this time.
- `HaikuClassifier`'s prompt itself is not tested against the live API, matching existing precedent in this package (no live-API tests exist today) — the `quoteData` injection-defense wrapping is preserved and unit-tested as before, updated for the 3-way prompt's response-parsing.
