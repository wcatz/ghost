# GhostMem Minimal Launch Rebrand Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Present the product as **GhostMem** in the approved launch surfaces while preserving every existing `ghost` binary, MCP, package, URL, configuration, data, and plugin identifier.

**Architecture:** This is a text-and-metadata-only rebrand. The implementation edits `README.md`, the documentation index, `overview.html`, and human-readable Claude plugin/marketplace metadata; it does not touch Go runtime code, MCP identifiers, `server.json`, `go.mod`, paths, URLs, or historical records. A small static boundary check compares the changed metadata against the base commit so compatibility fields cannot drift during the copy edits.

**Tech Stack:** Markdown, static HTML, JSON, `jq`, Python 3 standard library, GitHub CLI, existing Go/plugin validation commands.

---

## File and surface map

### Modify

- `README.md` — primary product title, headings, comparison labels, and explanatory prose.
- `docs/README.md` — documentation landing-page title and introductory labels.
- `overview.html` — visible title, navigation brand, headings, marketing copy, and the publishing-platform comparison sentence.
- `.claude-plugin/marketplace.json` — marketplace description and the three plugin `displayName`/`description` values.
- `plugin/.claude-plugin/plugin.json` — POSIX plugin `displayName` and description.
- `plugin-windows/.claude-plugin/plugin.json` — Windows plugin `displayName` and description.

### External update

- GitHub repository `wcatz/ghost` About description — set the new public description without renaming the repository slug.

### Preserve exactly

- `go.mod` module `github.com/wcatz/ghost`.
- `ghost` binary, commands, `ghost_*` tool names, and `ghost://` resources.
- `server.json` name/title/metadata.
- `ghcr.io/wcatz/ghost`, release archive names, and `wcatz/ghost` URLs.
- Config/data paths, `GHOST_*` variables, database filenames, and hook commands.
- Plugin `name` fields, package IDs, homepage/repository URLs, and source archive URLs.
- `CLAUDE.md`, focused technical docs, benchmark records, code comments, and `docs/superpowers/` historical records.

### Base reference

Before editing, bring the unpublished design branch up to the current base and
record that base commit:

```bash
git fetch origin main
git merge --no-ff origin/main -m "chore: merge origin/main into docs/ghostmem-rebrand"
BASE=$(git merge-base HEAD origin/main)
printf 'base=%s\n' "$BASE"
```

If the branch is already current, the merge command is a no-op. Do not rebase
or amend the design-spec commit while implementing this plan.

---

### Task 1: Rename the README and documentation index

**Files:**
- Modify: `README.md:1-246`
- Modify: `docs/README.md:1-21`
- Test: existing Markdown/link validation commands; no runtime test is needed because no executable code changes.

- [ ] **Step 1: Capture the human-facing brand references before editing**

Run:

```bash
git grep -n -E '\bGhost\b' -- README.md docs/README.md
```

Expected: matches in the title, headings, comparison table, and explanatory prose. Matches inside lowercase `ghost` commands, paths, URLs, or MCP identifiers are not targets.

- [ ] **Step 2: Update `README.md` visible copy**

Make these exact class of changes:

```text
# Ghost                                      → # GhostMem
alt="Ghost"                                  → alt="GhostMem"
Why Ghost?                                   → Why GhostMem?
#why-ghost                                   → #why-ghostmem
What Ghost remembers                         → What GhostMem remembers
|Ghost|                                      |GhostMem|
Ghost remembers                              → GhostMem remembers
Ghost provides                               → GhostMem provides
Ghost is                                     → GhostMem is
Ghost works                                  → GhostMem works
Ghost-specific                               → GhostMem-specific
Ghost is a solo project                      → GhostMem is a solo project
```

Update ordinary product prose such as “Ghost publishes…” and “Ghost injects…” to
`GhostMem`. Keep all of these unchanged:

```text
go install github.com/wcatz/ghost/cmd/ghost@latest
ghost mcp init
ghost mcp status
ghost reflect
$XDG_DATA_HOME/ghost/ghost.db
~/.local/share/ghost/ghost.db
GHOST_* variables
```

Do not add a “formerly Ghost” note or a `ghostmem` command. The technical
`ghost` examples are intentional compatibility references.

- [ ] **Step 3: Update the documentation landing page**

In `docs/README.md`, change only the visible product labels:

```text
# Ghost documentation                         → # GhostMem documentation
Ghost's documentation                         → GhostMem's documentation
## Using Ghost                                → ## Using GhostMem
[Using Ghost]                                 → [Using GhostMem]
## Understanding Ghost                        → ## Understanding GhostMem
```

