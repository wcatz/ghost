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

## Quick start

Two commands. No accounts, no keys, no docker-compose, no vector database.

```bash
go install github.com/wcatz/ghost/cmd/ghost@latest
ghost mcp init
```

`ghost mcp init` registers Ghost as an MCP server, installs the session-start hook, **migrates your existing Claude Code memories** — projects Ghost already knows are imported at init, the rest auto-import on their first `ghost_project_context` call (read-only, nothing is lost) — and disables the built-in file memory so the two don't fight. It's idempotent and non-destructive — safe to re-run anytime, and `--dry-run` previews every change.

Then start a session. Ghost injects your project's context automatically and starts remembering.

No Go toolchain? Grab a prebuilt binary from [Releases](https://github.com/wcatz/ghost/releases/latest) — linux, macOS, and Windows, amd64 and arm64, with `checksums.txt`. Building from source needs Go 1.26+ (older toolchains fetch it automatically via `GOTOOLCHAIN=auto`).

**Using Cursor, Goose, or another MCP client?** Ghost speaks standard MCP over stdio — point any client at the binary:

```json
{ "mcpServers": { "ghost": { "type": "stdio", "command": "ghost", "args": ["mcp"] } } }
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

As far as we know, Ghost is the only memory system that packs local hybrid vector + full-text search, automatic consolidation, time-decay scoring, and a memory graph into a single zero-infrastructure binary. The field as of July 2026 — corrections welcome, [open an issue](https://github.com/wcatz/ghost/issues):

| | What you install | Vector search | Consolidation | Time decay | Memory graph | Any MCP client |
|---|---|---|---|---|---|---|
| **Ghost** | one static Go binary | local (Ollama, optional) | yes | yes | yes | yes |
| Engram | one Go binary | no (FTS only) | no | no | no | yes |
| claude-mem | npm package | yes | yes | no | no | Claude Code only |
| Mem0 (self-hosted) | FastAPI + Postgres + Qdrant/Neo4j | server-side | yes | no | graph variant | via OpenMemory (Docker) |
| basic-memory | Python (AGPL) | yes | manual capture | no | wikilinks | yes |

Mem0, Zep, and supermemory are excellent hosted products — but self-hosting them means running a service stack. If all you want is full-text search in a single binary, Engram is a fine, simpler choice.

## The questions you should be asking

### Where does my data go?

One SQLite file under `~/.local/share/ghost` (or `$XDG_DATA_HOME/ghost`). Ghost makes no network calls in normal operation, with three exceptions you control: **localhost** Ollama for embeddings (optional), the Claude API *only if* you run `ghost reflect` with the Haiku tier (needs `ANTHROPIC_API_KEY`; the SQLite tier is fully offline), and the GitHub API *only if* you run `ghost upgrade`. That's the complete list.

### What exactly gets injected into my agent's context?

A bounded digest, and you can inspect it yourself. The session-start hook emits: project name, top memories, learned context, open tasks, and active decisions. Global memories (the `_global` project) are injected even when the cwd matches no known project. See precisely what your agent sees:

```bash
echo '{"cwd":"'"$PWD"'"}' | ghost hook session-start
```

No mystery blob in your system prompt. Save-time dedup keeps the digest from bloating, and time-decay scoring weights `ghost_project_context` and resource reads toward what's still true.

### What's the exit story?

Your memories are a plain SQLite database in one file. Open it with `sqlite3`, query it with any tool, back it up with `cp`. No proprietary format, no export request form. The schema is a readable Go string constant in [`internal/memory/schema.go`](internal/memory/schema.go). If you stop using Ghost tomorrow, your memories are sitting there in a format that will outlive all of us.

Switching *in* is just as easy: `ghost mcp init` imports Claude Code memories, and even without running init, Ghost auto-imports (read-only) on the first `ghost_project_context` call for a project with zero memories.

### Where's the off switch?

- **Per client:** remove the `ghost` entry from your MCP config. Ghost only runs when your client spawns it over stdio — there is no daemon.
- **Embeddings:** set `embedding.enabled: false` in `~/.config/ghost/config.yaml`.
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

Pinned memories get a 1.5× boost on top.

### Consolidation you can undo

`ghost reflect` merges duplicates, prunes noise, and promotes cross-project knowledge to global scope. Tiered: Claude Haiku first (needs an API key; cost scales with memory count — roughly $0.001 for a typical project, an estimate from Haiku 4.5's per-token pricing, not a measurement), falling back to a fully offline SQLite tier (Jaccard ≥ 0.5, same-category merges). Because an LLM rewriting your memory store is scary, the guardrails are layered:

- **Dry run by default** — see the diff before `--apply`
- **Auto-snapshot before every replace**, keeping the 3 most recent per project; `ghost reflect --restore` is the undo button
- **Empty-set refusal** — the store layer will not replace your memories with nothing, ever
- **Quality gate** — in auto mode, output shrinking below 30% of input is rejected and the next tier is tried (when input ≥ 6 memories)
- **Manually saved memories are always preserved**

### Tasks, decisions, and global memory

Beyond memories: tasks (`pending`/`active`/`done`/`blocked`), decision records with rationale and alternatives (`active`/`superseded`/`revisit`), and a `_global` project whose memories are included in every project's context. Projects resolve by longest path-prefix match with a basename fallback, so worktrees and moved checkouts still find their memory.

### Obsidian vault mirror

Your memories are yours to browse. `ghost obsidian export` mirrors memories, decisions, and tasks into plain Markdown notes — one folder per project — that Obsidian opens as a vault, with memory links rendered as wikilinks so the graph view maps your knowledge. `ghost obsidian sync` keeps the mirror fresh by polling for database changes; `--project` scopes the mirror to a single project (plus Global). The mirror is strictly one-way: it reads the database read-only (safe alongside a live MCP server), and it only ever prunes stale notes inside a directory carrying the `.ghost-vault` marker — backing off entirely when a listing might be incomplete, so stale extras beat silent deletions.

```bash
ghost obsidian export --out ~/Documents/GhostVault   # one-shot mirror
ghost obsidian sync --interval 30s                   # keep it fresh
```

## MCP surface

16 tools, 4 resources:

| Group | Tools |
|---|---|
| Memory | `ghost_memory_save` `ghost_memory_search` `ghost_search_all` `ghost_memories_list` `ghost_memory_delete` `ghost_memory_pin` `ghost_save_global` |
| Context | `ghost_project_context` `ghost_list_projects` `ghost_health` |
| Tasks | `ghost_task_create` `ghost_task_list` `ghost_task_update` `ghost_task_complete` |
| Decisions | `ghost_decision_record` `ghost_decisions_list` |

Resources: project context, global memories, project decisions, project tasks — pin them in clients that support it to survive context compaction.

The server ships with embedded instructions that teach the agent when to save, which categories to use, and how to leverage cross-project search — it works proactively without configuration. Full architecture notes in [docs/architecture.md](docs/architecture.md).

## CLI

```text
ghost mcp                    # Run MCP server on stdio (used by your MCP client)
ghost mcp init [--dry-run]   # Configure Claude Code integration
ghost mcp status             # Deep health checks (incl. Ollama reachability, model presence)
ghost hook session-start     # SessionStart hook — prints exactly what gets injected
ghost reflect <project>      # Memory consolidation (dry-run by default; --apply, --restore, --tier)
ghost obsidian export        # Mirror memories to an Obsidian vault (one-way; --out, --project)
ghost obsidian sync          # Keep the vault mirror fresh (--interval; polls for DB changes)
ghost upgrade                # Self-update from GitHub Releases (linux/macOS; Windows: re-download)
ghost version                # Print version
```

## Configuration

Ghost works with zero config. When you want to change something, layers are (later wins):

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. `~/.config/ghost/config.yaml`
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

Ghost ships a retrieval-quality benchmark you can run yourself in seconds — `ghost bench` — and every number below is deterministic, judge-free, and regression-guarded in CI. Full methodology in [docs/benchmarks.md](docs/benchmarks.md).

```text
$ ghost bench
condition          R@1     R@5    R@10   MRR@10  NDCG@10
fts-only         0.786   0.964   1.000    0.964    0.965
vector-only      0.786   0.929   0.964    0.952    0.946
hybrid           0.857   0.964   1.000    1.000    0.989
hybrid+graph     0.500   0.964   1.000    0.780    0.824

14 graded queries, 22 memories. Retrieval-only, no LLM judge.
```

What this shows, honestly stated:

- **Hybrid fusion earns its keep** — it beats both single legs (NDCG@10 0.989 vs 0.965 full-text, 0.946 vector), and CI asserts that relationship on every PR.
- **We ran the ablations, found our own regression, and fixed it.** The graph-expansion ranking bonus *hurt* retrieval (`hybrid+graph`, NDCG 0.824), so it now ships disabled — the table keeps measuring it so a redesign has a bar to clear. `ghost bench --sweep` grid-searches the fusion parameters if you want to check our tuning.
- **Scope caveat:** this is a self-authored 22-memory / 14-query graded dataset exercising Ghost's real search code paths — a regression guard and tuning instrument, not a leaderboard. It is not comparable to other systems' LOCOMO/LongMemEval scores.

For external comparability (in progress, in order): **LongMemEval-S retrieval metrics** — session-level Recall@k/NDCG@k against the dataset's official evidence labels, no LLM judge; a **deterministic staleness suite** ("prod ran Postgres 14, we migrated to 16" — does search rank the fresh fact first?); then **end-to-end LongMemEval-S** with the official GPT-4o judge. Several popular memory benchmarks have known problems (LOCOMO's answer key and judge have been publicly audited as unreliable), so numbers land here only with the harness, fixed seeds, and per-question logs to re-run them yourself.

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
