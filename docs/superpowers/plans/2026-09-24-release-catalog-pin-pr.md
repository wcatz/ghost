# Release catalog pin as a pull request — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the pinned marketplace catalog on `main` through a pull request the owner merges, so the release workflow stops failing at its last stage, without adding any credential or bypass actor.

**Architecture:** The `plugin` job's final two stages are replaced. Instead of a contents-API PUT against `main`, the job writes the pinned catalog to a unique unprotected branch `automation/marketplace-pin-v<version>` and opens (or updates) a pull request from it. Both existing guards — the idempotency byte-compare against `main` and the cross-release downgrade guard — are carried over unchanged, so a skipped release writes nothing and opens nothing. The follow-up verify stage reads the pin branch back instead of `main`, and is conditional on the pin having been written.

**Tech Stack:** GitHub Actions YAML, `gh` CLI 2.100.0, `jq`, `bash`, `actionlint`, `shellcheck`, the already-pinned `actions/checkout`.

**Spec:** `docs/superpowers/specs/2026-09-24-release-catalog-pin-pr-design.md`

---

## File and surface map

### Modify

- `.github/workflows/release.yml:215-345` — replace the two catalog stages, and split the
  workflow-level `permissions:` block into per-job blocks.
- `docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md` — the release-pipeline
  paragraph states the step is blocked; update it to describe the PR mechanism.

### Preserve exactly

- The `release` job's permission set: `contents: write`, `packages: write`.
- The idempotency guard and the cross-release downgrade guard, byte for byte.
- The contents-API route (not `raw.githubusercontent.com`) and its documented reason.
- The `gh` CLI usage, the `-f`/`--jq` calling style, and the
  "separate commands, not pipes or `&&`" errexit discipline the current steps use.
- The manual admin runbook text, which stays the fallback.

### Manual setup (not code)

- Repository setting: **Settings → General → Pull Requests → Automatically delete head
  branches.** Without it the per-release pin branches accumulate. The workflow carries no
  delete logic.

---

### Task 1: Split workflow permissions per job

**Files:**
- Modify: `.github/workflows/release.yml:8-10`

The block is currently workflow-level, so both jobs receive `packages: write` even though
only `release` publishes images. Splitting it is what lets the `plugin` job hold exactly
what it needs.

- [ ] **Step 1: Record the current block so the change is provably scoped**

Run:

```bash
sed -n '8,11p' .github/workflows/release.yml
```

Expected, byte for byte:

```yaml
permissions:
  contents: write
  packages: write
```

- [ ] **Step 2: Give `release` its existing permissions as a job-level block**

Insert immediately after the `release` job's `runs-on: ubuntu-latest` line (currently line
15), so the block sits with the job that uses it:

```yaml
  release:
    runs-on: ubuntu-latest
    # Unchanged from the workflow-level block: this job builds and pushes the
    # OCI image, so it needs both. Split out per job so the plugin job does not
    # inherit packages: write, which it never uses.
    permissions:
      contents: write
      packages: write
```

- [ ] **Step 3: Give `plugin` the write it actually needs**

Insert immediately after the `plugin` job's `runs-on: ubuntu-latest` line:

```yaml
  plugin:
    runs-on: ubuntu-latest
    # contents: write creates and updates the pin branch. pull-requests: write
    # opens the catalog PR. No packages: write — this job attaches archives to
    # an existing release, it does not publish an image.
    permissions:
      contents: write
      pull-requests: write
```

- [ ] **Step 4: Delete the workflow-level block**

Remove exactly:

```yaml
permissions:
  contents: write
  packages: write

```

The `jobs:` key must now follow the `on:` block directly.

- [ ] **Step 5: Prove each job has the permissions it needs and no more**

Run:

```bash
python3 - <<'PY'
import re, sys, pathlib
text = pathlib.Path('.github/workflows/release.yml').read_text()
# A workflow-level permissions block would silently re-grant both jobs everything.
top = re.search(r'^permissions:\n(?:  .*\n)+', text, re.M)
if top:
    sys.exit(f'FAIL: workflow-level permissions block still present:\n{top.group(0)}')
for job, required in (('release', {'contents: write', 'packages: write'}),
                      ('plugin', {'contents: write', 'pull-requests: write'})):
    body = re.search(rf'^  {job}:\n(.*?)(?=^  [a-z-]+:\n|\Z)', text, re.M | re.S)
    if not body:
        sys.exit(f'FAIL: job {job} not found')
    block = re.search(r'^    permissions:\n((?:      .*\n)+)', body.group(1), re.M)
    if not block:
        sys.exit(f'FAIL: job {job} has no permissions block')
    got = {line.strip() for line in block.group(1).splitlines() if line.strip()}
    if got != required:
        sys.exit(f'FAIL: {job} permissions = {sorted(got)}, want {sorted(required)}')
    print(f'ok: {job} -> {sorted(got)}')
print('permissions split verified')
PY
```

Expected:

```text
ok: release -> ['contents: write', 'packages: write']
ok: plugin -> ['contents: write', 'pull-requests: write']
permissions split verified
```

- [ ] **Step 6: Lint the workflow**

Run:

```bash
/tmp/opencode/actionlint .github/workflows/release.yml
```

