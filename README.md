# Ghost

[![CI](https://github.com/wcatz/ghost/actions/workflows/ci.yml/badge.svg)](https://github.com/wcatz/ghost/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/wcatz/ghost)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

A memory-first personal assistant daemon. Ghost remembers what matters about your projects across sessions — architecture decisions, conventions, gotchas, patterns — and surfaces that knowledge through an MCP server, HTTP API, Telegram bot, or interactive REPL.

## What Makes Ghost Different

Most AI tools start fresh every session. Ghost persists knowledge in SQLite with FTS5 full-text search and time-decay scoring, so stale information fades while core knowledge persists.

**Memory categories:**

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

## Features

### Memory Daemon (`ghost serve`)
- SQLite-backed memory with FTS5 search and optional vector embeddings (Ollama)
- HTTP API for memory CRUD and project management
- Automatic reflection — Haiku consolidates memories periodically
- Embedding worker for semantic search via `nomic-embed-text`

### MCP Server (`ghost mcp`)
- Exposes Ghost's memory as an [MCP](https://modelcontextprotocol.io/) server on stdio
- Works with Claude Code, Cursor, Goose, and any MCP-compatible client
- Tools: `ghost_memory_search`, `ghost_memory_save`, `ghost_project_context`, `ghost_memories_list`, `ghost_memory_delete`

### Telegram Bot
- Remote access via Telegram with user-ID whitelisting
- Commands: `/status`, `/notifications`, `/memory search`, `/remind`, `/briefing`, `/help`
- Proactive alerts for P0/P1 GitHub notifications and fired reminders

### GitHub Monitor
- Polls GitHub notifications with priority classification (P0-P4)
- P0/P1 alerts pushed to Telegram in real-time

### Scheduler
- Cron jobs and one-shot reminders with natural language date parsing
- Persisted in SQLite — survives restarts

### Morning Briefing
- Configurable cron-triggered daily briefing
- Aggregates GitHub notifications, calendar events, and pending reminders

### CalDAV Calendar
- Connects to any CalDAV server (iCloud, Fastmail, Nextcloud)
- Pulls upcoming events for briefings

### Interactive REPL (`ghost`)
- 6 operating modes: chat, code, debug, review, plan, refactor
- Native Claude tool_use for file operations, code search, git, and shell
- 3-block prompt caching for ~90% input token savings
- Multi-project sessions with isolated memory spaces
- Safety controls: approval levels for writes, destructive git commands blocked

## Install

Requires Go 1.24+ and CGO (for SQLite).

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

# Start the daemon (HTTP API + all subsystems)
ghost serve

# Run as MCP server for Claude Code / Cursor
ghost mcp
```

## Configuration

Ghost auto-creates `~/.config/ghost/config.yaml` on first run with all options documented.

Config is loaded in layers (later overrides earlier):
1. Compiled defaults
2. `/etc/ghost/config.yaml` (system-wide)
3. `~/.config/ghost/config.yaml` (user)
4. `.ghost/config.yaml` (per-project, checked in)
5. `.ghost/config.local.yaml` (per-project, gitignored)
6. `GHOST_*` environment variables
7. CLI flags

### Example

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

## MCP Integration

Add Ghost as an MCP server in your tool's config:

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

Then use `ghost_memory_search`, `ghost_memory_save`, and `ghost_project_context` from your AI tool to recall and store project knowledge.

## REPL Commands

```
/mode <name>       Switch mode: chat, code, debug, review, plan, refactor
/switch <project>  Switch active project (multi-project mode)
/projects          List active project sessions
/memory            List all memories for current project
/memory search <q> Search memories
/memory add <text> Add a manual memory
/reflect           Force memory consolidation
/context           Show project context
/cost              Show token usage
/clear             Clear conversation (keep memories)
/quit              Exit
```

## Architecture

```
cmd/ghost/main.go          CLI entrypoint + daemon bootstrap
internal/
  ai/                      Claude API client + streaming + tool_use
  memory/                  SQLite persistence, FTS5, time-decay scoring
  tool/                    Tool registry + built-in executors
  orchestrator/            Multi-project session manager
  reflection/              Memory extraction + consolidation engine
  prompt/                  3-block system prompt construction
  mode/                    Operating mode definitions
  project/                 Project detection + context gathering
  config/                  YAML config + environment variables
  tui/                     Terminal REPL with streaming output
  server/                  HTTP API (chi)
  mcpserver/               MCP server (stdio transport)
  telegram/                Telegram bot interface
  github/                  GitHub notification monitor + priority
  scheduler/               Cron jobs + one-shot reminders
  briefing/                Morning briefing aggregator
  calendar/                CalDAV client
  embedding/               Ollama embedding worker
  provider/                Interface contracts
migrations/                Embedded SQLite migrations
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
