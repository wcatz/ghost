# Ghost

<img src="assets/ghost.png" alt="Ghost" width="120" align="right" />

[![CI](https://github.com/wcatz/ghost/actions/workflows/ci.yml/badge.svg)](https://github.com/wcatz/ghost/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/wcatz/ghost)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

MCP memory server for Claude Code, Cursor, and any MCP client. Pure Go. Single binary. No external services required.

## Why Ghost?

Claude Code's built-in memory is a markdown file with a **200-line cap**. No search. No categories. No importance ranking. Every project is siloed. After ten sessions the file is 30% redundant, and the architecture decision you saved last week gets silently truncated because it landed on line 201.

Ghost replaces that with a real memory system:

| | Claude Code built-in | Ghost |
|---|---|---|
| Storage | Flat `.md` files, 200-line cap | SQLite + FTS5, unlimited |
| Search | None (linear load) | Full-text search + optional vector similarity |
| Categorization | None | 8 categories with importance scores (0.0–1.0) |
| Dedup | None (appends forever) | FTS-based upsert — merges on save |
| Consolidation | None | Haiku LLM or local Jaccard similarity |
| Time decay | None (stale facts persist equally) | Category-aware: conventions never decay, gotchas fade at 30 days |
| Cross-project | None (siloed per directory) | `ghost_search_all` + `_global` project |
| Migration | N/A | `ghost mcp init` imports your existing memories |
| Clients | Claude Code only | Any MCP client (Claude Code, Cursor, Goose, etc.) |

One command migrates your existing Claude Code memories into Ghost. Nothing is lost.

## Quick Start

### 1. Install

```bash
go install github.com/wcatz/ghost/cmd/ghost@latest
```

This puts the binary in your `$GOBIN` (default `~/go/bin/`). Make sure it's on your `$PATH`. Or download a pre-built binary from [Releases](https://github.com/wcatz/ghost/releases) and put it wherever you want.

### 2. Connect to Claude Code

```bash
ghost mcp init
```

```
[1/6] Checking prerequisites...
  ✓ ghost binary at ~/go/bin/ghost
  ✓ claude CLI at ~/.local/bin/claude

[2/6] Registering MCP server...
  ✓ ghost MCP server registered

[3/6] Adding tool permissions...
  + 16 mcp__ghost__* tools added to allow list

[4/6] Configuring SessionStart hook...
  + ghost hook session-start — injects project context at startup

[5/6] Importing Claude Code memories...
  ✓ myproject — 8 memories imported
  ✓ infra — 12 memories imported

[6/6] Writing project memory redirects...
  ✓ myproject — redirect written
  ✓ infra — redirect written

Done! Restart Claude Code to activate.
```

| Step | What happens |
|------|-------------|
| Prerequisites | Finds `ghost` and `claude` binaries on your PATH |
| MCP registration | `claude mcp add ghost` — Claude Code discovers Ghost's 16 tools |
| Permissions | Pre-approves all `mcp__ghost__*` tools — no per-call prompts |
| SessionStart hook | Injects project memories, tasks, decisions, globals, and session count into Claude's context |
| Memory import | Migrates Claude Code's `~/.claude/projects/*/memory/*.md` into Ghost (deduplicated) |
| Project redirects | Writes `MEMORY.md` pointing Claude to Ghost instead of its built-in memory |

After setup, Claude automatically loads your project context at session start and saves discoveries during work. No manual prompting needed.

```bash
ghost mcp status          # verify integration health
ghost mcp init --dry-run  # preview changes without writing
```

Idempotent and non-destructive — safe to re-run after updates. Existing Claude Code `MEMORY.md` files with user content are never overwritten. Permissions and hooks are added, never removed. Use `--dry-run` to preview before committing.

### Other MCP clients (Cursor, Goose, etc.)

Add Ghost to your MCP config:

```json
{
  "mcpServers": {
    "ghost": {
      "type": "stdio",
      "command": "ghost",
      "args": ["mcp"]
    }
  }
}
```

## How It Works

Ghost is a memory pipeline with four stages:

```
Save → Search → Consolidate → Decay
```

**Save** — Claude (or you) saves memories via MCP tools. Each memory has a category, importance score (0.0-1.0), and tags. FTS-based upsert deduplicates on save — if a similar memory already exists in the same category, it strengthens instead of creating a duplicate.

**Search** — FTS5 full-text search with optional vector similarity (Ollama embeddings). Cross-project search finds knowledge from other repos. Global memories (`_global`) are included in every project's context.

**Consolidate** — Periodic reflection merges duplicates, prunes noise, and promotes cross-project knowledge to global scope. Two tiers:

| Tier | Backend | Cost | How it works |
|------|---------|------|-------------|
| Haiku | Anthropic API | ~$0.001/run | LLM reads all memories + recent conversations, outputs consolidated set |
| SQLite | Local | Free | Jaccard token similarity, merges >50% overlap, always available |

A quality gate rejects garbage output (< 30% of input) and falls through to the next tier.

**Decay** — Time-based scoring fades stale memories by category:

| Category | Decay | What it captures |
|----------|-------|---------|
| preference | none | How you like to work |
| convention | none | Naming, formatting, workflow rules |
| fact | none | Endpoints, ports, credentials, constants |
| architecture | 45-day | System design, component relationships |
| pattern | 45-day | Recurring approaches, idioms |
| decision | 30-day | Choices made and why |
| gotcha | 30-day | Bugs, edge cases, surprises |
| dependency | 30-day | Library versions, API quirks |

## MCP Tools

Ghost exposes 16 tools to any MCP client:

| Tool | What it does |
|------|-------------|
| `ghost_project_context` | Load top memories ranked by importance + time decay |
| `ghost_memory_save` | Store a memory with category, importance, tags (upserts) |
| `ghost_memory_search` | FTS5 + vector hybrid search, optional category filter |
| `ghost_memories_list` | List memories, optionally filtered by category |
| `ghost_memory_delete` | Delete by ID |
| `ghost_memory_pin` | Pin/unpin — pinned memories stay at top and survive pruning |
| `ghost_list_projects` | All known projects with memory counts |
| `ghost_search_all` | Cross-project memory search |
| `ghost_save_global` | Save a memory that applies to all projects |
| `ghost_task_create` | Track bugs, features, follow-ups |
| `ghost_task_list` | List tasks by project and status |
| `ghost_task_update` | Update task status, priority, or description |
| `ghost_task_complete` | Mark done with optional notes |
| `ghost_decision_record` | Architectural decision with rationale and alternatives |
| `ghost_decisions_list` | List decisions with rationale, alternatives, status |
| `ghost_health` | System health (memory count, embedding status, costs) |

**Task statuses:** `pending` → `active` → `done`. Tasks can also be set to `blocked` from any non-done state and return to `active` when unblocked. `done` is terminal — use `ghost_task_create` to re-open as a new task.

**Resources (4):**

| Resource | Description |
|----------|-------------|
| `ghost://project/{id}/context` | Project context (memories + learned context + globals) |
| `ghost://project/{id}/decisions` | Active decisions — pin to survive context compaction |
| `ghost://project/{id}/tasks` | Open tasks — pin to survive context compaction |
| `ghost://memories/global` | Global memories accessible to all projects |

**Learned context** is a prose summary of the project generated by `ghost reflect`. It captures patterns and decisions that individual memories may not convey. Ghost injects it alongside top memories in every session; it is only updated during a manual `ghost reflect` run, never during normal MCP usage.

The MCP server ships with embedded instructions that teach Claude when to save, which categories to use, and how to leverage cross-project search — so it works proactively without configuration.

## CLI

```
ghost mcp                    # Run MCP server on stdio (used by Claude Code)
ghost mcp init [--dry-run]   # Configure Claude Code integration
ghost mcp status             # Check integration health
ghost hook session-start     # SessionStart hook (called by Claude Code)
ghost reflect <project>      # Manual memory consolidation (dry-run by default)
ghost upgrade                # Self-update from GitHub Releases
ghost version                # Print version
```

The `ghost hook session-start` command reads `{"cwd": "<path>"}` from stdin and writes a Markdown system-reminder to stdout containing top memories, open tasks, active decisions, and global memories — injected into the session before any MCP tools are available.

`ghost reflect` flags: `--apply` to save, `--restore` to undo, `--tier haiku|sqlite|auto`.

## Install

Requires Go 1.25+.

```bash
go install github.com/wcatz/ghost/cmd/ghost@latest
```

Or build from source:

```bash
git clone https://github.com/wcatz/ghost.git && cd ghost && make build
```

### Pre-built binaries

Download from [GitHub Releases](https://github.com/wcatz/ghost/releases) — linux, macOS, Windows (amd64 + arm64).

### Update

```bash
ghost upgrade    # self-update from GitHub Releases
```

### Docker

```bash
docker run -v ghost-data:/data ghcr.io/wcatz/ghost:latest
```

---

## With Superpowers

[Superpowers](https://github.com/obra/superpowers) is a skills framework for AI coding agents — it enforces brainstorm-first planning, mandatory TDD, and subagent-driven execution. Ghost and Superpowers are built to complement each other: Superpowers structures _how_ work gets done, Ghost remembers _what was learned_.

### The workflow

```
Session start
  └── Ghost SessionStart hook → injects project context (memories, tasks, decisions)
  
User asks for a new feature
  └── Superpowers brainstorm skill → clarifies requirements
  └── Superpowers calls ghost_project_context → loads codebase history and past decisions
  └── Superpowers writes a plan informed by Ghost's memory of conventions and gotchas

Subagents execute the plan
  └── Each phase ends with ghost_memory_save → persists what was learned
  └── Architectural choices go through ghost_decision_record
  └── Bugs found along the way → category: gotcha, importance: 0.9

Next session
  └── Ghost hook fires → top memories already in context
  └── No re-explaining the codebase, conventions, or past decisions
```

### Install Superpowers for Claude Code

In any Claude Code session, run:

```
/plugin install superpowers@claude-plugins-official
```

No additional configuration needed — skills trigger automatically based on what you ask for.

### Ghost MCP tools Superpowers uses

| Tool | When Superpowers calls it |
|------|--------------------------|
| `ghost_project_context` | Brainstorm phase — loads codebase history before planning |
| `ghost_memory_search` | Before touching any component — checks for known gotchas |
| `ghost_decision_record` | When an architectural choice is made |
| `ghost_memory_save` | After each subagent phase completes |
| `ghost_task_create` | To track discovered follow-ups across sessions |

---

## Configuration

Config loads in layers (later overrides earlier):

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. `~/.config/ghost/config.yaml`
4. `.ghost/config.yaml` (per-project)
5. `.ghost/config.local.yaml` (gitignored)
6. `GHOST_*` environment variables

**Minimal config (no file needed):**

Ghost stores memories in `~/.local/share/ghost/ghost.db`. No configuration required for basic MCP usage.

**Optional embedding (vector search):**

```yaml
embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"
```

Requires [Ollama](https://ollama.ai) running locally. Enables hybrid FTS5 + vector search.

**Optional reflection (memory consolidation with Haiku):**

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
ghost reflect myproject --apply
```

Without an API key, Ghost falls back to the free SQLite-based Jaccard consolidator.

## Architecture

```
cmd/ghost/main.go          CLI bootstrap
internal/
  memory/                  SQLite + FTS5 + vector search + time-decay scoring
  reflection/              Tiered consolidation (Haiku → SQLite)
  mcpserver/               MCP server (stdio, 16 tools + 4 resources)
  mcpinit/                 Claude Code integration setup (init, status, hook)
  claudeimport/            Auto-import Claude Code memory files
  ai/                      Claude API client (used by reflection only)
  embedding/               Ollama async vectorization worker
  config/                  Layered YAML/env config (koanf)
  selfupdate/              Self-update from GitHub releases
  provider/                Interface contracts (MemoryStore, LLMProvider)
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
