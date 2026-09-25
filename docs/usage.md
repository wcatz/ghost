# Using Ghost

Ghost is a memory layer for coding agents. This page explains the concepts and operations you need after installing it.

## The memory model

A memory is a short, durable note about a project. The agent saves memories through MCP tools as work produces useful knowledge. Keep each memory focused: one or two sentences is usually more useful than a transcript of the session.

Every memory has:

- A project scope
- One of eight categories
- An importance score from `0.0` to `1.0`
- Optional tags
- Creation and update timestamps
- Optional pinned state

Ghost detects near-duplicates on save within the same project — same-category saves fold at the standard similarity bar, and a cross-category re-save of the same rule folds too at token Jaccard >= 0.7 (resolved/superseded records are never fold targets). It preserves the new text as a linked row, strengthens the existing row, and records the relationship without overwriting the original; the existing memory keeps its category. A pinned memory is exempt from pruning and time-decay penalties.

## Choose a category

| Category | Use it for |
|---|---|
| `architecture` | Component boundaries, system design, and important relationships |
| `decision` | A choice with rationale or alternatives |
| `pattern` | A repeatable implementation or operating approach |
| `convention` | A repository or workflow rule |
| `gotcha` | A bug, pitfall, or surprising behavior |
| `dependency` | A version, API quirk, or external constraint |
| `preference` | A user preference that should follow future work |
| `fact` | General project knowledge that does not fit the other categories |

If a choice was made after considering alternatives, record a decision rather than reducing it to a bare fact.

## Projects and global memory

Agents should identify projects by the name reported for the current session. Ghost resolves a checkout through its longest recorded path prefix. For compatibility with clients that supply a path-shaped `project_id` on a save, Ghost also detects the checkout's normalized Git remote so worktrees and moved checkouts remain one project. If the project was first created under its plain name and has no recorded remote yet, the compatibility path binds the remote only when the repository name identifies exactly one unclaimed project; ambiguous or conflicting names are never guessed.

Project-specific knowledge belongs in that project. Use the special `_global` project for preferences and facts that should apply everywhere, such as a preferred validation workflow or a personal communication preference. Use `ghost_search_all` when the relevant knowledge might be stored under another project.

When the SessionStart hook reports that no project matched, ask the agent to establish or choose the correct project before saving new memories.

## Search and context

Ghost exposes pull-based MCP tools to the agent. The complete surface is listed in [`mcp.md`](mcp.md); the common operations are:

- Search the current project with `ghost_memory_search`.
- Browse a category with `ghost_memories_list` when you need exhaustive results.
- Search across projects with `ghost_search_all`.
- Load the current digest with `ghost_project_context` when context was not already injected.
- Pin project context, decisions, tasks, or global memories as MCP resources when the client supports resource pinning.

Search uses SQLite FTS5 by default. When local embeddings are available, Ghost combines full-text and vector results with Reciprocal Rank Fusion. Keywords remain useful for exact identifiers such as ports, versions, and hostnames; embeddings help with paraphrases and vocabulary mismatch.

Ranking is category-aware. Preferences, conventions, and facts do not decay. Architecture and patterns use a longer decay scale; decisions, gotchas, and dependencies use a shorter one. Pinned memories are always treated as stable. Decay changes ordering, not whether a memory can be found.

## Tasks

Tasks are work items that should survive across sessions. They have a title, optional description, priority, and one of these statuses:

```text
pending → active → done
              └→ blocked
```

Use tasks for follow-up work, not for facts that belong in memory. The agent can create, list, update, and complete tasks through MCP tools.

## Decisions

A decision record captures more than its final answer:

- The decision or chosen direction
- The rationale
- Alternatives considered
- Tags and lifecycle status

Use a decision record when a future reader needs to understand why an option was rejected or when a later decision may supersede the current one.

## Memory maintenance

The maintenance commands are explicit and dry-run by default. Preview first, then apply only after reviewing the output.

### Consolidate duplicates

```bash
ghost reflect myproject
ghost reflect myproject --apply
```

