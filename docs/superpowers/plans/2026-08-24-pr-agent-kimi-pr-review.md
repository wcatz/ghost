# PR-Agent + Kimi K2.7 Code Review Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace rate-limited CodeRabbit as ghost's PR reviewer with PR-Agent running in GitHub Actions, using Kimi K2.7 Code through Wayne's existing opencode Go subscription ($10/mo, already paid).

**Architecture:** One new GitHub Actions workflow (`.github/workflows/pr-agent.yml`) triggers `the-pr-agent/pr-agent` on `pull_request` events (opened / reopened / ready_for_review) and `issue_comment` (manual `/review`, `/improve`). The action calls Kimi K2.7 Code via PR-Agent's documented OpenAI-like API path (`OPENAI__API_BASE=https://opencode.ai/zen/go/v1`, `OPENAI__KEY=<Go API key>`). No third-party GitHub App; everything runs in GitHub-hosted runners (free — repo is public).

**Tech Stack:** GitHub Actions, `the-pr-agent/pr-agent@v0.43.0` (pinned release), opencode Go endpoint (`https://opencode.ai/zen/go/v1`), model `kimi-k2.7-code`.

---

## Background decisions (already made)

| Decision | Choice | Why |
|---|---|---|
| Tool | PR-Agent v0.43.0 | Apache-2.0, runs in our runner, no vendor repo access |
| Model | Kimi K2.7 Code via Go sub | $0 marginal cost; ~6,750 req/mo quota vs K3's ~490 (30x burn for marginal gain) |
| Trigger event | `pull_request`, NOT `pull_request_target` | All current authors (wayne, waltskinner) push same-repo branches, so secrets are available under `pull_request`. Fork PRs get no auto-review — acceptable for a solo public repo; revisit if an external contributor appears |
| Auto tools | `auto_review` + `auto_improve` on, `auto_describe` off | Descriptions are already good per repo convention; less comment noise |
| Re-review policy | NOT on synchronize; manual `/review` comment instead | Avoids burning Go quota every push (same rationale as opencode-review action) |
| CodeRabbit | Leave installed during trial | Parallel run lets us compare quality; remove after evaluation |

**Known risks:**
- CI reviews share the Go quota caps ($12/5h, $30/wk, $60/mo) with interactive opencode use. A review is cents of usage; monitor https://opencode.ai/auth console the first week.
- Prompt injection via PR content is possible for any LLM reviewer — bot findings stay advisory; human approval still gates merge (existing rule, unchanged).
- Fork PRs will not be auto-reviewed (secrets unavailable under `pull_request`). Their `/review` comments will fail visibly in Actions logs.

**Repo facts this plan relies on (verified 2026-08-24):**
- Repo `wcatz/ghost` is **public** → hosted-runner minutes are free.
- Existing workflows live flat in `.github/workflows/` (`ci.yml`, `codeql.yml`, `longmemeval.yml`, `release.yml`) — new file follows that pattern.
- Open PRs #352/#354/#355 are authored by waltskinner from same-repo branches (`isCrossRepository: false`).
- Multiple git remotes exist locally → all `gh` calls in this plan must pass `-R wcatz/ghost`.
- PR-Agent config env vars: settings sections map to env vars with dots in Actions (`config.model`) and double underscores inside values that reach litellm (`OPENAI__KEY`, `OPENAI__API_BASE`). Custom model needs the `openai/` prefix plus `custom_model_max_tokens` (per PR-Agent "OpenAI like API" and Neon gateway docs — identical pattern to ours).

---

### Task 1: Add the Go API key secret

**Files:** none (repo secret, external to git)

- [ ] **Step 1: Confirm whether the secret already exists**

Run:
```bash
gh secret list -R wcatz/ghost
```
Expected: list of existing secret names. If `OPENCODE_GO_API_KEY` appears, skip Step 2.

- [ ] **Step 2: Create the secret**

The key comes from Wayne's opencode account (https://opencode.ai/auth → API key). This step requires Wayne to paste the key interactively — do NOT invent or echo it:

```bash
gh secret set OPENCODE_GO_API_KEY -R wcatz/ghost
```
(paste key at prompt, Enter)

- [ ] **Step 3: Verify**

```bash
gh secret list -R wcatz/ghost | grep OPENCODE_GO_API_KEY
```
Expected: `OPENCODE_GO_API_KEY` listed. Updated: `<date>`.

---

### Task 2: Create the workflow file

