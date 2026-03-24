# Ghost

<img src="assets/ghost.png" alt="Ghost" width="160" align="right" />

[![CI](https://github.com/wcatz/ghost/actions/workflows/ci.yml/badge.svg)](https://github.com/wcatz/ghost/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/wcatz/ghost)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Ghost is a memory-first autonomous agent. It persists what it learns about your projects in SQLite — architecture decisions, patterns, conventions, gotchas — and carries that context into an agentic tool loop that can act on your behalf. It runs as a daemon with an HTTP API, MCP server, VSCode extension, Telegram bot, and terminal REPL.

Pure Go. No CGO. Single binary.

## What It Does

- **Agentic tool loop** — Claude's tool_use drives a read-act-reflect cycle with configurable approval gates
- **Persistent memory** across sessions — Ghost knows what you worked on last week
- **Claude Code memory import** — auto-imports Claude Code's memory files on first project contact, no manual migration
- **3-block prompt caching** — 90%+ cache hit rates, ~76% savings on input tokens
- **Hybrid search** — FTS5 keyword + vector cosine similarity via Reciprocal Rank Fusion
- **Free embeddings** — `nomic-embed-text:v1.5` runs locally through Ollama, no API costs
- **Tiered consolidation** — memory dedup via Haiku API, Ollama local LLM, or pure SQLite (works without API credits)
- **Time-decay scoring** — stale memories fade automatically by category half-life
- **Cost tracking** — per-session and monthly cost aggregation with cache savings, API vs subscription comparison
- **Google Calendar + Gmail** — meeting notifications, email summaries via OAuth2
- **CalDAV calendar** — alternative to Google (iCloud, Fastmail, etc.)
- **GitHub notifications** — P0-P4 priority-classified alerts forwarded to Telegram
- **Tool approval forwarding** — approve Ghost's actions from phone via Telegram, auto-cleanup on resolution
- **Voice input** — push-to-talk with Whisper/AssemblyAI STT and Piper/ElevenLabs TTS
- **Task management** — create, list, complete tasks via MCP; tracked in SQLite
- **Decision records** — architectural decisions with alternatives and rationale via MCP
- **Session resume** — conversations persist to SQLite, reload on restart
- **Context compression** — Haiku summarizes older messages when context gets long

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

### Pre-built binaries

Download from [GitHub Releases](https://github.com/wcatz/ghost/releases) — available for linux, macOS, and Windows (amd64 + arm64).

### Update

```bash
ghost upgrade
```

Downloads the latest release from GitHub, replaces the running binary in-place (wherever it lives), and prints the version diff. No package manager required.

### Docker

```bash
docker run -e ANTHROPIC_API_KEY="sk-ant-..." ghcr.io/wcatz/ghost:latest serve
```

### VSCode Extension

Download the `.vsix` from [GitHub Releases](https://github.com/wcatz/ghost/releases) and install:

```bash
code --install-extension ghost-*.vsix
```

## Quick Start — Claude Code / Cursor

Ghost's primary interface is as an MCP server. One command configures everything:

```bash
ghost mcp init
```

```
[1/5] Checking prerequisites...
  ✓ claude CLI found at /home/you/.local/bin/claude
  ✓ ghost binary at /home/you/.local/bin/ghost

[2/5] Registering MCP server...
  ✓ ghost MCP server registered (command: /home/you/.local/bin/ghost)

[3/5] Adding tool permissions...
  + 13 mcp__ghost__* tools added to allow list

[4/5] Configuring SessionStart hook...
  + ghost hook session-start — reminds Claude to load context

[5/5] Importing Claude Code memories...
  ✓ myproject — 8 memories imported
  ✓ infra — 12 memories imported

Done! Restart Claude Code to activate.
```

**What this does:**

| Step | Effect |
|------|--------|
| MCP registration | Runs `claude mcp add` so Claude Code discovers Ghost's 13 tools |
| Permissions | Pre-approves all `mcp__ghost__*` tools — no per-call prompts |
| SessionStart hook | At every session start, Claude sees a reminder to call `ghost_project_context` |
| Memory import | Reads Claude Code's `~/.claude/projects/*/memory/*.md` files into Ghost (deduplicated) |
| Project redirects | Writes `MEMORY.md` files pointing Claude to Ghost instead of its built-in memory |

After setup, Claude Code will automatically load your project context and save discoveries during work. No manual prompting needed.

```bash
ghost mcp status     # verify integration health
ghost mcp init --dry-run  # preview changes without modifying files
```

Idempotent — safe to re-run after updates or installs.

### Manual MCP setup (Cursor, other clients)

If you're not using Claude Code, add Ghost to your MCP config directly:

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

### Standalone usage

```bash
export ANTHROPIC_API_KEY="sk-ant-..."

ghost                                  # interactive REPL
ghost "explain the authentication flow" # one-shot query
echo "explain this" | ghost            # pipe mode
ghost serve                            # daemon (HTTP API + all subsystems)
ghost upgrade                          # self-update to latest release
```

## Runtime Modes

### `ghost` — Interactive REPL

| Flag | Description |
|------|-------------|
| `-mode` | `chat`, `code`, `debug`, `review`, `plan`, `refactor` |
| `-model` | Model override |
| `-project` | Project path (repeatable) |
| `-yolo` | Skip all tool approvals |
| `-continue` | Resume last conversation |
| `-no-memory` | Disable automatic memory extraction |
| `-no-tui` | Force legacy REPL (no bubbletea) |
| `-v`, `version` | Print version and exit |

**Slash commands:**

| Command | Description |
|---------|-------------|
| `/model <name>` | Switch model (sonnet/haiku/opus) |
| `/continue` | Continue from where left off |
| `/compact` | Compress conversation history |
| `/tokens` | Token estimates + cache stats |
| `/export` | Export conversation as markdown |
| `/sessions` | List sessions with counts |
| `/new` | Start fresh session |
| `/resume` | Resume last session |
| `/memory` | List all memories |
| `/memory search <q>` | Search memories |
| `/memory add` | Add a manual memory |
| `/cost` | Session cost breakdown |
| `/context` | Show project context |
| `/image <path>` | Send image to Claude |
| `/reflect` | Force memory consolidation |
| `/briefing` | Ask Ghost for a briefing |
| `/voice` | Voice mode info |
| `/health` | Memory, embeddings, cost |
| `/history` | Conversation stats |
| `/theme <name>` | Switch glamour theme |
| `/remind <t> <msg>` | Set a reminder |
| `/reminders` | List pending reminders |
| `/switch <name>` | Switch project |
| `/projects` | List project sessions |
| `/clear` | Clear conversation |
| `/quit` | Exit ghost |

**Keybindings:** `ctrl+k` command palette, `ctrl+y` copy last code block, `ctrl+space` push-to-talk, `esc` interrupt, `?` help overlay.

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
| Cost report | Monthly cost report to Telegram |

### `ghost mcp` — MCP Server

Exposes Ghost's memory to any MCP client via stdio. See [Quick Start](#quick-start--claude-code--cursor) for setup.

**Tools (13):**

| Tool | Description |
|------|-------------|
| `ghost_project_context` | Load top memories ranked by importance + recency |
| `ghost_memory_save` | Store with category, importance, tags |
| `ghost_memory_search` | FTS5 + vector hybrid search |
| `ghost_memories_list` | List with optional category filter |
| `ghost_memory_delete` | Delete by ID |
| `ghost_list_projects` | Discover all known projects with memory counts |
| `ghost_search_all` | Cross-project memory search |
| `ghost_save_global` | Save memory accessible to all projects |
| `ghost_task_create` | Create a task with priority and status |
| `ghost_task_list` | List tasks by project and status |
| `ghost_task_complete` | Mark a task as done |
| `ghost_decision_record` | Record an architectural decision with rationale |
| `ghost_health` | System stats (memory count, embeddings, costs) |

**Resources:**

| Resource | Description |
|----------|-------------|
| `ghost://project/{id}/context` | Push-based project context (memories + learned context) |
| `ghost://memories/global` | Global memories accessible to all projects |

The MCP server ships with comprehensive instructions that teach Claude when and how to save memories proactively, which categories to use, and how to leverage cross-project search.

## HTTP API

All endpoints under `/api/v1/`, authenticated via Bearer token when `server.auth_token` is configured.

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/health` | Health check |
| POST | `/memories/search` | Search memories (FTS5) |
| POST | `/memories/` | Create memory |
| GET | `/memories/{projectID}` | List memories for project |
| DELETE | `/memories/{memoryID}` | Delete memory |
| GET | `/projects` | List all projects |
| GET | `/costs/monthly` | Monthly cost aggregation by model |
| GET | `/transcribe/token` | AssemblyAI temporary token |
| POST | `/sessions/` | Create chat session |
| GET | `/sessions/` | List sessions |
| DELETE | `/sessions/{id}` | Delete session |
| POST | `/sessions/{id}/send` | Send message (SSE stream) |
| POST | `/sessions/{id}/approve` | Approve/deny pending tool |
| POST | `/sessions/{id}/mode` | Change session mode |
| POST | `/sessions/{id}/auto-approve` | Toggle auto-approve |
| GET | `/sessions/{id}/history` | Get conversation history |

## VSCode Extension

Chat interface directly in the editor. Open with `Alt+G`.

- Sidebar chat panel + editor tab chat
- SSE streaming with thinking, tool progress, inline diffs
- Markdown rendering with syntax highlighting + copy buttons
- Cost tracking — monthly in header, session in footer
- Auto-approve toggle (YOLO mode)
- Image paste/attach support
- Slash commands (`/mode`, `/clear`, `/cost`, `/auto-approve`)
- Memory browser with search
- Tool approval overlay with Allow/Deny
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
| `/mode` | List or switch session mode |
| `/cost` | Session cost and monthly summary |
| `/help` | Commands list |

Tool approvals forward to Telegram with Allow/Deny buttons. Approvals resolved from any client (VSCode, TUI) auto-delete the Telegram message. Notifications are silent when the user is active in VSCode.

## Google Integration

OAuth2 for Google Calendar + Gmail.

1. Create a Google Cloud project, enable Calendar API + Gmail API
2. Create OAuth2 Desktop credentials
3. Save to `~/.config/ghost/google-credentials.json`
4. On first `ghost serve`, authorize via the printed URL

CalDAV is also supported as an alternative calendar source (iCloud, Fastmail, etc.) — configure under `calendar:` in config.

## Memory System

| Category | Decay | Purpose |
|----------|-------|---------|
| preference | none | Developer preferences |
| convention | none | Style and naming |
| fact | none | Durable project facts |
| architecture | 45-day | Codebase structure |
| pattern | 45-day | Recurring code patterns |
| decision | 30-day | Why things were done |
| gotcha | 30-day | Bugs and edge cases |
| dependency | 30-day | Libraries and versions |

### Tiered Consolidation

Memory consolidation runs periodically to merge duplicates, prune stale entries, and maintain quality. Three tiers are available — Ghost tries the highest quality tier and falls back automatically:

| Tier | Backend | Cost | Quality | Requirements |
|------|---------|------|---------|-------------|
| 2 | Haiku API | ~$0.001/run | Best | Anthropic API key |
| 1 | Ollama (`qwen2.5:3b`) | Free | Good | Ollama running locally |
| 0 | SQLite (Jaccard dedup) | Free | Mechanical | None (always available) |

Default is `auto` — uses the best available tier. Configure explicitly:

```yaml
reflection:
  backend: "auto"              # auto, haiku, ollama, sqlite, disabled
  ollama_model: "qwen2.5:3b"  # model for Ollama tier
```

Users with a Claude subscription but no API credits: install [Ollama](https://ollama.com) and `ollama pull qwen2.5:3b` for free local consolidation.

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
  model_quality: "claude-opus-4-6-20250514"
  model_fast: "claude-sonnet-4-5-20250929"

server:
  listen_addr: "127.0.0.1:2187"
  auth_token: ""                     # generate: openssl rand -hex 32

embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"

reflection:
  backend: "auto"                      # auto, haiku, ollama, sqlite, disabled
  ollama_model: "qwen2.5:3b"

github:
  token: "ghp_..."
  interval: 60

telegram:
  token: "123456:ABC..."
  allowed_ids: "12345678"

google:
  credentials_file: "~/.config/ghost/google-credentials.json"

briefing:
  enabled: true
  schedule: "0 8 * * 1-5"

cost_report:
  enabled: false
  schedule: "0 9 1 * *"             # 9am on 1st of month
```

See `internal/config/config.example.yaml` for all options including voice, display, and CalDAV.

## Architecture

```
cmd/ghost/main.go          CLI + daemon bootstrap
internal/
  ai/                      Claude API client, streaming, tool_use, cost tracking
  memory/                  SQLite + FTS5 + vector search + time-decay
  tool/                    Tool registry + built-in executors (file, grep, glob, git, bash, memory)
  orchestrator/            Multi-project sessions, context compression, multi-turn caching
  claudeimport/            Auto-import Claude Code memory files on first project contact
  reflection/              Tiered memory consolidation (Haiku → Ollama → SQLite) + auto-extraction
  prompt/                  3-block cached system prompt
  mode/                    Operating modes (chat, code, debug, review, plan, refactor)
  project/                 Auto-detection (language, tests, git)
  config/                  Layered YAML/env/flag config (koanf)
  tui/                     Terminal REPL (bubbletea)
  server/                  HTTP REST API (chi)
  mcpserver/               MCP server (stdio, 13 tools + resources)
  mcpinit/                 Claude Code integration setup (init, status, hook)
  selfupdate/              Self-update from GitHub releases
  telegram/                Bot, approvals, session management
  google/                  Calendar + Gmail OAuth2
  calendar/                CalDAV client
  github/                  Notification monitor (P0-P4 priority)
  scheduler/               Cron + reminders (gocron)
  briefing/                Daily briefing
  embedding/               Ollama async worker
  voice/                   STT/TTS pipeline (Whisper, AssemblyAI, Piper, ElevenLabs)
  mdv2/                    MarkdownV2 escaping for Telegram
  provider/                Interface contracts
migrations/                Reference SQL schema (runtime: internal/memory/schema.go)
vscode-ghost/              VSCode extension (TypeScript)
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
