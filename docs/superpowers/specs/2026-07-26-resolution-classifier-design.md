# Resolution Classifier: de-weighting resolved work at injection

**Status:** Design approved (2026-07-26). Ready for implementation planning.
**Author:** Wayne (wcatz)
**Supersedes:** the earlier MMR/diversity approach explored in this thread, which was
falsified by live measurement (no embedding valley to threshold; see §7).

---

## 1. Problem

The session-start hook injects the top 25 project memories into every conversation
using a plain SQL sort:

```sql
ORDER BY pinned DESC, importance DESC, updated_at DESC LIMIT 25
```

No time-decay, no relevance, no diversity. On the live `ghost` corpus (44 memories),
**15 of the 25 injected slots** are consumed by a single burst of *resolved work* —
LongMemEval benchmark findings and the graph-expansion kill-experiment, all concluded.
They crowd out active, useful context every session and cost ~3.5k prompt tokens.

### 1.1 Why no structured signal can fix this

The pivotal finding of the research phase, verified against the live DB:

> The last ~6 commits are all LongMemEval + graph-expansion. These dead threads are
> simultaneously the **most recent** work in git **and** the **least useful** to inject.

Recency and relevance are therefore *orthogonal* to usefulness here, by construction.
Every structured signal was tested on live data and eliminated:

| Signal | Result | Why |
|---|---|---|
| Episode key (session FK) | dead | no session/conversation FK exists on `memories` |
| Embedding clustering (MMR / agglomerative) | dead | intra-cluster cosine 0.60–0.66 barely exceeds cross-cluster 0.58–0.59 — no valley to threshold |
| Category time-decay at injection | dead | burst is 4–5 days old; decay factor ~0.85, still ranks top |
| Tier-budgeting by age | dead | 31 of 44 memories are ≤7d; the burst sits *inside* the fresh tier |
| Existing emergent `resolved` tag | insufficient | reflection tags it topically and inconsistently — 1/20 coverage of the burst |
| Supersede (`memory_links`) | correctly inert | scoped to same-fact *value replacement*, not resolution; resolution ≠ supersession |

The only separator of resolved-vs-active work is **content semantics**. This design
adds a content-level classifier — the one mechanism the evidence supports.

---

## 2. Core concept: conclusion vs. evidence

The classifier question is crisp and one-word-answerable, deliberately the same shape
as the supersede classifier (which works *because* its question is crisp):

> **Is this memory a terminal conclusion, or intermediate evidence for a conclusion
> recorded elsewhere?**

- **Conclusion** → stays injectable. Example: `AD031046` "Graph-expansion RESOLVED
  NO-GO" — the decision record must survive.
- **Evidence** → resolved; drops from injection, stays searchable. Example: the three
  kill-experiment findings that *led to* that NO-GO, fixed-bug changelog notes, cost
  estimates, PR-locator facts.

### 2.1 Eval set (hand-labeled, 20 dead-thread memories)

| Verdict | Count | Representative IDs |
|---|---|---|
| KEEP (terminal conclusion / active decision) | ~5 | `AD031046`, `0D82000A`, `EEE92419`, `29CC663F`, `2CDB8A43` |
| DROP (evidence / resolved / changelog) | ~11 | `A7554AFC`, `821C4594`, `A37D5DE1`, `F3E6202C`, `3606F17C`, `312A2CEB`, `1F307E0C`, `74A37CBA`, `7A92C48D`, `1FDCCE4F`, `3B1F106D` |
| Borderline (reusable gotcha or current work) | ~4 | `EBF0549D`, `F486C788`, `AC8DCD90` |

**Honesty note:** ~4 of 20 are a genuine gray zone (a reusable gotcha *about* a resolved
thread is not the same as evidence). The classifier will have borderline cases; the prompt
must bias toward KEEP (a false-resolve buries a useful memory; a missed-resolve merely
leaves the status quo), mirroring supersede's bias-to-NO.

---

## 3. Mechanism

Four parts. Detection is a standalone CLI batch, fully off every hot path; consumption is pure SQL.

### 3.1 Storage — dedicated column, not a tag

Add a nullable column to `memories`:

```sql
resolved_at TIMESTAMP   -- NULL = active/unknown; set = classified as resolved evidence
```