Keep every link target unchanged. The linked focused pages remain technical
Ghost-named documentation under the approved boundary.

- [ ] **Step 4: Verify the README/index edits did not become a global replacement**

Run:

```bash
git diff -- README.md docs/README.md
git diff --word-diff=porcelain -- README.md docs/README.md | grep -E 'ghostmem|GhostMem' | sed -n '1,200p'
git grep -n -E 'github.com/wcatz/ghost|ghost mcp|ghost\.db|GHOST_' -- README.md docs/README.md
```

Expected: visible brand references use `GhostMem`; all command, URL, path, and
environment references remain lowercase `ghost`/`GHOST_`.

- [ ] **Step 5: Run the Markdown link/anchor check**

Run the repository's existing link-check procedure if present; otherwise run:

```bash
python3 - <<'PY'
from pathlib import Path
import re
import urllib.parse

files = [Path('README.md'), Path('docs/README.md')]
missing = []
for path in files:
    text = path.read_text()
    text = re.sub(r'```.*?```', '', text, flags=re.S)
    for match in re.finditer(r'(?<!!)\[[^\]]*\]\(([^)]+)\)', text):
        raw = match.group(1).strip()
        if not raw or raw.startswith(('#', 'http://', 'https://', 'mailto:')):
            continue
        target = raw.split()[0].strip('<>')
        file_part, _, anchor = target.partition('#')
        resolved = (path.parent / urllib.parse.unquote(file_part)).resolve()
        if file_part and not resolved.exists():
            missing.append(f'{path}: {raw}')
        elif anchor and resolved.suffix == '.md':
            slugs = []
            for line in resolved.read_text().splitlines():
                if line.startswith('#'):
                    slug = re.sub(r'[^\w\s-]', '', line.lstrip('#').strip().lower())
                    slugs.append(re.sub(r'\s+', '-', slug))
            if anchor.lower() not in slugs:
                missing.append(f'{path}: {raw}')
if missing:
    raise SystemExit('\n'.join(missing))
print('README link/anchor check passed')
PY
```

Expected: `README link/anchor check passed`.

- [ ] **Step 6: Commit the launch-copy change**

```bash
git add README.md docs/README.md
git commit -s -m "docs: use GhostMem in launch copy"
```

---

### Task 2: Rename the public overview page

**Files:**
- Modify: `overview.html:7,251,258,269-328,347-828`
- Test: HTML parser and a targeted visible-brand grep.

- [ ] **Step 1: Capture visible old-brand references**

Run:

```bash
git grep -n -E 'Ghost' -- overview.html
```

Expected matches include the `<title>`, navigation brand, headings, feature
copy, benchmark labels, FAQ copy, and the sentence comparing the name with a
well-known publishing platform.

- [ ] **Step 2: Update visible page copy and headings**

Change these visible labels:

```text
<title>Ghost — Local-first agent memory</title>
  → <title>GhostMem — Local-first agent memory</title>

alt="Ghost logo"                              → alt="GhostMem logo"
<span>ghost</span>                             → <span>ghostmem</span>
Build on Ghost                                → Build on GhostMem
Ghost gives Claude Code...                    → GhostMem gives Claude Code...
Ghost packs...                                → GhostMem packs...
Ghost's tools...                              → GhostMem's tools...
Ghost runs...                                 → GhostMem runs...
Ghost (hybrid)                                → GhostMem (hybrid)
Ghost (FTS only)                              → GhostMem (FTS only)
Ghost (judged smoke)                          → GhostMem (judged smoke)
Build on GhostMem's vertical product...       → Build on GhostMem's vertical product...
```

Replace ordinary product references in headings, paragraphs, labels, and FAQ
answers with `GhostMem`. Rephrase the existing publishing-platform comparison
so it no longer presents the old product name as a current brand; for example:

```text
The name is intentionally distinct from a well-known publishing platform.
```

Do not change these technical strings:

```text
https://github.com/wcatz/ghost/...
ghcr.io/wcatz/ghost
assets/ghost.png
~/.local/share/ghost
$XDG_DATA_HOME/ghost
ghost mcp init
ghost hook
ghost reflect
ghost.exe
```

Keep CSS class names such as `.btn-ghost` unchanged.

- [ ] **Step 3: Parse the HTML and inspect the changed text**

Run:

```bash
python3 - <<'PY'
from html.parser import HTMLParser
HTMLParser().feed(open('overview.html', encoding='utf-8').read())
print('overview.html parsed')
PY

git diff --check
git diff --word-diff=porcelain -- overview.html | grep -E 'GhostMem|ghostmem' | sed -n '1,240p'
```

Expected: the HTML parser succeeds; visible copy contains `GhostMem`; URLs,
commands, paths, and CSS class names remain unchanged.

