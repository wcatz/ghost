# Reviewer ground-truth baseline

> **Historical record — not current documentation.** This file preserves the
> design or implementation state at the time it was written. For current Ghost
> behavior, start with [`docs/README.md`](../../README.md) and the source.

**Date:** 2026-09-14
**Transport:** opencode 1.18.30, model `opencode/big-pickle`, `--format json`
**Fixture:** `.github/scripts/fixtures/known_bad.go` (`//go:build ignore`)
**Measured on:** PR #424, Reviewer run 34860044029, head `345d21c`

This is the number later prompt changes are measured against. Without a
recorded baseline, "the prompt got better" is unfalsifiable.

## Recall: 3 / 3

| # | Planted defect | Found | Anchor | Severity |
|---|---|---|---|---|
| 1 | `saveMemory` discards the error from `db.Exec`, reporting a failed write as success | yes | `known_bad.go:16` — inside `saveMemory` | should-fix |
| 2 | `countAll` mutates `counter` from multiple goroutines with no synchronisation | yes | `known_bad.go:29` — inside `countAll` | should-fix |
| 3 | `listIDs` never closes `rows`, leaking a connection per call | yes | `known_bad.go:50` — inside `listIDs` | should-fix |

Every finding anchored inside the function containing its defect, and every
one named a correct concrete fix (`defer rows.Close()` after the nil-error
check; `atomic.Int64` or a mutex; check and propagate the `Exec` error).

## False positives on the fixture: 0

Verdict `should-fix`, 3 findings, 0 nits. Nothing was reported that was not
planted.

## False positives observed elsewhere in the same session: 1

Worth recording, because recall alone overstates the picture. On PR #423 the
reviewer raised, as an unanchored `should-fix`:

> **Cache can never be saved — workflow lacks `actions: write`** — Sending a
> cache entry to the Actions cache service requires the `actions: write`
> workflow permission; with an explicit permissions block all unlisted scopes
> default to none. The `actions/cache` step therefore 403s on every save.

This is false, and the run that produced the finding disproves it:

```
Cache the opencode tarball        14:59:49  Cache not found for input keys: opencode-tarball-linux-x64-1.18.30-5500724685816549
Post Cache the opencode tarball   15:04:20  Cache saved with key: opencode-tarball-linux-x64-1.18.30-5500724685816549
```

The save succeeded under `contents: read` + `pull-requests: write`. The premise
is wrong: the cache service authenticates with `ACTIONS_RUNTIME_TOKEN`, issued
to the runner regardless of the `permissions:` block. Acting on the finding
would have widened the token for no benefit, in a workflow that deliberately
runs a model against untrusted input.

**Shape of the failure, for future prompt work:** confident, internally
coherent, specific about mechanism, and contradicted by the log of the very run
that raised it. The model does not read the run log — it sees only the diff and
the checkout — so it cannot self-check claims about runtime behaviour. Findings
that assert what CI *will* do are the class to distrust.

## True positives on real code in the same session: 4 / 4

All raised against PR #423 and all confirmed real:

1. Sweeper ran on failed and cancelled Reviewer runs — `types: [completed]`
   also fires on `failure`, so stale threads could be auto-resolved on the
   strength of a review that never produced a judgement.
2. The prior-threads query was not paginated — the same silent 100-thread cap
   `sweeper.yml` documents and fixes.
3. A fixed `actions/cache` key is a one-way door — the action never overwrites
   an existing key and skips its save on an exact hit.
4. The cache step had no `continue-on-error`, so a cache-service outage killed
   the review *before* the checksum-guarded recovery in Install could run.

Findings 3 and 4 concern `actions/cache` semantics the author had wrong.

## Severity partition, verified live

**Fixture:** `.github/scripts/fixtures/nit_probe.go` (`//go:build ignore`)
**Measured on:** PR #425, Reviewer run 34861183180, head `2c17f21`

Every finding produced up to this point had been `should-fix`, so the
`nit` half of the severity policy — the README's claim that nits go in a
collapsed body block and never block merge — had unit tests behind it
(`test_review_findings.py::test_nits_never_become_comments`) but no live
evidence. The fixture plants one correctness bug and two purely cosmetic
issues to force a mixed review.

The model classified all three correctly: `blocker` for the index-before-
length-check, `nit` for the stuttering type name and the redundant `else`.

**Inline comments (`pulls/425/comments`) — one, the blocker only:**

```
.github/scripts/fixtures/nit_probe.go:20 :: **🔴 blocker — index out of bounds before length check**
```

**Review body — both nits, collapsed and non-blocking:**

```
**Verdict:** `should-fix` — 1 inline finding(s), 2 nit(s).

<details><summary>Nits (2) — non-blocking</summary>

- `.github/scripts/fixtures/nit_probe.go:22` **stuttering type name** — ...
- `.github/scripts/fixtures/nit_probe.go:31` **redundant else after return** — ...

</details>
```

Neither nit produced an inline thread, so neither can hold the merge gate
under `required_conversation_resolution`. The step log alone cannot show
this: `posted review: N inline finding(s)` omits the nit count entirely, so
a working partition and a review with no nits at all log identically. The
two API reads above are the evidence.

Recorded verbatim because the probe branch was deleted with the PR.
