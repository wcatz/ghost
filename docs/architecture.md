# Ghost Architecture

## Runtime Modes

```
ghost              Interactive bubbletea TUI (default)
ghost "query"      One-shot mode (no TUI)
echo ... | ghost   Pipe mode (stdin)
ghost serve        HTTP daemon + subsystems
ghost mcp          MCP server (stdio)
```

## Package Map

```
cmd/ghost/main.go          CLI entrypoint, mode selection, daemon bootstrap
internal/
  ai/                      Claude API client
    client.go              HTTP client, ChatStream(), Reflect()
    stream.go              SSE parser, StreamEvent channel
    models.go              Message, ContentBlock, SystemBlock, TokenUsage
  config/                  Layered configuration (koanf)
    config.go              Config struct, Load(), EnsureConfigFile()
    config.example.yaml    Annotated defaults
  embedding/               Local vector embeddings
    client.go              Ollama HTTP client (/api/embed)
    worker.go              Async batch embedder (50/cycle)
  memory/                  Persistence layer
    store.go               SQLite CRUD, FTS5 search, token tracking
    schema.go              DDL (13 tables, embedded via const)
    vector.go              Cosine similarity, hybrid RRF search
  orchestrator/            Session management
    orchestrator.go        Multi-project session map
    session.go             Agentic loop: Send() → tool_use → execute → repeat
  provider/                Interface contracts
    provider.go            LLMProvider, MemoryStore, Frontend, ApprovalRequest
  prompt/                  System prompt construction
    builder.go             3-block caching (static, project, memories)
  reflection/              Memory consolidation
    engine.go              Haiku-based periodic reflection
    extractor.go           Extract memories from conversation
  tool/                    Tool execution
    registry.go            Register, Execute, approval levels
    memory_save.go         Save a memory via tool_use
    memory_search.go       Search memories via tool_use
  mode/                    Operating mode
    modes.go               chat (single mode)
  project/                 Project detection
    context.go             Language, git, test/lint commands, CLAUDE.md
  tui/                     Terminal UI
    app.go                 Bubbletea root model (MVU)
    bridge.go              StreamEvent/approval → tea.Msg adapters
    messages.go            Glamour markdown rendering
    input.go               Multi-line textarea with history
    viewport.go            Scrollable message history
    statusbar.go           Project, mode, tokens, cost
    toolbar.go             Tool progress spinners
    approval.go            Non-blocking approval overlay
    palette.go             Ctrl+K command palette
    images.go              Terminal image rendering (sixel/kitty/iTerm2)
    styles.go              Lipgloss style definitions
    keys.go                Key bindings
    oneshot.go             Pipe/one-shot mode (no bubbletea)
    repl.go                Legacy REPL fallback (dumb terminals)
  server/                  HTTP API
    server.go              chi router, middleware, routes
    chat.go                Session + SSE streaming endpoints
  mcpserver/               MCP server
    mcpserver.go           stdio transport, 5 memory tools
  telegram/                Telegram bot
    bot.go                 Commands, whitelist auth, alerts
    approval.go            Approval forwarding with inline keyboards
    sessions.go            /sessions, /chat, inline session picker
  google/                  Google Workspace integration
    auth.go                OAuth2 flow with token persistence
    calendar.go            Google Calendar API client
    gmail.go               Gmail API client (unread emails)
    notifier.go            Meeting alerts (10min + 5min via Telegram)
  github/                  GitHub monitor
    monitor.go             Notification polling, P0-P4 priority
    types.go               Notification types
  scheduler/               Job scheduling
    scheduler.go           gocron + NLP date parsing (when)
  briefing/                Daily briefing
    briefing.go            Aggregate GitHub + calendar + Gmail + reminders
  calendar/                CalDAV client
    client.go              Read-only event fetching
  mdv2/                    MarkdownV2 utilities
    escape.go              Shared escaper for Telegram formatting
  audit/                   Logging
    audit.go               Per-action cost + token tracking
migrations/
  001_init.sql             Schema reference (actual DDL in schema.go)
```

## Data Flow

### Interactive TUI
```
User input → textarea → Session.Send(ctx, msg, approvalCh)
                          ↓
                 prompt.Builder.BuildSystemBlocks()
                 [Block 1: personality, cached]
                 [Block 2: project ctx, cached]
                 [Block 3: memories, dynamic]
                          ↓
                 ai.Client.ChatStream() → Claude API (SSE)
                          ↓
                 <-chan StreamEvent → bridge → tea.Msg
                          ↓
                 text → glamour → viewport
                 tool_use → toolbar spinner → registry.Execute()
                 approval → overlay dialog → response channel
                 done → statusbar update (tokens, cost)
                          ↓
                 If StopReason == "tool_use": loop back to ChatStream
                 If StopReason == "end_turn": done
```

### MCP Server
```
Claude Code / Cursor → stdio JSON-RPC → mcpserver
                                          ↓
                                 ghost_memory_search → store.SearchFTS()
                                 ghost_memory_save → store.Upsert()
                                 ghost_project_context → store.GetTopMemories()
                                          ↓
                                 SQLite query (no LLM calls)
```

## Cost Optimization

### Prompt Caching
- Blocks 1+2 marked `cache_control: {"type": "ephemeral"}` (5min TTL)
- First request: 1.25x cost (cache write premium)
- Subsequent requests: 0.1x cost on cached blocks (~90% savings)
- In a 10-call agentic loop: 9 cache hits × ~2000 tokens = 18,000 tokens at 90% off

### Local Embeddings
- `nomic-embed-text:v1.5` via Ollama (274MB, CPU)
- Hybrid search: 70% vector + 30% FTS5, Reciprocal Rank Fusion (k=60)
- Falls back to FTS5-only if Ollama offline

### Cost Tracking
- `token_usage` table: per-request input/output/cache_create/cache_read/cost_usd
- `audit_log` table: per-action cost attribution
- Status bar shows cumulative session cost

## Configuration Layers

```
1. Compiled defaults
2. /etc/ghost/config.yaml
3. ~/.config/ghost/config.yaml
4. .ghost/config.yaml (per-project, checked in)
5. .ghost/config.local.yaml (per-project, gitignored)
6. GHOST_* environment variables
7. CLI flags
```

## Build

```bash
# Pure Go — no CGO required (modernc.org/sqlite with FTS5 built-in)
go build -o ghost ./cmd/ghost

# Release (goreleaser)
# Targets: linux/{amd64,arm64}, darwin/{amd64,arm64}
# ldflags: -s -w -X main.version={{.Version}}
```

## SQLite Schema

| Table | Purpose |
|-------|---------|
| `projects` | Project registry (id, path, name) |
| `memories` | Core memory store (category, content, importance, tags) |
| `memories_fts` | FTS5 virtual table (porter unicode61 tokenizer) |
| `memory_embeddings` | Vector embeddings (float32 blob) |
| `conversations` | Conversation sessions |
| `messages` | Conversation messages (role, content, tool metadata) |
| `ghost_state` | Per-project state (interaction count, learned context) |
| `token_usage` | Per-request token + cost tracking |
| `audit_log` | Action audit trail |
| `notifications` | GitHub notifications (priority P0-P4) |
| `scheduled_jobs` | Persistent cron jobs |
| `reminders` | One-shot reminders with due_at |
