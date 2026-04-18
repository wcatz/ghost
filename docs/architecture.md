# Ghost Architecture

## Runtime

Ghost runs as a single binary with one primary mode:

```
ghost mcp              MCP server on stdio (used by Claude Code, Cursor, Goose)
ghost mcp init         Configure Claude Code integration
ghost mcp status       Health check
ghost hook session-start   SessionStart hook (called by Claude Code)
ghost reflect <project>    Manual memory consolidation
ghost upgrade          Self-update from GitHub Releases
ghost version          Print version
```

## Package Map

```
cmd/ghost/main.go          CLI entrypoint + subcommand dispatch
internal/
  ai/                      Claude API client (used by reflection only)
    client.go              HTTP client, ChatStream(), Reflect()
    stream.go              SSE parser, StreamEvent channel
    models.go              Message, ContentBlock, SystemBlock, TokenUsage
    cost.go                Per-model pricing, CostForUsage()
  config/                  Layered configuration (koanf)
    config.go              Config struct, Load(), EnsureConfigFile()
    config.example.yaml    Annotated defaults
  embedding/               Local vector embeddings
    client.go              Ollama HTTP client (/api/embed)
    worker.go              Async batch embedder
  memory/                  Persistence layer
    store.go               SQLite CRUD, FTS5 search, time-decay scoring
    schema.go              DDL (embedded via go:embed)
    vector.go              Cosine similarity, hybrid RRF search
  mcpserver/               MCP server (stdio transport)
    mcpserver.go           16 tools + 4 resources via go-sdk
  mcpinit/                 Claude Code integration setup
    init.go                ghost mcp init — registers server, imports memories, writes redirects
    status.go              ghost mcp status — health check
    hook.go                ghost hook session-start — injects project context
  claudeimport/            One-time import of Claude Code auto-memory files
    import.go              Scans ~/.claude/projects/*/memory/*.md, upserts into Ghost
  reflection/              Memory consolidation
    consolidator.go        Consolidator interface + TieredConsolidator
    tier_haiku.go          Haiku LLM consolidation (requires ANTHROPIC_API_KEY)
    tier_sqlite.go         Local Jaccard similarity consolidation (free, always available)
    prompt.go              BuildReflectionPrompt()
  provider/                Interface contracts
    provider.go            LLMProvider, MemoryStore
  selfupdate/              Self-update from GitHub releases
    selfupdate.go          LatestRelease, Download, ExtractBinary, Replace
```

## Data Flow

### MCP Server (primary mode)
```
Claude Code / Cursor → stdio JSON-RPC → mcpserver
                                          ↓
                        Tools (pull-based, Claude must call):
                          ghost_memory_search → store.SearchHybrid() or SearchFTS()
                          ghost_memory_save   → store.Upsert()
                          ghost_project_context → store.GetTopMemories()
                          ghost_save_global   → store.Upsert("_global")
                          ghost_task_create/update/complete → store.CreateTask()...
                          ghost_decision_record → store.RecordDecision()
                          ghost_health        → store metadata query
                          ... 16 tools total
                                          ↓
                        Resources (pinnable, survive context compaction):
                          ghost://project/{id}/context   → GetTopMemories + GetLearnedContext
                          ghost://project/{id}/decisions → ListDecisions
                          ghost://project/{id}/tasks     → ListTasks
                          ghost://memories/global        → GetTopMemories("_global")
                                          ↓
                               SQLite (no LLM calls in hot path)
```

### SessionStart Hook
```
Claude Code session opens
  → ghost hook session-start (stdin: JSON with cwd + projectPath)
  → lookupProject(db, cwd)           # path-prefix match OR name fallback
  → buildProjectContext(store, id)   # top memories + tasks + decisions + globals
  → writes markdown to stdout
  → Claude Code injects into system prompt
```

### Memory Consolidation (ghost reflect)
```
ghost reflect <project> --apply
  → store.GetAll()           # existing memories
  → store.GetRecentExchanges() # recent conversation history
  → TieredConsolidator.Consolidate()
      → HaikuConsolidator (if ANTHROPIC_API_KEY set)
          → Anthropic API (haiku model)
      → SQLiteConsolidator (fallback)
          → Jaccard token similarity, merge >50% overlap
  → quality gate: reject if < 30% of existing memories returned
  → store.ReplaceNonManual()  # atomic replace of non-manual memories
  → store.UpdateLearnedContext()
```

## Embedding (optional, Ollama)

```
embedding.Worker goroutine:
  every 2min → store.UnembeddedMemoryIDs()
             → embedding.Client.Embed(content)  # Ollama /api/embed
             → store.StoreEmbedding(id, vec)
             
Search with embeddings enabled:
  store.SearchHybrid() → 70% vector (cosine) + 30% FTS5, RRF fusion (k=60)
  
Search without embeddings:
  store.SearchFTS() → FTS5 only (porter unicode61 tokenizer)
```

## SQLite Schema

| Table | Purpose |
|-------|---------|
| `projects` | Project registry (id, path, name) |
| `memories` | Core store (category, content, importance, tags, source, pinned) |
| `memories_fts` | FTS5 virtual table (porter unicode61 tokenizer) |
| `memory_embeddings` | Vector embeddings (float32 blob) |
| `conversations` | Conversation sessions (project, mode, timestamps) |
| `messages` | Conversation messages (role, content) |
| `ghost_state` | Per-project state (interaction count, learned context) |
| `token_usage` | Per-request token + cost tracking |
| `tasks` | Task tracker (title, status, priority, description) |
| `decisions` | Architectural decisions (rationale, alternatives, status) |
| `notifications` | GitHub notifications (P0-P4 priority) |
| `scheduled_jobs` | Persistent cron jobs |
| `reminders` | One-shot reminders |

## Time-Decay Scoring

Memories are scored by `importance × decay_factor` where:

| Category | Half-life |
|----------|-----------|
| preference, convention, fact | none (no decay) |
| architecture, pattern | 45-day |
| decision, gotcha, dependency | 30-day |

Pinned memories bypass decay entirely and always rank first.

## Build

```bash
# Pure Go — no CGO (modernc.org/sqlite with FTS5 built-in)
go build -o ghost ./cmd/ghost

# Release (goreleaser — triggered by git tag)
# Targets: linux/{amd64,arm64}, darwin/{amd64,arm64}, windows/{amd64,arm64}
# ldflags: -s -w -X main.version={{.Version}}
```
