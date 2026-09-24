# Release catalog pin as a pull request — design

**Status:** Approved for planning (2026-09-24)
**Scope:** The release workflow's catalog-pin step only. No other release stage, no other workflow.

## Problem

The `plugin` job's final stage, "Commit the pinned catalog to main", writes
`.claude-plugin/marketplace.json` straight to `main` with `GITHUB_TOKEN`. It
fails on every release.

Measured at v0.30.18 (run 35928382388): the contents-API PUT returned HTTP 409
GH013 — *"Repository rule violations found for refs/heads/main … 3 of 3 required
status checks are expected. Changes must be made through a pull request."* A
second probe (run 35929704681) drove the `git push` route under the identical
workflow context — `github-actions[bot]` identity, DCO trailer, persisted
checkout token — and was rejected with the same violations. Both write routes
are blocked, because the effective principal of a workflow's `GITHUB_TOKEN` is
the GitHub Actions app integration, which cannot be a bypass actor on a personal
repository (API 422 at ruleset setup). `main` is confirmed protected; admin
pushes bypass the rule, which is why the manual runbook works.

The step body is retained and correct. Only its delivery mechanism is wrong.

## Goals

- Land the pinned catalog on `main` through the review path the branch rule
  already mandates, so the release stops failing at its last step.
- Add no credential, secret, app, or unprotected standing branch.
- Preserve the two guards the step already implements, and start running them
  automatically rather than leaving them as manual steps.
- Keep the manual admin runbook documented as a fallback.

## Non-goals

- Automatically approving or merging the pin PR. A human merges it.
- Automatically merging releases, or changing any other release stage.
- Changing the branch protection rule, adding a bypass actor, or relaxing the
  required checks.
- Changing the idempotency or downgrade-guard semantics.

## Approach

`GITHUB_TOKEN` creates a branch and opens a pull request instead of writing to
`main`.

GitHub's trigger documentation makes this workable: when a workflow using
`GITHUB_TOKEN` creates or updates a pull request with `opened` activity, the
resulting `pull_request` event **does** create workflow runs, in an
approval-required state. A user with write access releases them from the merge
box with **Approve workflows to run**. The approval gate exists to prevent
recursive workflow runs while still letting CI run on automation-created PRs, so
this needs no elevated credential.

The documented alternative — a GitHub App installation token or PAT — exists to
skip the approval click. That is not worth a long-lived credential here, because
a human was always going to merge the PR.

GitHub's secure-use reference frames the same trade-off: *"Allowing workflows,
or any other automation, to create or approve pull requests could be a security
risk if the pull request is merged without proper oversight."* Automation
proposes; a human disposes. That is the property the branch rule exists to keep.

## Design

### Permissions

The workflow currently declares `contents: write` and `packages: write` at the
top level, so both jobs receive both. This change splits them per job, which is
the least privilege the two jobs actually need:

- `release` — `contents: write`, `packages: write` (unchanged set).
- `plugin` — `contents: write`, `pull-requests: write`. It does not publish
  packages, and it gains only what opening a PR requires.

`contents: write` on the `plugin` job is what creates and updates the pin
branch; the ruleset restricts `main`, not other refs. Nothing is written to
`main`.

### Branch and commit

- Branch: `automation/marketplace-pin-v<version>`, a unique branch per release,
  created with the contents API using `branch=` set to it. That is an ordinary
  commit on an unprotected ref, not a force-push, so a rejected or half-applied
  update cannot strand the branch.
- The branch's only difference from `main` is `.claude-plugin/marketplace.json`.
- The branch is deleted when its PR merges, by the repository setting
  **Settings → General → Pull Requests → Automatically delete head branches**
  rather than a workflow step. That setting applies to every merged PR including
  automation-created ones, so the workflow carries no delete logic and has no
  cleanup path that can fail. This matches the per-change-branch convention used
  by release-please, Renovate, and Dependabot.
- Commit subject: `chore(release): pin plugin archives for <tag>`, with the same
  `Signed-off-by: github-actions[bot]` trailer the current step writes, so
  history and DCO are unchanged in shape.

### Pull request

- Title: `chore(release): pin plugin archives for <tag>` — matches the squash
  subject and keeps the PR number in the final commit subject, per the merge
  guard.
- Body: the tag, the three archive URLs, and their sha256 digests as pinned.
- Opened when absent, updated when present, so a re-run converges instead of
  failing on "a pull request already exists".

### Guards, unchanged in semantics

Both guards run before anything is pushed, exactly as today:

1. **Idempotency** — byte-compare `main`'s catalog against the freshly pinned
   local file; skip when they match.