A dedicated column, not a tag, because tags are emergent and unreliable (1/20 coverage,
§1.1). Schema lives in the embedded string constant in `internal/memory/schema.go`; add a
migration guard so existing DBs gain the column idempotently (Ghost's schema is applied on
open — follow the existing additive-column pattern).

### 3.2 Detection — `ghost resolve` CLI batch, prefiltered

Runs as a **standalone `ghost resolve <project> [--apply]` command**, modeled exactly on
`ghost supersede`: dry-run by default, `--apply` writes. It is *not* wired into any hook.

Rationale (revised from the exploratory "stop hook, async" framing): `HandleStopHook`
carries a hard contract — "It performs no database access" (`internal/mcpinit/stophook.go:47`)
and "must never trap a session" (every failure path returns silently). A synchronous
per-memory LLM pass in that path would violate both. A standalone batch command honors the
contract, matches an existing precedent the codebase already trusts (`ghost supersede`), and
keeps detection fully off every hot path. A future fire-and-forget trigger from the stop hook
(spawning `ghost resolve --apply` detached, no inline DB/LLM work) is possible but explicitly
**deferred** — out of scope for this plan.

1. **Keyword prefilter** bounds LLM calls to plausible candidates. Seed set:
   `NO-GO`, `resolved`, `shipped`, `retracted`, `superseded`, `abandoned`,
   `fixed in`, `removed`, `merged` / PR-merge patterns, `deadlock ... root cause`.
   Only flagged, currently-`NULL`, non-exempt memories are adjudicated.
2. **LLM adjudication** — one call *per candidate memory* (strictly cheaper than
   supersede, which calls *per candidate pair*). The prompt asks the conclusion-vs-evidence
   question from §2, biased toward KEEP, with the same `«…»` data-delimiter untrusted-content
   guard used in `internal/supersede/haiku.go`.
3. **Provider** — default is the existing `ai.Client` (Haiku), already in the binary, zero
   new dependency. A local Ollama model (`qwen2.5:3b`, already pulled) is offered as a
   free/offline option behind config, not the foundation.
4. **Recall-loss visibility** — log the count of memories the prefilter *skipped* so
   silent recall loss is observable, not hidden.

### 3.3 Injection & browse — one SQL clause, applied to both ranked paths

Two ranked read paths gain the same predicate:

```sql
... WHERE project_id = ?  AND resolved_at IS NULL
ORDER BY pinned DESC, importance DESC, updated_at DESC LIMIT 25
```

- **Session-start injection** — `loadSessionContext` in `internal/mcpinit/hook.go:235-240`.
- **`GetTopMemories`** ranked browse in `internal/memory/store.go:430`. It is a *ranked
  top-N browse* mirroring injection intent, so it filters resolved memories too — otherwise
  the same memory is hidden from injection yet visible from browse with no documented reason.

Explicit **search** paths (`SearchFTS` / `SearchHybrid`, behind `ghost_memory_search`) are
left **unfiltered**: resolved memories stay fully findable. This is precisely the locked
decision — **drop from the ranked/injected surface, keep searchable.**

### 3.4 Reversibility — un-resolve on write, not on read

If resolved work resumes, the memory must come back. The revive happens on the **write**
paths that already signal "this memory is active again", not on read:

- `Upsert` strengthen branch (`internal/memory/store.go:396-401`) — fires when Claude
  re-saves a duplicate memory; adds `resolved_at = NULL` to the existing `UPDATE`.
- `UpdateMemory` (`internal/memory/store.go:636-640`) — an explicit edit; adds
  `resolved_at = NULL` to the existing `UPDATE`.

The read paths cannot un-resolve and must not try: injection uses a **read-only DSN**
(`roDSN`, `hook.go:23-30`) and search never touches rows. The dead `Touch()` is therefore
**left dead** — the earlier "revive `Touch()`" framing assumed a read-side access counter
that no path actually calls. Un-resolve on write is the concrete, caller-backed signal.

---

## 4. Guardrails (hard)

Never classify or drop:

- **pinned** memories;
- the **standing-preference categories**: `convention`, `preference`.

