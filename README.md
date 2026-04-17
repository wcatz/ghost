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

**Resources (4):**

| Resource | Description |
|----------|-------------|
| `ghost://project/{id}/context` | Project context (memories + learned context + globals) |
| `ghost://project/{id}/decisions` | Active decisions — pin to survive context compaction |
| `ghost://project/{id}/tasks` | Open tasks — pin to survive context compaction |
| `ghost://memories/global` | Global memories accessible to all projects |

The MCP server ships with embedded instructions that teach Claude when to save, which categories to use, and how to leverage cross-project search — so it works proactively without configuration.

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
docker run -v ghost-data:/data ghcr.io/wcatz/ghost:latest mcp
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

Ghost's 3-block prompt caching pairs well with Superpowers' subagent chunking: smaller, focused tasks mean fewer tokens burned re-establishing context between turns.

### Install Superpowers for Claude Code

In any Claude Code session, run:

```
/plugin install superpowers@claude-plugins-official
```

No additional configuration needed — skills trigger automatically based on what you ask for.

### Add a Go testing skill

Superpowers ships with TDD skills, but you can add a project-aware Go skill that encodes Ghost-specific conventions. Create `~/.config/superpowers/skills/go-testing/SKILL.md`:

```yaml
---
name: go-testing
description: "Use when writing Go tests, running go test, implementing table-driven tests, or adding test coverage to Go packages."
---
```

A complete skill file (table-driven pattern, Ghost store helpers, vet/test conventions) is included in this repo's [Ghost memory system](https://github.com/wcatz/ghost) and written to `~/.config/superpowers/skills/go-testing/SKILL.md` by `ghost mcp init`.

### Ghost MCP tools Superpowers uses

| Tool | When Superpowers calls it |
|------|--------------------------|
| `ghost_project_context` | Brainstorm phase — loads codebase history before planning |
| `ghost_memory_search` | Before touching any component — checks for known gotchas |
| `ghost_decision_record` | When an architectural choice is made |
| `ghost_memory_save` | After each subagent phase completes |
| `ghost_task_create` | To track discovered follow-ups across sessions |

---

## Beyond MCP — Optional Features

Everything below is optional. Ghost works as a pure MCP memory server with zero configuration beyond `ghost mcp init`. These features activate when you run `ghost serve` as a daemon.

### HTTP API

REST API on `127.0.0.1:2187`, authenticated via Bearer token when configured.

**Memory endpoints:**

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/memories/search` | Search memories (FTS5) |
| POST | `/api/v1/memories/` | Create/upsert memory |
| GET | `/api/v1/memories/{projectID}` | List project memories |
| DELETE | `/api/v1/memories/{memoryID}` | Delete memory |
| GET | `/api/v1/projects` | List all projects |

**Session endpoints (requires `ANTHROPIC_API_KEY`):**

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/sessions/` | Create chat session |
| POST | `/api/v1/sessions/{id}/send` | Send message (SSE streaming) |
| POST | `/api/v1/sessions/{id}/approve` | Approve/deny pending tool |
| POST | `/api/v1/sessions/{id}/mode` | Change operating mode |
| POST | `/api/v1/sessions/{id}/auto-approve` | Toggle auto-approve |
| GET | `/api/v1/sessions/{id}/history` | Conversation history |
| GET | `/api/v1/sessions/` | List active sessions |
| DELETE | `/api/v1/sessions/{id}` | Delete session |
| GET | `/api/v1/health` | Health check |
| GET | `/api/v1/costs/monthly` | Monthly cost by model |

**SSE stream events:**

| Event | Description |
|-------|-------------|
| `text` | Assistant text response |
| `thinking` | Extended thinking output |
| `tool_use_start` / `tool_use_end` | Tool invocation lifecycle |
| `tool_result` | Execution result with duration |
| `tool_diff` | File diff output |
| `approval_required` | Tool needs approval (from any client) |
| `approval_resolved` | Approval handled |
| `done` | Stream complete with usage stats |

### Telegram Bot

Remote access to Ghost from your phone. Run `ghost serve` with `telegram.token` configured.

| Command | Description |
|---------|-------------|
| `/sessions` | Active sessions with inline picker |
| `/chat <id> <msg>` | Chat with a Ghost session (streamed) |
| `/new` | Create new session |
| `/yolo` | Toggle auto-approve for a session |
| `/memory search <q>` | Search memories |
| `/notifications` | GitHub notifications (P0-P4 priority) |
| `/meetings` | Today's calendar with Meet links |
| `/emails` | Unread Gmail summaries |
| `/briefing` | Daily briefing |
| `/cost` | Session and monthly cost |
| `/remind <msg>` | Set a reminder |

