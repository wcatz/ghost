# GhostMem

<img src="assets/ghost.png" alt="GhostMem" width="120" align="right" />

**A local-first MCP memory server for Claude Code, opencode, Cursor, and any MCP client. One memory across clients, in one SQLite file you own.**

GhostMem remembers project knowledge between agent sessions. It stores memories, tasks, and decisions locally, searches them with SQLite FTS5 and optional local embeddings, and works with the MCP clients you already use.

- **Local by default:** one SQLite database; no account, cloud service, or required vector database.
- **Cross-client:** Claude Code, opencode, Codex, Goose, Cursor, and other MCP clients can share the same memory.
- **Graceful degradation:** Ollama is optional. Without it, GhostMem still provides full-text search.
- **Transparent lifecycle:** consolidation, resolution, and supersession are dry-run by default and can be undone or disabled.

## Contents

- [Why GhostMem?](#why-ghostmem)
- [Quick start](#quick-start)
- [Choose your client](#choose-your-client)
- [What GhostMem remembers](#what-ghostmem-remembers)
- [How it works](#how-it-works)
- [Privacy, control, and cost](#privacy-control-and-cost)
- [Optional integrations](#optional-integrations)
- [Commands](#commands)
- [Configuration](#configuration)
- [Benchmarks](#benchmarks)
- [Project status](#project-status)
- [Contributing](#contributing)

## Why GhostMem?

Most agent memory is trapped inside one product. GhostMem provides one portable memory layer:

| | Typical built-in memory | GhostMem |
|---|---|---|
| Reach | One assistant or client | Any MCP client |
| Storage | Product-specific files or services | One local SQLite file |
| Search | Flat context or product-specific retrieval | FTS5 plus optional hybrid search |
| Maintenance | Memories accumulate unchanged | Categories, dedup, linking, and lifecycle tools |
| Ownership | Depends on the provider | You own and can inspect the database |

Native memory features are useful inside their own products. GhostMem is for the gap between them: a project convention learned in one client can inform the next client you use.

## Quick start

### Requirements

- **Go 1.26+** for a source installation.
- A supported MCP client for integration setup.
- **Optional:** [Ollama](https://ollama.com/) with `nomic-embed-text:v1.5` for vector embeddings. GhostMem works without it using FTS5 only.

### Install and initialize

```bash
go install github.com/wcatz/ghost/cmd/ghost@latest
ghost mcp init
```

`ghost mcp init` detects supported clients on `PATH`, configures the integrations it finds, and creates the local GhostMem store. It is idempotent and non-destructive. Preview its changes first with:

```bash
ghost mcp init --dry-run
```

Verify the installation:

```bash
ghost version
ghost mcp status --client claude   # use opencode, codex, or goose for those integrations
```

After initialization, start a session in your project. GhostMem injects the project context automatically and exposes its tools to the client.

Prefer a prebuilt binary or a client-specific setup? See the [installation guide](docs/installation.md).

## Choose your client

| Client | Setup |
|---|---|
| Claude Code | Install the bundled plugin, or run `ghost mcp init --client claude` |
| opencode | `ghost mcp init --client opencode` |
| Codex | `ghost mcp init --client codex`, then approve the hooks once with `/hooks` |
| Goose | `ghost mcp init --client goose` |
| Cursor or another MCP client | Register `ghost mcp` manually; see [installation](docs/installation.md#cursor-and-other-mcp-clients) |

The full setup matrix, Windows instructions, Docker usage, and uninstall steps live in [`docs/installation.md`](docs/installation.md).

## What GhostMem remembers

### Memories

Memories are concise, durable notes with one of eight categories:

- `architecture` — system design and component relationships
- `decision` — a choice, rationale, or rejected alternative
- `pattern` — a recurring approach
- `convention` — a repository or workflow rule
- `gotcha` — a bug, pitfall, or surprising behavior
- `dependency` — a version, API quirk, or external constraint
- `preference` — a user preference
- `fact` — general project knowledge

The agent saves memories through MCP tools. Near-duplicates are detected within the same project: same-category saves fold at the standard similarity bar, and cross-category re-saves of the same rule fold too when their token overlap is near-identical (Jaccard >= 0.7) — and those cross-category folds never target dead records (resolved/superseded). GhostMem preserves the new text as a linked row, strengthens the existing row, and records the relationship without overwriting the original — the existing memory keeps its category. Memories can also be pinned, updated, promoted, searched, or deleted.

### Projects and global knowledge

GhostMem resolves a project by longest path prefix, with a basename fallback. Project knowledge stays scoped to that project; the special `_global` project holds preferences and facts that apply everywhere. Use cross-project search when the relevant context may live under another repository.

### Tasks and decisions

Alongside memories, GhostMem stores:

- **Tasks** with `pending`, `active`, `done`, and `blocked` states.
- **Decision records** with the chosen direction, rationale, alternatives, and status.

Both are available through MCP tools and are included in project context where useful.

See the [usage guide](docs/usage.md) for the complete mental model.

## How it works

```text
Save → Embed → Link → Search → Consolidate → Decay
```

1. **Save:** MCP tools store a memory, task, or decision in SQLite.
2. **Embed:** an optional local Ollama worker creates vectors asynchronously.
3. **Link:** an optional background worker links semantically related memories.
4. **Search:** FTS5 and vectors are fused with Reciprocal Rank Fusion when embeddings are available.
5. **Consolidate:** `ghost reflect` can merge duplicates and prune noise, with snapshots and dry-run protection.
6. **Decay:** category-aware scoring keeps stable conventions and preferences from fading while fresh operational facts can outrank stale ones.

Core memory reads, writes, and search do not need an LLM. Reflection, resolution, and supersession use the calling session's CLI harness (`claude`, `opencode`, `codex`, or `goose`) when invoked; GhostMem does not silently switch to a different harness.

For implementation details, see [`docs/architecture.md`](docs/architecture.md).

## Privacy, control, and cost

### Where the data lives

The database is normally:

```text
$XDG_DATA_HOME/ghost/ghost.db
# or, when XDG_DATA_HOME is unset:
~/.local/share/ghost/ghost.db
```

It is a plain SQLite file. You can inspect, back up, move, or delete it without a GhostMem-specific export format.

Ghost also keeps the data directory bounded: it retains the newest three pre-migration database copies by default, trims oversized lifecycle/Obsidian logs, and removes only dead retired per-phase PID/temp/lock claims. Current lifecycle/Obsidian claims and files whose open state cannot be verified are left untouched. Tune `retention.backup_count` and `retention.log_max_bytes` (or their `GHOST_RETENTION_*` environment overrides) in the [configuration reference](docs/configuration.md).

### What can leave the machine

In normal operation GhostMem does not make network calls. The exceptions are explicit:

- **Local Ollama** for optional embeddings.
- The **calling AI CLI harness** when you run or enable reflection, resolution, or supersession.
- The **GitHub API** when you run `ghost upgrade`.

GhostMem does not require a separate Anthropic API key. The selected CLI harness handles its own authentication and billing.

### Turning things off

- Remove the `ghost` MCP entry to stop the server for a client.
- Set `embedding.enabled: false` to use FTS5 only.
- Keep `reflection.auto_reflect`, `reflection.auto_resolve`, and `reflection.auto_supersede` disabled to avoid automatic lifecycle work.
- Delete `$XDG_DATA_HOME/ghost` (or `~/.local/share/ghost`) to remove the local store.

The configuration reference is [`docs/configuration.md`](docs/configuration.md).

## Optional integrations

### Obsidian mirror

Export memories, decisions, and tasks as a one-way Markdown vault:

```bash
ghost obsidian export --out ~/Documents/GhostVault
ghost obsidian sync --interval 30s
```

The mirror is read-only from GhostMem's perspective. Edits in the vault are not synced back, and a running sync can overwrite hand edits on the next database change. See [the usage guide](docs/usage.md#obsidian-vault-mirror).

### Agent workflows

GhostMem works well with structured agent workflows such as [Superpowers](https://github.com/obra/superpowers): recall context before planning, search memory before changing unfamiliar code, record decisions when alternatives matter, and save durable findings when a phase completes.

## Commands

The common commands are:

```text
ghost mcp                         # Run the MCP server over stdio
ghost mcp init                    # Configure detected MCP clients
ghost mcp status --client <name>  # Check one client integration
ghost reflect <project>           # Preview memory consolidation
ghost resolve <project>           # Preview resolved-evidence marking
ghost supersede <project>         # Preview supersession links
ghost obsidian export|sync        # Mirror the store to Obsidian
ghost bench                       # Run the built-in retrieval benchmark
ghost upgrade                     # Update a standalone binary
```

See [`docs/cli.md`](docs/cli.md) for flags, dry-run behavior, lifecycle details, and project operations, and [`docs/mcp.md`](docs/mcp.md) for the complete MCP tool/resource/prompt surface.

## Configuration

GhostMem works with zero configuration. A minimal optional setup is:

```yaml
embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"

linking:
  enabled: true
  threshold: 0.70
```

Configuration is loaded from compiled defaults, system YAML, user YAML, and `GHOST_*` environment variables, with supported command-line flags applied last. See [`docs/configuration.md`](docs/configuration.md) for paths, precedence, lifecycle settings, and all common keys.

## Benchmarks

GhostMem publishes reproducible retrieval and end-to-end results with the harnesses that produced them. The headline results are:

- **LongMemEval-S retrieval:** hybrid Recall@5 **93.0%** and Recall@10 **97.3%** on the 470 answerable questions.
- **End-to-end LongMemEval-S:** **96.2%** blended accuracy across 500 questions with the documented DeepSeek v4 Pro generator and judge.
- **`ghost bench`:** hybrid NDCG@10 **0.817** on 219 graded queries and 547 memories.

Different generators and judges make cross-system scores directional rather than strictly comparable. Full tables, methodology, caveats, and reproduction commands are in [`docs/benchmarks.md`](docs/benchmarks.md).

## Project status

GhostMem is a solo project used for real infrastructure work. The project intentionally favors a small, readable system:

- Pure Go with `CGO_ENABLED=0` and eight direct Go dependencies.
- SQLite + FTS5 persistence with optional local Ollama embeddings.
- Race-enabled tests, `go vet`, `golangci-lint`, vulnerability scanning, and actionlint in CI.
- Release binaries for Linux, macOS, and Windows on amd64 and arm64, plus a multi-architecture Docker image.

## Contributing

Issues and pull requests are welcome. Run `go test ./...` and `go vet ./...` before submitting changes, and keep work on a feature branch.

## License

Apache License 2.0 — see [`LICENSE`](LICENSE).
