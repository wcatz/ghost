# README Rewrite Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite the README to reflect Ghost's current architecture — opencode as the primary LLM path, 20 MCP tools + 2 prompts, and the self-hosted PR-Agent review pipeline.

**Architecture:** The README has drifted from the codebase in 6 areas. This plan patches each area surgically (one task per section), then adds the missing PR-Agent pipeline section.

**Tech Stack:** README.md (Markdown), cmd/ghost/main.go (help text already updated in 6c5e378)

---

### Task 1: Fix tool count and MCP surface table

**Files:**
- Modify: `README.md:266-278`

The tool table lists 19 tools; the actual count is 20. `ghost_project_delete` (shipped #313) is missing from the Memory group. Two prompts (`recall_project`, `record_decision`) are not mentioned.

- [ ] **Step 1: Update the MCP surface table**

Replace the current table and its surrounding text (lines 266-278) with:

```markdown
## MCP surface

20 tools, 4 resources, 2 prompts:

| Group | Tools |
|---|---|
| Memory | `ghost_memory_save` `ghost_memory_search` `ghost_search_all` `ghost_memories_list` `ghost_memory_update` `ghost_memory_delete` `ghost_memory_pin` `ghost_memory_promote` `ghost_save_global` `ghost_resolve` `ghost_project_delete` |
| Context | `ghost_project_context` `ghost_list_projects` `ghost_health` |
| Tasks | `ghost_task_create` `ghost_task_list` `ghost_task_update` `ghost_task_complete` |
| Decisions | `ghost_decision_record` `ghost_decisions_list` |

`ghost_resolve` scans a project's memories for resolved-evidence notes (intermediate findings, changelog entries, superseded experiments) using the calling session's own model via MCP sampling when the client supports it, falling back to a subscription-billed `claude -p` call otherwise (dry-run only on that fallback) — no Anthropic API credits spent either way. Args: `project` (required), `apply` (default false: dry-run preview only; pass `true` to stamp `resolved_at` on confirmed memories).

Resources: project context, global memories, project decisions, project tasks — pin them in clients that support it to survive context compaction.

Prompts: `recall_project` (injects project context into the conversation), `record_decision` (guides structured decision recording).

The server ships with embedded instructions that teach the agent when to save, which categories to use, and how to leverage cross-project search — it works proactively without configuration. Full architecture notes in [docs/architecture.md](docs/architecture.md).
```

- [ ] **Step 2: Verify the tool count matches code**

Run: `grep -c "ghost_" internal/mcpserver/tools.go` (or equivalent) to confirm the count is 20.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -s "docs(readme): update MCP surface table to 20 tools and 2 prompts"
```

---

### Task 2: Fix consolidation section — add CLI tier

**Files:**
- Modify: `README.md:209-218`

The consolidation section says "Tiered: Claude Haiku first..., falling back to a fully offline SQLite tier". This omits the CLI tier (claude/opencode/codex/goose) which is now the primary middle layer.

- [ ] **Step 1: Update consolidation prose**

Replace the consolidation text (lines 209-218) with:

```markdown
### Consolidation you can undo

`ghost reflect` merges duplicates, prunes noise, and promotes cross-project knowledge to global scope. Tiered: Anthropic API (Haiku) first, then a CLI tier (claude, opencode, codex, or goose — whichever is on PATH), falling back to a fully offline SQLite tier (Jaccard >= 0.5, same-category merges). When `--source` is set (e.g. `--source opencode`), the API tier is skipped entirely and the matching CLI binary is used directly.

Because an LLM rewriting your memory store is scary, the guardrails are layered:

- **Dry run by default** — see the diff before `--apply`
- **Auto-snapshot before every replace**, keeping the 3 most recent per project; `ghost reflect --restore` is the undo button
- **Empty-set refusal** — the store layer will not replace your memories with nothing, ever
- **Quality gate** — in auto mode, output shrinking below 30% of input is rejected and the next tier is tried (when input >= 6 memories)
- **Manually saved memories are always preserved**
```

- [ ] **Step 2: Update the comparison table row for consolidation**

In the comparison table at line 121, change:

```
| Consolidation | None | Haiku LLM or local Jaccard tier |
```

to:

```
| Consolidation | None (Dreams, managed) | Anthropic API, CLI fallback, or local Jaccard |
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -s "docs(readme): add CLI consolidation tier between Haiku API and SQLite"
```

---

### Task 3: Fix ANTHROPIC_API_KEY references throughout

**Files:**
- Modify: `README.md:150,177,312`

Three sections mention ANTHROPIC_API_KEY as only relevant to Haiku consolidation. The actual consumers are reflect (Haiku tier), resolve, and supersede — all fall back to the Anthropic API when no CLI binary is available.

- [ ] **Step 1: Fix "Where does my data go" section (line 150)**

Change the relevant sentence from:

```
Ghost makes no network calls in normal operation, with three exceptions you control: **localhost** Ollama for embeddings (optional), the Claude API *only if* you run `ghost reflect` with the Haiku tier (needs `ANTHROPIC_API_KEY`; the SQLite tier is fully offline), and the GitHub API *only if* you run `ghost upgrade`. That's the complete list.
```

to:

```
Ghost makes no network calls in normal operation, with four exceptions you control: **localhost** Ollama for embeddings (optional), the Anthropic API when `ANTHROPIC_API_KEY` is set and no CLI binary (claude/opencode/codex/goose) is available for consolidation, resolve, or supersede, the GitHub API *only if* you run `ghost upgrade`, and localhost Ollama for the optional Haiku reflection tier. That's the complete list.
```

- [ ] **Step 2: Fix "What does it cost" section (line 177)**

Change:

```
$0/month. No metered API in the hot path. The only paid call in the entire codebase is the optional Haiku consolidation tier — and it has a free offline fallback.
```

to:

```
$0/month. No metered API in the hot path. When `ANTHROPIC_API_KEY` is set and no CLI binary is available, consolidate/resolve/supersede use the Anthropic API (cost scales with memory count — roughly $0.001 for a typical project); otherwise they fall back to a free CLI call (claude, opencode, codex, or goose) or the fully offline SQLite tier.
```

- [ ] **Step 3: Fix Configuration section (line 312)**

Change:

```
4. `GHOST_*` environment variables, plus `ANTHROPIC_API_KEY` for the Haiku reflection tier
```

to:

```
4. `GHOST_*` environment variables, plus `ANTHROPIC_API_KEY` (used by reflect/resolve/supersede when no CLI binary is available)
```

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -s "docs(readme): fix ANTHROPIC_API_KEY references to reflect all consumers"
```

---

### Task 4: Add opencode integration details and env vars

**Files:**
- Modify: `README.md` (Configuration section, after the embedding/linking yaml block)

The `GHOST_OPENCODE_MODEL` env var (pins model for opencode-backed tiers) is not documented. `OPENCODE_API_KEY` (zen endpoint auth) is used by opencode but not Ghost. Session titles (`[ghost]` prefix) are not mentioned.

- [ ] **Step 1: Add opencode env vars to Configuration section**

After the existing yaml config block (line 322), add:

```markdown
**OpenCode integration:** set `GHOST_OPENCODE_MODEL` to pin the model for opencode-backed tiers (e.g. `GHOST_OPENCODE_MODEL=big-pickle`). The opencode backend runs `opencode run --pure --title "[ghost]"` per LLM call — headless sessions are titled `[ghost]` so they're filterable in opencode's session search. Authentication is handled by opencode's own provider config; Ghost does not need `OPENCODE_API_KEY` directly.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -s "docs(readme): document GHOST_OPENCODE_MODEL and opencode session titles"
```

---

### Task 5: Expand PR-Agent review pipeline section

**Files:**
- Modify: `README.md:415-418`

The current "Review pipeline" section is two sentences. The actual pipeline is self-hosted PR-Agent with zen/big-pickle, auto-review + auto-improve, verdict card, ticket compliance disabled, intro line disabled, and the 3-lens extra_instructions.

- [ ] **Step 1: Replace the Review pipeline section**

Replace lines 415-418 with:

```markdown
## Review pipeline

PRs are reviewed automatically on every push. The pipeline uses [PR-Agent](https://github.com/The-PR-Agent/pr-agent) self-hosted on GitHub Actions, powered by Big Pickle via the opencode zen endpoint (`https://opencode.ai/zen/v1`), with DeepSeek V4 Flash as a fallback.

**What it does:**
- `/review` posts a persistent review comment with score, effort estimate, and up to 5 findings (inline on diff lines)
- `/improve` posts committable code suggestions as GitHub suggestion blocks (top 4 per run)
- Reviews are anchored to the default branch via `apply_repo_settings` (fetches `.pr_agent.toml` and context files)

**What it doesn't do:**
- Ticket compliance analysis is disabled (`require_ticket_analysis_review = false`) — the native grading mislabeled clean PRs and the merge gate is conversation resolution
- The intro line ("Here are some key observations...") is disabled (`enable_intro_text = false`)
- Auto-describe is disabled — findings live in the review, not in the PR description

**Extra instructions** enforce 3 lenses beyond diff-vs-issue matching: invariant parity (cross-checking guard clauses against sibling mutators), protected resources (_global project, DB rows, subprocess env), and behavior preservation at modified call sites.

**Review identity:** Reviews post as the Review Loop GitHub App (`review-sweeper`) when the app token is available; falls back to `github-actions` when secrets are absent.

**Concurrency:** one agent run per PR per event type. Bot comments fire `issue_comment` runs; a shared group with event-type splitting prevents the bot from cancelling its own in-flight review.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -s "docs(readme): expand PR-Agent review pipeline section"
```

---

### Task 6: Minor CLI reference fixes

**Files:**
- Modify: `README.md:283-299`

Minor omissions: `--source` flag on reflect/supersede/resolve not shown, `ghost hook` is hidden from `--help` but shown in README.

- [ ] **Step 1: Update the CLI reference block**

Replace the CLI block (lines 283-299) with:

```text
ghost mcp                         # Run MCP server on stdio (used by your MCP client)
ghost mcp init [--client claude|opencode|codex|goose|all] [--dry-run]  # Configure MCP client integration (default: Claude Code)
ghost mcp status [--client claude|opencode|codex|goose]                # Deep health checks (incl. Ollama reachability, model presence)
ghost hook <event> --source <host>   # Contract-v1 lifecycle hook (session-start, stop, session-end)
ghost reflect <project> [flags]      # Memory consolidation (dry-run by default; --apply, --restore, --tier, --source)
ghost resolve <project> [flags]      # De-weight resolved-evidence memories (dry-run by default; --apply, --source)
ghost supersede <project> [flags]    # Link superseded memories (dry-run by default; --apply, --threshold, --source)
ghost project delete <name> [flags]  # Permanently delete a project (dry-run by default; --apply + name re-type to confirm)
ghost project merge <old> <new>      # Merge one project into another; child records move to the survivor
ghost context [--cwd <dir>]          # Print the passive session-start context block (for opencode)
ghost bench [--sweep]                # Retrieval-quality benchmark on the built-in dataset
ghost obsidian export                # Mirror memories to an Obsidian vault (one-way; --out, --project)
ghost obsidian sync                  # Keep the vault mirror fresh (--interval; polls for DB changes)
ghost upgrade                        # Self-update from GitHub Releases (linux/macOS; Windows: re-download)
ghost version                        # Print version
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -s "docs(readme): add --source flags to CLI reference, reorder for clarity"
```

---

### Task 7: Fix consolidation comparison table

**Files:**
- Modify: `README.md:115-126`

The comparison table's "Consolidation" row says "Haiku LLM or local Jaccard tier" — needs to reflect the full tier chain.

- [ ] **Step 1: Update the consolidation row**

Already addressed in Task 2 Step 2. Verify the row now reads:

```
| Consolidation | None (Dreams, managed) | Anthropic API, CLI fallback, or local Jaccard |
```

If already correct from Task 2, skip. Otherwise apply.

- [ ] **Step 2: Commit (only if Task 2 didn't already cover this)**

```bash
git add README.md
git commit -s "docs(readme): update consolidation row in comparison table"
```

---

### Task 8: Final verification

**Files:**
- Read: `README.md` (full file)

- [ ] **Step 1: Read the full README and verify all changes**

Check:
- Tool count says 20 (not 19)
- `ghost_project_delete` appears in the MCP surface table
- Consolidation section mentions CLI tier
- ANTHROPIC_API_KEY references list all consumers (reflect, resolve, supersede)
- `GHOST_OPENCODE_MODEL` is documented
- PR-Agent pipeline section is comprehensive
- CLI reference includes `--source` flags
- No remaining references to "Haiku tier" as the primary path

- [ ] **Step 2: Run `go build ./...` and `go test ./...` to ensure no regressions**

- [ ] **Step 3: Commit if any final fixes were needed**

```bash
git add README.md
git commit -s "docs(readme): final verification pass"
```