- [ ] **Step 4: Commit the overview change**

```bash
git add overview.html
git commit -s -m "docs: rename the public overview page"
```

---

### Task 3: Update human-readable plugin and marketplace metadata

**Files:**
- Modify: `.claude-plugin/marketplace.json:3-49` (descriptions/display names only)
- Modify: `plugin/.claude-plugin/plugin.json:3,5`
- Modify: `plugin-windows/.claude-plugin/plugin.json:3,5`
- Test: `internal/mcpinit/plugin_names_coupling_test.go` and JSON parsing.

- [ ] **Step 1: Record the compatibility fields before editing**

Run:

```bash
python3 - <<'PY'
import json
from pathlib import Path

files = [
    Path('.claude-plugin/marketplace.json'),
    Path('plugin/.claude-plugin/plugin.json'),
    Path('plugin-windows/.claude-plugin/plugin.json'),
]
for path in files:
    data = json.loads(path.read_text())
    if path.name == 'marketplace.json':
        print(path, 'marketplace name=', data['name'])
        print('plugin names=', [p['name'] for p in data['plugins']])
    else:
        print(path, 'name=', data['name'], 'homepage=', data['homepage'],
              'repository=', data['repository'])
PY
```

Expected values that must remain unchanged:

```text
marketplace name = ghost
plugin names = ghost, ghost-windows-amd64, ghost-windows-arm64
plugin manifest names = ghost, ghost-windows
homepage/repository = https://github.com/wcatz/ghost
```

- [ ] **Step 2: Change only display text**

Set the marketplace description to lead with:

```text
GhostMem — a local-first MCP memory server for Claude Code. One binary, one SQLite file, no cloud.
```

Set display names to:

```text
GhostMem (macOS & Linux)
GhostMem (Windows x64)
GhostMem (Windows ARM64)
```

Set the POSIX manifest `displayName` to `GhostMem` and the Windows manifest
`displayName` to `GhostMem (Windows)`.

Within descriptions, replace human-facing product references such as “projects
Ghost already knows” with “projects GhostMem already knows.” Preserve all
machine identifiers and commands, including `ghost-windows-amd64`,
`ghost-windows-arm64`, `ghost.exe`, `ghost mcp init`, `reflect`, `resolve`,
`supersede`, and all URLs/archive names.

- [ ] **Step 3: Validate JSON and compatibility fields**

Run:

```bash
jq empty .claude-plugin/marketplace.json
jq empty plugin/.claude-plugin/plugin.json
jq empty plugin-windows/.claude-plugin/plugin.json

python3 - <<'PY'
import json
from pathlib import Path

market = json.loads(Path('.claude-plugin/marketplace.json').read_text())
assert market['name'] == 'ghost'
assert [p['name'] for p in market['plugins']] == [
    'ghost', 'ghost-windows-amd64', 'ghost-windows-arm64'
]
for path in ('plugin/.claude-plugin/plugin.json', 'plugin-windows/.claude-plugin/plugin.json'):
    data = json.loads(Path(path).read_text())
    assert data['homepage'] == 'https://github.com/wcatz/ghost'
    assert data['repository'] == 'https://github.com/wcatz/ghost'
print('plugin compatibility fields unchanged')
PY
```

Expected: all commands succeed and print `plugin compatibility fields unchanged`.

- [ ] **Step 4: Run the existing plugin coupling tests**

```bash
go test ./internal/mcpinit -run 'TestPluginNameCoupling|TestPluginInstalledRegistryNames' -count=1
```

Expected: `ok` for `github.com/wcatz/ghost/internal/mcpinit`; package IDs and
assembled manifest names remain identical.

- [ ] **Step 5: Commit the metadata change**

```bash
git add .claude-plugin/marketplace.json plugin/.claude-plugin/plugin.json plugin-windows/.claude-plugin/plugin.json
git commit -s -m "chore(plugin): show GhostMem in plugin metadata"
```

---

### Task 4: Update the GitHub repository About description

**External state:** GitHub repository `wcatz/ghost`; no repository slug or
code path is changed.

- [ ] **Step 1: Read the current About description**

```bash
gh repo view wcatz/ghost --json description --jq '.description'
```

Expected: the current Ghost-branded description is visible before the change.

- [ ] **Step 2: Set the new description**

```bash
gh repo edit wcatz/ghost --description "GhostMem — a local-first MCP memory server for Claude Code, opencode, Cursor, and other MCP clients."
```

This changes only the About text. Do not rename the repository, change its
description slug, or alter the homepage URL.

- [ ] **Step 3: Read the description back**

