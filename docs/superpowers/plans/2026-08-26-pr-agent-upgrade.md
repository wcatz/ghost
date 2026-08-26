# PR-Agent Upgrade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** PR-Agent posts under the sweeper GitHub App's identity, generates committable suggestions on every PR within a 25-minute budget, and formats output as a verdict-first card with a six-lens check matrix.

**Architecture:** Three coordinated config changes, no Go code: (1) an `actions/create-github-app-token@v3` step mints an installation token that replaces `secrets.GITHUB_TOKEN` in the PR-Agent step; (2) workflow env flips `auto_improve` on and raises the runner cap; (3) `.pr_agent.toml` `[pr_reviewer] extra_instructions` gains the verdict-card format contract. Spec: `docs/superpowers/specs/2026-08-26-pr-agent-upgrade-design.md`.

**Tech Stack:** GitHub Actions YAML, PR-Agent v0.43.0 action (pinned SHA `4ebd5c5333c6ef21509e7304d27969eb825e6f22`), `actions/create-github-app-token@v3` (inputs `app-id`, `private-key`; output `token`).

**Branch:** `feat/pr-agent-upgrade` (spec already committed at 6b8ccdc). All commits DCO-signed `-s`, no AI attribution.

---

### Task 1: Verify App secrets exist on wcatz/ghost

**Files:** none (read-only verification)

- [ ] **Step 1: List repo secrets**

```bash
gh secret list
```

Expected: `GH_APP_ID` and `GH_APP_PRIVATE_KEY` appear among the names. If either is missing, STOP and ask Wayne to add them (values from the existing Review Loop App) before proceeding — Task 2's fallback exists but the feature is pointless without the identity change.

- [ ] **Step 2: Record the App's login name for validation**

```bash
gh api repos/wcatz/ghost/installation --jq '.app.slug' 2>/dev/null || echo "needs app token"
```

If the API call fails without a token, note it and rely on Task 4's end-to-end author check instead.

### Task 2: Wire the App token into pr-agent.yml

**Files:**
- Modify: `.github/workflows/pr-agent.yml` (pr_agent_job job: timeout, steps)

- [ ] **Step 1: Raise the runner cap**

Change line 78:

```yaml
    # Hard cap: litellm's ai_timeout does not bound a hung streamed call —
    # observed 651s before the upstream 500 came back. Kill at the runner.
    # auto_improve adds sequential per-finding LLM calls after review; three
    # historical cap-blows landed at ~8.5m under the old 10m limit, so 25m
    # covers review + top-4 suggestions with headroom while still bounding
    # a pathological hang.
    timeout-minutes: 25
```

(replaces `timeout-minutes: 10` and its preceding comment block)

- [ ] **Step 2: Mint the installation token before the agent step**

Insert after the `pr_agent_job` job's `steps:` line, before the existing `PR Agent action step`:

```yaml
      - name: Mint bot identity token
        id: apptoken
        # Reviews post as the Review Loop GitHub App instead of
        # github-actions. Falls back to the runner token when the App
        # secrets are absent — functionality intact, identity degrades.
        if: ${{ vars.USE_BOT_IDENTITY != 'false' }}
        uses: actions/create-github-app-token@v3
        with:
          app-id: ${{ secrets.GH_APP_ID }}
          private-key: ${{ secrets.GH_APP_PRIVATE_KEY }}
```

Note: `actions/create-github-app-token` fails the step when the secret inputs are empty rather than producing empty output, so absence of secrets must not fail the whole run — wrap via `continue-on-error: true` on this step and let Step 3 pick whichever token exists:

Add `continue-on-error: true` to the step above (directly under `if:`).

- [ ] **Step 3: Feed the minted token (or fallback) into the agent step**

In the `PR Agent action step`, replace:

```yaml
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

with:

```yaml
        env:
          GITHUB_TOKEN: ${{ steps.apptoken.outputs.token || secrets.GITHUB_TOKEN }}
```

- [ ] **Step 4: Validate workflow syntax locally**

Run: `go run ./cmd/ghost version >/dev/null && python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/pr-agent.yml')); print('yaml ok')"`

Expected: `yaml ok`. (The repo has python3 available; alternatively push and let the `config` gate job validate.)

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/pr-agent.yml
git commit -s -m "feat(ci): post PR-Agent reviews as the Review Loop App"
```

### Task 3: Always-on suggestions + verdict-card contract

**Files:**
- Modify: `.github/workflows/pr-agent.yml` (auto_improve line)
- Modify: `.pr_agent.toml` ([pr_code_suggestions], [pr_reviewer] extra_instructions)

- [ ] **Step 1: Flip auto_improve on**

In `pr-agent.yml`, replace the `auto_improve` block (lines ~131-136):

```yaml
          # Always-on: every PR gets inline committable suggestions.
          # Sequential per-finding LLM calls are why this was on-demand
          # (three 10-min cap-blows); the 25m runner cap plus num_code_
          # suggestions=4 keeps worst-case wall time bounded. Not a merge
          # gate — latency is cosmetic.
          github_action_config.auto_improve: "true"
