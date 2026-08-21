# Ghost

<img src="assets/ghost.png" alt="Ghost" width="120" align="right" />

**MCP memory server for Claude Code, Cursor, and any MCP client. Pure Go. Single binary. No external services required.**

Your agent's memory, on your disk — no cloud, no accounts, no subscription. One SQLite file you own.

[![CI](https://github.com/wcatz/ghost/actions/workflows/ci.yml/badge.svg)](https://github.com/wcatz/ghost/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/wcatz/ghost)](https://github.com/wcatz/ghost/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/wcatz/ghost)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

<!-- TODO: asciinema demo — `ghost mcp init` + a session-start context injection -->

---

**Ghost beats every published competitor on LongMemEval-S** (500-question blended, retrieve → generate → judge):

| System | Score | Generator | Source |
|--------|-------|-----------|--------|
| **Ghost (hybrid)** | **96.2%** | DeepSeek V4 Pro | This repo |
| Mem0 | 94.4% | Not specified | [mem0.ai/research](https://mem0.ai/research) — "managed platform, proprietary optimizations not in OSS SDK" |
| Hindsight | 91.4% | Gemini-3 Pro | [arxiv 2512.12818](https://arxiv.org/abs/2512.12818) — independently validated by Virginia Tech + Washington Post |
| Supermemory | 85.2% | Gemini-3 | [supermemory.ai/research](https://supermemory.ai/research/longmembench/) — self-reported |

**Read carefully:** these numbers are **not directly comparable** across rows — each uses a different generator and judge. Within the same generator+judge pair, differences are meaningful; across pairs, they're directional only. Full methodology and retrieval-only benchmarks further down in [Benchmarks](#benchmarks).

---

## Quick start

Two commands. No accounts, no keys, no docker-compose, no vector database.

```bash
go install github.com/wcatz/ghost/cmd/ghost@latest
ghost mcp init
```

`ghost mcp init` registers Ghost as an MCP server, installs the session-start hook, **migrates your existing Claude Code memories** — projects Ghost already knows are imported at init, the rest auto-import on their first `ghost_project_context` call (read-only, nothing is lost) — and disables the built-in file memory so the two don't fight. It's idempotent and non-destructive — safe to re-run anytime, and `--dry-run` previews every change.

Then start a session. Ghost injects your project's context automatically and starts remembering.

**Using opencode (with Ollama)?** Skip the Claude Code init entirely — register Ghost as an MCP server:

```bash
ghost mcp init --client opencode
# optional but recommended — enables hybrid vector search:
ollama pull nomic-embed-text:v1.5
```

This writes the `ghost` entry into `~/.config/opencode/opencode.json` (or merges it into an existing `opencode.jsonc`). Restart opencode — Ghost's `ghost_*` tools and context injection go live automatically. Verify with `ghost mcp status --client opencode`.

No Go toolchain? Grab a prebuilt binary from [Releases](https://github.com/wcatz/ghost/releases/latest) — linux, macOS, and Windows, amd64 and arm64, with `checksums.txt`. Building from source needs Go 1.26+ (older toolchains fetch it automatically via `GOTOOLCHAIN=auto`).

**On Windows?** Skip the manual download/unzip/PATH steps with the one-command installer:

```powershell
irm https://github.com/wcatz/ghost/releases/latest/download/install.ps1 | iex
```

This downloads the latest release, verifies its checksum, installs
`ghost.exe` to `%LOCALAPPDATA%\ghost\bin`, and adds that directory to your
user PATH. Open a new terminal afterward, then run `ghost mcp init`.

Re-running the command upgrades an existing install in place.

**Using Cursor, Goose, or another MCP client?** Ghost speaks standard MCP over stdio — point any client at the binary:

```json
{ "mcpServers": { "ghost": { "type": "stdio", "command": "ghost", "args": ["mcp"] } } }
```

**Using opencode?** The entry goes in `~/.config/opencode/opencode.json` (or merges into an existing `opencode.jsonc`):

```json
{ "mcp": { "ghost": { "type": "local", "command": ["ghost", "mcp"], "enabled": true } } }
```

**Docker** (multi-arch, amd64 + arm64):

```bash
docker run -i -e XDG_DATA_HOME=/data -v ghost-data:/data ghcr.io/wcatz/ghost:latest
```

`-i` matters — MCP speaks over stdio, and `XDG_DATA_HOME=/data` is what makes the volume actually hold `ghost.db`. For consolidation, run `reflect` against the same volume (the MCP server itself never uses the API key):

```bash
docker run -i -e XDG_DATA_HOME=/data -v ghost-data:/data \
  -e ANTHROPIC_API_KEY=sk-ant-... ghcr.io/wcatz/ghost:latest reflect myproject --apply
```

## Why not just use built-in memory?

ChatGPT, Claude, Gemini, and Copilot all ship native memory now — but each one is walled off inside its own product. Nothing you teach ChatGPT carries over to Claude Code, and nothing Claude Code learns carries over to Cursor or Goose. Ghost's bet isn't "better than any single one of those" — it's *one* memory, across every MCP client, that lives on your own disk: a local SQLite file you can query, back up, and delete, instead of a separate silo per product. See [Why Ghost?](#why-ghost) below for the specific comparison against Claude Code's built-in memory.

## Why Ghost?

Coding agents forget everything between sessions. You re-explain your architecture, your conventions, and that one gotcha with the staging database — every single day.

Claude Code's built-in memory is a markdown file with a limited load window ([~200 lines](https://code.claude.com/docs/en/memory)). No search, no categories, no dedup, and memory is siloed per repository. Ghost replaces it with a real memory system:

| | Claude Code built-in | Ghost |
|---|---|---|
| Storage | Flat `.md` files, limited load window | SQLite + FTS5, unlimited |
| Search | None (linear load) | Full-text + optional local vector search |
| Categorization | None | 8 categories with importance scores |
| Dedup | None (appends forever) | FTS-based upsert — merges on save |
| Consolidation | None | Haiku LLM or local Jaccard tier |
| Time decay | None (stale facts persist equally) | Category-aware: conventions never decay, gotchas fade |
| Cross-project | None (siloed per repository) | `ghost_search_all` + `_global` project |
| Memory graph | None | Auto-linked related memories, graph view in Obsidian |
| Clients | Claude Code only | Any MCP client |

Switching migrates your existing Claude Code memories into Ghost — at init or on first contact. Nothing is lost.

## How it stacks up

Ghost's bet: a memory system should be *smaller* than the thing it remembers. The alternatives make you choose between cloud memory services (your codebase's context on someone else's server, metered per request) and self-hosted stacks (Postgres plus a vector DB before you've saved a single memory).

As far as we know, Ghost is the only memory system that packs local hybrid vector + full-text search, on-demand consolidation, time-decay scoring, memory lifecycle management, and a memory graph into a single zero-infrastructure binary. The field as of August 2026 — corrections welcome, [open an issue](https://github.com/wcatz/ghost/issues):

| | What you install | Vector search | Consolidation | Time decay | Memory lifecycle | Any MCP client |
|---|---|---|---|---|---|---|
| **Ghost** | one static Go binary | local (Ollama, optional) | yes | yes | resolve + supersede + demote + link | yes |
| Mem0 (self-hosted) | FastAPI + Postgres + Qdrant/Neo4j | server-side | yes | no | extraction only | via OpenMemory (Docker) |
| Zep (self-hosted) | Graphiti + Neo4j/FalkorDB | server-side | no | temporal graph | temporal fact chains | yes (MCP) |
| Supermemory | self-hosted binary or cloud | hybrid | yes | temporal | dual-profile | yes (MCP) |
| Claude Code | built-in to CLI | file-based index | Dreams (managed) | no | none | Claude Code only |
| Engram | one Go binary | no (FTS only) | no | no | no | yes |

Mem0 and Zep are excellent products, but self-hosting them means running a service stack (Postgres, Neo4j, Qdrant). Supermemory offers a self-hosted binary or cloud option. Ghost ships as a single binary with zero infrastructure. If all you want is full-text search in a single binary, Engram is a fine, simpler choice.

## The questions you should be asking

### Where does my data go?

One SQLite file under `~/.local/share/ghost` (or `$XDG_DATA_HOME/ghost`) — this path is the same on every OS, including Windows (i.e. `%USERPROFILE%\.local\share\ghost`, not `%AppData%`); only the config file follows the OS-native convention (see Configuration below). Ghost makes no network calls in normal operation, with three exceptions you control: **localhost** Ollama for embeddings (optional), the Claude API *only if* you run `ghost reflect` with the Haiku tier (needs `ANTHROPIC_API_KEY`; the SQLite tier is fully offline), and the GitHub API *only if* you run `ghost upgrade`. That's the complete list.

### What exactly gets injected into my agent's context?

A bounded digest, and you can inspect it yourself. The session-start hook emits: project name, top memories, learned context, open tasks, and active decisions. Global memories (the `_global` project) are injected even when the cwd matches no known project. See precisely what your agent sees:

```bash
echo '{"cwd":"'"$PWD"'"}' | ghost hook session-start
```

No mystery blob in your system prompt. Save-time dedup keeps the digest from bloating, and time-decay scoring weights `ghost_project_context` and resource reads toward what's still true. Subagent sessions get nothing (they inherit context in-band from the parent); `resume` skips injection; `compact` emits a one-line pointer instead of re-dumping. Full details in [docs/architecture.md](docs/architecture.md).

### What's the exit story?

Your memories are a plain SQLite database in one file. Open it with `sqlite3`, query it with any tool, back it up with `cp`. No proprietary format, no export request form. The schema is a readable Go string constant in [`internal/memory/schema.go`](internal/memory/schema.go). If you stop using Ghost tomorrow, your memories are sitting there in a format that will outlive all of us.

Switching *in* is just as easy: `ghost mcp init` imports Claude Code memories, and even without running init, Ghost auto-imports (read-only) on the first `ghost_project_context` call for a project with zero memories.

### Where's the off switch?

- **Per client:** remove the `ghost` entry from your MCP config. Ghost only runs when your client spawns it over stdio — there is no daemon.
- **Embeddings:** set `embedding.enabled: false` in your config file (see [Configuration](#configuration) for the OS-specific path — `%AppData%\ghost\config.yaml` on Windows, `~/.config/ghost/config.yaml` elsewhere).
- **Consolidation:** never runs unless you invoke `ghost reflect` — and that's a dry run unless you pass `--apply`.
- **Everything:** delete `$XDG_DATA_HOME/ghost`, or `~/.local/share/ghost` when `XDG_DATA_HOME` is unset. There is nothing else.

### What does it cost to run?

$0/month. No metered API in the hot path. The only paid call in the entire codebase is the optional Haiku consolidation tier — and it has a free offline fallback.

## How it works

Ghost is a memory pipeline: **Save → Embed → Link → Search → Consolidate → Decay**.

### 8 memory categories

`architecture` · `decision` · `pattern` · `convention` · `gotcha` · `dependency` · `preference` · `fact` — enforced by a SQLite CHECK constraint, not vibes. Saving a near-duplicate strengthens the existing memory instead of piling up copies (FTS-overlap dedup, same category).

### Hybrid search

Full-text (FTS5) and vector results are fused with Reciprocal Rank Fusion (k=60), weighted 70% vector / 30% FTS. A background worker links similar memories (cosine ≥ 0.70) into a graph, which powers the Obsidian mirror's graph view and future link-aware features; links self-heal after consolidation rewrites memories. An experimental graph-expansion ranking bonus exists but ships disabled — our own benchmark sweep (`ghost bench --sweep`) showed it demoting exact matches, so it stays off until a redesign beats that measurement ([methodology](docs/benchmarks.md)).

Vectors come from a local Ollama instance (`nomic-embed-text:v1.5`, 768 dims) if one is running. **No Ollama? No error, no setup step** — Ghost is fully functional with FTS5-only search and quietly upgrades to hybrid the moment Ollama appears:

```bash
ollama pull nomic-embed-text:v1.5
```

### Time-decay scoring

Facts about your stack shouldn't expire. Last month's debugging detour should. The score multiplier is `max(floor, 1 / (1 + age_days / scale))`:

| Category | Decay | Floor |
|---|---|---|
| `preference`, `convention`, `fact` | never | — |
| `architecture`, `pattern` | 45-day scale | 0.3 |
| `decision`, `gotcha`, `dependency` | 30-day scale | 0.15 |

Pinned memories are fully exempt from decay — they score at raw importance regardless of age or category.

### Consolidation you can undo

`ghost reflect` merges duplicates, prunes noise, and promotes cross-project knowledge to global scope. Tiered: Claude Haiku first (needs an API key; cost scales with memory count — roughly $0.001 for a typical project, an estimate from Haiku 4.5's per-token pricing, not a measurement), falling back to a fully offline SQLite tier (Jaccard ≥ 0.5, same-category merges). Because an LLM rewriting your memory store is scary, the guardrails are layered:

- **Dry run by default** — see the diff before `--apply`
- **Auto-snapshot before every replace**, keeping the 3 most recent per project; `ghost reflect --restore` is the undo button
- **Empty-set refusal** — the store layer will not replace your memories with nothing, ever
- **Quality gate** — in auto mode, output shrinking below 30% of input is rejected and the next tier is tried (when input ≥ 6 memories)
- **Manually saved memories are always preserved**

### Tasks, decisions, and global memory

Beyond memories: tasks (`pending`/`active`/`done`/`blocked`), decision records with rationale and alternatives (`active`/`superseded`/`revisit`), and a `_global` project whose memories are included in every project's context. Projects resolve by longest path-prefix match with a basename fallback, so worktrees and moved checkouts still find their memory.

### Memory lifecycle

Saving a memory is the beginning, not the end. Ghost tracks what happened *after* you saved — which facts got replaced, which findings turned out to be intermediate, which memories are near-duplicates of each other — and uses that to keep search results honest:

- **Resolve** (`ghost resolve`) — marks resolved-evidence memories (changelog entries, cost estimates, closed experiment notes) with `resolved_at`, dropping them from ranked injection while keeping them searchable. Uses MCP Sampling for zero-credit classification in live sessions; the CLI path uses an API key.
- **Supersede** (`ghost supersede`) — creates directed `supersedes` links between memories (newer replaces older). A single LLM call classifies each candidate pair as SUPERSEDES / CAUSES / NEITHER. Re-runnable and self-healing after consolidation rewrites memories.
- **Demote** — when a superseded memory and its replacement both appear in search results, the older one is sunk below every present superseder. Targeted demotion on genuine replacement pairs only (a blanket age-only recency prior destroys old-but-correct retrieval — measured, published, ship-off). Flips staleness fresh-wins from 0.083 to 1.000 while leaving unrelated retrieval untouched (see [staleness suite](docs/benchmarks.md#phase-3--staleness-suite-the-flagship)).
- **Link** — a background worker auto-links related memories (cosine ≥ 0.70) into a graph. Links power the Obsidian mirror's graph view, supersedes ranking, and near-duplicate demotion at injection time.

Most memory systems extract a fact and forget about it. Ghost's lifecycle is the pipeline that keeps facts true over time.

### Obsidian vault mirror

Your memories are yours to browse. `ghost obsidian export` mirrors memories, decisions, and tasks into plain Markdown notes — one folder per project — that Obsidian opens as a vault, with memory links rendered as wikilinks so the graph view maps your knowledge. `ghost obsidian sync` keeps the mirror fresh by polling for database changes; `--project` scopes the mirror to a single project (plus Global). The mirror is strictly one-way: it reads the database read-only (safe alongside a live MCP server), and it only ever prunes stale notes inside a directory carrying the `.ghost-vault` marker — backing off entirely when a listing might be incomplete, so stale extras beat silent deletions.

```bash
ghost obsidian export --out ~/Documents/GhostVault   # one-shot mirror
ghost obsidian sync --interval 30s                   # keep it fresh
```

Set `obsidian.auto_sync: true` in config to have the session-start hook spawn `ghost obsidian sync` in the background automatically instead of running it by hand — off by default, so nobody gets a vault directory or a background process without asking for it.

#### Using the vault

Every note carries YAML frontmatter (`category`, `importance`, `pinned`, `project`, `tags`, `created`, `updated`, `source`) and an `aliases` entry — a short single-line preview of the content — so the graph view and Quick Switcher show a readable label instead of the id-suffixed filename. That structure turns the mirror into a queryable knowledge base with no extra tooling:

- **Graph view** already maps your memories: each `## Related` wikilink is an edge, so the auto-linked graph renders natively. Colour nodes by folder (project) or tag to see clusters.
- **[Dataview](https://github.com/blacksmithgu/obsidian-dataview) queries** over the frontmatter give live tables without touching Ghost — e.g. pinned gotchas, stale memories, or decisions by status:

  ````markdown
  ```dataview
  TABLE importance, updated FROM "Ghost"
  WHERE type = "memory" AND pinned = true AND category = "gotcha"
  SORT importance DESC
  ```
  ````

- **Backlinks** show what links into a memory; the **tag pane** browses `tags:`; **local graph** and **Canvas** explore or arrange nodes spatially.

Because the mirror is one-way, edits inside the vault are informational only and are **not** synced back to Ghost — and if `sync` is running it will overwrite hand-edits on the next database change. Change memories through the MCP tools (or `ghost` CLI), not by editing notes.

## MCP surface

19 tools, 4 resources:

| Group | Tools |
|---|---|
| Memory | `ghost_memory_save` `ghost_memory_search` `ghost_search_all` `ghost_memories_list` `ghost_memory_update` `ghost_memory_delete` `ghost_memory_pin` `ghost_memory_promote` `ghost_save_global` `ghost_resolve` |
| Context | `ghost_project_context` `ghost_list_projects` `ghost_health` |
| Tasks | `ghost_task_create` `ghost_task_list` `ghost_task_update` `ghost_task_complete` |
| Decisions | `ghost_decision_record` `ghost_decisions_list` |

`ghost_resolve` scans a project's memories for resolved-evidence notes (intermediate findings, changelog entries, superseded experiments) using the calling session's own model via MCP sampling — no Anthropic API credits spent. Args: `project` (required), `apply` (default false: dry-run preview only; pass `true` to stamp `resolved_at` on confirmed memories).

Resources: project context, global memories, project decisions, project tasks — pin them in clients that support it to survive context compaction.

The server ships with embedded instructions that teach the agent when to save, which categories to use, and how to leverage cross-project search — it works proactively without configuration. Full architecture notes in [docs/architecture.md](docs/architecture.md).

## CLI

```text
ghost mcp                    # Run MCP server on stdio (used by your MCP client)
ghost mcp init [--client claude|opencode] [--dry-run]   # Configure MCP client integration (default: Claude Code)
ghost mcp status [--client claude|opencode]             # Deep health checks (incl. Ollama reachability, model presence)
ghost hook session-start     # SessionStart hook — prints exactly what gets injected
ghost hook stop              # Stop hook — blocks stop once if a tool-using session saved nothing
ghost reflect <project>      # Memory consolidation (dry-run by default; --apply, --restore, --tier)
ghost resolve <project>      # De-weight resolved-evidence memories from injection (dry-run by default; --apply)
ghost supersede <project>    # Link superseded memories (dry-run by default; --apply, --threshold)
ghost bench [--sweep]        # Retrieval-quality benchmark on the built-in dataset
ghost obsidian export        # Mirror memories to an Obsidian vault (one-way; --out, --project)
ghost obsidian sync          # Keep the vault mirror fresh (--interval; polls for DB changes)
ghost upgrade                # Self-update from GitHub Releases (linux/macOS; Windows: re-download)
ghost version                # Print version
```

`ghost mcp init` and `ghost mcp status` default to Claude Code; `--client opencode` targets opencode instead, writing the `ghost` entry to `~/.config/opencode/opencode.json` (or merging into an existing `opencode.jsonc`).

When `reflection.auto_resolve` is enabled in config (default off), the stop hook also spawns `ghost resolve <project> --apply` as a detached background process after each session, so resolved-evidence memories get marked automatically without waiting for a manual run. This never blocks the hook itself — the spawn is fire-and-forget, logged to `resolve.log` in the ghost data directory. If the Anthropic API is out of credit at spawn time, the spawned process fails and logs the failure; it does not degrade to a lower-quality answer.

## Configuration

Ghost works with zero config. When you want to change something, layers are (later wins):

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. `~/.config/ghost/config.yaml` (honors `$XDG_CONFIG_HOME` when set; on Windows, absent an `XDG_CONFIG_HOME` override, this resolves to `%AppData%\ghost\config.yaml`)
4. `GHOST_*` environment variables, plus `ANTHROPIC_API_KEY` for the Haiku reflection tier

```yaml
embedding:
  enabled: true                          # default; degrades gracefully without Ollama
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"
linking:
  enabled: true                          # on by default when embedding is enabled
  threshold: 0.70                        # min cosine similarity to auto-link memories
```

Note: env-var names map underscores to config dots, so keys that themselves contain underscores (e.g. `embedding.ollama_url`) must be set in a config file, not via env.

## Benchmarks

Every Ghost number below is reproducible with the in-repo harnesses, and shipped with per-question logs — the competitor figures in the comparison table further down are externally sourced and not reproducible from this repo. Retrieval-only metrics are deterministic given the embedding cache; end-to-end scores are recorded runs (model-pinned, single-run — rerun variance is possible but small at temperature 0). Full methodology in [docs/benchmarks.md](docs/benchmarks.md).

**LongMemEval-S** ([the consensus long-term-memory benchmark](https://arxiv.org/abs/2410.10813); cleaned variant, session-level retrieval against the official evidence labels, all 470 answerable questions, no LLM judge):

```text
condition   R@1     R@5     R@10    MRR@10  NDCG@10   wall clock (470 questions)
fts-only    0.429   0.751   0.832   0.758   0.738     44s
vector      0.558   0.926   0.968   0.911   0.909     ~1m (warm embedding cache)
hybrid      0.532   0.930   0.973   0.901   0.903     one-time cold embedding ~12h on ARM64 CPU
```

**Hybrid session Recall@5 93.0%, Recall@10 97.3%** — in the band of the best-reported hybrid retrieval results on this benchmark, using nothing but the single Ghost binary and local Ollama embeddings. Harness + per-question logs: [`bench/longmemeval/`](bench/longmemeval/). Honest nuances: retrieval-only numbers are not comparable to end-to-end answer-accuracy percentages (those depend mostly on the generator model); and on this chat-style data vector-only ties hybrid — the keyword leg earns its keep on exact identifiers (ports, versions, hostnames), which is what the next table shows. These are full-run wall-clock totals, not per-query latency — Ghost's harness doesn't instrument per-query p50/p95 yet, so unlike some competitor benchmarks, there's no per-query latency figure to publish here honestly.

**`ghost bench`** — the in-repo dev-facts dataset, runs in seconds, regression-guarded in CI:

```text
$ ghost bench
condition          R@1     R@5    R@10   MRR@10  NDCG@10
fts-only         0.786   0.964   1.000    0.964    0.965
vector-only      0.786   0.929   0.964    0.952    0.946
hybrid           0.857   0.964   1.000    1.000    0.989

14 graded queries, 22 memories. Retrieval-only, no LLM judge.
```

- **Hybrid fusion beats both single legs here** (NDCG@10 0.989 vs 0.965 full-text, 0.946 vector) — CI asserts that relationship on every PR. Across both benchmarks, fusion is the robustness play: vectors win conversational recall, keywords win exact identifiers.
- **We ran the ablations, found our own regression, and removed it.** An additive graph-expansion ranking bonus hurt retrieval — a public LongMemEval-S kill experiment showed its recoveries were a strict subset of a deeper vector-k's, with no headroom at production depth — so it was removed entirely rather than kept disabled. The link graph itself is retained for the Obsidian mirror and `supersedes` ranking. `ghost bench --sweep` grid-searches the fusion parameters if you want to check our tuning.
- **The staleness suite** ("prod ran Postgres 14, we migrated to 16" — does search rank the fresh fact first?) runs report-only in CI. A *recency-trap* fixture (older memory is the correct answer) proved a blanket age-only prior can't be the default: it's a cliff, every weight that fixes staleness destroys old-but-still-correct retrieval. The fix that survives it is *category-aware*: search now applies the time-decay factor (pinned / preference / convention / fact never decay; pattern/architecture τ=45; decision/gotcha/dependency τ=30) to reorder the result window, so the staleness suite's updated-deployment facts (`dependency` category) flip fresh-wins **0.083 → 1.000** while the trap's `fact` memories stay flat at **0.929** — the free lunch the blanket prior couldn't achieve. Decay is ordering-only (it never drops a relevant memory). Both halves ship: `ghost supersede` creates `supersedes` links (cosine proposes, Haiku confirms — 8/8 on a labeled set), and `DefaultSearchParams` ships `DecayEnabled: true` + `SupersedeDemote: true`, so production search (`ghost_memory_search`, `ghost_search_all`) is time-aware and consumes `supersedes` links by default. Link creation stays opt-in: the demote is a hard no-op until you run `ghost supersede --apply`. Publishing the negative result, the reason, *and* the fix that survives it is the point.

**End-to-end LongMemEval-S** (retrieve → generate → judge — DeepSeek v4 Pro as both generator and judge, **500 questions** including 30 abstention, `topk_context=5`):

```text
condition   blended(500)  non-abstention(470)  abstention(30)
hybrid      96.2%         96.8%                86.7%
fts-only    83.4%         83.6%                80.0%
```

Per-category, hybrid vs FTS-only (the delta shows where vector search earns its keep):

| Question type | Hybrid | FTS-only | Delta |
|---|---|---|---|
| single-session-user (64) | 100.0% | 98.4% | +1.6pp |
| single-session-assistant (56) | 98.2% | 67.9% | **+30.3pp** |
| single-session-preference (30) | 96.7% | 80.0% | +16.7pp |
| multi-session (121) | 92.6% | 72.7% | **+19.9pp** |
| temporal-reasoning (127) | 97.6% | 88.2% | +9.4pp |
| knowledge-update (72) | 98.6% | 94.4% | +4.2pp |

The biggest lifts land on vocabulary-mismatch classes — `single-session-assistant` (+30pp) and `multi-session` (+20pp) — exactly where embeddings fix what FTS misses. Not leaderboard-comparable (DeepSeek v4 Pro, not GPT-4o), but the retrieval → answer pipeline is identical to the official harness.

**Competitor comparison** (500-question blended, all systems):

| System | Score | Generator | Source |
|--------|-------|-----------|--------|
| **Ghost (hybrid)** | **96.2%** | DeepSeek V4 Pro | This repo |
| Mem0 | 94.4% | Not specified | [mem0.ai/research](https://mem0.ai/research) — "managed platform, proprietary optimizations not in OSS SDK" |
| Hindsight | 91.4% | Gemini-3 Pro | [arxiv 2512.12818](https://arxiv.org/abs/2512.12818) — independently validated by Virginia Tech + Washington Post |
| Supermemory | 85.2% | Gemini-3 | [supermemory.ai/research](https://supermemory.ai/research/longmembench/) — self-reported |

**Read carefully:** These numbers are **not directly comparable** across rows — each uses a different generator and judge. Within the same generator+judge pair, differences are meaningful; across pairs, they're directional only.

Reproduce: see [`bench/longmemeval/phase4/`](bench/longmemeval/phase4/). Full methodology, the `ghost bench` parameter sweep, and the staleness-suite deep dive: [docs/benchmarks.md](docs/benchmarks.md).

Skipped deliberately: LOCOMO (publicly audited answer-key and judge problems) and DMR.

## Works well with Superpowers

[Superpowers](https://github.com/obra/superpowers) structures *how* agent work gets done (brainstorm-first planning, TDD, subagent execution); Ghost remembers *what was learned*. A workflow pattern that works well: load `ghost_project_context` before planning, `ghost_memory_search` before touching a component, `ghost_decision_record` when an architectural choice is made, `ghost_memory_save` when a phase completes.

## Project status

Ghost is a solo project, built because I wanted my own agents to stop forgetting, and used daily on real infrastructure work. What you can verify rather than trust:

- Pure Go, `CGO_ENABLED=0`, 7 direct dependencies (SQLite via `modernc.org/sqlite` — no C toolchain anywhere); a static binary around 12.5 MB
- ~1:1 test-to-code ratio; CI runs `go vet`, `golangci-lint`, and race-enabled tests on every PR and push to main
- Releases for 6 OS/arch targets built by GoReleaser with checksums, plus a multi-arch Docker image

Small enough to read the whole thing in an afternoon. That's on purpose — and because the exit story is one SQLite file, the cost of trying Ghost and walking away is a `go install` and an `rm`.

## Contributing

Issues and PRs welcome. `go test ./...` and `go vet ./...` must pass; feature branches only.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