**Files:**
- Create: `.github/workflows/pr-agent.yml`

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/pr-agent.yml` with exactly:

```yaml
name: PR Agent

on:
  pull_request:
    types: [opened, reopened, ready_for_review]
  issue_comment:
    types: [created]

permissions:
  issues: write
  pull-requests: write

jobs:
  pr_agent_job:
    # Same-repo PRs only: fork PRs get no secrets under pull_request events,
    # so fail fast at the gate instead of erroring inside the action.
    if: >-
      github.event.sender.type != 'Bot' && (
        github.event_name == 'issue_comment' ||
        github.event.pull_request.head.repo.full_name == github.repository
      )
    runs-on: ubuntu-latest
    steps:
      - name: PR Agent action step
        id: pragent
        # Pinned release — matches repo convention of pinning tool versions
        # for reproducibility (see ci.yml golangci-lint pin).
        uses: the-pr-agent/pr-agent@v0.43.0
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}

          # Kimi K2.7 Code via opencode Go subscription.
          # OpenAI-compatible endpoint, so litellm's openai provider +
          # api_base override routes there (PR-Agent "OpenAI like API" docs).
          OPENAI__KEY: ${{ secrets.OPENCODE_GO_API_KEY }}
          OPENAI__API_BASE: https://opencode.ai/zen/go/v1

          # openai/ prefix routes through litellm's OpenAI-compatible path;
          # kimi-k2.7-code is the Go endpoint's model id.
          config.model: "openai/kimi-k2.7-code"
          config.fallback_models: '["openai/kimi-k2.7-code"]'
          # Not in PR-Agent's MAX_TOKENS catalog → must set explicitly.
          # Conservative slice of K2.7's context window; plenty for any diff.
          config.custom_model_max_tokens: "65536"

          # Reviewer scope: bugs and real regressions, not style nits.
          pr_reviewer.extra_instructions: >-
            This is a Go CLI project (Go 1.26+, modernc.org/sqlite, no CGO).
            Focus on correctness bugs, race conditions, error handling gaps,
            and breaking changes to MCP tool contracts or the SQLite schema.
            Do not report style preferences handled by golangci-lint.

          github_action_config.auto_review: "true"
          github_action_config.auto_improve: "true"
          github_action_config.auto_describe: "false"
          github_action_config.pr_actions: '["opened", "reopened", "ready_for_review", "review_requested"]'
```

- [ ] **Step 2: Validate YAML syntax**

Run:
```bash
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/pr-agent.yml")); print("yaml ok")'
```
Expected output: `yaml ok`

If `actionlint` is installed (`command -v actionlint`), also run `actionlint .github/workflows/pr-agent.yml` and fix anything it reports.

- [ ] **Step 3: Commit on a feature branch**

Per repo rules: never commit to main directly.

```bash
git checkout -b chore/pr-agent-review-pipeline
git add .github/workflows/pr-agent.yml
git commit -m "chore(ci): add PR-Agent review workflow (Kimi K2.7 via opencode Go)"
git push -u origin chore/pr-agent-review-pipeline
```

---

### Task 3: Pilot test on a throwaway PR

**Files:**
- Modify: `README.md` (one line, reverted/squashed away after trial)

- [ ] **Step 1: Open the pilot PR**

Note: the workflow only exists on its own branch until merged, and `pull_request` workflows run from the base branch. So first push the branch and open a PR for it — the workflow file itself becomes the pilot diff:

```bash
gh pr create -R wcatz/ghost \
  --base main --head chore/pr-agent-review-pipeline \
  --title "chore(ci): add PR-Agent review workflow (Kimi K2.7 via opencode Go)" \
  --body "Pilot for replacing CodeRabbit. Testing: workflow syntax, Go endpoint auth, model routing, review quality. See docs/superpowers/plans/2026-08-24-pr-agent-kimi-pr-review.md"
```

Wait — since the workflow runs from `main` and doesn't exist there yet, this first PR will NOT get reviewed. That is expected. Merge this PR first (after `ci.yml` checks pass), then Task 4 tests on a second PR.

- [ ] **Step 2: Watch CI before merging**

```bash
gh pr checks --watch -R wcatz/ghost
```
Expected: build-and-test, analyze (CodeQL), fts all green. Merge squash + delete branch once green (standing directive applies).

---

### Task 4: Verify the reviewer end-to-end

**Files:**
- Modify: `README.md` (one trivial line — the pilot diff)

- [ ] **Step 1: Open a second PR carrying a trivial diff**

```bash
git checkout main && git pull origin main
git checkout -b chore/pr-agent-pilot-diff
printf '\nReview pipeline pilot: this line exists so PR-Agent has a diff to analyze.\n' >> README.md
git add README.md
git commit -m "docs: pilot line for PR-Agent verification"
git push -u origin chore/pr-agent-pilot-diff
gh pr create -R wcatz/ghost \
  --base main --head chore/pr-agent-pilot-diff \
  --title "docs: pilot line for PR-Agent verification" \
  --body "Second pilot PR: verifies the merged pr-agent.yml fires, authenticates against the opencode Go endpoint, and posts a review."