Expected: no output, exit 0. `actionlint` flags a job that references a permission its block
does not grant, so this is the check that catches an under-grant here.

- [ ] **Step 7: Commit**

```bash
git add .github/workflows/release.yml
git commit -s -m "ci(release): scope workflow permissions per job"
```

---

### Task 2: Replace the commit stage with a branch-and-PR stage

**Files:**
- Modify: `.github/workflows/release.yml:215-290` (the stage named
  `Commit the pinned catalog to main`)

The guards are copied over verbatim. Only the write target and the follow-up action change.

- [ ] **Step 1: Record the exact stage being replaced**

Run:

```bash
awk '/^      - name: Commit the pinned catalog to main$/,/^      - name: Verify the catalog on main/' .github/workflows/release.yml | head -5
```

Expected first line: `      - name: Commit the pinned catalog to main`. Record the line number of
the following `- name:` — the whole stage is replaced.

- [ ] **Step 2: Write the replacement stage**

Replace the entire `Commit the pinned catalog to main` stage with the following. The comment
block explains why the write moved off `main`; the two guards are unchanged from the current
implementation.

```yaml
      - name: Propose the pinned catalog as a pull request
        # The catalog is pinned to version-pinned release URLs plus each
        # archive's sha256. It must reach main, and main requires a pull
        # request plus three status checks (measured at v0.30.18, run
        # 35928382388: the contents-API PUT with GITHUB_TOKEN returned HTTP 409
        # GH013, and the git-push route under the same context, run
        # 35929704681, was rejected with the same violations).
        #
        # A workflow's GITHUB_TOKEN cannot be a bypass actor on a personal
        # repository, so this writes the catalog to a unique unprotected branch
        # and opens a PR instead. That is GitHub's documented behaviour for
        # automation-created PRs: the pull_request event DOES create workflow
        # runs, held in an approval-required state, released from the merge box
        # with "Approve workflows to run". A GitHub App or PAT token would skip
        # that click, but a human was always going to merge the PR, so it is not
        # worth a long-lived credential.
        #
        # The branch is unique per release and is deleted at merge by the
        # repository's "Automatically delete head branches" setting, so there
        # is no delete logic here to fail.
        #
        # MANUAL STEP after this job succeeds: open the PR below, click
        # "Approve workflows to run", then merge it. Until the pin is merged,
        # the catalog keeps pointing at the previous release — which is safe,
        # because those URLs remain valid.
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          set -euo pipefail
          PATH_IN_REPO=".claude-plugin/marketplace.json"
          VERSION="${GITHUB_REF_NAME#v}"
          BRANCH="automation/marketplace-pin-v${VERSION}"
          gh api "repos/${GITHUB_REPOSITORY}/contents/${PATH_IN_REPO}?ref=main" > /tmp/main-contents.json
          # Split into two simple commands, not a pipe (and not `&&`): under
          # Actions' default `bash -e {0}` (no pipefail) a jq failure piped
          # into base64 is masked by base64's success, and a failing left
          # side of `&&` does not trigger errexit either. As separate lines
          # each command is errexit-visible; -e makes jq fail on null/empty.
          jq -er '.content' /tmp/main-contents.json > /tmp/main-marketplace.b64
          base64 -d /tmp/main-marketplace.b64 > /tmp/main-marketplace.json
          if cmp -s /tmp/main-marketplace.json "$PATH_IN_REPO"; then
            echo "main already pins ${GITHUB_REF_NAME}; skipping catalog pin"
            echo "SKIPPED=1" >> "$GITHUB_OUTPUT"
            exit 0
          fi
          # Cross-release guard (carried over unchanged): a re-run of an OLD
          # release after a newer release already repinned main must never
          # downgrade the catalog. Extract the version main pins now via jq —
          # scoped to source.url so no description/homepage text can poison it
          # (empty while main still carries releases/latest URLs) — and skip
          # when it is newer than this tag. Same version with different bytes
          # falls through: that is a legitimate re-pin (e.g. --clobber
          # replaced the assets, so the catalog must follow the new digests).
          MAIN_VERSION="$(jq -r '[.plugins[]?.source.url? | strings | select(test("releases/download/v[0-9]+\\.[0-9]+\\.[0-9]+/")) | capture("releases/download/v(?<v>[0-9]+\\.[0-9]+\\.[0-9]+)/").v] | .[0] // empty' /tmp/main-marketplace.json || true)"
          NEWEST="$(printf '%s\n%s\n' "$MAIN_VERSION" "$VERSION" | sort -V | tail -n 1)"
          if [ -n "$MAIN_VERSION" ] && [ "$MAIN_VERSION" != "$VERSION" ] && [ "$NEWEST" = "$MAIN_VERSION" ]; then
            echo "main pins newer release v${MAIN_VERSION}; not downgrading to v${VERSION} — skipping catalog pin"
            echo "SKIPPED=1" >> "$GITHUB_OUTPUT"
            exit 0
          fi
          # Commit the pinned bytes to the pin branch. The contents API creates
          # the branch on first use and adds a commit on later releases; it is
          # an ordinary commit, not a force-push, so a half-applied write cannot
          # strand the branch.
          BLOB_SHA="$(jq -r '.sha' /tmp/main-contents.json)"
          CONTENT_B64="$(base64 <"$PATH_IN_REPO" | tr -d '\n')"
          gh api -X PUT "repos/${GITHUB_REPOSITORY}/contents/${PATH_IN_REPO}" \
            -f message="chore(release): pin plugin archives for ${GITHUB_REF_NAME}"$'\n\n'"Signed-off-by: github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>" \
            -f content="$CONTENT_B64" \
            -f sha="$BLOB_SHA" \
            -f branch="$BRANCH" >/dev/null
          echo "wrote pinned catalog to ${BRANCH}"
          echo "BRANCH=${BRANCH}" >> "$GITHUB_OUTPUT"
          echo "SKIPPED=0" >> "$GITHUB_OUTPUT"
```

