# A CodeRabbit-class PR reviewer on the opencode CLI transport

> **Historical record — the reviewer pipeline has shipped.** This design is
> retained for rationale; use `.github/workflows/reviewer.yml` and
> `.github/scripts/` for the current implementation.

**Status:** Designed (2026-09-14) — supersedes the approach in PR #421.
**Author:** Wayne (wcatz)
**Builds on:** `2026-08-26-pr-agent-upgrade-design.md`; decision `21731F28F7EA4B9BE58BF2153FA98CF0` (PR-Agent replaced CodeRabbit after rate-limit friction); PR #421's opencode CLI transport work.

## 1. Problem

wcatz/ghost has had no working automated code review for roughly two months.
This is measured, not asserted (§2): across the 55 PRs merged since CodeRabbit
was uninstalled on 2026-08-24, exactly **two** received any inline review
comment and exactly **one** committable suggestion was posted repo-wide.

PR #421 correctly diagnosed the transport failure — the opencode free tier is
CLI-only, so PR-Agent's HTTP path to `big-pickle` can never work — and built a
sound replacement: the `opencode run` CLI invocation, model-step credential
isolation, and a recursive instruction-file sweep against prompt injection from
the PR head tree. That work is worth keeping.

But in migrating the transport, #421 also discarded the review *product*: it
deletes `pr-loop.yml`, drops from line-anchored review threads to a single
prose comment, and therefore produces no unresolved conversations for
`required_conversation_resolution` to gate on. It also cannot converge — over
~9 hours it accumulated 44 commits and 32 bot reviews that oscillated between
`clean` and `should-fix` on the same items, and two of its own late "fixes"
broke CI outright.

The goal is a reviewer with CodeRabbit's shape — inline resolvable findings,
committable suggestions, a readable walkthrough, and conversational
pushback — running on the opencode CLI transport that #421 proved out.

## 2. Grounding: what the two prior eras actually produced

Measured 2026-09-14 against the GitHub API (`/repos/wcatz/ghost/pulls/{n}/comments`
over the last 80 merged PRs).