`reflect` can merge duplicates, prune noise, and promote useful cross-project knowledge. Cross-project candidates remain project-scoped unless you pass `--promote-globals`; that explicit opt-in writes them to `_global`, where they are injected into every project. It takes a snapshot before replacing project memories, refuses to replace the store with an empty result, and preserves manually saved memories. Use `--restore` to restore the most recent project snapshot; promoted globals are not covered by that restore and must be removed from `_global` separately.

The default `auto` tier routes through the calling session's CLI harness. When a source is known but its CLI binary is unavailable, it falls back to the offline SQLite/Jaccard tier; if the calling source cannot be detected, it fails rather than guessing. `--require-llm` disables the fallback and fails if the selected harness is unavailable.

### Mark resolved evidence

```bash
ghost resolve myproject
ghost resolve myproject --apply
```

`resolve` finds intermediate findings, changelog notes, and other resolved evidence. Applying the result stamps `resolved_at`, which removes the note from ranked session injection while keeping it searchable. Only explicit KEEP decisions are cached by content hash; garbled or otherwise unknown verdicts are reported as UNKNOWN, remain uncached, and are offered again on a later pass, so converged projects avoid repeated calls only after a real KEEP verdict.

### Link superseding memories

```bash
ghost supersede myproject
ghost supersede myproject --apply
```

`supersede` proposes semantically similar candidate pairs and asks the selected CLI harness to classify them as `supersedes`, `causes`, or `neither`. Applying the pass writes directed links. Search can then demote a stale memory below the replacement that supersedes it.

All three maintenance operations route through the calling harness when no explicit `--source` is supplied. Ghost fails rather than silently switching to a different harness or billing path. See the [CLI reference](cli.md#memory-maintenance) for all flags.

## Automatic lifecycle work

The Stop hook can spawn a detached `ghost lifecycle <project>` process after a session. Each enabled phase runs in order:

```text
reflect → resolve → supersede
```

The phases are disabled by default. When enabled, each phase has a timeout and failures are logged without blocking the hook. The lifecycle log is stored in Ghost's data directory. Keep automatic lifecycle work off until you understand the maintenance model and have a backup strategy.

Ghost's database-open and detached-lifecycle maintenance passes keep the newest three pre-migration database copies by default, bound the known lifecycle/Obsidian logs, and remove only dead retired per-phase PID/temp/lock claims. Current lifecycle/Obsidian claims are not reaped. Configure the limits with `retention.backup_count` and `retention.log_max_bytes`; see the [configuration reference](configuration.md#data-dir-retention).

## Obsidian vault mirror

Ghost can export memories, decisions, and tasks as Markdown for browsing in Obsidian:

```bash
ghost obsidian export --out ~/Documents/GhostVault
ghost obsidian sync --interval 30s
```

Use `--project myproject` to mirror one project plus global records. The mirror contains frontmatter, readable aliases, and wikilinks for related memories. It is strictly one-way:

- Ghost reads the database and writes notes.
- Vault edits are not imported back into Ghost.
- A running sync may overwrite hand-edited notes after a database change.
- A marker directory protects unrelated files from cleanup.
- Crashed-export temporaries are reclaimed only after a one-hour grace period, an exact generated-name match, and an open-handle check.

Set `obsidian.auto_sync: true` only if you want the SessionStart hook to launch a background sync process.

## Agent workflow

A practical loop is:

1. Load project context before planning.
2. Search memory before changing an unfamiliar component.
3. Save a gotcha, convention, preference, or architecture note as soon as it is learned.
4. Record a decision when alternatives were considered.
5. Use dry-run maintenance commands periodically and review snapshots before applying changes.

This complements structured workflows such as [Superpowers](https://github.com/obra/superpowers): the workflow controls how the agent works, while Ghost retains what the work discovered.

## Ownership and removal

The SQLite database is the source of truth. Back it up before destructive maintenance:

```bash
cp "$HOME/.local/share/ghost/ghost.db" ghost.db.backup
```

To remove a single memory, use the corresponding MCP delete tool. To remove an entire project, use `ghost project delete <name>` without `--apply` first. The CLI then requires the project name to be retyped before permanent deletion; see the [CLI reference](cli.md#project-operations).