**Tool approval forwarding** — when Ghost needs permission to run a tool (bash, file writes, git), it sends an approval request to Telegram with Allow/Deny buttons. Tap to approve from your phone. Approvals resolved from any client (VSCode, TUI, Telegram) auto-delete the message on other clients.

### VSCode Extension

Chat interface in the editor. Download `.vsix` from [Releases](https://github.com/wcatz/ghost/releases), open with `Alt+G`.

- SSE streaming with thinking blocks, tool progress, inline diffs
- Tool approval overlay (Allow/Deny)
- Memory browser with search
- Cost tracking (monthly + session)
- Auto-approve toggle, image paste, `@file.ext` references
- Session resume across restarts

### Interactive REPL

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
ghost                    # interactive TUI
ghost "question"         # one-shot
echo "question" | ghost  # pipe mode
```

| Flag | Description |
|------|-------------|
| `-mode` | `chat`, `code`, `debug`, `review`, `plan`, `refactor` |
| `-model` | Model override |
| `-project` | Project path (repeatable) |
| `-yolo` | Skip all tool approvals |
| `-continue` | Resume last conversation |

**Keybindings:** `ctrl+k` command palette, `ctrl+y` copy last code block, `ctrl+space` push-to-talk, `esc` interrupt.

### Daemon Subsystems

`ghost serve` runs all subsystems:

| Subsystem | What it does | Requires |
|-----------|-------------|----------|
| HTTP API | REST + SSE on `:2187` | nothing |
| Embedding worker | Vectorizes memories locally | Ollama |
| Telegram bot | Remote access + approvals | `telegram.token` |
| GitHub monitor | P0-P4 notification alerts | `github.token` |
| Google Calendar | Meeting alerts via Telegram | OAuth2 credentials |
| Gmail | Email summaries | OAuth2 credentials |
| CalDAV | Alternative calendar (iCloud, etc.) | `calendar.url` |
| Scheduler | Cron jobs + reminders | nothing |
| Morning briefing | Daily summary to Telegram | `briefing.enabled` |
| Voice | STT/TTS (Whisper/AssemblyAI + Piper/ElevenLabs) | provider config |

## Configuration

Config loads in layers (later overrides earlier):

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. `~/.config/ghost/config.yaml`
4. `.ghost/config.yaml` (per-project)
5. `.ghost/config.local.yaml` (gitignored)
6. `GHOST_*` environment variables
7. CLI flags

**Minimal config (MCP only — no daemon features):**

No config file needed. Ghost stores memories in `~/.local/share/ghost/ghost.db`.

**Full config (daemon with all subsystems):**

```yaml
api:
  model_quality: "claude-opus-4-6-20250514"
  model_fast: "claude-sonnet-4-5-20250929"

server:
  listen_addr: "127.0.0.1:2187"
  auth_token: ""                     # openssl rand -hex 32

embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"

reflection:
  backend: "auto"                    # auto, haiku, sqlite, disabled

telegram:
  token: "123456:ABC..."
  allowed_ids: "12345678"

github:
  token: "ghp_..."
  interval: 60

google:
  credentials_file: "~/.config/ghost/google-credentials.json"

briefing:
  enabled: true
  schedule: "0 8 * * 1-5"
```

See `internal/config/config.example.yaml` for all options including voice, display, and CalDAV.

## Architecture

```
cmd/ghost/main.go          CLI + daemon bootstrap
internal/
  memory/                  SQLite + FTS5 + vector search + time-decay scoring
  reflection/              Tiered consolidation (Haiku → SQLite) + auto-extraction
  mcpserver/               MCP server (stdio, 16 tools + 4 resources)
  mcpinit/                 Claude Code integration setup (init, status, hook)
  claudeimport/            Auto-import Claude Code memory files
  ai/                      Claude API client, streaming, tool_use, cost tracking
  tool/                    Tool registry + executors (file, grep, glob, git, bash, memory)
  orchestrator/            Multi-project sessions, context compression, caching
  prompt/                  3-block cached system prompt
  mode/                    Operating modes (chat, code, debug, review, plan, refactor)
  project/                 Auto-detection (language, tests, git)
  config/                  Layered YAML/env/flag config (koanf)
  server/                  HTTP REST API + SSE streaming (chi)
  tui/                     Terminal REPL (bubbletea)
  telegram/                Bot, session management, approval forwarding
  google/                  Calendar + Gmail OAuth2
  github/                  Notification monitor (P0-P4 priority)
  scheduler/               Cron + reminders (gocron)
  briefing/                Daily briefing generator
  embedding/               Ollama async vectorization worker
  voice/                   STT/TTS pipeline (Whisper, AssemblyAI, Piper, ElevenLabs)
  selfupdate/              Self-update from GitHub releases
  provider/                Interface contracts
  mdv2/                    MarkdownV2 escaping for Telegram
vscode-ghost/              VSCode extension (TypeScript)
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