"Never push to main" (`D4611B40`, **convention**) must never be evictable — the convention
exemption covers exactly the standing-rule risk that motivates this guardrail. `fact` is
**deliberately not exempt**: the corpus's resolved `fact` rows are changelog/PR-locator notes
(`3606F17C`, `312A2CEB`, `F3E6202C`, `1F307E0C`) that are the very evidence we want to
de-weight — exempting `fact` would block 4 of the 11 clear-DROP candidates for no safety
gain. Exempt categories are excluded at the **prefilter stage** — they never reach the LLM.

---

## 5. Measured impact (bounded, honest)

Simulated from the hand-labels against the live injection sort:

| | corpus | dead-in-injection BEFORE | AFTER dropping resolved evidence |
|---|---|---|---|
| LIMIT 25 | 44 | 15/25 | 9/25 |
| LIMIT 15 | 44 | 10/15 | 7/15 |

At LIMIT 25, **7 evidence memories** occupy injection slots today; all 7 drop, freeing 7
slots for active context. The residual "dead-looking" 9/25 is *mostly the ~5 conclusions we
deliberately keep* (`AD031046`, `0D82000A`, …) — correct behavior, not a miss.

**Saturation caveat (must stay in the spec):** at n=44 with 25 injected, 57% of the corpus
is injected every session and only 23 memories are non-dead. The measured win is *bounded by
corpus composition at this size* and grows as the corpus grows. The LIMIT-15 de-saturated
run confirms the mechanism discriminates (drops evidence) rather than merely exhausting the
pool.

---

## 6. Components & boundaries

| Unit | Purpose | Depends on |
|---|---|---|
| `memories.resolved_at` column + `migrateV2` | persist the resolution signal | `internal/memory/schema.go`, `internal/memory/migrate.go` |
| `internal/resolve` package (`Classifier`, `Prefilter`, `Run`) | conclusion-vs-evidence, one call/candidate, dry-run/apply | `internal/ai`, supersede's delimiter guard |
| store helpers (`ResolveCandidates`, `SetResolved`) | load non-exempt NULL rows; write `resolved_at` | `internal/memory/store.go` |
| `ghost resolve <project> [--apply]` command | prefilter → adjudicate → write | `internal/resolve`, `cmd/ghost/main.go` |
| injection + browse predicate | `AND resolved_at IS NULL` on both ranked paths | `internal/mcpinit/hook.go`, `internal/memory/store.go` |
| un-resolve on write | clear `resolved_at` in `Upsert`/`UpdateMemory` | `internal/memory/store.go` |

Each is independently testable: the classifier against the §2.1 eval set; the injection
predicate against a seeded DB; un-resolve against an `Upsert`/`UpdateMemory` fixture.

---

## 7. Rejected alternatives

- **MMR / diversity re-ranking** — no embedding valley on this corpus (§1.1); would not
  separate the burst. Falsified by live cosine measurement.
- **Reuse supersede** — correctly scoped to value-replacement; resolution is a different
  concept and supersede is properly inert on it.
- **Reuse reflection's stale-drop** — reflection is a whole-corpus destructive rewrite
  gated by an empty-set guard, not a reversible per-memory signal; wrong granularity and
  wrong risk profile for the hot-path injection problem.
- **Age/tier budgeting** — the burst is the freshest work; age can't separate it.

---

## 8. Testing

- **Classifier accuracy** — run against the 20-memory hand-labeled eval set (§2.1) with a
  fake classifier for deterministic unit tests; require clear-DROP resolved and the
  convention/preference/pinned exemptions honored; report borderline behavior explicitly.
- **Injection & browse predicate** — seeded DB with `resolved_at` set on N rows; assert they
  are absent from both injection and `GetTopMemories`, and present in `SearchFTS`.
- **Reversibility** — set `resolved_at`, then `Upsert` a duplicate / `UpdateMemory` the row;
  assert `resolved_at` is cleared and the row re-injects.
- **Prefilter** — assert exempt categories and pinned rows never reach the classifier;
  assert the skipped-count log fires.
- **Migration** — open a populated pre-v2 DB, run `migrateV2`, assert the column exists, all
  rows survive, and `SearchFTS` still returns rows (FTS external-content unbroken).
- `go vet ./...` before commit; feature branch + PR.
