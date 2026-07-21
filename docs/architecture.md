# Ghost Architecture

## Runtime

Ghost runs as a single binary with one primary mode:

```
ghost mcp              MCP server on stdio (used by Claude Code, Cursor, Goose)
ghost mcp init         Configure Claude Code integration
ghost mcp status       Health check
ghost hook session-start   SessionStart hook (called by Claude Code)
ghost hook stop            Stop hook — save-nudge, blocks stop once (called by Claude Code)
ghost reflect <project>    Manual memory consolidation
ghost supersede <project>  LLM-classified 'supersedes' link creation
ghost obsidian export|sync One-way Markdown vault mirror
ghost bench [--sweep]      Retrieval-quality benchmark
ghost upgrade          Self-update from GitHub Releases (sha256-verified)
ghost version          Print version
```

## Package Map

```
cmd/ghost/main.go          CLI entrypoint + subcommand dispatch
internal/
  ai/                      Claude API client (used by reflection and supersede)
    client.go              HTTP client, Reflect(), CountTokens() — non-streaming
    models.go              Message, ContentBlock, SystemBlock, TokenUsage
    cost.go                Per-model pricing, CostForUsage()
  config/                  Layered configuration (koanf)
    config.go              Config struct, Load(), EnsureConfigFile()
    config.example.yaml    Annotated defaults
  embedding/               Local vector embeddings
    client.go              Ollama HTTP client (/api/embed)
    worker.go              Async batch embedder
  linking/                 Memory auto-linking
    worker.go              Sweeps embedded memories, links cosine neighbors ≥ threshold
  supersede/               ghost supersede — 'supersedes' link creation
    supersede.go           Candidate selection (cosine proposes, created_at directs), Run()
    haiku.go               LLM classifier confirming genuine replacements
  bench/                   ghost bench — retrieval-quality benchmark harness
    dataset.go             JSONL dataset loading + seeding with embedding fixtures
    runner.go              Graded conditions (fts/vector/hybrid)
    metrics.go             Recall@k, MRR, NDCG
    sweep.go               Fusion-parameter grid search
    staleness.go           Fresh-fact-wins suite (supersede demote proof)
    recencytrap.go         Older-answer-correct suite (recency-prior frontier)
  memory/                  Persistence layer
    store.go               SQLite CRUD, FTS5 search, time-decay scoring
    schema.go              DDL (embedded Go string constant — the single source of truth)
    vector.go              Cosine similarity, hybrid RRF search
    links.go               Memory links: edge CRUD (related/supersedes)
  mcpserver/               MCP server (stdio transport)
    mcpserver.go           18 tools + 4 resources via go-sdk
  mcpinit/                 Claude Code integration setup
    init.go                ghost mcp init — registers server, imports memories, writes redirects
    status.go              ghost mcp status — health check
    hook.go                ghost hook session-start — injects project context
    stophook.go            ghost hook stop — save-nudge, blocks stop once when nothing was saved
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
                          ... 18 tools total
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

linking.Worker goroutine:
  every 2min → store.UnscannedEmbeddedMemoryIDs()
             → store.SearchVector(own embedding)   # top cosine neighbors
             → store.CreateLink(≥ threshold, 'related')
             → store.MarkLinkScanned()
  Links cascade-delete with memories and are rebuilt after reflection
  rewrites them — same self-healing lifecycle as embeddings.
```

A link-graph expansion bonus was evaluated and removed (dominated by a deeper vector-k; links and the vector leg are both cosine). The memory_links graph is retained for Obsidian export and supersedes ranking.

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
| `memory_links` | Memory graph edges (related/supersedes/contradicts/elaborates/causes; soft-invalidated, cascade-delete) |
| `link_scans` | Tracks which embedded memories the linking worker has scanned |
| `memory_snapshots` | Pre-replace backups consumed by `ghost reflect --restore` |
| `audit_log` | Append-only record of destructive/consolidation operations |

The schema lives solely in `internal/memory/schema.go` (embedded Go constant).
Note that `CREATE TABLE IF NOT EXISTS` never migrates an existing database —
schema changes only reach databases created after the change.

## Time-Decay Scoring

Memories are scored by `importance × decay_factor × pinned_boost`, where
`decay_factor = max(floor, 1 / (1 + age_days / scale))`:

| Category | Scale (half-life) | Floor |
|----------|-------------------|-------|
| preference, convention, fact | none (no decay) | — |
| architecture, pattern | 45-day | 0.3 |
| decision, gotcha, dependency | 30-day | 0.15 |

Pinned memories get a 1.5× boost (`pinned_boost`) on top of their decayed score —
they do **not** bypass decay, so a sufficiently stale pinned memory can still rank
below a fresh unpinned one. See `GetTopMemories` in `internal/memory/store.go`.

## Build

```bash
# Pure Go — no CGO (modernc.org/sqlite with FTS5 built-in)
go build -o ghost ./cmd/ghost

# Release (goreleaser — triggered by git tag)
# Targets: linux/{amd64,arm64}, darwin/{amd64,arm64}, windows/{amd64,arm64}
# ldflags: -s -w -X main.version={{.Version}}
```