```

- [ ] **Step 2: Cap suggestion count in .pr_agent.toml**

In `[pr_code_suggestions]` section append:

```toml
# Budget: top-N suggestions per run keeps auto-improve inside the 25m
# runner cap (each suggestion is a sequential LLM call).
num_code_suggestions = 4
```

- [ ] **Step 3: Prepend the verdict-card contract to [pr_reviewer] extra_instructions**

In `.pr_agent.toml`, inside the existing `extra_instructions = """..."""` (added by PR #395), insert BEFORE the three lenses so the format demand comes first:

```toml
extra_instructions = """
Output format (mandatory): open with a verdict card exactly in this shape:
'## <emoji> Verdict: <approve|approve with nits|comment|request changes> — <one-line why>',
then '**Findings:** N 🔴 · N 🟡 · N 🟢 | Effort: 🔵🔵⚪⚪⚪',
then a six-row markdown lens table (Tests, Security, Error handling,
Invariant parity, Protected resources, Behavior preservation) whose Result
column is '✅' or '⚠️ N', then a '### Findings' list ordered 🔴→🟡→🟢 where
each finding is '<severity> **file:line** — what is wrong → why it matters →
concrete fix'. Severity: 🔴 blocker (correctness/race/data-loss),
🟡 should-fix, 🟢 nit. Put ticket compliance analysis, effort breakdown,
and focus areas inside a '<details><summary>Ticket compliance · full
analysis</summary>' block after Findings. The card must be readable in
under 15 seconds.

Beyond diff-vs-issue compliance, apply these lenses and report findings per lens:
[...existing three lenses unchanged...]
"""
```

- [ ] **Step 4: Validate TOML parses**

Run: `python3 -c "import tomllib; tomllib.load(open('.pr_agent.toml','rb')); print('toml ok')"`

Expected: `toml ok`.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/pr-agent.yml .pr_agent.toml
git commit -s -m "feat(ci): always-on code suggestions and verdict-card review output"
```

### Task 4: End-to-end validation on a scratch PR

**Files:** none (validation only)

- [ ] **Step 1: Push branch and open the PR**

```bash
git push -u origin feat/pr-agent-upgrade
gh pr create --title "feat(ci): named bot identity, always-on suggestions, verdict card" --fill
```

(The spec doc from 6b8ccdc rides along; body links the spec path.)

- [ ] **Step 2: Watch checks**

```bash
sleep 40 && gh pr checks --watch --interval 20
```

Expected: all pass including `pr_agent_job`.

- [ ] **Step 3: Verify identity**

```bash
gh api repos/wcatz/ghost/issues/$(gh pr view --json number --jq .number)/comments --jq '.[-1] | .user.login + " type=" + .user.type'
```

Expected: the App's slug/login with `type=Bot` — NOT `github-actions`. If still `github-actions`, check the mint step's logs (`continue-on-error` may have swallowed a secret-name mismatch) before touching anything else.

- [ ] **Step 4: Verify suggestions posted**

Read the newest comments/reviews for inline suggestion blocks (```suggestion fences) or the improve table. Expected: ≥0 findings but the improve tool ran (job log shows two tool invocations); on this docs+config-only PR zero suggestions is a VALID outcome — confirm via job log lines `Number of PR chunk calls` appearing twice-ish region and no tool errors.

- [ ] **Step 5: Verify no trigger loop**

After both tools post, wait 60s then:

```bash
gh run list --workflow=pr-agent.yml --limit 3
```

Expected: no NEW runs beyond the initial one (Bot-authored comments are filtered by the config job's `sender.type != 'Bot'` guard).

- [ ] **Step 6: Report wall time**

From `gh pr checks`: `pr_agent_job` duration ≤ 25 min. If it exceeded 20 minutes consistently, halve `num_code_suggestions` to 3 in a follow-up commit before merge.

---

## Self-review notes

- Spec coverage: identity (Tasks 2), always-on budget (Task 2 Step 1 + Task 3 Steps 1-2), verdict card (Task 3 Step 3), degradation path (Task 2 Step 2 continue-on-error + fallback expression), loop safety (existing Bot filter — verified present at pr-agent.yml:45), validation (Task 4).
- No placeholders; every code step shows full content.
- Type consistency: token flows via step id `apptoken` output `token`; knob names match upstream (`pr_code_suggestions.num_code_suggestions`, `github_action_config.auto_improve`).