```bash
gh repo view wcatz/ghost --json description,url --jq '{description: .description, url: .url}'
```

Expected: the description starts with `GhostMem`; the repository URL remains
`https://github.com/wcatz/ghost`.

---

### Task 5: Run the complete rebrand validation

**Files:** all files changed by Tasks 1–3; no additional source changes.

- [ ] **Step 1: Verify the approved change set**

```bash
BASE=$(git merge-base HEAD origin/main)
git diff --name-only "$BASE"..HEAD | sort
```

Expected implementation paths are limited to:

```text
README.md
docs/README.md
overview.html
.claude-plugin/marketplace.json
plugin/.claude-plugin/plugin.json
plugin-windows/.claude-plugin/plugin.json
```

The already-approved design/plan records may also appear in the branch diff:

```text
docs/superpowers/specs/2026-09-23-ghostmem-rebrand-design.md
docs/superpowers/plans/2026-09-23-ghostmem-rebrand.md
```

No other implementation path is allowed.

- [ ] **Step 2: Verify compatibility identifiers did not change**

```bash
BASE=$(git merge-base HEAD origin/main)
python3 - "$BASE" <<'PY'
import json
import subprocess
import sys
from pathlib import Path

base = sys.argv[1]
paths = [
    '.claude-plugin/marketplace.json',
    'plugin/.claude-plugin/plugin.json',
    'plugin-windows/.claude-plugin/plugin.json',
]
for path in paths:
    before = json.loads(subprocess.check_output(
        ['git', 'show', f'{base}:{path}'], text=True
    ))
    after = json.loads(Path(path).read_text())
    if path.endswith('marketplace.json'):
        assert before['name'] == after['name'] == 'ghost'
        assert [p['name'] for p in before['plugins']] == [p['name'] for p in after['plugins']]
    else:
        for key in ('name', 'homepage', 'repository', 'version', 'license', 'keywords'):
            assert before[key] == after[key], (path, key)
for path in ('go.mod', 'server.json'):
    before = subprocess.check_output(['git', 'show', f'{base}:{path}'], text=True)
    after = Path(path).read_text()
    assert before == after, path
print('compatibility identifiers unchanged')
PY
```

Expected: `compatibility identifiers unchanged`.

- [ ] **Step 3: Run repository validation**

```bash
git diff --check
go test ./... -count=1
go vet ./...
go test ./internal/mcpinit -run 'TestPluginNameCoupling|TestPluginInstalledRegistryNames' -count=1
```

Expected: all commands exit 0.

- [ ] **Step 4: Run document and manifest checks**

```bash
jq empty .claude-plugin/marketplace.json
jq empty plugin/.claude-plugin/plugin.json
jq empty plugin-windows/.claude-plugin/plugin.json
```

Run Markdown link/anchor validation for `README.md` and `docs/README.md` as
specified in Task 1, and parse `overview.html` with Python's `HTMLParser`.

- [ ] **Step 5: Review the final diff for accidental technical renames**

```bash
BASE=$(git merge-base HEAD origin/main)
git diff --unified=0 "$BASE"..HEAD -- \
  README.md docs/README.md overview.html \
  .claude-plugin/marketplace.json \
  plugin/.claude-plugin/plugin.json \
  plugin-windows/.claude-plugin/plugin.json | \
  grep -E '^[+-].*(ghostmem|ghost_|ghost://|github.com/wcatz/ghost|ghcr.io/wcatz/ghost|ghost\.db|GHOST_)' || true
```

Expected: no added or removed compatibility identifier. `GhostMem` changes may
appear in prose and display fields; lowercase `ghost` commands and URLs must
not be rewritten.

- [ ] **Step 6: Commit any validation-only corrections**

If a correction is required, stage only the affected approved file and commit:

```bash
git add README.md docs/README.md overview.html .claude-plugin/marketplace.json plugin/.claude-plugin/plugin.json plugin-windows/.claude-plugin/plugin.json
git commit -s -m "docs: polish GhostMem launch copy"
```

If no correction is required, do not create an empty commit.

---

## Final handoff checklist

- [ ] `README.md`, `docs/README.md`, and `overview.html` visibly use `GhostMem`.
- [ ] GitHub About description starts with `GhostMem`.
- [ ] Plugin display names/descriptions visibly use `GhostMem`.
- [ ] `ghost` binary, MCP names, URLs, module path, package/image IDs, config/data paths, and environment variables are unchanged.
- [ ] No runtime source file or historical record was modified.
- [ ] JSON, HTML, Markdown, plugin coupling, Go tests, and `go vet` pass.
- [ ] The final branch contains only the approved branding/documentation changes plus the already-reviewed design and plan records.