2. **Cross-release downgrade guard** — extract the version `main` pins via `jq`
   scoped to `plugins[].source.url`; skip when `main` pins something strictly
   newer than this tag (`sort -V`). Same version with different bytes still
   proceeds, because a legitimate re-pin after `--clobber` must be followed.

Skipping either guard leaves the pin branch untouched, so a skipped release does
not open an empty PR.

### The one manual step

The checks on the new PR are created pending. The release job writes to the
summary, in bold:

```
Approve workflows to run on the catalog PR, then merge it:
  <pr-url>
```

Without that reminder the release looks stalled: the PR is open, the checks are
listed but queued, and nothing explains why. This is the single non-obvious
action in the whole procedure.

### Verification

The existing post-commit "Verify the catalog on main matches the pinned copy"
stage cannot verify anything at that point in the job, because the pin has not
been merged yet. It is replaced by a stage that verifies the **pin branch**
carries the pinned bytes, reading it back through the contents API on that
branch. The contents API is used rather than `raw.githubusercontent.com` for the
reason the current step already documents: the raw branch CDN caches for
minutes and can serve pre-write bytes immediately after a successful write.

This stage is conditional on the pin having been written. It is skipped when
either guard caused an early exit, because in those cases no branch update
happened and there is nothing new to verify — the same condition that suppresses
the PR. A step that ran unconditionally would compare a stale branch against the
local copy and fail a correctly-skipped release.

The `main` catalog is verified by the PR's own required checks, which is the
stronger property: the three required status checks must pass before the branch
rule permits the merge at all. The downgrade-guard acceptance branch in the old
verify stage becomes dead once the comparison target is the branch the job
itself just wrote, and is removed.

## Failure modes

| Condition | Behavior |
|---|---|
| Idempotency skip | No branch write, no PR, job succeeds |
| Downgrade-guard skip | No branch write, no PR, job succeeds |
| Branch write fails | Step fails; release assets are already published and valid |
| PR already open for this tag | Branch updated, PR body updated, job succeeds |
| Owner never approves the runs | PR sits with queued checks; assets unaffected |
| Owner abandons the pin | Manual admin runbook in `release.yml` still applies |

A failed pin leaves a stale-but-valid catalog, exactly as today: every pinned
`releases/download/vX.Y.Z` URL still resolves.

Re-running a tag whose pin already merged does not resurrect a branch. The
idempotency byte-compare sees `main` matching the freshly pinned copy and exits
before any write, so convergence comes from that guard rather than from branch
persistence. An abandoned PR leaves its branch behind, which is ordinary PR
hygiene and the same for any unmerged change.

## Security properties

- **No new credential.** `GITHUB_TOKEN` is already scoped to this repository and
  is short-lived per job.
- **No bypass actor.** The ruleset is unchanged; `main` still requires a PR and
  the three required checks.
- **Automation cannot merge.** The workflow proposes only. A human with write
  access approves the runs and merges, so the pin receives the same review as
  any other change.
- **Reviewer App untouched.** The `GH_APP_ID` / `GH_APP_PRIVATE_KEY` app mints
  its token in `reviewer.yml`, which runs on `pull_request` — the untrusted-input
  path. It is not granted `contents: write`, and this design does not require it
  to be.

## Validation

- `actionlint` and `shellcheck` clean on the modified workflow.
- `grep` assertions in the plan: exactly one catalog-pin step, no `contents` PUT
  against `main`, and the `pull-requests: write` permission present.
- The idempotency and downgrade-guard truth table driven locally against a `gh`
  shim, as the existing release docs do for these guards: `main == local` →
  no-op; `main` pins newer → no-op; unpinned/older `main` → PR opened; equal
  version with different bytes → PR opened.
- Re-run the release workflow for a superseded tag and confirm it takes the
  downgrade-guard skip rather than opening a PR.

## Accepted trade-offs

- One extra click per release (Approve workflows to run) on top of the merge.
  This is the documented default GitHub offers in exchange for not issuing a
  credential, and it is the reason this design needs no secret.
- An unprotected branch exists for the duration of each release, then is
  deleted by the repository's auto-delete setting. The window in which a ref
  outside branch protection exists is bounded by one release rather than
  permanent, and that window is unavoidable: the commit has to exist somewhere
  reviewable before a human merges it.
- The release is published before the catalog lands, so there is still a window
  where the catalog points at the previous release. That window is unchanged
  from today, and it is safe: the previous release's URLs remain valid.

## Manual setup

One repository setting, outside the workflow:

- **Settings → General → Pull Requests → Automatically delete head branches.**

Without it the per-release pin branches accumulate, which the squash-merge
history makes hard to read: each is one commit ahead of `main` whose change is
already present in `main` under a different SHA. The setting is also what keeps
the workflow free of delete logic.
