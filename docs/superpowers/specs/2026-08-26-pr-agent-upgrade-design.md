# PR-Agent Upgrade: Named Bot Identity, Always-On Suggestions, Scannable Output — Design

Date: 2026-08-26. Status: approved in brainstorm; implementation follows.

## Goal

Three user-approved upgrades to the PR-Agent CI review loop:

1. **Identity** — reviews currently post as `github-actions`; Wayne wants a
   named bot identity.
2. **Suggestions always-on** — `/improve`-style inline committable suggestions
   exist today but only on demand (`auto_improve: false` since always-on blew
   the 10-minute runner cap three runs straight). He wants them on every PR.
3. **Scannable output** — the Reviewer Guide is a wall of HTML tables. Target:
   a verdict-first card (delegated design decision) so a human decides
   merge/no-merge in one glance.

## Non-goals

- Forking PR-Agent templates or self-hosting the Python service — upstream
  owns output structure; we shape content via `extra_instructions`, toggles,
  and token plumbing only.
- Making any bot job a required merge gate — conversation resolution on
  inline threads remains the sole gate (pr-loop.yml header).
- Creating a new GitHub App — the Review Loop's existing App
  (`GH_APP_ID`/`GH_APP_PRIVATE_KEY`) is reused.

## Design

### 1. Named identity via installation token

An `actions/create-github-app-token` step mints an installation token from
the existing sweeper App secrets; that token is fed to the PR-Agent step as
its `GITHUB_TOKEN`. Every comment/review then posts under the App's name and
avatar. Chosen over PR-Agent's native App-auth env vars because the
token-swap approach is universal, version-pinned-action-independent, and
verifiable by inspection.

- Prerequisite: `GH_APP_ID` + `GH_APP_PRIVATE_KEY` secrets must exist on
  wcatz/ghost (validated on ci-mech-spike 2026-08-25; presence here checked
  at implementation).
- Degradation: if either secret is absent, skip minting and fall back to
  `${{ github.token }}` with a job-warning annotation. Functionality intact;
  identity stays `github-actions`.
- Loop safety: pr-agent.yml's `config` job already ignores
  `sender.type == 'Bot'` authors, so App-posted comments cannot re-trigger.

Side benefit: the same token pattern can later replace the sweeper's
report-only degradation path.

### 2. Suggestions always-on within a realistic budget

- `github_action_config.auto_improve: "true"`.
- Runner `timeout-minutes`: 10 → 25 (per-finding suggestion generation is
  sequential LLM calls; three historical cap-blows were at exactly ~8.5m).
- `pr_code_suggestions.num_code_suggestions`: trimmed to 4 highest-value
  findings per run.
- Existing knobs unchanged: `commitable_code_suggestions = true`,
  dual-publish threshold ≥7, score floor 3.
- Accepted cost: full review+improve runs take ~5–15 min. Not a merge gate,
  not required context — latency is cosmetic. `concurrency`
  cancel-in-progress already collapses redundant runs on rapid pushes.

### 3. Output contract (verdict card)

Enforced through `[pr_reviewer] extra_instructions` formatting demands —
the model fills our shape inside upstream templates:

```markdown
## 🟢 Verdict: <approve | approve with nits | comment | request changes> — <one line why>
**Findings:** N 🔴 · N 🟡 · N 🟢 | Effort: 🔵🔵⚪⚪⚪

| Lens                  | Result       |
|-----------------------|--------------|
| Tests                 | ✅ / ⚠️       |
| Security              | ✅ / ⚠️       |
| Error handling        | ✅ / ⚠️ N     |
| Invariant parity      | ✅ / ⚠️ N     |
| Protected resources   | ✅ / ⚠️ N     |
| Behavior preservation | ✅ / ⚠️ N     |

### Findings
<severity> **file:line** — what's wrong → why it matters → concrete fix

<details><summary>Ticket compliance · full analysis</summary>
…everything else…
</details>
```

Severity taxonomy: 🔴 blocker (correctness/race/data-loss), 🟡 should-fix
(gap likely to bite), 🟢 nit. Lens rows are the three lenses from PR #395
(invariant parity, protected resources, behavior preservation) plus tests,
security, and error handling — six fixed rows so every review self-reports
which lenses it checked and what it found. The compliance table and focus
areas move inside the collapsed section.

## Error handling

- Missing App secrets → fallback token + warning annotation (above).
- Hung LLM calls: unchanged defenses (`config.ai_timeout: 90`,
  `propagate_tool_errors: true`, runner kill at the new 25-min cap).
- Suggestion generation failure must not lose the review: auto_review and
  auto_improve are separate tool invocations in one step; a failed improve
  still leaves the review posted (propagate_tool_errors fails the JOB after
  both tools ran — verified behavior per the 2026-08-25 incident note).

## Validation

On a scratch PR: comment author is the App identity (`user.type == "Bot"`,
login ≠ github-actions); suggestions appear as one-click commits;
collapsed sections render; no infinite trigger loop; wall time ≤ 25 min.

Rollback: revert the single workflow/config commit.
