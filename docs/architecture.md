# Ghost architecture

This document is for contributors and maintainers. For installation and everyday use, start with [`installation.md`](installation.md), [`usage.md`](usage.md), and [`cli.md`](cli.md).

## Design goals

Ghost is intentionally:

- **Single-writer and local-first:** one SQLite database, normally accessed by one process at a time.
- **Pull-based during normal MCP use:** the server exposes tools and resources; it does not inject an LLM call into ordinary memory reads.
- **CGO-free:** `modernc.org/sqlite` supplies SQLite and FTS5 without a C toolchain.
- **Host-aware:** lifecycle events are normalized into a small contract shared by Claude Code, opencode, Codex, and Goose.
- **Source-aware for maintenance:** reflect, resolve, and supersede route through the calling session's CLI harness rather than silently selecting another subscription.

## Runtime modes

The same binary provides several modes:

```text
ghost mcp                         MCP server over stdio
ghost mcp init                    Configure MCP clients
ghost mcp status                  Check client and store health
ghost hook <event> --source <host> Normalize a host lifecycle event
ghost reflect <project>           Consolidate memories
ghost resolve <project>           Mark resolved evidence
ghost supersede <project>         Classify replacement relationships
ghost lifecycle <project>         Run the detached maintenance phases
ghost project delete|merge        Manage project records
ghost obsidian export|sync        Mirror the store to Markdown
ghost context                     Render passive session context
ghost bench [--sweep]             Run the built-in benchmark
ghost upgrade                     Update a standalone binary
ghost version                     Print the version
```

The MCP server is the primary mode. It starts embedding and linking workers when enabled, but those workers do not make LLM calls.

## Package map

```text
cmd/ghost/                         CLI entrypoint and command dispatch

internal/ai/                        Source-aware CLI harness adapters
  provider.go                      Provider and TokenUsage contracts
  source_detect.go                 Environment/process source detection
  source_provider.go               Routes to the caller's harness
  cli_client.go                    Claude-compatible CLI adapter
  opencode_client.go               OpenCode V1/V2 adapter
  codex_client.go                  Codex adapter
  goose_client.go                  Goose adapter
  scratch.go                       Harness scratch/temp handling

internal/bench/                     Built-in retrieval benchmark and sweeps
internal/claudeimport/              One-time Claude Code memory import
internal/config/                    Layered YAML/environment configuration
internal/embedding/                 Optional Ollama embedding client/worker
internal/hostevent/                 Normalized host-event contract and scanners
internal/linking/                   Background related-memory linking worker
internal/mcpinit/                   Client installers, status checks, hooks
internal/mcpserver/                 MCP server, tools, resources, prompts
internal/memory/                    SQLite store, FTS5, vectors, links, schema
internal/obsidian/                  One-way Markdown vault exporter/sync
internal/procstat/                  Cross-platform process liveness/start time
internal/provider/                  MemoryStore and LLMProvider interfaces
internal/reflection/                Tiered memory consolidation
internal/resolve/                   Resolved-evidence classifier and cache
internal/scratch/                   Ghost-owned scratch root cleanup
internal/selfupdate/                Checksum-verified GitHub release updater
internal/supersede/                 Directed supersession relation classifier
```

The database schema is an embedded Go string constant in `internal/memory/schema.go`; it is the single source of truth for the store schema.

## Host integration

`internal/mcpinit` owns the host-specific setup paths:

- Claude Code: MCP registration, permissions, SessionStart/Stop hooks, file-memory migration, and redirects.
- opencode: one lifecycle TypeScript adapter that registers MCP and bridges idle events.
- Codex: `config.toml` registration plus `hooks.json` entries that require user trust.
- Goose: an Agent Plugins package with MCP and Open Plugins hooks.

All hook paths converge on `internal/hostevent`, which parses the versioned event envelope and dispatches normalized events. The `scratch` and `procstat` packages keep harness scratch and detached-process liveness handling separate from host adapters.

If a host reports an unknown source, `internal/ai` does not cascade to a default harness. The caller must provide a source or the operation fails with an actionable error. This prevents an opencode or Claude session from silently spending the wrong subscription. OpenCode children receive an explicit `opencode/big-pickle` model unless a per-phase pin or `GHOST_OPENCODE_MODEL` override is supplied; the child config is intentionally scrubbed, so the user's global OpenCode model is not inherited.