- [ ] **Step 3: Declare the step outputs the verify stage depends on**

Replace the stage's name line and add an `id`, so the next task can condition on it:

```yaml
      - name: Propose the pinned catalog as a pull request
        id: pin
```

The full opening of the stage therefore reads:

```yaml
      - name: Propose the pinned catalog as a pull request
        id: pin
        # The catalog is pinned to version-pinned release URLs plus each
```

- [ ] **Step 4: Add the stage that opens or updates the PR**

Insert immediately after the `Propose the pinned catalog as a pull request` stage:

```yaml
      - name: Open or update the catalog pull request
        # GITHUB_TOKEN-created PRs do produce workflow runs, but held pending
        # approval (see the step above), so the merge box shows a banner. The
        # summary below says so explicitly: without it a successful release
        # looks stalled, because the PR is open and its checks are queued with
        # nothing explaining why.
        if: steps.pin.outputs.SKIPPED == '0'
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          set -euo pipefail
          VERSION="${GITHUB_REF_NAME#v}"
          BRANCH="${{ steps.pin.outputs.BRANCH }}"
          {
            echo "Pins \`.claude-plugin/marketplace.json\` to the"
            echo "\`releases/download/${GITHUB_REF_NAME}\` archives attached by this release,"
            echo "with each archive's sha256."
            echo
            jq -r '.plugins[] | "- `\(.name)`: \(.source.url) (`\(.source.sha256)`)"' \
              .claude-plugin/marketplace.json
            echo
            echo "Written by the release workflow for ${GITHUB_REF_NAME}."
          } > /tmp/pr-body.md
          EXISTING="$(gh pr list --head "$BRANCH" --state open --json number --jq '.[0].number // empty')"
          if [ -n "$EXISTING" ]; then
            gh pr edit "$EXISTING" --body-file /tmp/pr-body.md
            echo "updated existing catalog PR #${EXISTING}"
            echo "PR_URL=$(gh pr view "$EXISTING" --json url --jq .url)" >> "$GITHUB_OUTPUT"
          else
            gh pr create --base main --head "$BRANCH" \
              --title "chore(release): pin plugin archives for ${GITHUB_REF_NAME}" \
              --body-file /tmp/pr-body.md
            echo "PR_URL=$(gh pr view "$BRANCH" --json url --jq .url)" >> "$GITHUB_OUTPUT"
          fi
        id: open-pr
```

- [ ] **Step 5: Add the summary that names the manual step**

Insert immediately after the `Open or update the catalog pull request` stage:

```yaml
      - name: Report the manual merge step
        if: steps.pin.outputs.SKIPPED == '0'
        run: |
          {
            echo "### Catalog pin needs a manual merge"
            echo
            echo "1. Open ${{ steps.open-pr.outputs.PR_URL }}"
            echo "2. Click **Approve workflows to run** in the merge box — the checks"
            echo "   are held pending because the PR was created by the workflow"
            echo "3. Merge once the three required checks pass"
            echo
            echo "Until then the catalog keeps pointing at the previous release."
            echo "Those URLs stay valid, so installs are not broken."
          } >> "$GITHUB_STEP_SUMMARY"
```

- [ ] **Step 6: Prove no write still targets `main`**

Run:

```bash
grep -n 'branch="main"' .github/workflows/release.yml && { echo 'FAIL: a contents write still targets main'; exit 1; } || echo 'ok: no contents write targets main'
grep -c 'Propose the pinned catalog as a pull request' .github/workflows/release.yml
```

Expected:

```text
ok: no contents write targets main
1
```

- [ ] **Step 7: Prove the guards survived verbatim**

Run:

```bash
for needle in \
  'if cmp -s /tmp/main-marketplace.json "$PATH_IN_REPO"' \
  'MAIN_VERSION="$(jq -r' \
  'NEWEST="$(printf' \
  'sort -V | tail -n 1' \
  'not downgrading to v${VERSION}' \
  'base64 -d /tmp/main-marketplace.b64' ; do
  if grep -qF "$needle" .github/workflows/release.yml; then
    echo "ok: guard fragment present: $needle"
  else
    echo "FAIL: guard fragment missing: $needle"; exit 1
  fi
done
```

Expected: five `ok:` lines. Any `FAIL:` means a guard was dropped or reworded and must be
restored from the pre-change stage.

- [ ] **Step 8: Lint and commit**

Run:

```bash
/tmp/opencode/actionlint .github/workflows/release.yml
```

Expected: no output, exit 0.

```bash
git add .github/workflows/release.yml
git commit -s -m "ci(release): propose the catalog pin as a pull request"
```

---

### Task 3: Retarget the verify stage at the pin branch

**Files:**
- Modify: `.github/workflows/release.yml` — the stage named
  `Verify the catalog on main matches the pinned copy`

The current stage reads `main` and therefore cannot pass at this point in the job: the pin
has not been merged yet. It must read the branch the job just wrote.

- [ ] **Step 1: Replace the stage body**

Replace the whole stage with:

```yaml
      - name: Verify the catalog on the pin branch matches the pinned copy
        # Reads the pin branch, not main: at this point in the job the PR is open
        # but not merged, so main still carries the previous release and a
        # comparison against it could only fail. Route: contents API, not
        # raw.githubusercontent.com — the raw branch CDN caches for minutes, so
        # immediately after a successful write it can still serve pre-write
        # bytes and spuriously fail a write that already landed. The API is
        # read-your-writes consistent.
        #
        # Skipped when a guard caused the pin step to exit early, because then no
        # branch write happened and there is nothing new to verify; running it
        # anyway would compare a stale branch against the local copy and fail a
        # correctly-skipped release. The main catalog is verified by the PR's own
        # required checks, which the branch rule will not let a merge bypass.
        if: steps.pin.outputs.SKIPPED == '0'
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          set -euo pipefail
          PATH_IN_REPO=".claude-plugin/marketplace.json"
          BRANCH="${{ steps.pin.outputs.BRANCH }}"
          attempt=1
          while [ "$attempt" -le 3 ]; do
            if gh api "repos/${GITHUB_REPOSITORY}/contents/${PATH_IN_REPO}?ref=${BRANCH}" > /tmp/verify-contents.json; then
              # Two simple commands, not a pipe (nor `&&`): `bash -e {0}` has
              # no pipefail, so a jq failure piped into base64 would be
              # masked — and `&&` doesn't trip errexit on its left side.
              jq -er '.content' /tmp/verify-contents.json > /tmp/verify-marketplace.b64
              base64 -d /tmp/verify-marketplace.b64 > /tmp/verify-marketplace.json
              if cmp -s /tmp/verify-marketplace.json "$PATH_IN_REPO"; then
                echo "pin branch ${BRANCH} verified against the pinned copy"
                exit 0
              fi
            fi
            if [ "$attempt" -lt 3 ]; then
              echo "attempt ${attempt}: ${BRANCH} not in sync; retrying in 10s"
              sleep 10
            fi
            attempt=$((attempt + 1))
          done
          if [ ! -f /tmp/verify-marketplace.json ]; then
            echo "::error::could not fetch marketplace.json from ${BRANCH} via the contents API"
            exit 1
          fi
          echo "::error::${BRANCH} differs from the pinned local copy"
          diff -u .claude-plugin/marketplace.json /tmp/verify-marketplace.json || true
```

Note the removed lines: the `MAIN_VERSION` extraction and the downgrade-guard acceptance
branch. They are dead once the comparison target is the branch this job just wrote — the
local file and the branch are the same write, so "differs" can only mean the write did not
land.

- [ ] **Step 2: Prove the dead guard branch is gone from the verify stage**

Run:

```bash
awk '/^      - name: Verify the catalog on the pin branch/,/^      - name: |^$/' .github/workflows/release.yml \
  | grep -n 'MAIN_VERSION' && { echo 'FAIL: dead downgrade-guard branch still in verify stage'; exit 1; } \
  || echo 'ok: verify stage no longer carries the dead guard'
```

Expected: `ok: verify stage no longer carries the dead guard`

- [ ] **Step 3: Prove the verify stage is conditional**

Run:

```bash
grep -A2 'name: Verify the catalog on the pin branch' .github/workflows/release.yml | grep -q 'if: steps.pin.outputs.SKIPPED' \
  && echo 'ok: verify stage is conditional on the pin' \
  || { echo 'FAIL: verify stage would run after a skipped pin'; exit 1; }
```

Expected: `ok: verify stage is conditional on the pin`

- [ ] **Step 4: Lint and commit**

Run:

```bash
/tmp/opencode/actionlint .github/workflows/release.yml
```

Expected: no output, exit 0.

```bash
git add .github/workflows/release.yml
git commit -s -m "ci(release): verify the pin branch instead of main"
```

---

### Task 4: Drive the guards against a `gh` shim

**Files:**
- Create: `/tmp/opencode/pin-shim/gh` (throwaway; not committed)
- Create: `/tmp/opencode/pin-shim/cases.sh` (throwaway; not committed)

The guards are shell, and the release workflow cannot be executed here. Extracting each
stage's `run` block and driving it through `bash -e` with a fake `gh` is how the existing
release docs validate these guards, and it is the only way to prove the skip paths before a
tag exists.

- [ ] **Step 1: Extract the three stage bodies into real files**

Run:

