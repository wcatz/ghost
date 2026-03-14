# Ghost

[![CI](https://github.com/wcatz/ghost/actions/workflows/ci.yml/badge.svg)](https://github.com/wcatz/ghost/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/wcatz/ghost)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

A memory-first personal assistant daemon. Ghost remembers what matters about your projects across sessions — architecture decisions, conventions, gotchas, patterns — and surfaces that knowledge through an MCP server, HTTP API, Telegram bot, or interactive REPL.

## Why Ghost

Most AI tools start fresh every session. Ghost gives them persistent memory.

**Cheaper.** 3-block prompt caching puts static system prompt behind Claude's `cache_control: ephemeral` (5min TTL). Cache reads cost 10% of regular input tokens. In a typical agentic tool loop (5-20 API calls per interaction), that's ~90% savings on 1300-2600 tokens of system prompt per cached call.

**Faster search.** Hybrid retrieval combines FTS5 keyword search (30%) with local vector cosine similarity (70%) via Reciprocal Rank Fusion. Better recall than either method alone.

**Free embeddings.** `nomic-embed-text:v1.5` runs locally through Ollama — 274MB, works on CPU. No embedding API costs. If Ollama is offline, search falls back to FTS5 with no hard failure.

**Self-pruning.** Time-decay scoring fades stale memories by category half-life. Context windows stay small and relevant without manual cleanup.

| | Without Ghost | With Ghost |
|--|--|--|
| System prompt tokens | Full price every call | 90% cheaper after first call |
| Embedding cost | $0.001-0.005/embed (API) | $0 (local Ollama) |
| Search method | Keyword only | Semantic + keyword hybrid |
| Cross-session memory | None | Persistent, scored, categorized |
| Offline capable | No | Yes (embeddings + search) |

## Memory System

Ghost stores memories in SQLite with FTS5 full-text search, optional vector embeddings, and time-decay scoring.

| Category | Decay | Purpose |
|----------|-------|---------|
| architecture | none | How the codebase is organized |
| decision | 30-day | Why things were done a certain way |
| pattern | 45-day | Recurring code patterns |
| convention | none | Formatting, naming, testing style |
| gotcha | 30-day | Bugs, edge cases, tricky behavior |
| dependency | 30-day | Libraries, versions, integration |
| preference | none | Developer's preferred approaches |
| fact | none | Durable project facts |

Memories with no decay persist indefinitely. Decaying memories lose importance over their half-life, keeping context windows focused on what's still relevant.

## Install

Requires Go 1.25+ and CGO (for SQLite).

```bash
go install -tags fts5 github.com/wcatz/ghost/cmd/ghost@latest
```

Or build from source:

```bash
git clone https://github.com/wcatz/ghost.git
cd ghost
go build -tags fts5 -o ghost ./cmd/ghost
```

## Quick Start

```bash
# Set your API key
export ANTHROPIC_API_KEY="sk-ant-..."

# Interactive REPL
ghost

# One-shot query
ghost "explain the authentication flow"

# Pipe mode
echo "summarize this project" | ghost

# Start the daemon (HTTP API + all subsystems)
ghost serve

# Run as MCP server for Claude Code / Cursor
ghost mcp
```

## Runtime Modes

### `ghost` — Interactive REPL

Conversational session with tool use, memory, and streaming output.

```bash
ghost                                    # REPL in current directory
ghost "what does the auth middleware do"  # one-shot query
echo "explain this" | ghost              # pipe mode
```

| Flag | Description |
|------|-------------|
| `-mode <name>` | `chat`, `code`, `debug`, `review`, `plan`, `refactor` |
| `-model <id>` | Model override (e.g. `claude-opus-4-6-20250514`) |
| `-project <path>` | Project path (repeatable for multi-project) |
| `-yolo` | Skip all tool approval prompts |
| `-no-memory` | Disable memory for this session |
| `-continue` | Resume last conversation |

**REPL commands:**

```text
/mode <name>       Switch operating mode
/switch <project>  Switch active project
/projects          List project sessions
/memory            List memories
/memory search <q> Search memories
/memory add <text> Add a manual memory
/reflect           Force memory consolidation
/context           Show project context
/cost              Show token usage and spend
/clear             Clear conversation (keep memories)
/quit              Exit
```

### `ghost serve` — Daemon

Headless background service. Starts the HTTP API and all configured subsystems.

```bash
ghost serve                    # use config defaults
ghost serve -addr :3000        # override listen address
```

| Subsystem | Config key | What it does |
|-----------|-----------|--------------|
| HTTP API | `server.listen_addr` | REST API on `127.0.0.1:2187` |
| Embedding worker | `embedding.enabled` | Vectorizes memories via Ollama |
| Scheduler | *(always on)* | Cron jobs + one-shot reminders |
| Telegram bot | `telegram.token` | Remote access + alerts |
| GitHub monitor | `github.token` | Polls notifications, classifies P0-P4 |
| Calendar | `calendar.url` | Pulls CalDAV events for briefings |
| Morning briefing | `briefing.enabled` | Cron-triggered daily summary |

### `ghost mcp` — MCP Server

[MCP](https://modelcontextprotocol.io/) server over stdio. Connects Claude Code, Cursor, Goose, or any MCP-compatible client to Ghost's memory.

**Claude Code** (`~/.claude.json`):

```json
{
  "mcpServers": {
    "ghost": {
      "type": "stdio",
      "command": "/path/to/ghost",
      "args": ["mcp"]
    }
  }
}
```

**Cursor** (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "ghost": {
      "command": "ghost",
      "args": ["mcp"]
    }
  }
}
```

| MCP Tool | Description |
|----------|-------------|
| `ghost_memory_search` | Search memories by keyword (FTS5) |
| `ghost_memory_save` | Store a memory with category, importance, tags |
| `ghost_memories_list` | List memories, optionally filtered by category |
| `ghost_memory_delete` | Delete a memory by ID |
| `ghost_project_context` | Top memories ranked by importance and recency |

All three modes share the same SQLite database — memories saved from the REPL are searchable via MCP and the HTTP API.

## Configuration

Ghost auto-creates `~/.config/ghost/config.yaml` on first run.

Config loads in layers (later overrides earlier):

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. `~/.config/ghost/config.yaml`
4. `.ghost/config.yaml` (per-project, checked in)
5. `.ghost/config.local.yaml` (per-project, gitignored)
6. `GHOST_*` environment variables
7. CLI flags

### Example Config

```yaml
api:
  model_quality: "claude-sonnet-4-5-20250929"
  model_fast: "claude-haiku-4-5-20251001"

defaults:
  mode: "code"
  auto_memory: true
  approval_mode: "normal"       # normal, yolo, strict

server:
  listen_addr: "127.0.0.1:2187"

github:
  token: "ghp_..."              # or GHOST_GITHUB_TOKEN
  interval: 60

telegram:
  token: "123456:ABC..."        # or GHOST_TELEGRAM_TOKEN
  allowed_ids: "12345678"       # comma-separated user IDs

embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"

briefing:
  enabled: true
  schedule: "0 8 * * 1-5"      # 8am weekdays

calendar:
  url: "https://caldav.example.com/..."
  username: "user"
  password: "app-specific-password"
```

### Per-Project Config

`.ghost/config.yaml` in your project root:

```yaml
project:
  name: "my-project"

conventions:
  test_command: "go test ./..."
  lint_command: "golangci-lint run"
  build_command: "go build ./..."

context:
  include_files: ["CLAUDE.md", "ARCHITECTURE.md"]
  ignore_patterns: ["vendor/", "node_modules/"]
```

### Embeddings

Ghost uses [Ollama](https://ollama.com/) for local embeddings. The worker retries automatically — just start Ollama and pull the model:

```bash
ollama pull nomic-embed-text:v1.5
```

Ghost connects on its next retry cycle. No restart required.

## Architecture

```text
cmd/ghost/main.go          CLI entrypoint + daemon bootstrap
internal/
  ai/                      Claude API client + streaming + tool_use
  memory/                  SQLite + FTS5 + vector search + time-decay
  tool/                    Tool registry + 10 built-in executors
  orchestrator/            Multi-project session manager
  reflection/              Haiku-based memory consolidation
  prompt/                  3-block cached system prompt
  mode/                    Operating mode definitions
  project/                 Auto-detection (language, tests, git)
  config/                  Layered YAML/env/flag config (koanf)
  tui/                     Terminal REPL with streaming
  server/                  HTTP REST API (chi)
  mcpserver/               MCP server (stdio)
  telegram/                Telegram bot + alerts
  github/                  Notification monitor + P0-P4 priority
  scheduler/               Cron + one-shot reminders (gocron)
  briefing/                Daily briefing aggregator
  calendar/                CalDAV client
  embedding/               Ollama async worker
  provider/                Interface contracts
  audit/                   Per-action cost + token logging
migrations/                Embedded SQLite schema
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
