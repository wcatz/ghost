# Ghost

<img src="assets/ghost.png" alt="Ghost" width="160" align="right" />

[![CI](https://github.com/wcatz/ghost/actions/workflows/ci.yml/badge.svg)](https://github.com/wcatz/ghost/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/wcatz/ghost)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Ghost is a memory-first autonomous agent. It persists what it learns about your projects in SQLite — architecture decisions, patterns, conventions, gotchas — and carries that context into an agentic tool loop that can act on your behalf. It runs as a daemon with an HTTP API, MCP server, VSCode extension, Telegram bot, and terminal REPL.

Pure Go. No CGO. Single binary.

## What It Does

- **Agentic tool loop** — Claude's tool_use drives a read → act → reflect cycle; Ghost executes tools on your behalf with configurable approval gates
- **Persistent memory** across sessions — Ghost knows what you worked on last week
- **3-block prompt caching** — 90% savings on input tokens in agentic tool loops
- **Multi-turn caching** — conversation turns cached across API calls
- **Hybrid search** — FTS5 keyword + vector cosine similarity via Reciprocal Rank Fusion
- **Free embeddings** — `nomic-embed-text:v1.5` runs locally through Ollama, no API costs
- **Time-decay scoring** — stale memories fade automatically by category half-life
- **Google Calendar + Gmail** — meeting notifications, email summaries via OAuth2
- **GitHub notifications** — priority-classified alerts forwarded to Telegram
- **Tool approval forwarding** — approve Ghost's actions from your phone via Telegram
- **Session resume** — conversations persist to SQLite, reload on restart
- **Context compression** — Haiku summarizes older messages when context gets long
- **Cost tracking** — real-time per-session and cumulative USD cost with cache savings

## Install

Requires Go 1.25+.

```bash
go install github.com/wcatz/ghost/cmd/ghost@latest
```

Or build from source:

```bash
git clone https://github.com/wcatz/ghost.git
cd ghost
make build
```

## Quick Start

```bash
# Set your API key
export ANTHROPIC_API_KEY="sk-ant-..."

# Interactive REPL
ghost

# One-shot query
ghost "explain the authentication flow"

# Start the daemon
ghost serve

# MCP server for Claude Code / Cursor
ghost mcp
```

## Runtime Modes

### `ghost` — Interactive REPL

```bash
ghost                                    # REPL
ghost "what does the auth middleware do"  # one-shot
echo "explain this" | ghost              # pipe
```

| Flag | Description |
|------|-------------|
| `-mode` | `chat`, `code`, `debug`, `review`, `plan`, `refactor` |
| `-model` | Model override |
| `-project` | Project path (repeatable) |
| `-yolo` | Skip all tool approvals |
| `-continue` | Resume last conversation |

**REPL commands:**

```
/mode <name>       Switch mode
/switch <project>  Switch project
/memory search <q> Search memories
/memory add <text> Manual memory
/reflect           Force consolidation
/cost              Token usage + spend
/clear             Clear conversation
/quit              Exit
```

### `ghost serve` — Daemon

Headless background service with HTTP API and all subsystems.

| Subsystem | What it does |
|-----------|-------------|
| HTTP API | REST API on `127.0.0.1:2187` |
| Embedding worker | Vectorizes memories via Ollama |
| Scheduler | Cron jobs + reminders |
| Telegram bot | Remote access, approvals, alerts |
| GitHub monitor | Notification polling, P0-P4 priority |
| Google Calendar | Meeting alerts (10min + 5min) via Telegram |
| Gmail | Unread email summaries |
| Morning briefing | Cron-triggered daily summary |

### `ghost mcp` — MCP Server

Connects Claude Code, Cursor, or any MCP client to Ghost's memory via stdio.

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

| MCP Tool | Description |
|----------|-------------|
| `ghost_memory_search` | FTS5 keyword search |
| `ghost_memory_save` | Store with category, importance, tags |
| `ghost_memories_list` | List with optional category filter |
| `ghost_memory_delete` | Delete by ID |
| `ghost_project_context` | Top memories ranked by importance + recency |

## VSCode Extension

Chat interface directly in the editor.

```bash
cd vscode-ghost && npm install && npm run compile
npx @vscode/vsce package --allow-missing-repository
code --install-extension ghost-0.1.0.vsix
```

- Editor tab chat (`Ctrl+Shift+G`) + sidebar panel
- SSE streaming with thinking, tool progress, inline diffs
- Markdown rendering with syntax highlighting + copy buttons
- Cost tracking with cache savings display
- Auto-approve toggle (YOLO mode)
- Image paste/attach support
- Slash commands (`/mode`, `/clear`, `/cost`, `/auto-approve`)
- Memory browser with search
- Message queuing during streaming
- Session resume across restarts
- `@file.ext` references to attach file contents

## Telegram Bot

| Command | Description |
|---------|-------------|
| `/status` | System status |
| `/notifications` | GitHub notifications with "Open" buttons |
| `/meetings` | Today's calendar with "Join Meet" buttons |
| `/emails` | Unread Gmail with "Open" buttons |
| `/sessions` | Active sessions with inline picker |
| `/chat <id> <msg>` | Message a Ghost session |
| `/memory search` | Search memories |
| `/remind <msg>` | Set a reminder |
| `/briefing` | Daily briefing with progressive loading |
| `/help` | Commands list |

Tool approvals forward to Telegram with Allow/Deny buttons. Reply with text to deny with instructions.

## Google Integration

OAuth2 for Google Calendar + Gmail.

1. Create a Google Cloud project, enable Calendar API + Gmail API
2. Create OAuth2 Desktop credentials
3. Save to `~/.config/ghost/google-credentials.json`
4. On first `ghost serve`, authorize via the printed URL

## Memory System

| Category | Decay | Purpose |
|----------|-------|---------|
| architecture | none | Codebase structure |
| decision | 30-day | Why things were done |
| pattern | 45-day | Recurring code patterns |
| convention | none | Style and naming |
| gotcha | 30-day | Bugs and edge cases |
| dependency | 30-day | Libraries and versions |
| preference | none | Developer preferences |
| fact | none | Durable project facts |

## Configuration

Config loads in layers (later overrides earlier):

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. `~/.config/ghost/config.yaml`
4. `.ghost/config.yaml` (per-project)
5. `.ghost/config.local.yaml` (gitignored)
6. `GHOST_*` environment variables
7. CLI flags

```yaml
api:
  model_quality: "claude-sonnet-4-5-20250929"
  model_fast: "claude-haiku-4-5-20251001"

server:
  listen_addr: "127.0.0.1:2187"

github:
  token: "ghp_..."
  interval: 60

telegram:
  token: "123456:ABC..."
  allowed_ids: "12345678"

embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"

google:
  credentials_file: "~/.config/ghost/google-credentials.json"

briefing:
  enabled: true
  schedule: "0 8 * * 1-5"
```

## Architecture

```
cmd/ghost/main.go          CLI + daemon bootstrap
internal/
  ai/                      Claude API client, streaming, tool_use, cost tracking
  memory/                  SQLite + FTS5 + vector search + time-decay
  tool/                    Tool registry + 9 built-in executors
  orchestrator/            Multi-project sessions, context compression, multi-turn caching
  reflection/              Haiku memory consolidation
  prompt/                  3-block cached system prompt
  mode/                    Operating modes
  project/                 Auto-detection (language, tests, git)
  config/                  Layered YAML/env/flag config (koanf)
  tui/                     Terminal REPL
  server/                  HTTP REST API (chi)
  mcpserver/               MCP server (stdio)
  telegram/                Bot, approvals, session management
  google/                  Calendar + Gmail OAuth2
  github/                  Notification monitor
  scheduler/               Cron + reminders (gocron)
  briefing/                Daily briefing
  embedding/               Ollama async worker
  mdv2/                    MarkdownV2 escaping
  voice/                   Voice pipeline (WIP)
  provider/                Interface contracts
migrations/                Embedded SQLite schema
vscode-ghost/              VSCode extension (TypeScript)
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