```bash
mkdir -p /tmp/opencode/pin-shim
python3 - <<'PY'
import pathlib, re
text = pathlib.Path('.github/workflows/release.yml').read_text()
out = pathlib.Path('/tmp/opencode/pin-shim')
wanted = {
    'Propose the pinned catalog as a pull request': 'propose.sh',
    'Open or update the catalog pull request': 'open-pr.sh',
    'Verify the catalog on the pin branch matches the pinned copy': 'verify.sh',
}
for name, filename in wanted.items():
    m = re.search(rf'^      - name: {re.escape(name)}\n(.*?)(?=^      - name: |\Z)',
                  text, re.M | re.S)
    if not m:
        raise SystemExit(f'FAIL: stage not found: {name}')
    run = re.search(r'^        run: \|\n((?:          .*\n)+)', m.group(1), re.M)
    if not run:
        raise SystemExit(f'FAIL: no run block in stage: {name}')
    body = ''.join(line[10:] + '\n' for line in run.group(1).splitlines())
    (out / filename).write_text(body)
    print(f'ok: extracted {filename} ({len(body.splitlines())} lines)')
PY
```

Expected: three `ok: extracted ...` lines.

- [ ] **Step 2: Write the `gh` shim**

Create `/tmp/opencode/pin-shim/gh`:

```bash
#!/usr/bin/env bash
# Fake gh for driving release.yml stage bodies offline. Scenario is selected by
# GHOST_SHIM_SCENARIO; the recorded calls land in $GHOST_SHIM_LOG.
set -uo pipefail
echo "gh $*" >> "${GHOST_SHIM_LOG:?GHOST_SHIM_LOG required}"

main_catalog() {
  case "${GHOST_SHIM_SCENARIO}" in
    idempotent)      cat "${GHOST_SHIM_DIR}/main-pinned.json" ;;
    downgrade)       cat "${GHOST_SHIM_DIR}/main-newer.json" ;;
    needs-pin)       cat "${GHOST_SHIM_DIR}/main-old.json" ;;
  esac
}

# What the pin branch holds. After a successful write it carries the bytes this
# job just pinned — which is the local .claude-plugin/marketplace.json, NOT
# main's copy. The divergent scenario stands in for a write that did not land.
pin_branch_catalog() {
  case "${GHOST_SHIM_SCENARIO}" in
    divergent)       cat "${GHOST_SHIM_DIR}/main-old.json" ;;
    *)               cat "${GHOST_SHIM_DIR}/pinned.json" ;;
  esac
}

if [ "${1:-}" = "api" ]; then
  case "$*" in
    *"/contents/.claude-plugin/marketplace.json?ref=main"*)
      main_catalog | jq '{content: (. | @base64), sha: "deadbeef"}'
      ;;
    *"/contents/.claude-plugin/marketplace.json?ref=automation/marketplace-pin-"*)
      pin_branch_catalog | jq '{content: (. | @base64), sha: "deadbeef"}'
      ;;
    *" -X PUT "*) echo '{"commit":{"sha":"abc123"}}' ;;
    *) echo '{}' ;;
  esac
  exit 0
fi

case "$*" in
  "pr list"*)  echo "${GHOST_SHIM_PR_NUMBER:-}" ;;
  "pr edit"*)  echo '{"url":"https://github.com/wcatz/ghost/pull/999"}' ;;
  "pr create"*) echo '{"url":"https://github.com/wcatz/ghost/pull/999"}' ;;
  "pr view"*)  echo "https://github.com/wcatz/ghost/pull/999" ;;
  *) echo '{}' ;;
esac
```

Make it executable:

```bash
chmod +x /tmp/opencode/pin-shim/gh
```

- [ ] **Step 3: Build the three catalog fixtures**

Run:

```bash
cd /tmp/opencode/pin-shim
python3 - <<'PY'
import json, pathlib

def catalog(url, sha):
    return {"name": "ghost", "description": "d", "plugins": [
        {"name": "ghost", "displayName": "GhostMem",
         "source": {"url": url, "sha256": sha}}]}

old = "https://github.com/wcatz/ghost/releases/download/v0.30.17/ghost-plugin.zip"
new = "https://github.com/wcatz/ghost/releases/download/v0.30.19/ghost-plugin.zip"
pinned = "https://github.com/wcatz/ghost/releases/download/v0.30.18/ghost-plugin.zip"

pathlib.Path("main-old.json").write_text(json.dumps(catalog(old, "a" * 64), indent=2) + "\n")
pathlib.Path("main-newer.json").write_text(json.dumps(catalog(new, "b" * 64), indent=2) + "\n")
# The idempotent case must be byte-identical to the local pinned copy.
pathlib.Path("main-pinned.json").write_text(json.dumps(catalog(pinned, "c" * 64), indent=2) + "\n")
pathlib.Path("pinned.json").write_text(json.dumps(catalog(pinned, "c" * 64), indent=2) + "\n")
print("ok: 4 fixtures written")
PY
```

- [ ] **Step 4: Drive the idempotency-skip case**

Run:

```bash
cd /tmp/opencode/pin-shim
cp pinned.json .claude-plugin/marketplace.json 2>/dev/null || { mkdir -p .claude-plugin && cp pinned.json .claude-plugin/marketplace.json; }
export GHOST_SHIM_DIR=/tmp/opencode/pin-shim
export GHOST_SHIM_LOG=/tmp/opencode/pin-shim/log-idempotent
export GHOST_SHIM_SCENARIO=idempotent
export GITHUB_REF_NAME=v0.30.18 GITHUB_REPOSITORY=wcatz/ghost
export GITHUB_OUTPUT=/tmp/opencode/pin-shim/out-idempotent GITHUB_STEP_SUMMARY=/tmp/opencode/pin-shim/sum-idempotent
: > "$GITHUB_OUTPUT"; : > "$GITHUB_STEP_SUMMARY"; : > "$GHOST_SHIM_LOG"
PATH="/tmp/opencode/pin-shim:$PATH" bash -e propose.sh
echo "--- outputs ---"; cat "$GITHUB_OUTPUT"
grep -q 'SKIPPED=1' "$GITHUB_OUTPUT" && echo 'ok: idempotent skip sets SKIPPED=1' || { echo 'FAIL: expected SKIPPED=1'; exit 1; }
grep -q ' -X PUT ' "$GHOST_SHIM_LOG" && { echo 'FAIL: a write happened on the idempotent path'; exit 1; } || echo 'ok: no write on the idempotent path'
```

Expected: `ok: idempotent skip sets SKIPPED=1` and `ok: no write on the idempotent path`.

- [ ] **Step 5: Drive the downgrade-guard-skip case**

Run:

```bash
cd /tmp/opencode/pin-shim
export GHOST_SHIM_DIR=/tmp/opencode/pin-shim
export GHOST_SHIM_LOG=/tmp/opencode/pin-shim/log-downgrade
export GHOST_SHIM_SCENARIO=downgrade
export GITHUB_REF_NAME=v0.30.18 GITHUB_REPOSITORY=wcatz/ghost
export GITHUB_OUTPUT=/tmp/opencode/pin-shim/out-downgrade GITHUB_STEP_SUMMARY=/tmp/opencode/pin-shim/sum-downgrade
: > "$GITHUB_OUTPUT"; : > "$GITHUB_STEP_SUMMARY"; : > "$GHOST_SHIM_LOG"
PATH="/tmp/opencode/pin-shim:$PATH" bash -e propose.sh
echo "--- outputs ---"; cat "$GITHUB_OUTPUT"
grep -q 'SKIPPED=1' "$GHOST_SHIM_LOG" "$GHOST_SHIM_LOG" 2>/dev/null; grep -q 'SKIPPED=1' "$GITHUB_OUTPUT" && echo 'ok: downgrade skip sets SKIPPED=1' || { echo 'FAIL: expected SKIPPED=1'; exit 1; }
grep -q ' -X PUT ' "$GHOST_SHIM_LOG" && { echo 'FAIL: a write happened on the downgrade path'; exit 1; } || echo 'ok: no write on the downgrade path'
```

Expected: `ok: downgrade skip sets SKIPPED=1` and `ok: no write on the downgrade path`. The
staged run is a re-run of v0.30.18 while main pins v0.30.19, so the guard must skip.

- [ ] **Step 6: Drive the normal-pin case and confirm the branch target**

Run:

```bash
cd /tmp/opencode/pin-shim
export GHOST_SHIM_DIR=/tmp/opencode/pin-shim
export GHOST_SHIM_LOG=/tmp/opencode/pin-shim/log-pin
export GHOST_SHIM_SCENARIO=needs-pin
export GITHUB_REF_NAME=v0.30.18 GITHUB_REPOSITORY=wcatz/ghost
export GITHUB_OUTPUT=/tmp/opencode/pin-shim/out-pin GITHUB_STEP_SUMMARY=/tmp/opencode/pin-shim/sum-pin
: > "$GITHUB_OUTPUT"; : > "$GITHUB_STEP_SUMMARY"; : > "$GHOST_SHIM_LOG"
PATH="/tmp/opencode/pin-shim:$PATH" bash -e propose.sh
echo "--- outputs ---"; cat "$GITHUB_OUTPUT"
grep -q 'SKIPPED=0' "$GITHUB_OUTPUT" && echo 'ok: normal path sets SKIPPED=0' || { echo 'FAIL: expected SKIPPED=0'; exit 1; }
grep -q 'BRANCH=automation/marketplace-pin-v0.30.18' "$GITHUB_OUTPUT" && echo 'ok: branch name carries the version' || { echo 'FAIL: unexpected branch name'; exit 1; }
grep -q 'branch=automation/marketplace-pin-v0.30.18' "$GHOST_SHIM_LOG" && echo 'ok: write targets the pin branch' || { echo 'FAIL: write did not target the pin branch'; exit 1; }
grep -q 'branch=main' "$GHOST_SHIM_LOG" && { echo 'FAIL: write targeted main'; exit 1; } || echo 'ok: write never targets main'
```

Expected: three `ok:` lines and no `FAIL:`.

- [ ] **Step 7: Drive the PR-open and PR-update paths**

Run:

```bash
cd /tmp/opencode/pin-shim
export GHOST_SHIM_DIR=/tmp/opencode/pin-shim
export GITHUB_REF_NAME=v0.30.18 GITHUB_REPOSITORY=wcatz/ghost
export GITHUB_STEP_SUMMARY=/tmp/opencode/pin-shim/sum-pr
: > "$GITHUB_STEP_SUMMARY"

export GHOST_SHIM_LOG=/tmp/opencode/pin-shim/log-pr-create
export GHOST_SHIM_PR_NUMBER=""
export GITHUB_OUTPUT=/tmp/opencode/pin-shim/out-pr-create
: > "$GITHUB_OUTPUT"; : > "$GHOST_SHIM_LOG"
# The step body interpolates ${{ steps.pin.outputs.BRANCH }}; substitute it as
# the Actions runner would before executing the extracted body.
sed 's|\${{ steps.pin.outputs.BRANCH }}|automation/marketplace-pin-v0.30.18|' open-pr.sh > open-pr.run.sh
PATH="/tmp/opencode/pin-shim:$PATH" bash -e open-pr.run.sh
grep -q 'pr create' "$GHOST_SHIM_LOG" && echo 'ok: opens a PR when none exists' || { echo 'FAIL: no pr create'; exit 1; }

export GHOST_SHIM_LOG=/tmp/opencode/pin-shim/log-pr-edit
export GHOST_SHIM_PR_NUMBER=999
export GITHUB_OUTPUT=/tmp/opencode/pin-shim/out-pr-edit
: > "$GITHUB_OUTPUT"; : > "$GHOST_SHIM_LOG"
PATH="/tmp/opencode/pin-shim:$PATH" bash -e open-pr.run.sh
grep -q 'pr edit 999' "$GHOST_SHIM_LOG" && echo 'ok: updates the existing PR instead of failing' || { echo 'FAIL: expected pr edit 999'; exit 1; }
grep -q 'pr create' "$GHOST_SHIM_LOG" && { echo 'FAIL: tried to create a duplicate PR'; exit 1; } || echo 'ok: no duplicate PR created'
```

Expected: `ok: opens a PR when none exists`, `ok: updates the existing PR instead of failing`,
`ok: no duplicate PR created`.

- [ ] **Step 8: Drive the verify stage on both paths**

Run:

```bash
cd /tmp/opencode/pin-shim
export GHOST_SHIM_DIR=/tmp/opencode/pin-shim
export GITHUB_REPOSITORY=wcatz/ghost
sed 's|\${{ steps.pin.outputs.BRANCH }}|automation/marketplace-pin-v0.30.18|' verify.sh > verify.run.sh

export GHOST_SHIM_LOG=/tmp/opencode/pin-shim/log-verify
export GHOST_SHIM_SCENARIO=needs-pin
: > "$GHOST_SHIM_LOG"
PATH="/tmp/opencode/pin-shim:$PATH" bash -e verify.run.sh
echo 'ok: verify passes when the branch matches'

export GHOST_SHIM_SCENARIO=divergent
: > "$GHOST_SHIM_LOG"
if PATH="/tmp/opencode/pin-shim:$PATH" bash -e verify.run.sh 2>/dev/null; then
  echo 'FAIL: verify passed on a divergent branch'; exit 1
else
  echo 'ok: verify fails when the branch diverges'
fi
```

Expected: `ok: verify passes when the branch matches` and `ok: verify fails when the branch
diverges`. The `needs-pin` scenario serves both roles here: main still carries the old
catalog, while the pin branch correctly carries the bytes just written, so `cmp` succeeds.
`divergent` returns main's stale bytes for the branch, standing in for a write that did not
land.

- [ ] **Step 9: Confirm the throwaway harness is not committed**

Run:

```bash
git -C /home/wayne/git/ghost/.worktrees/review-nits status --short
git -C /home/wayne/git/ghost/.worktrees/review-nits ls-files | grep -c 'pin-shim' || echo 'ok: no shim files tracked'
```

Expected: `ok: no shim files tracked`. The shim lives in `/tmp/opencode` and is never added.

---

### Task 5: Update the release-pipeline spec paragraph

**Files:**
- Modify: `docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md` — the
  paragraph that currently says the catalog commit is blocked and describes the manual runbook
  as the only path.

- [ ] **Step 1: Locate the paragraph**

Run:

```bash
grep -n 'GH013\|Bypassed rule violations\|manual admin' docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md
```

Expected: the line numbers of the measured-blockage sentence and the runbook sentence.

- [ ] **Step 2: Rewrite the sentence describing the delivery mechanism**

Replace the clause that says the commit is blocked and the runbook is the only path, keeping
the measured evidence intact, with:

```text
The catalog reaches main as a pull request rather than a direct write: `main` requires a
pull request plus three status checks, and a workflow's `GITHUB_TOKEN` cannot be a bypass
actor on a personal repository (measured at v0.30.18: contents-API PUT returned HTTP 409
GH013, run 35928382388; the git-push route under the same context was rejected identically,
run 35929704681). The job writes the pinned catalog to
`automation/marketplace-pin-v<version>` and opens a PR from it. GitHub creates workflow runs
for a `GITHUB_TOKEN`-opened PR in an approval-required state, released from the merge box
with "Approve workflows to run", so the three required checks run and the branch rule governs
the merge. A GitHub App or PAT token would skip that click but adds a standing credential
for a PR a human was always going to merge. The branch is unique per release and is deleted
at merge by the repository's "Automatically delete head branches" setting. If the PR is
abandoned, the manual admin runbook below remains the fallback.
```