```

- [ ] **Step 2: Watch the pr_agent_job run**

```bash
sleep 15 && gh run list -R wcatz/ghost --workflow=pr-agent.yml --limit 1
gh run watch -R wcatz/ghost $(gh run list -R wcatz/ghost --workflow=pr-agent.yml --limit 1 --json databaseId --jq '.[0].databaseId')
```
Expected: job succeeds within ~2-5 min; a `github-actions` bot comment appears on the PR with "PR Analysis" / review content.

- [ ] **Step 3: Triage failure modes if the job fails**

Read the log before changing anything (`gh run view --log-fail`):

| Symptom | Cause | Fix |
|---|---|---|
| 401 / invalid api key | Secret missing/misnamed, or key lacks Go sub | Re-run Task 1; confirm sub active at https://opencode.ai/auth |
| model not found / 404 | Wrong model id or wrong api_base path | Model id must be exactly `kimi-k2.7-code`; base must end at `/v1` (litellm appends `/chat/completions`) |
| litellm "UnsupportedParamsError" etc. | Provider misrouted | Ensure `config.model` keeps the `openai/` prefix |
| context length exceeded | `custom_model_max_tokens` too high for endpoint | Lower to `32768` |
| Empty/no review comment | `pr_actions` filtered the event | Check `github_action_config.*` env vars reached the action (they appear in run log header) |

Re-run manual trigger without pushing:
```bash
gh pr comment -R wcatz/ghost <pr-number> --body "/review"
```

- [ ] **Step 4: Test the manual re-review command**

```bash
gh pr comment -R wcatz/ghost <pilot-pr-number> --body "/improve"
```
Expected: second bot response (inline suggestions). Confirms issue_comment path works.

- [ ] **Step 5: Close out the pilot PR**

Squash-merge if green (or close without merging if the README line shouldn't land — either way delete the branch):

```bash
git branch -D chore/pr-agent-pilot-diff
git remote prune origin
```

---

### Task 5: Post-pilot bookkeeping

**Files:** none in git (Ghost memory updates)

- [ ] **Step 1: Record the decision**

Use `ghost_decision_record`: title "Replace CodeRabbit with PR-Agent (Kimi K2.7 via opencode Go) for wcatz repos"; alternatives rejected: Copilot code review (rejected by user), Greptile/Qodo/Ellipsis SaaS (third-party repo access + paid), Robin (smaller feature set than PR-Agent).

- [ ] **Step 2: Update the standing directive memory**

Update Ghost memory `478E90E02902CC36B7FCC94240E6258F` (and twin `1A6A19F30243ACDE4850929DBE973687`): swap "CodeRabbit review comments" for "PR-Agent review comments (bot login `github-actions`)" for wcatz-owned repos, keeping the watch→fix→re-verify→merge loop otherwise unchanged. Keep CodeRabbit references for Blink org repos (unchanged there).

- [ ] **Step 3: Schedule CodeRabbit removal decision**

Leave CodeRabbit installed through ~5 real PRs. If PR-Agent coverage is sufficient, remove the CodeRabbit GitHub App from wcatz/ghost (Settings → GitHub Apps) — ask Wayne before removing since billing/admin is his.

---

## Self-review notes

- Spec coverage: free/existing-subscription cost (Task 1-2 use paid-already Go quota), opencode-based model routing (Task 2 env block), PR review automation (Task 2 triggers), verification (Task 4), workflow handoff update (Task 5).
- No placeholders: every step has exact commands/files; the only runtime-derived value (pilot PR number) uses `<pilot-pr-number>` captured from `gh pr create` output in Task 4 Step 1.
- Type/config consistency: model id `kimi-k2.7-code` and base URL `https://opencode.ai/zen/go/v1` used identically in Task 2 and the Task 4 triage table; secret name `OPENCODE_GO_API_KEY` consistent across Tasks 1, 2.