## Data flow

### Normal MCP session

```text
MCP client
  → stdio JSON-RPC
  → internal/mcpserver
  → provider.MemoryStore
  → SQLite / FTS5
```

The MCP server registers 20 tools, 4 resources, and 2 prompts. Core memory CRUD and search tools do not invoke an LLM; the maintenance-oriented `ghost_resolve` and lifecycle paths can invoke the selected CLI harness. Resources expose project context, decisions, tasks, and global memories for clients that support resource pinning.

### Session start

```text
host SessionStart
  → ghost hook session-start --source <host>
  → resolve project by longest path prefix/name
  → rank and bound the context digest
  → write the digest to the host
```

opencode uses `ghost context` because it cannot consume the hook's stdout injection directly. The OpenCode adapter injects that rendered block as instructions instead.

### Stop and maintenance

```text
host Stop
  → ghost hook stop --source <host>
  → save reminder / bounded host behavior
  → optional detached ghost lifecycle <project>
       → reflect
       → resolve
       → supersede
```

The lifecycle is opt-in. A phase failure is logged and does not prevent later phases from running. The reflect phase can use a source-matched CLI harness or an explicitly selected offline tier; the autonomous path requires a real harness when it is configured to rewrite memories.

## Persistence and search

`internal/memory` owns:

- Project and memory CRUD
- FTS5 indexing and query sanitization
- Optional vector storage and cosine similarity
- Reciprocal Rank Fusion for hybrid results
- Category-aware time-decay ordering
- Pinned and near-duplicate handling
- Directed memory links
- Snapshots, audit history, tasks, decisions, and usage data

The main schema tables are:

| Table | Purpose |
|---|---|
| `projects` | Project names, IDs, and paths |
| `memories` | Core memory content, category, importance, tags, and state |
| `memories_fts` | FTS5 virtual table |
| `memory_embeddings` | Float32 embedding vectors |
| `memory_links` | Related, supersedes, causes, and other graph edges |
| `tasks` | Cross-session work items |
| `decisions` | Decisions, rationale, alternatives, and status |
| `ghost_state` | Per-project learned context and interaction state |
| `memory_snapshots` | Reflection rollback snapshots |
| `token_usage` | Harness usage and cost records |
| `audit_log` | Destructive and consolidation operations |

### Retrieval

When embeddings are available, search combines:

- SQLite FTS5 for lexical and exact-identifier matches
- Cosine-similarity vector candidates for paraphrases
- Reciprocal Rank Fusion with the shipped 70% vector / 30% FTS weighting
- Category-aware decay applied to the surviving result window
- Targeted demotion when a present memory is superseded by another present memory

Without Ollama, the same API remains available with FTS5-only results. Search membership is not discarded solely because of age; decay changes ordering.

### Memory lifecycle

`reflect` replaces non-manual memories through a tiered consolidator. It snapshots before replacement, rejects empty results, preserves manual memories, and can restore the latest snapshot. `resolve` stamps resolved evidence so it leaves injection but remains searchable. `supersede` creates directed replacement links after source-matched classification.

## Configuration and filesystem layout

`internal/config` loads compiled defaults, `/etc/ghost/config.yaml`, the user config file, and `GHOST_*` environment variables. Commands apply supported flag overrides after loading. The data directory is resolved from `XDG_DATA_HOME` or the user's home directory and contains `ghost.db`.

See [`configuration.md`](configuration.md) for the user-facing contract and [`internal/config/config.example.yaml`](../internal/config/config.example.yaml) for the annotated template.

## Build and release

Ghost is built as a static binary with CGO disabled:

```bash
CGO_ENABLED=0 go build -o ghost ./cmd/ghost
```

GoReleaser produces Linux, macOS, and Windows binaries for amd64 and arm64, with checksums. The Docker build uses a Go Alpine builder and an Alpine runtime, also with `CGO_ENABLED=0`. CI runs tests, race tests, vetting, linting, vulnerability scanning, and workflow validation.

## Historical design records

The `docs/superpowers/` tree contains archived specifications, plans, and reports. It explains how the architecture reached its current shape but is not the canonical source for current behavior. Start with this page, the source, and the current user documentation.