| Era | Merged PRs | PRs with ≥1 inline comment | Committable `suggestion` blocks |
|---|---|---|---|
| CodeRabbit (#313–356) | 25 | 15 (60%) — 14 authored by `coderabbitai[bot]` | 14 |
| PR-Agent (#357–412) | 55 | 2 (3.6%) | 1 |

Three conclusions follow, and each one changes the design:

1. **`auto_improve` was never a working feature here.**
   `.pr_agent.toml` set `commitable_code_suggestions = true`,
   `suggestions_score_threshold = 3`, and `num_code_suggestions = 4` for the
   entire PR-Agent era, and produced one suggestion block in 55 PRs
   (`github-actions[bot]` on `bench/memoryagentbench/embed.go`, PR #383).
   #421's PR body calls losing it "a behavioral regression," and the bot
   review loop repeatedly demanded it be preserved. That premise is false.
   Committable suggestions are worth building, but as **new work measured
   against CodeRabbit's baseline of 14 suggestions across 25 PRs**, not as
   restoration of a flag.

2. **The merge gate has been vacuous, not newly broken.**
   `required_conversation_resolution: true` is live on `main` (verified via
   `/branches/main/protection`: checks `["build-and-test","lint"]`, 0 required
   approvals, `enforce_admins: false`). But 53 of 55 merged PRs produced zero
   inline threads, so there was nothing to resolve. This is consistent with the
   recorded gotcha that PR-Agent's HTTP call to the free stealth model killed
   `pr_agent_job` silently. #421 made an existing vacuum explicit rather than
   creating it.

3. **The thread-gate machinery itself works.** PR #412 shows 12 inline threads
   authored by `review-sweeper[bot]`. The mechanism is proven; only its input
   was missing.

### 2.1 Constraints already validated (do not re-derive)

From `pr-loop.yml`'s header, validated in wcatz/ci-mech-spike on 2026-08-25:

- `GITHUB_TOKEN` **cannot** resolve review threads (`not accessible by
  integration`) and **cannot** submit `APPROVED` reviews. Resolution requires
  the `review-sweeper` App installation token (`GH_APP_ID` +
  `GH_APP_PRIVATE_KEY`).
- `GITHUB_TOKEN` **can** submit `REQUEST_CHANGES`.
- Outdated-but-unresolved threads still block merge, so stale findings must be
  swept once a fresh review has re-judged the diff.
- `workflow_run` executes in default-branch context, so nothing in that lane
  may become a required status check — required checks would attach to the
  wrong SHA.

## 3. Design

### 3.1 Decomposition

The current single `pr-agent.yml` is 475 lines at 54% comments and carries four
concerns. It is replaced by four workflows, each with one job and one purpose:

| Workflow | Trigger | Purpose |
|---|---|---|
| `reviewer.yml` | `pull_request` opened/reopened/synchronize, `issue_comment` `/review` | Produce findings, post inline threads |
| `sweeper.yml` | `workflow_run` after reviewer | Resolve stale threads, signal `REQUEST_CHANGES` |
| `summary.yml` | called by `reviewer.yml` | Upsert the sticky walkthrough comment |
| `chat.yml` | `pull_request_review_comment` | Reply in-thread |

### 3.2 The `findings.json` contract

The single seam the whole system hangs on. The model's only output is
structured data; every downstream step is deterministic shell against this
schema. No regex rescue of prose.

```json
{
  "verdict": "blocker|should-fix|nit|clean",
  "summary": "One-paragraph plain-English walkthrough of the change.",
  "findings": [
    {
      "file": ".github/workflows/gate.yml",
      "line": 46,
      "end_line": 48,
      "severity": "blocker|should-fix|nit",
      "title": "Short imperative label",
      "body": "What is wrong, why it matters, and the concrete fix.",
      "suggestion": "optional exact replacement text for line..end_line"
    }
  ]
}
```

Validation is a hard gate: a malformed document fails the job loudly rather
than posting a degraded review. `line`/`end_line` must fall inside the
incremental diff hunks, or the finding is dropped with a warning — the GitHub
Reviews API rejects out-of-diff anchors, and silently losing the whole review
to one bad anchor is the failure mode to avoid.

This replaces #421's `parts[-1]` JSON-event scraping and its first-line
`verdict:` token gate, both of which exist only because the model emits prose.

### 3.3 Convergence

Three inputs, absent today, are what stop the oscillation:

- **Incremental diff.** Review `last_reviewed_sha..head`, not `gh pr diff`.
  The prior SHA is read from the sticky summary comment's
  `<!-- reviewed:<sha> -->` marker. On first review, or when the marker is
  missing, fall back to the full PR diff.
- **Thread state as prompt input.** Fetch open and resolved bot threads with
  the cursor-paginated GraphQL query `pr-loop.yml` already implements. Open
  findings enter the prompt as *already raised — do not repeat*; resolved ones
  as *already dismissed — do not re-raise*. This is the memory the current
  reviewer lacks entirely, and the direct cause of the round-29 oscillation.
- **Base-ref conventions.** `git show main:best_practices.md` and
  `git show main:CLAUDE.md` into the prompt.

The base-ref detail is load-bearing for security. #421's recursive sweep of
`AGENTS.md`/`CLAUDE.md`/`opencode.json`/`.mcp.json` from the PR head tree, and
its `rm -rf .git`, are correct and stay exactly as built: the head tree is
attacker-controllable. Conventions are re-injected from the **base ref**, which
is not. This closes the `best_practices.md` loop — today
`best-practices-loop.yml` mines findings into that file monthly and nothing
ever reads it back.

### 3.4 Severity policy

| Severity | Inline thread | Blocks merge | Appears in summary |
|---|---|---|---|
| `blocker` | yes | yes | yes |
| `should-fix` | yes | yes | yes |
| `nit` | no | no | collapsed `<details>` block |

`REQUEST_CHANGES` fires only when live `blocker` or `should-fix` threads exist.
Nits never touch `required_conversation_resolution`. Given a free-tier model
whose observed output quality oscillates, blocking on every finding would make
the gate a nuisance rather than a signal.

### 3.5 Linter cross-check

`golangci-lint run --out-format json` and `govulncheck -json` run before the
model, and their findings enter the prompt as *already reported by
deterministic tooling — do not duplicate; treat as corroboration*. Research
memo `15EEC6CDD8BF03BCEB41B96FE23AA42E` identifies exactly this scanner layer
as CodeRabbit's noise control, and ghost already runs both tools in CI.

### 3.6 Credential boundary

Unchanged from #421, which got this right:

- The model step receives **no** `GITHUB_TOKEN`, and asserts at runtime that no
  ambient credential variable is non-empty before invoking the model.
- Checkout uses `persist-credentials: false`; `.git` is removed before the
  model runs so head-ref instruction files are unreachable via `git show`.
- The `review-sweeper` App token exists only in `sweeper.yml`, never in a job
  that runs the model.

## 4. Phasing

Each phase is one PR, green on real traffic before the next begins.

1. **Structured findings + inline threads.** `reviewer.yml` emits and validates
   `findings.json`; posts via `POST /pulls/{n}/reviews` with `comments[]`.
   Severity policy live. *This is the phase that restores the merge gate.*
2. **Sweeper.** Restore `pr-loop.yml` as `sweeper.yml`; widen the author filter
   from `startswith("github-actions")` to also match `review-sweeper[bot]`;
   gate `REQUEST_CHANGES` on severity.
3. **Convergence.** Incremental diff, thread-state feedback, base-ref
   conventions.
4. **Summary comment.** Sticky walkthrough, upserted in place per push. Carries
   the `reviewed:<sha>` marker phase 3 consumes — so if phase 3 needs the
   marker first, 4 lands before 3.
5. **Committable suggestions.** `suggestion` field rendered as fenced blocks.
6. **Linter cross-check.**
7. **Thread chat.**

Phases 1–3 deliver the core. 4–7 are additive and independently droppable.

## 5. Testing

The discipline #421 lacked: it shipped an invalid `administration:` permissions
key and a column-0 YAML break to CI, both caught only by a red run.

- **`actionlint` in CI**, as a required check on any `.github/workflows/**`
  change. Catches both of the above classes statically. `permissions:` keys are
  a closed set — `administration` is a GitHub App permission, not an Actions
  one, and Actions rejects the whole file at parse time (0 jobs, "workflow file
  issue").
- **Schema validation** of `findings.json` as a hard gate, with a fixture suite
  of malformed documents asserting each fails loudly.
- **Ground-truth fixture PR.** A scratch repo with seeded known-bad Go
  (a data race, an unchecked error, a schema-breaking struct change). The
  reviewer must find the planted bugs and anchor threads at the right lines.
  This tests against ground truth rather than eyeballing prose quality, and
  gives phases 1–3 a regression net.
- **Anchor-range unit tests** for the diff-hunk containment check, since a
  single out-of-range anchor otherwise rejects the entire review.

## 6. Disposition of PR #421

Cherry-pick onto a fresh branch off `main`: the opencode install step, the
credential-isolation assertions, and the injection sweep. Apply the two fixes
its own last commit introduced:

- add `echo "$HOME/.opencode/bin" >> "$GITHUB_PATH"` — the hand-rolled tarball
  install dropped the PATH export the old `curl|bash` installer provided,
  giving `opencode: command not found` (exit 127);
- drop `administration: read` from `gate.yml`, which invalidates the workflow.

Note that `best-practices-loop.yml` invokes `opencode` in the **same step** as
the install, where `$GITHUB_PATH` cannot help — that call needs the absolute
path `"$HOME/.opencode/bin/opencode"`.

Abandon the 44-commit history. `best-practices-loop.yml` (146 → 406 lines) and
the `gate.yml` branch-protection prober leave #421's scope and become their own
PRs; the prober in particular needs an admin-scoped credential to work at all,
and its jq filter currently carries shell-literal `\` line continuations inside
a single-quoted program, which is a syntax error on first real execution.

Close #420 (dependabot bump of the PR-Agent action being removed) and delete
`.pr_agent.toml` once phase 1 lands.

## 7. Out of scope

- Extraction to a reusable workflow for other wcatz repos. Prove it on ghost
  first; the prompt, severity policy, and config stay in clearly separated
  blocks so extraction is mechanical later.
- Ghost as the reviewer's cross-PR memory. `best_practices.md` from the base
  ref is v1. Dogfooding Ghost is the natural phase 2 of the learning loop and
  gets its own spec.
- `request_changes` auto-approve-on-resolution. `GITHUB_TOKEN` cannot approve,
  and approvals are deliberately outside branch protection.