- [ ] **Step 3: Prove the stale claim is gone and the measured evidence survived**

Run:

```bash
grep -q 'this step fails at every release' docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md \
  && { echo 'FAIL: the old blocked-step claim is still present'; exit 1; } \
  || echo 'ok: blocked-step claim removed'
grep -q 'GH013' docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md \
  && echo 'ok: measured evidence retained' \
  || { echo 'FAIL: the measurement was dropped; it is the justification'; exit 1; }
grep -q 'manual admin runbook' docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md \
  && echo 'ok: runbook retained as fallback' \
  || { echo 'FAIL: fallback runbook reference was removed'; exit 1; }
```

Expected: three `ok:` lines.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md
git commit -s -m "docs(release): record the catalog pin as a pull request"
```

---

### Task 6: Full validation and hand off

**Files:** none modified; validation only.

- [ ] **Step 1: Confirm the working tree is clean and only expected files changed**

Run:

```bash
git status --short
git diff --name-only origin/main...HEAD
```

Expected: the diff lists only `.github/workflows/release.yml`,
`docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md`, and the two documents
this plan and its spec add. No runtime source, no `server.json`.

- [ ] **Step 2: Lint the workflow**

Run:

```bash
/tmp/opencode/actionlint .github/workflows/release.yml && echo 'ok: actionlint clean'
```

Expected: `ok: actionlint clean`.

- [ ] **Step 3: Syntax-check every run block**

Run:

```bash
python3 - <<'PY'
import pathlib, re, subprocess, sys
text = pathlib.Path('.github/workflows/release.yml').read_text()
blocks = re.findall(r'^        run: \|\n((?:          .*\n)+)', text, re.M)
bad = 0
for i, b in enumerate(blocks, 1):
    body = ''.join(line[10:] + '\n' for line in b.splitlines())
    p = subprocess.run(['bash', '-n'], input=body, text=True, capture_output=True)
    if p.returncode != 0:
        bad += 1
        print(f'FAIL block {i}: {p.stderr.strip()}')
print(f'ok: {len(blocks) - bad}/{len(blocks)} run blocks parse')
sys.exit(1 if bad else 0)
PY
```

Expected: `ok: N/N run blocks parse` with no `FAIL` line.

- [ ] **Step 4: Run the Go suite**

The change is workflow-only, so the suite is a regression check, not a coverage check.

Run:

```bash
go build ./... && go vet ./... && go test ./... -count=1 \
  -skip 'TestRelationClassifierLive|TestRelationClassifierLiveBatch|TestResolutionClassifierLive|TestResolutionClassifierLiveBatch'
```

Expected: exit 0. The four `*ClassifierLive*` tests shell out to the live LLM CLI and cannot
spawn it from inside an opencode session; they skip when no CLI is on `PATH` and are excluded
here for the same reason. `go build` covers `.github` not being Go.

- [ ] **Step 5: Confirm the release is not tagged yet**

Run:

```bash
git tag -l 'v0.31*' | grep -q . && { echo 'FAIL: a v0.31.x tag already exists'; exit 1; } || echo 'ok: v0.31.0 untagged'
```

Expected: `ok: v0.31.0 untagged`. This change must merge before the tag, or the release will
fail its last stage exactly as v0.30.18 did.

- [ ] **Step 6: Push and open the pull request**

```bash
git push -u origin fix/release-catalog-pin-pr
```

Then open a PR whose body states, in this order: the measured GH013 failure, the PR-based
mechanism and why it needs no credential, the two preserved guards, the one manual step
(approve the runs, then merge), and the repository setting that must be enabled. Do not
mention the release tag chronology or that this precedes v0.31.0 — the commit graph records
that.

- [ ] **Step 7: Report the hand-off**

State: the change is workflow-only, actionlint and `bash -n` clean, the four guard paths
driven green through the `gh` shim, the Go suite green, and **the repository setting is
outstanding** — "Automatically delete head branches" must be enabled by the owner or the
per-release branches accumulate. Also state that the plan could not be validated by running
the real release, because that requires a tag.

---

## Final handoff checklist

- [ ] Workflow-level `permissions:` is gone; each job carries exactly what it uses.
- [ ] No contents write targets `main`; the only write targets `automation/marketplace-pin-v<version>`.
- [ ] The idempotency and downgrade guards are present byte for byte and still exit before any write.
- [ ] Both new stages are conditional on `steps.pin.outputs.SKIPPED == '0'`.
- [ ] The verify stage reads the pin branch, and its dead downgrade branch is gone.
- [ ] The manual step is stated in the job summary.
- [ ] `actionlint` and `bash -n` clean; four guard paths driven green through the shim.
- [ ] `go build`, `go vet`, `go test ./...` green.
- [ ] The 2026-08-20 design spec no longer claims the step fails every release, and still
      carries the GH013 measurement and the runbook fallback.
- [ ] Repository setting "Automatically delete head branches" enabled by the owner.
- [ ] `v0.31.0` still untagged, and this change merged before it is cut.
